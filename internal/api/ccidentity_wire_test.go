package api

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"antigravity-go-proxy/internal/ccidentity"
	"antigravity-go-proxy/internal/claudecode"
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

	// The test server's certificate is self-signed: trust it on the shipped
	// transport, so everything else about that transport is what runs. The
	// replayed handshake reads its trust roots from the transport at dial time.
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	customEndpointIdentityTransport.TLSClientConfig = &tls.Config{RootCAs: pool}
	t.Cleanup(func() {
		customEndpointIdentityTransport.TLSClientConfig = nil
		customEndpointIdentityTransport.CloseIdleConnections()
	})

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

var billingPrevRE = regexp.MustCompile(`cc_prev_req=([^;]*);`)

// replyUpstream answers request n with a reply holding tool call toolu_up_n
// under request id req_up_n, and remembers the cc_prev_req each request sent.
type replyUpstream struct {
	*httptest.Server
	mu     sync.Mutex
	served int
	prev   []string
}

func newReplyUpstream(t *testing.T, stream bool) *replyUpstream {
	t.Helper()
	upstream := &replyUpstream{}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstream.mu.Lock()
		upstream.served++
		n := upstream.served
		prev := ""
		if m := billingPrevRE.FindSubmatch(body); m != nil {
			prev = string(m[1])
		}
		upstream.prev = append(upstream.prev, prev)
		upstream.mu.Unlock()

		w.Header().Set("request-id", fmt.Sprintf("req_up_%d", n))
		toolUse := fmt.Sprintf(`{"type":"tool_use","id":"toolu_up_%d","name":"Bash","input":{}}`, n)
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"type":"message","id":"msg_%d","role":"assistant","content":[%s]}`, n, toolUse)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_%d\",\"content\":[]}}\n\n"+
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":%s}\n\n"+
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", n, toolUse)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func (u *replyUpstream) prevSent() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.prev...)
}

// toolFollowUp is a conversation whose latest assistant message echoes the tool
// call of reply n.
func toolFollowUp(n int) string {
	return fmt.Sprintf(`{"model":"%%s","max_tokens":16,"messages":[`+
		`{"role":"user","content":"list the files"},`+
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_up_%d","name":"Bash","input":{}}]},`+
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_up_%d","content":"a.go"}]}]}`, n, n)
}

const firstPrompt = `{"model":"%s","max_tokens":16,"messages":[{"role":"user","content":"list the files"}]}`

// postInSession sends a /v1/messages body under sessionID, or under no session
// when it is empty, and fails the test unless it is answered 200.
func postInSession(t *testing.T, serve func(http.ResponseWriter, *http.Request), sessionID, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "local-key")
	if sessionID != "" {
		req.Header.Set("X-Claude-Code-Session-Id", sessionID)
	}
	rec := httptest.NewRecorder()
	serve(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// The capture shows cc_prev_req on every follow-up, naming the request-id of
// the request whose reply the follow-up continues, and on no first request.
func TestCustomEndpoint_FollowUpNamesThePreviousRequest(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			withConfigDir(t)
			upstream := newReplyUpstream(t, stream)
			saveCustomEndpoint(t, upstream.URL+"/v1/messages", nil)
			h := newTestHandler(t, &fakeUpstream{streamData: standardStream()}, "test-proj")
			model := "claude-custom-model"

			postInSession(t, h.ServeHTTP, "session-1", fmt.Sprintf(firstPrompt, model))
			postInSession(t, h.ServeHTTP, "session-1", fmt.Sprintf(toolFollowUp(1), model))
			postInSession(t, h.ServeHTTP, "session-2", fmt.Sprintf(toolFollowUp(1), model))
			postInSession(t, h.ServeHTTP, "", fmt.Sprintf(toolFollowUp(1), model))

			want := []string{"", "req_up_1", "", ""}
			got := upstream.prevSent()
			for i, name := range []string{"first request", "follow-up", "another session", "a request with no session"} {
				if i >= len(got) || got[i] != want[i] {
					t.Errorf("%s: cc_prev_req = %q, want %q (all: %q)", name, at(got, i), want[i], got)
				}
			}
		})
	}
}

func TestForwardToClaudeCode_FollowUpNamesThePreviousRequest(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			upstream := newReplyUpstream(t, stream)
			cfg := claudecode.Config{
				Enabled:   true,
				BaseURL:   upstream.URL,
				Mode:      "pool",
				Accounts:  []claudecode.AccountConfig{{ID: "acc1", Token: "sk-ant-oat01-test-token", Enabled: true}},
				Allowlist: claudecode.DefaultAllowlist(),
				Routing:   claudecode.DefaultRoutingConfig(),
			}
			ccPoolMu.Lock()
			ccPoolInst = nil
			ccHTTPClient = nil
			ccPoolMu.Unlock()

			srv := &Server{ccReplies: ccidentity.NewReplyLog()}
			model := "claude-sonnet-5"
			serve := func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				srv.forwardToClaudeCode(w, r, cfg, body, model)
			}
			postInSession(t, serve, "session-1", fmt.Sprintf(firstPrompt, model))
			postInSession(t, serve, "session-1", fmt.Sprintf(toolFollowUp(1), model))
			postInSession(t, serve, "session-1", fmt.Sprintf(toolFollowUp(2), model))

			want := []string{"", "req_up_1", "req_up_2"}
			got := upstream.prevSent()
			for i := range want {
				if at(got, i) != want[i] {
					t.Errorf("request %d: cc_prev_req = %q, want %q (all: %q)", i+1, at(got, i), want[i], got)
				}
			}
		})
	}
}

func at(values []string, i int) string {
	if i < len(values) {
		return values[i]
	}
	return "<not sent>"
}
