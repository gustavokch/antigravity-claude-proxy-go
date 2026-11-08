package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
)

const countPath = "/v1/messages/count_tokens"

// newCountServer installs cfg and returns a server on the clock now. Its
// account-backed path is a recording backend; the Claude Code pool and the Zen
// keyless warning start fresh, and the config dir is the test's own.
func newCountServer(t *testing.T, cfg config.Config, now func() time.Time) *Server {
	t.Helper()
	t.Setenv("OPENCODE_API_KEY", "")
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", t.TempDir())
	orig := config.Get()
	t.Cleanup(func() { config.SetForTest(orig) })
	config.SetForTest(cfg)
	resetCCPoolForTest()
	t.Cleanup(resetCCPoolForTest)
	resetZenKeylessWarning()
	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &dispatchRecordingBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     now,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server
}

// sendInSession posts body to path with the proxy key apiKey, under the Claude
// Code session header when session is not empty, plus header. It takes no
// *testing.T so a burst can call it from many goroutines.
func sendInSession(handler http.Handler, path, apiKey, session, body string, header http.Header) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", apiKey)
	if session != "" {
		request.Header.Set("X-Claude-Code-Session-Id", session)
	}
	for name, values := range header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// countOf is the count a count_tokens answer carries. The answer must be a 200
// holding input_tokens and nothing else, as Anthropic's does.
func countOf(t *testing.T, recorder *httptest.ResponseRecorder) int {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("count_tokens status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("count_tokens body %q: %v", recorder.Body.String(), err)
	}
	if len(answer) != 1 {
		t.Fatalf("count_tokens body = %s, want only input_tokens", recorder.Body.String())
	}
	var tokens int
	if err := json.Unmarshal(answer["input_tokens"], &tokens); err != nil {
		t.Fatalf("input_tokens in %s: %v", recorder.Body.String(), err)
	}
	return tokens
}

// usageStream is one Cloud Code answer reporting promptTokens of input, cached
// of them read from the prompt cache. promptTokens 0 reports no usage at all.
func usageStream(promptTokens, cached int) [][]byte {
	if promptTokens == 0 {
		return [][]byte{[]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"FORMAT_OK"}]},"finishReason":"STOP"}]}}`)}
	}
	return [][]byte{[]byte(fmt.Sprintf(`{"response":{"candidates":[{"content":{"parts":[{"text":"FORMAT_OK"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":%d,"cachedContentTokenCount":%d,"candidatesTokenCount":2}}}`, promptTokens, cached))}
}

// The estimate builds on what the conversation's last turn really cost: 5,000
// tokens of input, 1,200 of them cache reads. A count of the next turn in the
// same session is that plus a few tokens for the two new messages; the same
// count in another session knows nothing of it. A later turn whose answer
// reported no usage does not erase what the session learned.
func TestCountTokens_CloudCodeEstimateBuildsOnTheConversationsRealCount(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			upstream := &fakeUpstream{streamData: usageStream(5000, 1200)}
			handler := newTestHandler(t, upstream, "managed-project")
			turn := func(messages string) string {
				return fmt.Sprintf(`{"model":"claude-sonnet-4-6","max_tokens":64,"stream":%v,"messages":%s}`, stream, messages)
			}
			if rec := sendInSession(handler, "/v1/messages", "local-key", "sess-count", turn(`[{"role":"user","content":"hello"}]`), nil); rec.Code != http.StatusOK {
				t.Fatalf("first turn: %d %s", rec.Code, rec.Body.String())
			}

			next := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"FORMAT_OK"},{"role":"user","content":"and now?"}]}`
			if got := countOf(t, sendInSession(handler, countPath, "local-key", "sess-count", next, nil)); got < 5000 || got >= 5200 {
				t.Errorf("count in the session = %d, want the real 5000 plus a few tokens for the new messages", got)
			}
			if got := countOf(t, sendInSession(handler, countPath, "local-key", "another-session", next, nil)); got <= 0 || got >= 200 {
				t.Errorf("count in another session = %d, want a small estimate of the text alone", got)
			}

			upstream.mu.Lock()
			upstream.streamData = usageStream(0, 0)
			upstream.mu.Unlock()
			if rec := sendInSession(handler, "/v1/messages", "local-key", "sess-count", turn(`[{"role":"user","content":"hello"},{"role":"assistant","content":"FORMAT_OK"},{"role":"user","content":"and now?"}]`), nil); rec.Code != http.StatusOK {
				t.Fatalf("second turn: %d %s", rec.Code, rec.Body.String())
			}
			later := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"FORMAT_OK"},{"role":"user","content":"and now?"},{"role":"assistant","content":"FORMAT_OK"},{"role":"user","content":"more"}]}`
			if got := countOf(t, sendInSession(handler, countPath, "local-key", "sess-count", later, nil)); got < 5000 || got >= 5200 {
				t.Errorf("count after a turn with no usage = %d, want it still built on the real 5000", got)
			}
			if upstream.streamCalls != 2 {
				t.Errorf("upstream generations = %d, want 2: a count never generates", upstream.streamCalls)
			}
		})
	}
}

// Zen has no count endpoint either. Its real count is input plus cache reads
// plus cache writes of the Anthropic usage, here 3,000 + 1,500 + 500.
func TestCountTokens_ZenEstimateBuildsOnTheConversationsRealCount(t *testing.T) {
	var generations atomic.Int32
	zenUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			generations.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_zen","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"FORMAT_OK"}],"stop_reason":"end_turn","usage":{"input_tokens":3000,"cache_read_input_tokens":1500,"cache_creation_input_tokens":500,"output_tokens":3}}`)
	}))
	t.Cleanup(zenUpstream.Close)
	cfg := config.DefaultConfig()
	cfg.Zen.Enabled = true
	cfg.Zen.BaseURL = zenUpstream.URL
	cfg.Zen.APIKey = "k-zen"
	cfg.Zen.Allowlist = []config.ZenModelConfig{{ID: dispatchModel, Enabled: true}}
	handler := newCountServer(t, cfg, time.Now).Handler()

	first := `{"model":"claude-sonnet-4-6","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`
	if rec := sendInSession(handler, "/v1/messages", "test-proxy-key", "sess-zen", first, nil); rec.Code != http.StatusOK {
		t.Fatalf("Zen turn: %d %s", rec.Code, rec.Body.String())
	}
	next := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"FORMAT_OK"},{"role":"user","content":"and now?"}]}`
	if got := countOf(t, sendInSession(handler, countPath, "test-proxy-key", "sess-zen", next, nil)); got < 5000 || got >= 5200 {
		t.Errorf("count in the session = %d, want the real 5000 plus a few tokens", got)
	}
	if got := countOf(t, sendInSession(handler, countPath, "test-proxy-key", "another-session", next, nil)); got <= 0 || got >= 200 {
		t.Errorf("count in another session = %d, want a small estimate of the text alone", got)
	}
	if n := generations.Load(); n != 1 {
		t.Errorf("Zen messages requests = %d, want 1: Zen counts are estimated, never forwarded", n)
	}
}

func TestCountTokens_RequiresAMessagesArray(t *testing.T) {
	handler := newTestHandler(t, &fakeUpstream{}, "managed-project")
	for _, body := range []string{`{}`, `{"model":"claude-sonnet-4-6","messages":"hi"}`, `not json`} {
		rec := sendInSession(handler, countPath, "local-key", "", body, nil)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_request_error") {
			t.Errorf("body %s: status %d %s, want 400 invalid_request_error", body, rec.Code, rec.Body.String())
		}
	}
}

func TestCountTokens_AnthropicMirrorAnswers(t *testing.T) {
	upstream := &fakeUpstream{streamData: standardStream()}
	handler := newTestHandler(t, upstream, "managed-project")
	body := `{"model":"gemini-3.5-flash","messages":[{"role":"user","content":"hello there"}]}`
	if got := countOf(t, sendInSession(handler, "/anthropic/v1/messages/count_tokens", "local-key", "", body, nil)); got <= 0 {
		t.Errorf("count = %d, want a positive estimate", got)
	}
	if upstream.streamCalls != 0 {
		t.Errorf("upstream generations = %d, want 0", upstream.streamCalls)
	}
}
