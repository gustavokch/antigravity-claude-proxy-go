package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-go-proxy/internal/ccidentity"
	"antigravity-go-proxy/internal/claudecode"
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

// ccCountBody is a count body as Claude Code's /context sends it (messages,
// model, tools) plus a system prompt and spacing the forward must keep: the
// body goes upstream as the client wrote it.
const ccCountBody = `{"model":"claude-sonnet-5", "system":"be brief <ok>","messages":[{"role":"user","content":"hi & bye"}],"tools":[]}`

// testClock is a clock a test moves by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// estimateOf is what server's estimator answers for body: the count a request
// that is not forwarded gets.
func estimateOf(t *testing.T, server *Server, session, model, body string) int {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatal(err)
	}
	return server.tokens.Estimate(session, model, decoded)
}

// countSeen is one count_tokens request an upstream received.
type countSeen struct {
	path, query string
	header      http.Header
	body        string
}

// countUpstream answers every count_tokens request with one status, body and
// header set, and records what it was sent. Other paths (OpenRouter's catalog
// warmup) get 404 and are not recorded.
type countUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	seen []countSeen
}

func newCountUpstream(t *testing.T, status int, answer string, header http.Header) *countUpstream {
	t.Helper()
	upstream := &countUpstream{}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/count_tokens") {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		upstream.mu.Lock()
		upstream.seen = append(upstream.seen, countSeen{path: r.URL.Path, query: r.URL.RawQuery, header: r.Header.Clone(), body: string(body)})
		upstream.mu.Unlock()
		for name, values := range header {
			w.Header()[name] = values
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func (u *countUpstream) requests() []countSeen {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]countSeen(nil), u.seen...)
}

// ccCountAccount is the OAuth pool account of the Claude Code count tests.
var ccCountAccount = claudecode.AccountConfig{ID: "acc1", Token: "sk-ant-oat01-count", Enabled: true}

func newCCCountServer(t *testing.T, upstreamURL string, now func() time.Time) (*Server, config.Config) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.ClaudeCode = ccPoolConfig(upstreamURL, ccCountAccount)
	return newCountServer(t, cfg, now), cfg
}

// ccCountAccountState is the pool's view of ccCountAccount.
func ccCountAccountState(t *testing.T, server *Server, cfg config.Config) claudecode.AccountSnapshot {
	t.Helper()
	pool, _ := server.getOrCreateCCPool(cfg.ClaudeCode)
	acc, ok := pool.GetAccount(ccCountAccount.ID)
	if !ok {
		t.Fatalf("pool has no account %q", ccCountAccount.ID)
	}
	return acc.Snapshot()
}

// The Claude Code count goes out with the count identity and the client's own
// bytes, and its answer is the client's answer. Its account is released, and
// the pool's rate-limit state is not read from a count, even one that carried
// unified headers.
func TestCountTokens_ClaudeCodeForwardsTheClientsBody(t *testing.T) {
	upstream := newCountUpstream(t, http.StatusOK, `{"input_tokens":4242}`, http.Header{
		"Anthropic-Ratelimit-Unified-Status": {"rejected"},
		"Anthropic-Ratelimit-Unified-Reset":  {strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)},
	})
	server, cfg := newCCCountServer(t, upstream.URL, time.Now)

	got := countOf(t, sendInSession(server.Handler(), countPath, "test-proxy-key", "sess-cc", ccCountBody,
		http.Header{"User-Agent": {"foreign/1.0"}, "Anthropic-Beta": {"foreign-beta"}}))
	if got != 4242 {
		t.Errorf("count = %d, want the upstream's 4242", got)
	}
	seen := upstream.requests()
	if len(seen) != 1 {
		t.Fatalf("upstream got %d count requests, want 1", len(seen))
	}
	r := seen[0]
	if r.path != "/v1/messages/count_tokens" || r.query != "beta=true" {
		t.Errorf("upstream target = %s?%s, want /v1/messages/count_tokens?beta=true", r.path, r.query)
	}
	if r.body != ccCountBody {
		t.Errorf("forwarded body:\n%s\nwant the client's bytes:\n%s", r.body, ccCountBody)
	}
	for name, want := range map[string]string{
		"Authorization":               "Bearer sk-ant-oat01-count",
		"x-api-key":                   "",
		"x-claude-code-request-class": "auxiliary",
		"anthropic-beta":              strings.Join(ccidentity.CountTokensBetas, ","),
		"User-Agent":                  ccidentity.MessagesUserAgent,
		"X-Stainless-Timeout":         "",
		"x-claude-code-prompt-id":     "",
	} {
		if got := r.header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	state := ccCountAccountState(t, server, cfg)
	if state.Status != "healthy" || state.RateLimits.Unified != nil {
		t.Errorf("account status %q, unified %+v: a count must not set rate-limit state", state.Status, state.RateLimits.Unified)
	}
	if state.InFlight != 0 {
		t.Errorf("account in flight = %d, want 0: the count did not release it", state.InFlight)
	}
}

// /context sends about 34 counts at once on one session, so on one account.
// The upstream answers none until all 34 have arrived: a forward that
// serialized them would leave the first waiting, and it gives up after 5 s so
// a failure costs one wait, not 34.
func TestCountTokens_ClaudeCodeBurstIsNotSerialized(t *testing.T) {
	const burst = 34
	var arrived atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if arrived.Add(1) == burst {
			releaseOnce.Do(func() { close(release) })
		}
		select {
		case <-release:
		case <-time.After(5 * time.Second):
			releaseOnce.Do(func() { close(release) })
			http.Error(w, "the burst arrived serialized", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":777}`)
	}))
	t.Cleanup(upstream.Close)
	server, cfg := newCCCountServer(t, upstream.URL, time.Now)
	handler := server.Handler()

	recorders := make([]*httptest.ResponseRecorder, burst)
	var wg sync.WaitGroup
	for i := range burst {
		wg.Go(func() {
			recorders[i] = sendInSession(handler, countPath, "test-proxy-key", "sess-burst", ccCountBody, nil)
		})
	}
	wg.Wait()
	for i, recorder := range recorders {
		if got := countOf(t, recorder); got != 777 {
			t.Errorf("count %d = %d, want the upstream's 777: the burst was serialized", i, got)
		}
	}
	if n := arrived.Load(); n != burst {
		t.Errorf("upstream saw %d counts, want %d", n, burst)
	}
	if state := ccCountAccountState(t, server, cfg); state.InFlight != 0 {
		t.Errorf("account in flight = %d after the burst, want 0", state.InFlight)
	}
}

// A 429 on a count cools the account down as one on /v1/messages does; the
// count itself is estimated, and so is the next, without asking again.
func TestCountTokens_ClaudeCode429CoolsTheAccountDown(t *testing.T) {
	upstream := newCountUpstream(t, http.StatusTooManyRequests,
		`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, http.Header{"Retry-After": {"120"}})
	server, cfg := newCCCountServer(t, upstream.URL, time.Now)
	handler := server.Handler()
	want := estimateOf(t, server, "sess-cc", "claude-sonnet-5", ccCountBody)

	for i := range 2 {
		if got := countOf(t, sendInSession(handler, countPath, "test-proxy-key", "sess-cc", ccCountBody, nil)); got != want {
			t.Errorf("count %d = %d, want the estimate %d", i, got, want)
		}
	}
	if n := len(upstream.requests()); n != 1 {
		t.Errorf("upstream asked %d times, want 1: a cooled-down account was asked again", n)
	}
	state := ccCountAccountState(t, server, cfg)
	if state.Status != "cooldown" {
		t.Errorf("account status = %q, want cooldown", state.Status)
	}
	if left := time.Until(state.CooldownUntil); left < 100*time.Second {
		t.Errorf("cooldown ends in %v, want the 120 s Retry-After", left)
	}
}

// 404, 405 and 501 say the upstream does not count tokens: the count is
// estimated, and the gateway is not asked again for an hour.
func TestCountTokens_UnsupportedUpstreamIsEstimatedForAnHour(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			upstream := newCountUpstream(t, status, `{"type":"error"}`, nil)
			clock := &testClock{now: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
			server, _ := newCCCountServer(t, upstream.URL, clock.Now)
			handler := server.Handler()
			want := estimateOf(t, server, "", "claude-sonnet-5", ccCountBody)
			for i, step := range []struct {
				advance time.Duration
				asked   int
			}{
				{0, 1},                // asked, answered unsupported, marked
				{59 * time.Minute, 1}, // still marked: estimated without a call
				{time.Minute, 2},      // the hour is over: asked again
			} {
				clock.Advance(step.advance)
				if got := countOf(t, sendInSession(handler, countPath, "test-proxy-key", "", ccCountBody, nil)); got != want {
					t.Errorf("step %d: count = %d, want the estimate %d", i, got, want)
				}
				if n := len(upstream.requests()); n != step.asked {
					t.Errorf("step %d: upstream asked %d times, want %d", i, n, step.asked)
				}
			}
		})
	}
}

// Any other failure is estimated for that request only: the next count asks
// the upstream again.
func TestCountTokens_FailedForwardEstimatesThatRequestOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		answer string
	}{
		{"server error", http.StatusInternalServerError, `{"type":"error"}`},
		{"bad gateway", http.StatusBadGateway, ``},
		{"unauthorized", http.StatusUnauthorized, `{"type":"error"}`},
		{"bad request", http.StatusBadRequest, `{"type":"error"}`},
		{"no count in a 200", http.StatusOK, `{"tokens":12}`},
		{"a 200 that is not JSON", http.StatusOK, `nope`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newCountUpstream(t, tc.status, tc.answer, nil)
			server, _ := newCCCountServer(t, upstream.URL, time.Now)
			handler := server.Handler()
			want := estimateOf(t, server, "", "claude-sonnet-5", ccCountBody)
			for i := range 2 {
				if got := countOf(t, sendInSession(handler, countPath, "test-proxy-key", "", ccCountBody, nil)); got != want {
					t.Errorf("count %d = %d, want the estimate %d", i, got, want)
				}
			}
			if n := len(upstream.requests()); n != 2 {
				t.Errorf("upstream asked %d times, want 2: this failure must not mark the gateway", n)
			}
		})
	}
}
