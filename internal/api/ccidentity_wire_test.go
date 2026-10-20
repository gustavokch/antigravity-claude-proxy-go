package api

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

var (
	billingPromptRE = regexp.MustCompile(`cc_prompt_id=([0-9a-f-]{36});`)
	billingTurnRE   = regexp.MustCompile(`cc_turn_index=(\d+);`)
)

// promptWire is what one custom-endpoint request showed the upstream about its
// place in the conversation.
type promptWire struct {
	headerPromptID  string
	billingPromptID string
	turnIndex       string
}

// promptUpstream is a custom-endpoint target that remembers the prompt fields of
// the last request it served.
type promptUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	last promptWire
}

func newPromptUpstream(t *testing.T) *promptUpstream {
	t.Helper()
	upstream := &promptUpstream{}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		text := string(body)
		wire := promptWire{headerPromptID: headerFold(r.Header, "x-claude-code-prompt-id")}
		if m := billingPromptRE.FindStringSubmatch(text); m != nil {
			wire.billingPromptID = m[1]
		}
		if m := billingTurnRE.FindStringSubmatch(text); m != nil {
			wire.turnIndex = m[1]
		}
		upstream.mu.Lock()
		upstream.last = wire
		upstream.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","id":"msg_ok","content":[]}`))
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// send posts one /v1/messages body under a fixed client session and returns the
// prompt fields the upstream saw. Every call carries its whole conversation:
// nothing is stored between calls.
func (u *promptUpstream) send(t *testing.T, handler http.Handler, messages ...string) promptWire {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-custom-model","max_tokens":16,"messages":[`+strings.Join(messages, ",")+`]}`))
	req.Header.Set("x-api-key", "local-key")
	req.Header.Set("X-Claude-Code-Session-Id", "client-session-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last
}

// TestCustomEndpoint_PromptFieldsFollowTheConversation: the capture shows a new
// cc_prompt_id and cc_turn_index per user prompt, repeated across that prompt's
// tool follow-ups, with x-claude-code-prompt-id equal to cc_prompt_id in every
// request.
func TestCustomEndpoint_PromptFieldsFollowTheConversation(t *testing.T) {
	withConfigDir(t)

	upstream := newPromptUpstream(t)
	saveCustomEndpoint(t, upstream.URL+"/v1/messages", nil)
	h := newTestHandler(t, &fakeUpstream{streamData: standardStream()}, "test-proj")

	const (
		first   = `{"role":"user","content":"list the files"}`
		reply   = `{"role":"assistant","content":[{"type":"text","text":"done"}]}`
		second  = `{"role":"user","content":"and the tests"}`
		toolUse = `{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]}`
		result  = `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}`
	)

	one := upstream.send(t, h, first)
	two := upstream.send(t, h, first, reply, second)
	twoAgain := upstream.send(t, h, first, reply, second, toolUse, result)

	for _, tc := range []struct {
		name  string
		got   promptWire
		index string
	}{
		{"first prompt", one, "1"},
		{"second prompt", two, "2"},
		{"second prompt's follow-up", twoAgain, "2"},
	} {
		if tc.got.turnIndex != tc.index {
			t.Errorf("%s: cc_turn_index = %q, want %q", tc.name, tc.got.turnIndex, tc.index)
		}
		if tc.got.billingPromptID == "" || tc.got.headerPromptID != tc.got.billingPromptID {
			t.Errorf("%s: x-claude-code-prompt-id = %q, cc_prompt_id = %q; the capture shows one value in both",
				tc.name, tc.got.headerPromptID, tc.got.billingPromptID)
		}
	}
	if one.billingPromptID == two.billingPromptID {
		t.Errorf("two prompts shared prompt id %s", one.billingPromptID)
	}
	if two.billingPromptID != twoAgain.billingPromptID {
		t.Errorf("a tool follow-up changed its prompt's id: %s then %s", two.billingPromptID, twoAgain.billingPromptID)
	}
}

// TestCustomEndpoint_NormalizedRequestSpeaksHTTP11 pins the other half of the
// wire identity: the real client reaches Anthropic over HTTP/1.1, and a
// transport that offers h2 to an h2-capable server gets h2. The server here
// supports both, so only a transport that declines h2 gets HTTP/1.1.
func TestCustomEndpoint_NormalizedRequestSpeaksHTTP11(t *testing.T) {
	withConfigDir(t)

	var (
		mu           sync.Mutex
		protoMajor   int
		offeredALPN  []string
		handshakeRan bool
	)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		protoMajor = r.ProtoMajor
		mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","id":"msg_ok","content":[]}`))
	}))
	upstream.EnableHTTP2 = true
	upstream.TLS = &tls.Config{
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			mu.Lock()
			offeredALPN = append([]string(nil), hello.SupportedProtos...)
			handshakeRan = true
			mu.Unlock()
			return nil, nil
		},
	}
	upstream.StartTLS()
	t.Cleanup(upstream.Close)

	// The test server's certificate is self-signed: trust it on a copy of the
	// shipped transport, so everything else about that transport is what runs.
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	shipped := customEndpointIdentityTransport
	trusting := shipped.Clone()
	trusting.TLSClientConfig.RootCAs = pool
	customEndpointIdentityTransport = trusting
	t.Cleanup(func() { customEndpointIdentityTransport = shipped })

	saveCustomEndpoint(t, upstream.URL+"/v1/messages", nil)
	h := newTestHandler(t, &fakeUpstream{streamData: standardStream()}, "test-proj")
	if rec := serveCustomEndpointRequest(t, h); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if !handshakeRan {
		t.Fatal("the upstream saw no TLS handshake")
	}
	if protoMajor != 1 {
		t.Errorf("upstream saw HTTP/%d, want HTTP/1.1", protoMajor)
	}
	if len(offeredALPN) != 1 || offeredALPN[0] != "http/1.1" {
		t.Errorf("ClientHello offered ALPN %v, want [http/1.1] as the real client does", offeredALPN)
	}
}
