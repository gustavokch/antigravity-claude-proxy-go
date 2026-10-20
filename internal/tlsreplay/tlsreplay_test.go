package tlsreplay

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"testing"

	"antigravity-go-proxy/internal/proxydial/proxytest"

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

// greaseNormalized maps every GREASE value to one placeholder: JA4 ignores which
// GREASE value a client picked, and a replay may pick another, but not another
// position.
func greaseNormalized(types []uint16) []uint16 {
	out := slices.Clone(types)
	for i, v := range out {
		if v&0x0f0f == 0x0a0a {
			out[i] = 0x0a0a
		}
	}
	return out
}

// net/http ignores DialTLSContext for an https target behind a proxy: it dials
// the proxy in plain text and runs crypto/tls itself, so without help the
// captured hello is never sent and nothing reports it. The tunnel must be
// opened by the dialer, whatever the proxy speaks (http, https, socks5, socks5h
// are what http.ProxyFromEnvironment returns), and the captured hello must come
// out of it.
func TestHelloIsReplayedThroughEveryProxyScheme(t *testing.T) {
	for _, tc := range []string{"http", "https", "socks5", "socks5h"} {
		t.Run(tc, func(t *testing.T) {
			backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
			listener := &recordingListener{Listener: backend.Listener}
			backend.Listener = listener
			backend.StartTLS()
			t.Cleanup(backend.Close)

			var (
				proxyURL *url.URL
				check    func()
			)
			pool := x509.NewCertPool()
			pool.AddCert(backend.Certificate())
			user := url.UserPassword("user", "pass")
			_, port, _ := net.SplitHostPort(backend.Listener.Addr().String())
			switch tc {
			case "http", "https":
				proxy := proxytest.NewHTTPProxy(t, tc == "https", backend.Listener.Addr().String())
				if cert := proxy.Certificate(); cert != nil {
					pool.AddCert(cert)
				}
				proxyURL = proxy.ProxyURL(user)
				check = func() {
					connects := proxy.Connects()
					if len(connects) != 1 || connects[0].Target != "example.com:"+port || connects[0].Auth != "Basic dXNlcjpwYXNz" {
						t.Errorf("CONNECTs = %+v, want one for example.com:%s with the proxy URL's credentials", connects, port)
					}
				}
			default:
				socks := proxytest.NewSOCKS5(t, true, backend.Listener.Addr().String())
				proxyURL = &url.URL{Scheme: tc, Host: socks.Addr, User: user}
				check = func() {
					requests := socks.Requests()
					if len(requests) != 1 || requests[0].Target != "example.com:"+port || requests[0].Cred != "user:pass" {
						t.Errorf("SOCKS requests = %+v, want one for example.com:%s with the proxy URL's credentials", requests, port)
					}
				}
			}

			tr := MustNew(readCapture(t), []string{"http/1.1"}).Transport(&http.Transport{
				Proxy:           http.ProxyURL(proxyURL),
				TLSClientConfig: &tls.Config{RootCAs: pool},
			})
			t.Cleanup(tr.CloseIdleConnections)

			// example.com is on the httptest certificate; the proxy ignores the
			// target and tunnels to the backend either way.
			resp, err := (&http.Client{Transport: tr}).Get("https://example.com:" + port + "/")
			if err != nil {
				t.Fatalf("GET through the proxy: %v", err)
			}
			_ = resp.Body.Close()
			check()

			listener.mu.Lock()
			defer listener.mu.Unlock()
			if len(listener.hellos) != 1 {
				t.Fatalf("the backend saw %d hellos, want 1", len(listener.hellos))
			}
			got, want := greaseNormalized(extensionTypes(listener.hellos[0])), greaseNormalized(extensionTypes(readCapture(t)))
			if !slices.Equal(got, want) {
				t.Errorf("extensions behind the proxy = %x, want the capture's %x: the stdlib hello went through instead", got, want)
			}
		})
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
