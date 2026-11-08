package transport

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// codexServer starts a TLS server that speaks HTTP/2 when h2 is true and
// HTTP/1.1 only otherwise, counting the connections it accepts.
func codexServer(t *testing.T, h2 bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	}))
	srv.EnableHTTP2 = h2
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, &conns
}

// codexClient is Codex() trusting the test server's certificate.
func codexClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()
	tr, ok := Codex().(*http.Transport)
	if !ok {
		t.Fatalf("Codex() is %T, want *http.Transport", Codex())
	}
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	tr.TLSClientConfig = &tls.Config{RootCAs: roots}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func getProto(t *testing.T, client *http.Client, url string) (string, *http.Response) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body), resp
}

// TestCodexSpeaksHTTP2WhenALPNSelectsIt proves the Chrome-dialed connection is
// handed to the HTTP/2 client when the server picks h2, and that concurrent
// requests share it.
func TestCodexSpeaksHTTP2WhenALPNSelectsIt(t *testing.T) {
	srv, conns := codexServer(t, true)
	client := codexClient(t, srv)

	proto, resp := getProto(t, client, srv.URL)
	if proto != "HTTP/2.0" || resp.ProtoMajor != 2 {
		t.Fatalf("server saw %s, client %s; want HTTP/2 after ALPN h2", proto, resp.Proto)
	}
	done := make(chan struct{})
	for range 4 {
		go func() {
			defer func() { done <- struct{}{} }()
			getProto(t, client, srv.URL)
		}()
	}
	for range 4 {
		<-done
	}
	if n := conns.Load(); n != 1 {
		t.Errorf("5 requests opened %d connections, want one pooled HTTP/2 connection", n)
	}
}

// TestCodexFallsBackToHTTP11 covers a server (or TLS-inspecting proxy) that
// selects http/1.1: the same dialer works, and the connection is kept alive.
func TestCodexFallsBackToHTTP11(t *testing.T) {
	srv, conns := codexServer(t, false)
	client := codexClient(t, srv)

	for range 3 {
		if proto, _ := getProto(t, client, srv.URL); proto != "HTTP/1.1" {
			t.Fatalf("server saw %s, want HTTP/1.1", proto)
		}
	}
	if n := conns.Load(); n != 1 {
		t.Errorf("3 sequential requests opened %d connections, want the first reused", n)
	}
}

// TestCodexSendsChromeHello checks the handshake itself: Chrome offers both h2
// and http/1.1 and GREASE values, which Go's crypto/tls never sends.
func TestCodexSendsChromeHello(t *testing.T) {
	hello := make(chan *tls.ClientHelloInfo, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
		select {
		case hello <- info:
		default:
		}
		return nil, nil // keep the server's own config
	}}
	srv.StartTLS()
	defer srv.Close()
	client := codexClient(t, srv)
	getProto(t, client, srv.URL)

	info := <-hello
	if len(info.SupportedProtos) != 2 || info.SupportedProtos[0] != "h2" || info.SupportedProtos[1] != "http/1.1" {
		t.Errorf("ALPN = %v, want Chrome's [h2 http/1.1]", info.SupportedProtos)
	}
	grease := false
	for _, s := range info.CipherSuites {
		if s&0x0f0f == 0x0a0a {
			grease = true
		}
	}
	if !grease {
		t.Errorf("cipher suites %x carry no GREASE value; this is not Chrome's hello", info.CipherSuites)
	}
}

func TestCodexProxyFallsBackToAllProxy(t *testing.T) {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "ALL_PROXY", "all_proxy", "REQUEST_METHOD"} {
		t.Setenv(name, "")
	}
	target := func(host string) *http.Request {
		req, _ := http.NewRequest(http.MethodPost, "https://"+host+"/backend-api/codex/responses", nil)
		return req
	}
	proxyFor := func(host string) string {
		u, err := codexProxy()(target(host))
		if err != nil {
			t.Fatal(err)
		}
		if u == nil {
			return ""
		}
		return u.String()
	}

	if got := proxyFor("chatgpt.com"); got != "" {
		t.Errorf("no proxy variables: proxy %q", got)
	}
	t.Setenv("ALL_PROXY", "socks5://socks.example:1080")
	if got := proxyFor("chatgpt.com"); got != "socks5://socks.example:1080" {
		t.Errorf("ALL_PROXY only: proxy %q", got)
	}
	t.Setenv("HTTPS_PROXY", "http://corp.example:3128")
	if got := proxyFor("chatgpt.com"); got != "http://corp.example:3128" {
		t.Errorf("HTTPS_PROXY and ALL_PROXY: proxy %q, want HTTPS_PROXY", got)
	}
	t.Setenv("NO_PROXY", "chatgpt.com")
	if got := proxyFor("chatgpt.com"); got != "" {
		t.Errorf("NO_PROXY match: proxy %q, want none", got)
	}
}
