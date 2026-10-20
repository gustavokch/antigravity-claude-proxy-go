package tlsreplay

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"sync"
	"testing"

	utls "github.com/refraction-networking/utls"
)

const fetchCapture = "../transport/claude-code-fetch-clienthello.bin"

func readCapture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(fetchCapture)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// JA4 counts ALPN protocols instead of naming them, so a recapture that offered
// h2 would pass a fingerprint gate while the custom-dial path of net/http cannot
// serve HTTP/2. New must refuse any capture whose list is not the one asked for.
func TestNewChecksTheCapturedALPN(t *testing.T) {
	raw := readCapture(t) // offers [http/1.1]
	cases := []struct {
		name    string
		want    []string
		wantErr bool
	}{
		{"exact", []string{"http/1.1"}, false},
		{"capture offers more than asked", nil, true},
		{"capture offers something else", []string{"h2"}, true},
		{"two asked, one offered", []string{"h2", "http/1.1"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(raw, tc.want)
			if tc.wantErr != (err != nil) {
				t.Errorf("New(want %v) error = %v, wantErr %v", tc.want, err, tc.wantErr)
			}
		})
	}
	if _, err := New([]byte("not a hello"), nil); err == nil {
		t.Error("garbage was accepted as a ClientHello")
	}
}

// A spec carries per-handshake state (the key shares utls generates), so a Hello
// reused across connections must still complete every handshake, not only the
// first. A client that fails its second connection looks like an outage.
func TestHelloCompletesRepeatedHandshakes(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(server.Close)
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())

	hello := MustNew(readCapture(t), []string{"http/1.1"})
	for i := range 3 {
		tr := hello.Transport(&http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}})
		resp, err := (&http.Client{Transport: tr}).Get(server.URL)
		if err != nil {
			t.Fatalf("connection %d: %v", i+1, err)
		}
		_ = resp.Body.Close()
		tr.CloseIdleConnections()
	}
}

func TestSpecALPNReadsTheExtension(t *testing.T) {
	spec := &utls.ClientHelloSpec{Extensions: []utls.TLSExtension{&utls.ALPNExtension{AlpnProtocols: []string{"h2", "http/1.1"}}}}
	if got := specALPN(spec); !slices.Equal(got, []string{"h2", "http/1.1"}) {
		t.Errorf("specALPN = %v", got)
	}
	if got := specALPN(&utls.ClientHelloSpec{}); got != nil {
		t.Errorf("specALPN without the extension = %v, want nil", got)
	}
}

// connectProxy is an HTTP proxy that tunnels CONNECT to a fixed backend and
// records what it was asked for.
type connectProxy struct {
	*httptest.Server
	mu       sync.Mutex
	targets  []string
	authHdrs []string
}

func newConnectProxy(t *testing.T, backend string) *connectProxy {
	t.Helper()
	p := &connectProxy{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		p.mu.Lock()
		p.targets = append(p.targets, r.Host)
		p.authHdrs = append(p.authHdrs, r.Header.Get("Proxy-Authorization"))
		p.mu.Unlock()
		upstream, err := net.Dial("tcp", backend)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		_, _ = client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		go func() { _, _ = io.Copy(upstream, client); _ = upstream.Close() }()
		go func() { _, _ = io.Copy(client, upstream); _ = client.Close() }()
	}))
	t.Cleanup(p.Close)
	return p
}

// net/http ignores DialTLSContext for an https target behind a proxy: it dials
// the proxy in plain text and runs crypto/tls itself, so without help the
// captured hello is never sent and nothing reports it. The tunnel must be
// opened by the dialer, and the hello must come out of it.
func TestHelloIsReplayedThroughAnHTTPProxy(t *testing.T) {
	var (
		mu   sync.Mutex
		alpn []string
	)
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	backend.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		mu.Lock()
		alpn = append([]string(nil), hello.SupportedProtos...)
		mu.Unlock()
		return nil, nil
	}}
	backend.StartTLS()
	t.Cleanup(backend.Close)

	proxy := newConnectProxy(t, backend.Listener.Addr().String())
	proxyURL, _ := url.Parse(proxy.URL)
	proxyURL.User = url.UserPassword("user", "pass")

	pool := x509.NewCertPool()
	pool.AddCert(backend.Certificate())
	base := &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}
	tr := MustNew(readCapture(t), []string{"http/1.1"}).Transport(base)
	t.Cleanup(tr.CloseIdleConnections)

	// example.com is on the httptest certificate; the proxy ignores the target
	// and tunnels to the backend either way.
	_, port, _ := net.SplitHostPort(backend.Listener.Addr().String())
	resp, err := (&http.Client{Transport: tr}).Get("https://example.com:" + port + "/")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	_ = resp.Body.Close()

	proxy.mu.Lock()
	defer proxy.mu.Unlock()
	if want := "example.com:" + port; !slices.Equal(proxy.targets, []string{want}) {
		t.Errorf("CONNECT targets = %v, want [%s]", proxy.targets, want)
	}
	if got := proxy.authHdrs[0]; got != "Basic dXNlcjpwYXNz" {
		t.Errorf("Proxy-Authorization = %q, want the proxy URL's credentials", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(alpn, []string{"http/1.1"}) {
		t.Errorf("the backend saw ALPN %v, want the captured [http/1.1]: the stdlib hello went through instead", alpn)
	}
}

// A proxy that refuses the tunnel is an error, not a plain-text fallback.
func TestProxyRefusalIsAnError(t *testing.T) {
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusForbidden)
	}))
	t.Cleanup(refuse.Close)
	proxyURL, _ := url.Parse(refuse.URL)

	tr := MustNew(readCapture(t), []string{"http/1.1"}).Transport(&http.Transport{Proxy: http.ProxyURL(proxyURL)})
	if _, err := (&http.Client{Transport: tr}).Get("https://localhost:1/"); err == nil {
		t.Error("a refused CONNECT produced no error")
	}
}

func TestUnsupportedProxySchemeIsAnError(t *testing.T) {
	proxyURL, _ := url.Parse("socks5://127.0.0.1:1080")
	tr := MustNew(readCapture(t), []string{"http/1.1"}).Transport(&http.Transport{Proxy: http.ProxyURL(proxyURL)})
	if _, err := (&http.Client{Transport: tr}).Get("https://localhost:1/"); err == nil {
		t.Error("a SOCKS proxy was accepted and silently bypassed")
	}
}
