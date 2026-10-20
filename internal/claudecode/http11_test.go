package claudecode

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"antigravity-go-proxy/internal/ccidentity"
)

// TestDefaultClientSpeaksHTTP11 pins the wire fact the 2.1.304 capture records:
// the real client reaches api.anthropic.com over HTTP/1.1 for every request,
// discovery GETs included, and a transport that offers h2 to api.anthropic.com
// gets h2. The server here supports both, so only a client built the way
// NewClient builds its default one speaks HTTP/1.1 to it.
//
// The shipped transport is cloned and given a trust root for the test
// certificate, and nothing else, so the protocol settings that run are the
// shipped ones.
func TestDefaultClientSpeaksHTTP11(t *testing.T) {
	var (
		mu       sync.Mutex
		protos   = map[string]int{} // "METHOD path" -> HTTP major version
		offered  [][]string
		upstream = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			protos[r.Method+" "+r.URL.Path] = r.ProtoMajor
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
	)
	upstream.EnableHTTP2 = true
	upstream.TLS = &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			mu.Lock()
			offered = append(offered, append([]string(nil), hello.SupportedProtos...))
			mu.Unlock()
			return nil, nil
		},
	}
	upstream.StartTLS()
	t.Cleanup(upstream.Close)

	shipped, ok := NewClient("https://api.anthropic.com", nil).httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("the default client's transport is %T, want *http.Transport", NewClient("https://api.anthropic.com", nil).httpClient.Transport)
	}
	trusting := shipped.Clone()
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	trusting.TLSClientConfig.RootCAs = pool
	t.Cleanup(trusting.CloseIdleConnections)

	client := NewClient(upstream.URL, &http.Client{Transport: trusting})

	resp, err := client.SendMessage(context.Background(), MessageRequest{
		Token:     "sk-ant-oat01-test-oauth-token",
		Body:      []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`),
		Normalize: true,
		Identity:  ccidentity.Identity{SessionKey: "session-abc"},
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	_ = resp.Body.Close()
	if err := client.ValidateAccount(context.Background(), "sk-ant-oat01-test-oauth-token"); err != nil {
		t.Fatalf("ValidateAccount: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, request := range []string{"POST /v1/messages", "GET /v1/models"} {
		if major, seen := protos[request]; !seen {
			t.Errorf("the upstream never saw %s", request)
		} else if major != 1 {
			t.Errorf("%s arrived as HTTP/%d, want HTTP/1.1", request, major)
		}
	}
	for _, alpn := range offered {
		if len(alpn) != 1 || alpn[0] != "http/1.1" {
			t.Errorf("ClientHello offered ALPN %v, want [http/1.1] as the real client does", alpn)
		}
	}
}
