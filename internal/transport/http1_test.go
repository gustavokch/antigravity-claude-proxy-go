package transport

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// h2Server is a TLS server that speaks both HTTP/2 and HTTP/1.1 and records what
// each client offered and got.
type h2Server struct {
	*httptest.Server
	mu         sync.Mutex
	alpn       []string
	protoMajor int
}

func newH2Server(t *testing.T) *h2Server {
	t.Helper()
	s := &h2Server{}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.protoMajor = r.ProtoMajor
		s.mu.Unlock()
	}))
	s.EnableHTTP2 = true
	s.TLS = &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			s.mu.Lock()
			s.alpn = append([]string(nil), hello.SupportedProtos...)
			s.mu.Unlock()
			return nil, nil
		},
	}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}

// get issues one GET with tr, trusting the server's self-signed certificate, and
// returns the HTTP major version the server saw and the ALPN list the client
// offered. tr is changed only by adding that trust.
func (s *h2Server) get(t *testing.T, tr *http.Transport) (protoMajor int, alpn []string) {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{}
	}
	tr.TLSClientConfig.RootCAs = pool
	t.Cleanup(tr.CloseIdleConnections)

	resp, err := (&http.Client{Transport: tr}).Get(s.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.protoMajor, s.alpn
}

// TestUpstreamHTTP1DeclinesH2 is the behaviour the Claude Code wire identity
// needs: against a server that would happily speak h2, this transport speaks
// HTTP/1.1 and says so in its ALPN offer. The control leg proves the server
// really would have given h2, so the assertion cannot pass by the server being
// unable to.
func TestUpstreamHTTP1DeclinesH2(t *testing.T) {
	control := newH2Server(t)
	if major, alpn := control.get(t, Upstream()); major != 2 {
		t.Fatalf("control: the default upstream transport got HTTP/%d (ALPN %v), want h2; this test would prove nothing", major, alpn)
	}

	server := newH2Server(t)
	major, alpn := server.get(t, UpstreamHTTP1())
	if major != 1 {
		t.Errorf("UpstreamHTTP1 got HTTP/%d, want HTTP/1.1", major)
	}
	if len(alpn) != 1 || alpn[0] != "http/1.1" {
		t.Errorf("ClientHello offered ALPN %v, want [http/1.1]", alpn)
	}
}

// TestUpstreamHTTP1KeepsTheBoundAndLeavesTheDefaultAlone: the helper builds on
// Upstream, so the header bound must survive, and the shared default transport
// must not pick up the HTTP/1.1 restriction.
func TestUpstreamHTTP1KeepsTheBoundAndLeavesTheDefaultAlone(t *testing.T) {
	tr := UpstreamHTTP1()
	if tr.ResponseHeaderTimeout != ResponseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, ResponseHeaderTimeout)
	}
	if first, second := UpstreamHTTP1(), UpstreamHTTP1(); first == second {
		t.Error("each call must return its own transport")
	}

	server := newH2Server(t)
	if major, _ := server.get(t, Upstream()); major != 2 {
		t.Errorf("Upstream got HTTP/%d after UpstreamHTTP1 was built, want h2: the restriction leaked", major)
	}
}
