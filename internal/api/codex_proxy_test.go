package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"antigravity-go-proxy/internal/codex"
	"antigravity-go-proxy/internal/config"
)

// codexSSE is a complete Codex reply that streams the text "hi".
const codexSSE = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n" +
	"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
	"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"hi\"}\n\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n"

// codexCallRecord is one request the fake Codex backend received.
type codexCallRecord struct {
	path   string
	header http.Header
	body   map[string]any
}

// fakeCodexBackend emulates the Codex backend: reply answers each request and
// every request is recorded.
type fakeCodexBackend struct {
	mu    sync.Mutex
	calls []codexCallRecord
}

func (f *fakeCodexBackend) record(r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, codexCallRecord{path: r.URL.Path, header: r.Header.Clone(), body: body})
}

func (f *fakeCodexBackend) tokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, strings.TrimPrefix(c.header.Get("Authorization"), "Bearer "))
	}
	return out
}

func (f *fakeCodexBackend) last(t *testing.T) codexCallRecord {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("the Codex backend received no request")
	}
	return f.calls[len(f.calls)-1]
}

func newFakeCodexBackend(t *testing.T, reply func(w http.ResponseWriter, r *http.Request, token string)) (*fakeCodexBackend, *httptest.Server) {
	t.Helper()
	backend := &fakeCodexBackend{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backend.record(r)
		reply(w, r, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	}))
	t.Cleanup(server.Close)
	return backend, server
}

// writeCodexSSE answers with codexSSE and the given extra headers.
func writeCodexSSE(w http.ResponseWriter, header map[string]string) {
	for k, v := range header {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, codexSSE)
}

// newCodexTestServer returns a server whose config.json holds an enabled Codex
// gateway on upstream with the given accounts and the default allowlist.
func newCodexTestServer(t *testing.T, upstream *httptest.Server, accounts ...codex.AccountConfig) *Server {
	t.Helper()
	srv, _, _ := newTestServerWithManager(t)
	srv.codexUpstream = upstream.Client().Transport
	seedCodexConfig(t, codex.Config{Enabled: true, BaseURL: upstream.URL, Accounts: accounts, Allowlist: codex.DefaultAllowlist()})
	return srv
}

func seedCodexConfig(t *testing.T, cfg codex.Config) {
	t.Helper()
	if _, err := config.Save(map[string]any{"codex": cfg}); err != nil {
		t.Fatalf("seed codex config: %v", err)
	}
}

// fakeCodexTokenEndpoint emulates the OpenAI token endpoint: reply answers
// each form POST.
func fakeCodexTokenEndpoint(t *testing.T, reply func(w http.ResponseWriter, form url.Values)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		reply(w, r.PostForm)
	}))
	t.Cleanup(server.Close)
	return server
}

// fakeCodexIDToken builds an unsigned id_token carrying the ChatGPT claims.
func fakeCodexIDToken(email, accountID, planType string) string {
	claims, _ := json.Marshal(map[string]any{
		"email": email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  planType,
		},
	})
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(claims) + ".sig"
}

func postCodexMessages(t *testing.T, srv *Server, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-api-key")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func codexSnapshot(t *testing.T, srv *Server, id string) codex.AccountSnapshot {
	t.Helper()
	pool, _ := srv.codexPoolFor(config.Get().Codex)
	for _, s := range pool.Snapshots() {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("account %s not in the pool", id)
	return codex.AccountSnapshot{}
}

// requireNoCodexInFlight checks every acquired account was released.
func requireNoCodexInFlight(t *testing.T, srv *Server) {
	t.Helper()
	pool, _ := srv.codexPoolFor(config.Get().Codex)
	for _, s := range pool.Snapshots() {
		if s.InFlight != 0 {
			t.Errorf("account %s inFlight = %d after the request, want 0", s.ID, s.InFlight)
		}
	}
}

func decodeAnthropicError(t *testing.T, rec *httptest.ResponseRecorder) (kind, message string) {
	t.Helper()
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Type != "error" {
		t.Fatalf("body is not an Anthropic error envelope: %s", rec.Body.String())
	}
	return env.Error.Type, env.Error.Message
}

const codexQuotaHeaders = "x-codex-primary-used-percent:25|x-codex-primary-window-minutes:300|x-codex-primary-reset-after-seconds:600|" +
	"x-codex-secondary-used-percent:60|x-codex-secondary-window-minutes:10080|x-codex-secondary-reset-after-seconds:86400"

func codexQuotaHeaderMap() map[string]string {
	out := map[string]string{}
	for _, kv := range strings.Split(codexQuotaHeaders, "|") {
		k, v, _ := strings.Cut(kv, ":")
		out[k] = v
	}
	return out
}

// A non-streaming client gets one Anthropic message built from the Codex
// stream. The upstream request carries the account's token and workspace, the
// session's prompt cache key, a body translated for Codex with max_tokens
// clamped to the model's output limit, and the reply's usage windows are
// recorded on the account.
func TestCodexGateway_NonStreamingRequest(t *testing.T) {
	backend, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		writeCodexSSE(w, codexQuotaHeaderMap())
	})
	srv := newCodexTestServer(t, upstream, codex.AccountConfig{ID: "codex-a", Token: "tok-a", RefreshToken: "r-a", AccountID: "acct-a", Enabled: true})

	rec := postCodexMessages(t, srv,
		`{"model":"GPT-5.5","max_tokens":500000,"messages":[{"role":"user","content":"hello"}]}`,
		map[string]string{"X-Claude-Code-Session-Id": "sess-1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("response is not JSON: %s", rec.Body.String())
	}
	if msg["type"] != "message" || msg["model"] != "gpt-5.5" {
		t.Errorf("message type/model = %v/%v, want message/gpt-5.5 (the canonical id)", msg["type"], msg["model"])
	}
	content, _ := msg["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "hi" {
		t.Errorf("content = %v, want one text block \"hi\"", msg["content"])
	}

	call := backend.last(t)
	wantKey := codex.PromptCacheKey("gpt-5.5", "sess-1", "main")
	if call.path != "/responses" {
		t.Errorf("upstream path = %q, want /responses", call.path)
	}
	for header, want := range map[string]string{
		"Authorization":        "Bearer tok-a",
		"Chatgpt-Account-Id":   "acct-a",
		"Session-Id":           wantKey,
		"X-Codex-Routing-Hint": "model=gpt-5.5",
	} {
		if got := call.header.Get(header); got != want {
			t.Errorf("upstream %s = %q, want %q", header, got, want)
		}
	}
	if call.body["model"] != "gpt-5.5" || call.body["stream"] != true || call.body["store"] != false || call.body["prompt_cache_key"] != wantKey {
		t.Errorf("upstream body = %v, want model gpt-5.5, stream true, store false and the cache key", call.body)
	}
	if got := call.body["max_output_tokens"]; got != float64(128000) {
		t.Errorf("max_output_tokens = %v, want 128000 (clamped to the entry's limit)", got)
	}

	snap := codexSnapshot(t, srv, "codex-a")
	if snap.Quota == nil || snap.Quota.Primary == nil || snap.Quota.Primary.UsedPercent != 25 || snap.Quota.Secondary == nil {
		t.Errorf("quota = %+v, want the windows from the reply headers", snap.Quota)
	}
	if snap.TotalRequests != 1 {
		t.Errorf("totalRequests = %d, want 1", snap.TotalRequests)
	}
	requireNoCodexInFlight(t, srv)
}

// A subagent's requests carry their own agent id, so they get a prompt cache
// key of their own.
func TestCodexGateway_AgentIDScopesPromptCacheKey(t *testing.T) {
	backend, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		writeCodexSSE(w, nil)
	})
	srv := newCodexTestServer(t, upstream, codex.AccountConfig{ID: "codex-a", Token: "tok-a", Enabled: true})

	rec := postCodexMessages(t, srv, `{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-Claude-Code-Session-Id": "sess-1", "X-Claude-Code-Agent-Id": "agent-7"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	want := codex.PromptCacheKey("gpt-5.5", "sess-1", "agent-7")
	if got := backend.last(t).header.Get("Session-Id"); got != want || want == codex.PromptCacheKey("gpt-5.5", "sess-1", "main") {
		t.Errorf("Session-Id = %q, want the agent's key %q", got, want)
	}
}

// A streaming client gets the Anthropic event sequence, flushed as an event
// stream.
func TestCodexGateway_StreamingRequest(t *testing.T) {
	_, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		writeCodexSSE(w, nil)
	})
	srv := newCodexTestServer(t, upstream, codex.AccountConfig{ID: "codex-a", Token: "tok-a", Enabled: true})

	rec := postCodexMessages(t, srv, `{"model":"gpt-5.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"event: message_start", `"model":"gpt-5.5"`, `"text":"hi"`, "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %s:\n%s", want, body)
		}
	}
	if !rec.Flushed {
		t.Error("the stream was never flushed")
	}
	requireNoCodexInFlight(t, srv)
}

// An upstream stream that stops before its terminal event reaches the client
// as an Anthropic error event, and the account is still released.
func TestCodexGateway_TruncatedStreamReportsError(t *testing.T) {
	_, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n")
	})
	srv := newCodexTestServer(t, upstream, codex.AccountConfig{ID: "codex-a", Token: "tok-a", Enabled: true})

	rec := postCodexMessages(t, srv, `{"model":"gpt-5.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if !strings.Contains(rec.Body.String(), "event: error") || !strings.Contains(rec.Body.String(), "Codex") {
		t.Errorf("truncated stream did not end in a Codex error event:\n%s", rec.Body.String())
	}
	requireNoCodexInFlight(t, srv)

	nonStream := postCodexMessages(t, srv, `{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if nonStream.Code != http.StatusBadGateway {
		t.Errorf("non-streaming status = %d, want 502 for a truncated upstream", nonStream.Code)
	}
}

// A spent usage window rests the account until the reset the body names, and
// the request fails over to the next account.
func TestCodexGateway_UsageLimitFailsOver(t *testing.T) {
	backend, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, token string) {
		if token == "tok-a" {
			for k, v := range codexQuotaHeaderMap() {
				w.Header().Set(k, v)
			}
			w.Header().Set("x-codex-primary-used-percent", "100")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","resets_in_seconds":3600}}`)
			return
		}
		writeCodexSSE(w, nil)
	})
	srv := newCodexTestServer(t, upstream,
		codex.AccountConfig{ID: "codex-a", Token: "tok-a", Priority: 1, Enabled: true},
		codex.AccountConfig{ID: "codex-b", Token: "tok-b", Priority: 2, Enabled: true})

	before := time.Now()
	rec := postCodexMessages(t, srv, `{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := backend.tokens(); len(got) != 2 || got[0] != "tok-a" || got[1] != "tok-b" {
		t.Errorf("upstream calls = %v, want tok-a then tok-b", got)
	}
	snap := codexSnapshot(t, srv, "codex-a")
	if snap.Status != "cooldown" || snap.CooldownReason != "usage_limit_reached" {
		t.Errorf("codex-a status/reason = %s/%s, want cooldown/usage_limit_reached", snap.Status, snap.CooldownReason)
	}
	if want := before.Add(time.Hour); snap.CooldownUntil.Before(want.Add(-time.Minute)) || snap.CooldownUntil.After(want.Add(time.Minute)) {
		t.Errorf("cooldownUntil = %v, want about %v (resets_in_seconds)", snap.CooldownUntil, want)
	}
	if snap.Quota == nil || snap.Quota.Primary == nil || snap.Quota.Primary.UsedPercent != 100 {
		t.Errorf("quota = %+v, want the 429's spent window recorded", snap.Quota)
	}
	requireNoCodexInFlight(t, srv)
}

// A 429 without a usage-limit body rests the account for Retry-After, and
// when no account is left the client sees the upstream's rate_limit_error.
func TestCodexGateway_RateLimitWithoutAccountLeftSurfaces429(t *testing.T) {
	_, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
	})
	srv := newCodexTestServer(t, upstream, codex.AccountConfig{ID: "codex-a", Token: "tok-a", Enabled: true})

	before := time.Now()
	rec := postCodexMessages(t, srv, `{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body = %s", rec.Code, rec.Body.String())
	}
	if kind, msg := decodeAnthropicError(t, rec); kind != "rate_limit_error" || msg != "Codex: slow down" {
		t.Errorf("error = %s %q, want rate_limit_error \"Codex: slow down\"", kind, msg)
	}
	if got := rec.Header().Get("Retry-After"); got != "17" {
		t.Errorf("Retry-After = %q, want 17 forwarded", got)
	}
	snap := codexSnapshot(t, srv, "codex-a")
	if snap.CooldownReason != "rate_limited" || snap.CooldownUntil.Before(before.Add(16*time.Second)) || snap.CooldownUntil.After(before.Add(time.Minute)) {
		t.Errorf("cooldown = %v (%s), want about 17s, rate_limited", snap.CooldownUntil, snap.CooldownReason)
	}

	// The account is resting now, so the next request has none to use and is
	// told to come back when it wakes.
	again := postCodexMessages(t, srv, `{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if again.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429; body = %s", again.Code, again.Body.String())
	}
	if s, err := strconv.Atoi(again.Header().Get("Retry-After")); err != nil || s < 1 || s > 17 {
		t.Errorf("second Retry-After = %q, want the rest of the 17s cooldown", again.Header().Get("Retry-After"))
	}
}

// A server error rests the account and fails over; with no account left the
// last server error reaches the client.
func TestCodexGateway_ServerErrorFailsOver(t *testing.T) {
	backend, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, token string) {
		if token == "tok-a" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
			return
		}
		writeCodexSSE(w, nil)
	})
	srv := newCodexTestServer(t, upstream,
		codex.AccountConfig{ID: "codex-a", Token: "tok-a", Priority: 1, Enabled: true},
		codex.AccountConfig{ID: "codex-b", Token: "tok-b", Priority: 2, Enabled: true})

	rec := postCodexMessages(t, srv, `{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := backend.tokens(); len(got) != 2 || got[0] != "tok-a" {
		t.Errorf("upstream calls = %v, want tok-a then tok-b", got)
	}
	if snap := codexSnapshot(t, srv, "codex-a"); snap.Status != "cooldown" || snap.CooldownReason != "server_error" {
		t.Errorf("codex-a status/reason = %s/%s, want cooldown/server_error", snap.Status, snap.CooldownReason)
	}
	requireNoCodexInFlight(t, srv)
}

func TestCodexGateway_ServerErrorWithNoAccountLeft(t *testing.T) {
	_, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"detail":"upstream overloaded"}`)
	})
	srv := newCodexTestServer(t, upstream, codex.AccountConfig{ID: "codex-a", Token: "tok-a", Enabled: true})

	rec := postCodexMessages(t, srv, `{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
	if kind, msg := decodeAnthropicError(t, rec); kind != "overloaded_error" || msg != "Codex: upstream overloaded" {
		t.Errorf("error = %s %q, want overloaded_error \"Codex: upstream overloaded\"", kind, msg)
	}
}

// A 401 refreshes the token once and resends on the same account. The rotated
// tokens are in config.json before the resend uses them.
func TestCodexGateway_UnauthorizedRefreshesAndRetries(t *testing.T) {
	var persistedBeforeResend string
	backend, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, token string) {
		if token == "tok-old" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"token expired"}}`)
			return
		}
		persistedBeforeResend = config.Get().Codex.Accounts[0].Token
		writeCodexSSE(w, nil)
	})
	tokenEndpoint := fakeCodexTokenEndpoint(t, func(w http.ResponseWriter, form url.Values) {
		if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "r-old" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"tok-new","refresh_token":"r-new","expires_in":864000}`)
	})
	srv := newCodexTestServer(t, upstream, codex.AccountConfig{ID: "codex-a", Token: "tok-old", RefreshToken: "r-old", Enabled: true})
	srv.codexOAuthMgr.SetEndpoints("", tokenEndpoint.URL)

	rec := postCodexMessages(t, srv, `{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := backend.tokens(); len(got) != 2 || got[0] != "tok-old" || got[1] != "tok-new" {
		t.Errorf("upstream calls = %v, want tok-old then tok-new", got)
	}
	if persistedBeforeResend != "tok-new" {
		t.Errorf("config token at resend = %q, want tok-new persisted first", persistedBeforeResend)
	}
	saved := config.Get().Codex.Accounts[0]
	if saved.Token != "tok-new" || saved.RefreshToken != "r-new" || saved.ExpiresAt == "" {
		t.Errorf("saved account = %+v, want the rotated tokens and their expiry", saved)
	}
	requireNoCodexInFlight(t, srv)
}

// A refresh the token endpoint refuses for good marks the account for a new
// login and the request fails over.
func TestCodexGateway_RevokedGrantNeedsLogin(t *testing.T) {
	_, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, token string) {
		if token == "tok-a" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeCodexSSE(w, nil)
	})
	tokenEndpoint := fakeCodexTokenEndpoint(t, func(w http.ResponseWriter, _ url.Values) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"refresh token reused"}`)
	})
	srv := newCodexTestServer(t, upstream,
		codex.AccountConfig{ID: "codex-a", Token: "tok-a", RefreshToken: "r-a", Priority: 1, Enabled: true},
		codex.AccountConfig{ID: "codex-b", Token: "tok-b", RefreshToken: "r-b", Priority: 2, Enabled: true})
	srv.codexOAuthMgr.SetEndpoints("", tokenEndpoint.URL)

	rec := postCodexMessages(t, srv, `{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if snap := codexSnapshot(t, srv, "codex-a"); snap.Status != "needs_login" {
		t.Errorf("codex-a status = %s, want needs_login", snap.Status)
	}
	requireNoCodexInFlight(t, srv)
}

// A 4xx other than 401 and 429 is the request's own fault: no failover, and
// the client gets the upstream's reason in an Anthropic envelope.
func TestCodexGateway_ClientErrorIsNotRetried(t *testing.T) {
	cases := []struct {
		status   int
		wantType string
	}{
		{http.StatusBadRequest, "invalid_request_error"},
		{http.StatusForbidden, "permission_error"},
		{http.StatusNotFound, "not_found_error"},
		{http.StatusRequestEntityTooLarge, "request_too_large"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			backend, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, _ string) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":{"message":"bad input"}}`)
			})
			srv := newCodexTestServer(t, upstream,
				codex.AccountConfig{ID: "codex-a", Token: "tok-a", Priority: 1, Enabled: true},
				codex.AccountConfig{ID: "codex-b", Token: "tok-b", Priority: 2, Enabled: true})

			rec := postCodexMessages(t, srv, `{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body.String())
			}
			if kind, msg := decodeAnthropicError(t, rec); kind != tc.wantType || msg != "Codex: bad input" {
				t.Errorf("error = %s %q, want %s \"Codex: bad input\"", kind, msg, tc.wantType)
			}
			if n := len(backend.tokens()); n != 1 {
				t.Errorf("upstream calls = %d, want 1 (no failover)", n)
			}
			requireNoCodexInFlight(t, srv)
		})
	}
}

func TestCodexGateway_NoAccountsReplies503(t *testing.T) {
	_, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		writeCodexSSE(w, nil)
	})
	srv := newCodexTestServer(t, upstream)

	rec := postCodexMessages(t, srv, `{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
	if kind, msg := decodeAnthropicError(t, rec); kind != "overloaded_error" || !strings.Contains(msg, "No Codex accounts available") {
		t.Errorf("error = %s %q, want overloaded_error naming the cause", kind, msg)
	}
}

// A request the builder cannot translate is the client's error, and no
// account is spent on it.
func TestCodexGateway_UntranslatableRequestIs400(t *testing.T) {
	backend, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		writeCodexSSE(w, nil)
	})
	srv := newCodexTestServer(t, upstream, codex.AccountConfig{ID: "codex-a", Token: "tok-a", Enabled: true})

	rec := postCodexMessages(t, srv, `{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"x"}}]}]}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if n := len(backend.tokens()); n != 0 {
		t.Errorf("upstream calls = %d, want 0", n)
	}
}

// The pool persists across requests and is rebuilt, keeping its account state,
// only when the configured accounts change; a token rotation the persister
// wrote is not such a change.
func TestCodexPoolRebuildKeepsState(t *testing.T) {
	_, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		writeCodexSSE(w, codexQuotaHeaderMap())
	})
	srv := newCodexTestServer(t, upstream, codex.AccountConfig{ID: "codex-a", Token: "tok-a", RefreshToken: "r-a", Enabled: true})
	if rec := postCodexMessages(t, srv, `{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	first, _ := srv.codexPoolFor(config.Get().Codex)

	if err := srv.persistCodexTokens("codex-a", codex.TokenSet{AccessToken: "tok-a2", RefreshToken: "r-a2", ExpiresAt: time.Now().Add(48 * time.Hour)}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if again, _ := srv.codexPoolFor(config.Get().Codex); again != first {
		t.Error("a persisted token rotation rebuilt the pool")
	}

	cfg := config.Get().Codex
	cfg.Accounts = append(cfg.Accounts, codex.AccountConfig{ID: "codex-b", Token: "tok-b", Enabled: true})
	seedCodexConfig(t, cfg)
	rebuilt, _ := srv.codexPoolFor(config.Get().Codex)
	if rebuilt == first {
		t.Fatal("an added account did not rebuild the pool")
	}
	if snap := codexSnapshot(t, srv, "codex-a"); snap.TotalRequests != 1 || snap.Quota == nil {
		t.Errorf("codex-a after the rebuild = %+v, want its counters and quota inherited", snap)
	}
}

// The Codex gateway takes its place in the configured precedence: under the
// default order an earlier gateway that owns the same model wins, hoisted it
// wins, and disabled it declines and leaves the request to the next gateway.
func TestDispatch_CodexFollowsGatewayOrder(t *testing.T) {
	probe := newDispatchProbe()
	kimi, zen, cc, or, custom, backend := dispatchUpstreams(t, probe)
	codexUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/responses" {
			probe.record("codex", body)
		}
		writeCodexSSE(w, nil)
	}))
	t.Cleanup(codexUp.Close)

	cfg := dispatchBaseConfig(kimi.URL, zen.URL, cc.URL, or.URL, custom.URL)
	cfg.Codex = codex.Config{
		Enabled:   true,
		BaseURL:   codexUp.URL,
		Accounts:  []codex.AccountConfig{{ID: "codex-a", Token: "tok-a", Enabled: true}},
		Allowlist: []codex.ModelConfig{{ID: dispatchModel, Enabled: true}},
	}
	server := newDispatchServer(t, cfg, backend)
	server.codexUpstream = codexUp.Client().Transport

	requireDispatchWinner(t, probe, backend, postDispatchMessages(t, server, dispatchModel), "kimi")

	cfg.GatewayOrder.Order = []config.GatewayID{config.GatewayCodex}
	config.SetForTest(cfg)
	before := probe.snapshot()
	requireDispatchWinnerDelta(t, probe, before, backend, postDispatchMessages(t, server, dispatchModel), "codex")
	if got := probe.model("codex"); got != dispatchModel {
		t.Errorf("codex upstream model = %q, want %q", got, dispatchModel)
	}

	cfg.Codex.Enabled = false
	config.SetForTest(cfg)
	before = probe.snapshot()
	requireDispatchWinnerDelta(t, probe, before, backend, postDispatchMessages(t, server, dispatchModel), "kimi")
}

// A failure the backend reports inside a 200 stream before any output is
// handled like the HTTP status it stands for: a spent usage window cools the
// account down and the request moves to the next account, for streaming and
// non-streaming clients alike. The session's next request skips the account.
func TestCodexGateway_InStreamFailureBeforeOutputFailsOver(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			backend, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, token string) {
				if token == "tok-a" {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_a\"}}\n\n"+
						"data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_a\",\"error\":{\"type\":\"usage_limit_reached\",\"message\":\"limit\",\"resets_in_seconds\":3600}}}\n\n")
					return
				}
				writeCodexSSE(w, nil)
			})
			srv := newCodexTestServer(t, upstream,
				codex.AccountConfig{ID: "codex-a", Token: "tok-a", Priority: 1, Enabled: true},
				codex.AccountConfig{ID: "codex-b", Token: "tok-b", Priority: 2, Enabled: true})
			body := fmt.Sprintf(`{"model":"gpt-5.5","max_tokens":64,"stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream)
			session := map[string]string{"X-Claude-Code-Session-Id": "s1"}

			rec := postCodexMessages(t, srv, body, session)
			if out := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(out, `"hi"`) || strings.Contains(out, "Codex: limit") {
				t.Fatalf("status = %d, body = %s; want codex-b's answer", rec.Code, out)
			}
			snap := codexSnapshot(t, srv, "codex-a")
			if snap.Status != "cooldown" || snap.CooldownReason != "usage_limit_reached" {
				t.Errorf("codex-a = %s/%s, want cooldown/usage_limit_reached", snap.Status, snap.CooldownReason)
			}
			postCodexMessages(t, srv, body, session)
			if got := backend.tokens(); len(got) != 3 || got[0] != "tok-a" || got[1] != "tok-b" || got[2] != "tok-b" {
				t.Errorf("upstream calls = %v, want tok-a, tok-b, then tok-b", got)
			}
			requireNoCodexInFlight(t, srv)
		})
	}
}

// Once output reached the client, a failure stays in the stream: the reply
// cannot be taken back, so the request is not sent again.
func TestCodexGateway_InStreamFailureAfterOutputIsReported(t *testing.T) {
	backend, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n"+
			"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"par\"}\n\n"+
			"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"server_error\",\"message\":\"boom\"}}}\n\n")
	})
	srv := newCodexTestServer(t, upstream,
		codex.AccountConfig{ID: "codex-a", Token: "tok-a", Priority: 1, Enabled: true},
		codex.AccountConfig{ID: "codex-b", Token: "tok-b", Priority: 2, Enabled: true})

	rec := postCodexMessages(t, srv, `{"model":"gpt-5.5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if out := rec.Body.String(); !strings.Contains(out, `"text":"par"`) || !strings.Contains(out, "Codex: boom") {
		t.Errorf("stream = %s, want the partial text, then the error event", out)
	}
	if n := len(backend.tokens()); n != 1 {
		t.Errorf("upstream calls = %d, want 1", n)
	}
	requireNoCodexInFlight(t, srv)
}

// A backend that cannot read a replayed reasoning item refuses the whole
// request. The gateway resends it once without reasoning, on the same account,
// so the conversation does not stay stuck on a signature that can never work.
func TestCodexGateway_InvalidEncryptedContentResendsWithoutReasoning(t *testing.T) {
	var backend *fakeCodexBackend
	backend, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		if codexInputHasReasoning(backend.last(t).body) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"The encrypted content could not be verified.","type":"invalid_request_error","code":"invalid_encrypted_content"}}`)
			return
		}
		writeCodexSSE(w, nil)
	})
	srv := newCodexTestServer(t, upstream,
		codex.AccountConfig{ID: "codex-a", Token: "tok-a", Priority: 1, Enabled: true},
		codex.AccountConfig{ID: "codex-b", Token: "tok-b", Priority: 2, Enabled: true})
	raw := make([]byte, 1+8+16+16+32) // a Fernet-shaped token: version, time, IV, one block, HMAC
	raw[0] = 0x80
	sig := base64.URLEncoding.EncodeToString(raw)
	body := `{"model":"gpt-5.5","max_tokens":64,"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"` + sig + `"},{"type":"text","text":"ok"}]},` +
		`{"role":"user","content":"again"}]}`

	rec := postCodexMessages(t, srv, body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := backend.tokens(); len(got) != 2 || got[0] != "tok-a" || got[1] != "tok-a" {
		t.Errorf("upstream calls = %v, want tok-a twice (same account)", got)
	}
	if codexInputHasReasoning(backend.last(t).body) {
		t.Error("the resend still carries a reasoning item")
	}
	requireNoCodexInFlight(t, srv)
}

func codexInputHasReasoning(body map[string]any) bool {
	items, _ := body["input"].([]any)
	for _, raw := range items {
		if item, _ := raw.(map[string]any); item["type"] == "reasoning" {
			return true
		}
	}
	return false
}

// When every account is resting after a spent usage window, the client is
// told it is rate limited and until when, not that the backend is overloaded.
func TestCodexGateway_AllAccountsCoolingDownReplies429(t *testing.T) {
	backend, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		writeCodexSSE(w, nil)
	})
	srv := newCodexTestServer(t, upstream, codex.AccountConfig{ID: "codex-a", Token: "tok-a", Enabled: true})
	pool, _ := srv.codexPoolFor(config.Get().Codex)
	pool.RecordCooldown("codex-a", time.Now().Add(3*time.Hour), "usage_limit_reached")

	rec := postCodexMessages(t, srv, `{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body = %s", rec.Code, rec.Body.String())
	}
	if kind, _ := decodeAnthropicError(t, rec); kind != "rate_limit_error" {
		t.Errorf("error type = %s, want rate_limit_error", kind)
	}
	if s, err := strconv.Atoi(rec.Header().Get("Retry-After")); err != nil || s < 3*3600-60 || s > 3*3600 {
		t.Errorf("Retry-After = %q, want about 10800", rec.Header().Get("Retry-After"))
	}
	if n := len(backend.tokens()); n != 0 {
		t.Errorf("upstream calls = %d, want 0", n)
	}
}
