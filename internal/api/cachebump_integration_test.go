package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"antigravity-go-proxy/internal/cachebump"
	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
)

// cacheBumpUpstream emulates Anthropic /v1/messages and records every
// request's auth identity and body.
type cacheBumpUpstream struct {
	mu       sync.Mutex
	requests []upstreamRequest
	respBody string
	status   int
}

type upstreamRequest struct {
	authToken string
	body      string
}

func (u *cacheBumpUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		token := r.Header.Get("x-api-key")
		if token == "" {
			token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		u.requests = append(u.requests, upstreamRequest{authToken: token, body: string(body)})
		status, respBody := u.status, u.respBody
		u.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}
}

func (u *cacheBumpUpstream) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	defer u.mu.Unlock()
	token := r.Header.Get("x-api-key")
	if token == "" {
		token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	u.requests = append(u.requests, upstreamRequest{authToken: token, body: string(body)})
}

func (u *cacheBumpUpstream) lenRequests() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

func (u *cacheBumpUpstream) lastRequest() upstreamRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests[len(u.requests)-1]
}

func cacheBumpTestConfig(t *testing.T, upstreamURL string) config.Config {
	t.Helper()
	// The turn path rewrites config.json (token sync) and reloads in-memory
	// config from disk, so the test config must be persisted, not just set.
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", t.TempDir())
	base := config.DefaultConfig()
	base.CacheBump.Enabled = true
	base.CacheBump.Routes.ClaudeCode = true
	base.ClaudeCode = claudecode.Config{
		Enabled: true,
		BaseURL: upstreamURL,
		Mode:    "pool",
		Accounts: []claudecode.AccountConfig{
			{ID: "acc-a", Token: "tok-a", Type: "api_key", Priority: 1, Enabled: true},
			{ID: "acc-b", Token: "tok-b", Type: "api_key", Priority: 2, Enabled: true},
		},
		Allowlist: claudecode.DefaultAllowlist(),
		Routing:   claudecode.DefaultRoutingConfig(),
	}

	persistTestConfig(t, base)
	return base
}

// persistTestConfig writes cfg to disk and sets it in memory.
func persistTestConfig(t *testing.T, cfg config.Config) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path, err := config.ConfigFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	config.SetForTest(cfg)
}

func resetCCPoolForTest() {
	ccPoolMu.Lock()
	ccPoolInst = nil
	ccHTTPClient = nil
	ccPoolMu.Unlock()
}

// newCacheBumpServer builds a bare server with a fresh cache-bump store and
// scheduler wired for tests.
func newCacheBumpServer(t *testing.T) (*Server, *cachebump.Store, *cachebump.Scheduler) {
	t.Helper()
	srv := &Server{}
	store, sched := srv.getCacheBump()
	t.Cleanup(func() { store.Clear() })
	return srv, store, sched
}

const cacheBumpTurnBody = `{
  "model": "claude-sonnet-5",
  "max_tokens": 64000,
  "stream": false,
  "thinking": {"type": "enabled", "budget_tokens": 8000},
  "tool_choice": {"type": "auto"},
  "system": [{"type": "text", "text": "sys prompt", "cache_control": {"type": "ephemeral"}}],
  "messages": [{"role": "user", "content": [{"type": "text", "text": "turn one", "cache_control": {"type": "ephemeral"}}]}]
}`

func TestCacheBump_HeaderNeverForwardedUpstream(t *testing.T) {
	upstream := &cacheBumpUpstream{
		status:   http.StatusOK,
		respBody: `{"id":"m1","type":"message","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":800,"cache_creation_input_tokens":0}}`,
	}
	var gotBumpHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBumpHeader = r.Header.Get("X-Cache-Bump")
		upstream.handler().ServeHTTP(w, r)
	}))
	defer ts.Close()

	srv, _, _ := newTestServerWithManager(t)

	cfg := cacheBumpTestConfig(t, ts.URL)
	cfg.ClaudeCode.Enabled = false // route the turn to Kimi, not Claude Code
	cfg.Kimi = config.KimiConfig{
		Enabled: true,
		BaseURL: ts.URL,
		APIKey:  "kimi-key",
		Allowlist: []config.KimiModelConfig{
			{ID: "kimi-k2-thinking", Enabled: true},
		},
	}
	cfg.CacheBump.Routes.Kimi = true
	persistTestConfig(t, cfg)
	resetCCPoolForTest()

	handler := srv.Handler()
	kimiBody := strings.Replace(cacheBumpTurnBody, `"claude-sonnet-5"`, `"kimi-k2-thinking"`, 1)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(kimiBody))
	req.Header.Set("x-api-key", "test-api-key")
	req.Header.Set("x-session-id", "sess-strip")
	req.Header.Set("X-Cache-Bump", "on")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotBumpHeader != "" {
		t.Errorf("X-Cache-Bump leaked upstream: %q", gotBumpHeader)
	}

	store, _ := srv.getCacheBump()
	if _, ok := store.Get(cachebump.RecordKey(cachebump.RouteKimi, "sess-strip")); !ok {
		t.Errorf("expected session recorded (header was honored); store=%+v", store.Snapshot())
	}
}

func TestCacheBump_KimiRecordsAndBumps(t *testing.T) {
	upstream := &cacheBumpUpstream{
		status:   http.StatusOK,
		respBody: `{"id":"m1","type":"message","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":800,"cache_creation_input_tokens":0}}`,
	}
	ts := httptest.NewServer(upstream.handler())
	defer ts.Close()

	cfg := cacheBumpTestConfig(t, ts.URL)
	cfg.Kimi = config.KimiConfig{
		Enabled: true,
		BaseURL: ts.URL,
		APIKey:  "kimi-key",
	}
	cfg.CacheBump.Routes.Kimi = true
	persistTestConfig(t, cfg)
	resetCCPoolForTest()

	srv, store, sched := newCacheBumpServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(cacheBumpTurnBody))
	req.Header.Set("x-session-id", "sess-kimi")
	w := httptest.NewRecorder()
	srv.forwardToKimi(w, req, config.Get().Kimi, []byte(cacheBumpTurnBody), "kimi-k2-thinking")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	key := cachebump.RecordKey(cachebump.RouteKimi, "sess-kimi")
	rec, ok := store.Get(key)
	if !ok {
		t.Fatal("expected kimi session recorded")
	}
	if rec.Route != cachebump.RouteKimi || rec.Model != "kimi-k2-thinking" {
		t.Errorf("unexpected record: %+v", rec)
	}

	future := time.Now().Add(10 * time.Minute)
	sched.Now = func() time.Time { return future }
	sched.Tick(context.Background())

	if upstream.lenRequests() != 2 {
		t.Fatalf("expected bump request, got %d upstream requests", upstream.lenRequests())
	}
	bump := upstream.lastRequest()
	if bump.authToken != "kimi-key" {
		t.Errorf("expected Bearer kimi-key bump, got %q", bump.authToken)
	}
	var replay map[string]any
	if err := json.Unmarshal([]byte(bump.body), &replay); err != nil {
		t.Fatal(err)
	}
	if replay["max_tokens"].(float64) != 16 {
		t.Errorf("expected kimi bump max_tokens 16, got %v", replay["max_tokens"])
	}
}

func TestCacheBump_CustomEndpointRecordsAndBumps(t *testing.T) {
	upstream := &cacheBumpUpstream{
		status:   http.StatusOK,
		respBody: `{"id":"m1","type":"message","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":800,"cache_creation_input_tokens":0}}`,
	}
	ts := httptest.NewServer(upstream.handler())
	defer ts.Close()

	cfg := cacheBumpTestConfig(t, ts.URL)
	cfg.CustomEndpoints = map[string]config.EndpointConfig{
		"custom-model": {URL: ts.URL + "/v1/messages", APIKey: "ep-key"},
	}
	cfg.CacheBump.Routes.CustomEndpoints = true
	persistTestConfig(t, cfg)
	resetCCPoolForTest()

	srv, store, sched := newCacheBumpServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(cacheBumpTurnBody))
	req.Header.Set("x-session-id", "sess-custom")
	w := httptest.NewRecorder()
	srv.forwardToCustomEndpoint(w, req, config.Get().CustomEndpoints["custom-model"], "custom-model", []byte(cacheBumpTurnBody))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	key := cachebump.RecordKey(cachebump.RouteCustom, "sess-custom")
	rec, ok := store.Get(key)
	if !ok {
		t.Fatal("expected custom session recorded")
	}
	if rec.EndpointID != "custom-model" {
		t.Errorf("expected EndpointID custom-model, got %q", rec.EndpointID)
	}

	future := time.Now().Add(10 * time.Minute)
	sched.Now = func() time.Time { return future }
	sched.Tick(context.Background())

	if upstream.lenRequests() != 2 {
		t.Fatalf("expected bump request, got %d upstream requests", upstream.lenRequests())
	}
	bump := upstream.lastRequest()
	if bump.authToken != "ep-key" {
		t.Errorf("expected x-api-key ep-key bump, got %q", bump.authToken)
	}
	var replay map[string]any
	if err := json.Unmarshal([]byte(bump.body), &replay); err != nil {
		t.Fatal(err)
	}
	if replay["max_tokens"].(float64) != 16 {
		t.Errorf("expected custom bump max_tokens 16, got %v", replay["max_tokens"])
	}
}

func TestCacheBump_ClaudeCodeRecordsReplayBody(t *testing.T) {
	upstream := &cacheBumpUpstream{
		status:   http.StatusOK,
		respBody: `{"id":"m1","type":"message","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":1200,"cache_creation_input_tokens":0}}`,
	}
	ts := httptest.NewServer(upstream.handler())
	defer ts.Close()

	config.SetForTest(cacheBumpTestConfig(t, ts.URL))
	resetCCPoolForTest()

	srv, store, _ := newCacheBumpServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(cacheBumpTurnBody))
	req.Header.Set("x-session-id", "sess-bump-1")
	w := httptest.NewRecorder()
	srv.forwardToClaudeCode(w, req, config.Get().ClaudeCode, []byte(cacheBumpTurnBody), "claude-sonnet-5")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	rec, ok := store.Get(cachebump.RecordKey(cachebump.RouteClaudeCode, "sess-bump-1"))
	if !ok {
		t.Fatal("expected session recorded")
	}
	if rec.AccountID != "acc-a" {
		t.Errorf("expected AccountID acc-a, got %q", rec.AccountID)
	}
	if rec.TTL != 5*time.Minute {
		t.Errorf("expected TTL 5m, got %v", rec.TTL)
	}

	var replay map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body, &replay); err != nil {
		t.Fatalf("replay body invalid JSON: %v", err)
	}
	if string(replay["max_tokens"]) != "1" {
		t.Errorf("expected max_tokens 1, got %s", replay["max_tokens"])
	}
	if string(replay["stream"]) != "false" {
		t.Errorf("expected stream false, got %s", replay["stream"])
	}
	if _, ok := replay["thinking"]; ok {
		t.Error("expected thinking dropped in replay body")
	}
	if _, ok := replay["tool_choice"]; ok {
		t.Error("expected tool_choice dropped in replay body")
	}

	var orig map[string]json.RawMessage
	if err := json.Unmarshal([]byte(cacheBumpTurnBody), &orig); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"system", "messages"} {
		if string(orig[field]) != string(replay[field]) {
			t.Errorf("field %q not byte-identical", field)
		}
	}
}

func TestCacheBump_ClaudeCodeNotRecordedWithoutMarkers(t *testing.T) {
	upstream := &cacheBumpUpstream{
		status:   http.StatusOK,
		respBody: `{"id":"m1","type":"message","usage":{"input_tokens":10,"output_tokens":5}}`,
	}
	ts := httptest.NewServer(upstream.handler())
	defer ts.Close()

	config.SetForTest(cacheBumpTestConfig(t, ts.URL))
	resetCCPoolForTest()

	srv, store, _ := newCacheBumpServer(t)

	body := `{"model":"claude-sonnet-5","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("x-session-id", "sess-nomarker")
	w := httptest.NewRecorder()
	srv.forwardToClaudeCode(w, req, config.Get().ClaudeCode, []byte(body), "claude-sonnet-5")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if _, ok := store.Get(cachebump.RecordKey(cachebump.RouteClaudeCode, "sess-nomarker")); ok {
		t.Error("expected no record for body without cache_control markers")
	}
}

func TestCacheBump_ClaudeCodeDisabledRecordsNothing(t *testing.T) {
	upstream := &cacheBumpUpstream{
		status:   http.StatusOK,
		respBody: `{"id":"m1","type":"message","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100}}`,
	}
	ts := httptest.NewServer(upstream.handler())
	defer ts.Close()

	cfg := cacheBumpTestConfig(t, ts.URL)
	cfg.CacheBump.Enabled = false
	persistTestConfig(t, cfg)
	resetCCPoolForTest()

	srv, store, _ := newCacheBumpServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(cacheBumpTurnBody))
	req.Header.Set("x-session-id", "sess-off")
	w := httptest.NewRecorder()
	srv.forwardToClaudeCode(w, req, config.Get().ClaudeCode, []byte(cacheBumpTurnBody), "claude-sonnet-5")

	if _, ok := store.Get(cachebump.RecordKey(cachebump.RouteClaudeCode, "sess-off")); ok {
		t.Error("expected no record when cache bump disabled")
	}
}

func TestCacheBump_HeaderOverride(t *testing.T) {
	upstream := &cacheBumpUpstream{
		status:   http.StatusOK,
		respBody: `{"id":"m1","type":"message","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100}}`,
	}
	ts := httptest.NewServer(upstream.handler())
	defer ts.Close()

	cfg := cacheBumpTestConfig(t, ts.URL)
	cfg.CacheBump.Enabled = false
	persistTestConfig(t, cfg)
	resetCCPoolForTest()

	srv, store, _ := newCacheBumpServer(t)

	// The global switch is a kill switch: X-Cache-Bump: on cannot arm a
	// session while Cache Bump is off.
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(cacheBumpTurnBody))
	req.Header.Set("x-session-id", "sess-hdr-on-killed")
	req.Header.Set("X-Cache-Bump", "on")
	w := httptest.NewRecorder()
	srv.forwardToClaudeCode(w, req, config.Get().ClaudeCode, []byte(cacheBumpTurnBody), "claude-sonnet-5")
	if _, ok := store.Get(cachebump.RecordKey(cachebump.RouteClaudeCode, "sess-hdr-on-killed")); ok {
		t.Error("expected header on to be ignored while the global switch is off")
	}

	// With the switch on, X-Cache-Bump: on arms a session on a route whose
	// own flag is off.
	cfg.CacheBump.Enabled = true
	cfg.CacheBump.Routes.ClaudeCode = false
	persistTestConfig(t, cfg)
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(cacheBumpTurnBody))
	req.Header.Set("x-session-id", "sess-hdr-on")
	req.Header.Set("X-Cache-Bump", "on")
	w = httptest.NewRecorder()
	srv.forwardToClaudeCode(w, req, config.Get().ClaudeCode, []byte(cacheBumpTurnBody), "claude-sonnet-5")
	if _, ok := store.Get(cachebump.RecordKey(cachebump.RouteClaudeCode, "sess-hdr-on")); !ok {
		t.Error("expected header on to arm a route-disabled session")
	}

	// X-Cache-Bump: off suppresses recording even with the switch on.
	cfg.CacheBump.Routes.ClaudeCode = true
	persistTestConfig(t, cfg)
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(cacheBumpTurnBody))
	req.Header.Set("x-session-id", "sess-hdr-off")
	req.Header.Set("X-Cache-Bump", "off")
	w = httptest.NewRecorder()
	srv.forwardToClaudeCode(w, req, config.Get().ClaudeCode, []byte(cacheBumpTurnBody), "claude-sonnet-5")
	if _, ok := store.Get(cachebump.RecordKey(cachebump.RouteClaudeCode, "sess-hdr-off")); ok {
		t.Error("expected header off to suppress recording")
	}
}

func TestCacheBump_BumpRateLimitCoolsRecordedAccount(t *testing.T) {
	upstream := &cacheBumpUpstream{
		status:   http.StatusOK,
		respBody: `{"id":"m1","type":"message","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":1200}}`,
	}
	// The client turn succeeds; the bump that follows is rate limited.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if upstream.lenRequests() >= 1 {
			w.Header().Set(claudecode.HeaderRetryAfter, "120")
			w.Header().Set("Content-Type", "application/json")
			upstream.record(r)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error"}`))
			return
		}
		upstream.handler().ServeHTTP(w, r)
	}))
	defer ts.Close()

	config.SetForTest(cacheBumpTestConfig(t, ts.URL))
	resetCCPoolForTest()

	srv, store, sched := newCacheBumpServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(cacheBumpTurnBody))
	req.Header.Set("x-session-id", "sess-429")
	w := httptest.NewRecorder()
	srv.forwardToClaudeCode(w, req, config.Get().ClaudeCode, []byte(cacheBumpTurnBody), "claude-sonnet-5")

	rec, ok := store.Get(cachebump.RecordKey(cachebump.RouteClaudeCode, "sess-429"))
	if !ok {
		t.Fatal("expected session recorded")
	}

	sched.Now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	sched.Tick(context.Background())

	pool, _ := srv.getOrCreateCCPool(config.Get().ClaudeCode)
	acc, ok := pool.GetAccount(rec.AccountID)
	if !ok {
		t.Fatalf("account %q missing from pool", rec.AccountID)
	}
	// A bump burns the same window a real turn does: a 429 it triggers must
	// cool the account down instead of leaving the pool believing it is free.
	if !acc.CooldownUntil.After(time.Now()) {
		t.Errorf("expected bump 429 to cool the account down, CooldownUntil=%v", acc.CooldownUntil)
	}
}

func TestCacheBump_BumpUpdatesAccountRateLimits(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(claudecode.HeaderRequestsLimit, "1000")
		w.Header().Set(claudecode.HeaderRequestsRemaining, "42")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"m1","type":"message","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":1200}}`))
	}))
	defer ts.Close()

	config.SetForTest(cacheBumpTestConfig(t, ts.URL))
	resetCCPoolForTest()

	srv, store, sched := newCacheBumpServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(cacheBumpTurnBody))
	req.Header.Set("x-session-id", "sess-rl")
	w := httptest.NewRecorder()
	srv.forwardToClaudeCode(w, req, config.Get().ClaudeCode, []byte(cacheBumpTurnBody), "claude-sonnet-5")

	rec, ok := store.Get(cachebump.RecordKey(cachebump.RouteClaudeCode, "sess-rl"))
	if !ok {
		t.Fatal("expected session recorded")
	}

	pool, _ := srv.getOrCreateCCPool(config.Get().ClaudeCode)
	pool.UpdateAccountRateLimits(rec.AccountID, claudecode.RateLimits{RequestsRemaining: 999, LastUpdated: time.Now()})

	sched.Now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	sched.Tick(context.Background())

	acc, _ := pool.GetAccount(rec.AccountID)
	if acc.RateLimits.RequestsRemaining != 42 {
		t.Errorf("expected bump response rate limits recorded, got RequestsRemaining=%d", acc.RateLimits.RequestsRemaining)
	}
}

func TestCacheBump_TokenRefreshFailureStopsWithoutStaleToken(t *testing.T) {
	upstream := &cacheBumpUpstream{
		status:   http.StatusOK,
		respBody: `{"id":"m1","type":"message","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":1200}}`,
	}
	ts := httptest.NewServer(upstream.handler())
	defer ts.Close()

	config.SetForTest(cacheBumpTestConfig(t, ts.URL))
	resetCCPoolForTest()

	srv, store, sched := newCacheBumpServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(cacheBumpTurnBody))
	req.Header.Set("x-session-id", "sess-refresh")
	w := httptest.NewRecorder()
	srv.forwardToClaudeCode(w, req, config.Get().ClaudeCode, []byte(cacheBumpTurnBody), "claude-sonnet-5")

	rec, ok := store.Get(cachebump.RecordKey(cachebump.RouteClaudeCode, "sess-refresh"))
	if !ok {
		t.Fatal("expected session recorded")
	}
	turnRequests := upstream.lenRequests()

	// The recorded account's token is about to expire and every refresh
	// attempt fails.
	pool, _ := srv.getOrCreateCCPool(config.Get().ClaudeCode)
	pool.SetTokenRefresher(func(string) (string, string, int, error) {
		return "", "", 0, errors.New("refresh endpoint down")
	})
	expired := time.Now().Add(-time.Minute)
	pool.AddOrUpdateAccount(claudecode.AccountConfig{
		ID:           rec.AccountID,
		Token:        "tok-stale",
		RefreshToken: "refresh-token",
		ExpiresAt:    &expired,
		Type:         "oauth",
		Enabled:      true,
	})

	sched.Now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	sched.Tick(context.Background())

	if upstream.lenRequests() != turnRequests {
		t.Errorf("expected no bump sent with a stale token, got %d extra requests",
			upstream.lenRequests()-turnRequests)
	}
	got, _ := store.Get(rec.Key)
	if !got.Stopped || got.StopReason != "account_unavailable" {
		t.Errorf("expected account_unavailable stop, got %+v", got)
	}
}

func TestCacheBump_BumpFiresPinnedToRecordedAccount(t *testing.T) {
	upstream := &cacheBumpUpstream{
		status:   http.StatusOK,
		respBody: `{"id":"m1","type":"message","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":1200,"cache_creation_input_tokens":0}}`,
	}
	ts := httptest.NewServer(upstream.handler())
	defer ts.Close()

	config.SetForTest(cacheBumpTestConfig(t, ts.URL))
	resetCCPoolForTest()

	srv, store, sched := newCacheBumpServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(cacheBumpTurnBody))
	req.Header.Set("x-session-id", "sess-pin")
	w := httptest.NewRecorder()
	srv.forwardToClaudeCode(w, req, config.Get().ClaudeCode, []byte(cacheBumpTurnBody), "claude-sonnet-5")

	if upstream.lenRequests() != 1 {
		t.Fatalf("expected 1 upstream request from the client turn, got %d", upstream.lenRequests())
	}

	// Fire the bump: the scheduler must replay against acc-a's token even
	// though the pool could pick acc-b.
	future := time.Now().Add(10 * time.Minute)
	sched.Now = func() time.Time { return future }
	sched.Tick(context.Background())

	if upstream.lenRequests() != 2 {
		t.Fatalf("expected bump request fired, got %d upstream requests", upstream.lenRequests())
	}
	bump := upstream.lastRequest()
	if bump.authToken != "tok-a" {
		t.Errorf("expected bump pinned to tok-a, got %q", bump.authToken)
	}

	var replay map[string]any
	if err := json.Unmarshal([]byte(bump.body), &replay); err != nil {
		t.Fatalf("bump body invalid: %v", err)
	}
	if replay["max_tokens"].(float64) != 1 {
		t.Errorf("expected bump max_tokens 1, got %v", replay["max_tokens"])
	}

	rec, _ := store.Get(cachebump.RecordKey(cachebump.RouteClaudeCode, "sess-pin"))
	if rec.Bumps != 1 || rec.Stopped {
		t.Errorf("expected one bump, active: %+v", rec)
	}
	if rec.LastCacheReadTokens != 1200 {
		t.Errorf("expected cache read tokens 1200, got %d", rec.LastCacheReadTokens)
	}
}

func TestCacheBump_DisabledAccountNeverServesBump(t *testing.T) {
	upstream := &cacheBumpUpstream{
		status:   http.StatusOK,
		respBody: `{"id":"m1","type":"message","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":1200,"cache_creation_input_tokens":0}}`,
	}
	ts := httptest.NewServer(upstream.handler())
	defer ts.Close()

	cfg := cacheBumpTestConfig(t, ts.URL)
	config.SetForTest(cfg)
	resetCCPoolForTest()

	pool, _ := getOrCreateCCPool(cfg.ClaudeCode)

	srv, store, sched := newCacheBumpServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(cacheBumpTurnBody))
	req.Header.Set("x-session-id", "sess-disable")
	w := httptest.NewRecorder()
	srv.forwardToClaudeCode(w, req, config.Get().ClaudeCode, []byte(cacheBumpTurnBody), "claude-sonnet-5")

	// Disable the account that owns the cache entry. A bump must never
	// silently fail over to acc-b.
	acc, _ := pool.GetAccount("acc-a")
	acc.Enabled = false

	future := time.Now().Add(10 * time.Minute)
	sched.Now = func() time.Time { return future }
	sched.Tick(context.Background())

	if upstream.lenRequests() != 1 {
		t.Errorf("expected no bump request to upstream, got %d requests", upstream.lenRequests())
	}
	rec, _ := store.Get(cachebump.RecordKey(cachebump.RouteClaudeCode, "sess-disable"))
	if !rec.Stopped || rec.StopReason != "account_unavailable" {
		t.Errorf("expected account_unavailable stop, got %+v", rec)
	}
}

func TestCacheBump_PaidWriteStopsSession(t *testing.T) {
	upstream := &cacheBumpUpstream{
		status:   http.StatusOK,
		respBody: `{"id":"m1","type":"message","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":5000}}`,
	}
	ts := httptest.NewServer(upstream.handler())
	defer ts.Close()

	config.SetForTest(cacheBumpTestConfig(t, ts.URL))
	resetCCPoolForTest()

	srv, store, sched := newCacheBumpServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(cacheBumpTurnBody))
	req.Header.Set("x-session-id", "sess-paid")
	w := httptest.NewRecorder()
	srv.forwardToClaudeCode(w, req, config.Get().ClaudeCode, []byte(cacheBumpTurnBody), "claude-sonnet-5")

	future := time.Now().Add(10 * time.Minute)
	sched.Now = func() time.Time { return future }
	sched.Tick(context.Background())

	rec, _ := store.Get(cachebump.RecordKey(cachebump.RouteClaudeCode, "sess-paid"))
	if !rec.Stopped || rec.StopReason != "paid_write" {
		t.Errorf("expected paid_write stop, got %+v", rec)
	}
}

func TestCacheBump_NewTurnRearmsSession(t *testing.T) {
	upstream := &cacheBumpUpstream{
		status:   http.StatusOK,
		respBody: `{"id":"m1","type":"message","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":1200,"cache_creation_input_tokens":0}}`,
	}
	ts := httptest.NewServer(upstream.handler())
	defer ts.Close()

	config.SetForTest(cacheBumpTestConfig(t, ts.URL))
	resetCCPoolForTest()

	srv, store, sched := newCacheBumpServer(t)

	key := cachebump.RecordKey(cachebump.RouteClaudeCode, "sess-rearm")
	sendTurn := func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(cacheBumpTurnBody))
		req.Header.Set("x-session-id", "sess-rearm")
		w := httptest.NewRecorder()
		srv.forwardToClaudeCode(w, req, config.Get().ClaudeCode, []byte(cacheBumpTurnBody), "claude-sonnet-5")
		if w.Code != http.StatusOK {
			t.Fatalf("turn failed: %d", w.Code)
		}
	}

	sendTurn()
	future := time.Now().Add(10 * time.Minute)
	sched.Now = func() time.Time { return future }
	sched.Tick(context.Background())

	rec, _ := store.Get(key)
	if rec.Bumps != 1 {
		t.Fatalf("expected 1 bump, got %d", rec.Bumps)
	}

	// The bump paid a write upstream would stop it; simulate an upstream
	// that stopped serving cache reads for this session. Advance the clock
	// past the rescheduled NextBump first.
	upstream.mu.Lock()
	upstream.respBody = `{"id":"m2","type":"message","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":9000}}`
	upstream.mu.Unlock()
	later := future.Add(5 * time.Minute)
	sched.Now = func() time.Time { return later }
	sched.Tick(context.Background())

	rec, _ = store.Get(key)
	if !rec.Stopped || rec.StopReason != "paid_write" {
		t.Fatalf("expected paid_write stop, got %+v", rec)
	}

	// A new real client turn re-arms the record.
	upstream.mu.Lock()
	upstream.respBody = `{"id":"m3","type":"message","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":100,"cache_creation_input_tokens":0}}`
	upstream.mu.Unlock()
	sendTurn()

	rec, _ = store.Get(key)
	if rec.Stopped || rec.StopReason != "" || rec.Bumps != 0 {
		t.Errorf("expected record re-armed, got %+v", rec)
	}
}
