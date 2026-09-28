package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/headroom"
	"antigravity-go-proxy/internal/headroom/stages/ccr"
)

// upstreamUnifiedHeaders mirrors the subscription headers seen in
// .reference/claude-code-headers-*.jsonl, plus a made-up window to pin the
// prefix match.
var upstreamUnifiedHeaders = map[string]string{
	"anthropic-ratelimit-unified-status":               "allowed",
	"anthropic-ratelimit-unified-reset":                "1790000000",
	"anthropic-ratelimit-unified-representative-claim": "five_hour",
	"anthropic-ratelimit-unified-5h-utilization":       "0.04",
	"anthropic-ratelimit-unified-7d-utilization":       "0.31",
	"anthropic-ratelimit-unified-future-window":        "x",
}

func unifiedUpstream(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range upstreamUnifiedHeaders {
			w.Header().Set(k, v)
		}
		w.Header().Set("X-Upstream-Private", "secret")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func resetCCUnifiedPoolForTest(t *testing.T) {
	t.Helper()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", t.TempDir())
	ccPoolMu.Lock()
	ccPoolInst = nil
	ccHTTPClient = nil
	ccPoolStale = false
	ccPoolMu.Unlock()
}

func TestForwardToClaudeCode_ForwardsUnifiedHeaders(t *testing.T) {
	cases := []struct {
		name    string
		forward *bool
		want    bool
	}{
		{"default", nil, true},
		{"enabled", boolPtr(true), true},
		{"disabled", boolPtr(false), false},
	}
	for _, tc := range cases {
		for _, status := range []int{http.StatusOK, http.StatusTooManyRequests} {
			t.Run(tc.name+"/"+http.StatusText(status), func(t *testing.T) {
				resetCCUnifiedPoolForTest(t)
				body := `{"id":"msg_01","type":"message","content":[]}`
				if status == http.StatusTooManyRequests {
					body = `{"type":"error","error":{"type":"rate_limit_error","message":"Error"}}`
				}
				upstream := unifiedUpstream(t, status, body)
				cfg := claudecode.Config{
					Enabled:               true,
					BaseURL:               upstream.URL,
					Mode:                  "pool",
					Accounts:              []claudecode.AccountConfig{{ID: "acc1", Token: "sk-ant-test", Enabled: true}},
					Allowlist:             claudecode.DefaultAllowlist(),
					Routing:               claudecode.DefaultRoutingConfig(),
					ForwardUnifiedHeaders: tc.forward,
				}

				reqBody := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
				req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
				w := httptest.NewRecorder()
				(&Server{}).forwardToClaudeCode(w, req, cfg, []byte(reqBody), "claude-sonnet-5")

				if w.Code != status {
					t.Fatalf("status = %d, want %d (body: %s)", w.Code, status, w.Body.String())
				}
				for k, v := range upstreamUnifiedHeaders {
					got := w.Header().Get(k)
					if tc.want && got != v {
						t.Errorf("%s = %q, want %q", k, got, v)
					}
					if !tc.want && got != "" {
						t.Errorf("%s = %q forwarded with the switch off", k, got)
					}
				}
				if got := w.Header().Get("X-Upstream-Private"); got != "" {
					t.Errorf("unrelated header forwarded: %q", got)
				}
			})
		}
	}
}

func TestClaudeCodeConfig_ForwardUnifiedHeadersRoundTrip(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	seedCCConfig(t)

	getValue := func(t *testing.T) any {
		t.Helper()
		rec := doJSON(t, srv, http.MethodGet, "/api/claudecode/config", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET status = %d: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Config map[string]any `json:"config"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		return resp.Config["forwardUnifiedHeaders"]
	}

	if got := getValue(t); got != true {
		t.Fatalf("default forwardUnifiedHeaders = %v, want true", got)
	}

	rec := doJSON(t, srv, http.MethodPost, "/api/claudecode/config", `{"forwardUnifiedHeaders":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := getValue(t); got != false {
		t.Errorf("after POST false, GET = %v", got)
	}
	if config.Get().ClaudeCode.ForwardUnifiedHeadersEnabled() {
		t.Error("config still has forwarding enabled")
	}

	// Rewrites of the claudecode section that do not name the field keep it.
	rec = doJSON(t, srv, http.MethodPost, "/api/claudecode/config", `{"enabled":true,"mode":"single"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := getValue(t); got != false {
		t.Errorf("unrelated save reset the switch: GET = %v", got)
	}

	rec = doJSON(t, srv, http.MethodPost, "/api/claudecode/config", `{"forwardUnifiedHeaders":"no"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-boolean: status = %d, want 400", rec.Code)
	}
	if config.Get().ClaudeCode.ForwardUnifiedHeadersEnabled() {
		t.Error("rejected value changed the setting")
	}

	rec = doJSON(t, srv, http.MethodPost, "/api/claudecode/config", `{"forwardUnifiedHeaders":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := getValue(t); got != true {
		t.Errorf("after POST true, GET = %v", got)
	}

	// null clears the explicit value and falls back to the default.
	_ = doJSON(t, srv, http.MethodPost, "/api/claudecode/config", `{"forwardUnifiedHeaders":false}`)
	rec = doJSON(t, srv, http.MethodPost, "/api/claudecode/config", `{"forwardUnifiedHeaders":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST null status = %d: %s", rec.Code, rec.Body.String())
	}
	if config.Get().ClaudeCode.ForwardUnifiedHeaders != nil {
		t.Errorf("null did not clear the setting: %v", *config.Get().ClaudeCode.ForwardUnifiedHeaders)
	}
	if got := getValue(t); got != true {
		t.Errorf("after POST null, GET = %v, want true", got)
	}
}

func unifiedAt(base time.Time) *claudecode.Unified {
	u := 0.25
	return &claudecode.Unified{
		Status:     "allowed",
		Reset:      base.Add(3 * time.Hour),
		FiveHour:   claudecode.UnifiedWindow{Utilization: &u, Reset: base.Add(3 * time.Hour), Status: "allowed"},
		ObservedAt: base,
	}
}

// A pool rebuilt after a config change keeps the in-memory unified snapshot
// of each surviving account and retires the old pool, so the old pool can no
// longer save its (now stale) account set.
func TestGetOrCreateCCPool_RebuildInheritsUnifiedAndRetiresOld(t *testing.T) {
	resetCCUnifiedPoolForTest(t)
	accs := []claudecode.AccountConfig{
		{ID: "keep", Token: "t1", Type: "oauth", Enabled: true, Source: "oauth"},
		{ID: "gone", Token: "t2", Type: "oauth", Enabled: true, Source: "oauth"},
	}
	cfg := claudecode.Config{BaseURL: "https://example.invalid", Accounts: accs}
	oldPool, _ := getOrCreateCCPool(cfg)

	snap := unifiedAt(time.Now().Truncate(time.Second))
	keep, _ := oldPool.GetAccount("keep")
	keep.RateLimits.Unified = snap

	cfg.Accounts = accs[:1]
	newPool, _ := getOrCreateCCPool(cfg)
	if newPool == oldPool {
		t.Fatal("pool was not rebuilt")
	}
	got, ok := newPool.GetAccount("keep")
	if !ok {
		t.Fatal("surviving account missing from the new pool")
	}
	if got.RateLimits.Unified != snap {
		t.Errorf("surviving account lost its unified snapshot: %+v", got.RateLimits.Unified)
	}

	// A change that is always saved, on an account only the old pool has.
	rejected := unifiedAt(time.Now().Truncate(time.Second))
	rejected.Status = "rejected"
	oldPool.RecordSuccess("gone", 1, 0, claudecode.RateLimits{LastUpdated: time.Now(), Unified: rejected})
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(claudecode.DefaultStoragePath()); err == nil && strings.Contains(string(data), `"gone"`) {
			t.Fatalf("retired pool saved its stale account set: %s", data)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Invalidation from the management handlers rebuilds the pool on next use
// but still hands the old pool's snapshots over.
func TestGetOrCreateCCPool_InvalidateKeepsUnified(t *testing.T) {
	resetCCUnifiedPoolForTest(t)
	cfg := claudecode.Config{
		BaseURL:  "https://example.invalid",
		Accounts: []claudecode.AccountConfig{{ID: "a", Token: "t", Enabled: true}},
	}
	oldPool, _ := getOrCreateCCPool(cfg)
	snap := unifiedAt(time.Now().Truncate(time.Second))
	acc, _ := oldPool.GetAccount("a")
	acc.RateLimits.Unified = snap

	ccPoolMu.Lock()
	invalidateCCPoolLocked()
	ccPoolMu.Unlock()

	newPool, client := getOrCreateCCPool(cfg)
	if newPool == oldPool {
		t.Fatal("invalidated pool was reused")
	}
	if client == nil {
		t.Fatal("no client after rebuild")
	}
	got, _ := newPool.GetAccount("a")
	if got.RateLimits.Unified != snap {
		t.Errorf("unified snapshot not inherited across invalidation")
	}
	if again, _ := getOrCreateCCPool(cfg); again != newPool {
		t.Error("pool rebuilt again without a change")
	}
}

// The CCR (headroom) path forwards the unified headers of a successful
// upstream response too, for both streaming and JSON requests.
func TestForwardToClaudeCode_CCRForwardsUnifiedHeaders(t *testing.T) {
	const sse = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"m","role":"assistant","model":"claude-sonnet-5","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	const jsonBody = `{"id":"m","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

	for _, stream := range []bool{true, false} {
		for _, forward := range []bool{true, false} {
			name := "json"
			if stream {
				name = "stream"
			}
			if forward {
				name += "/on"
			} else {
				name += "/off"
			}
			t.Run(name, func(t *testing.T) {
				resetCCUnifiedPoolForTest(t)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					for k, v := range upstreamUnifiedHeaders {
						w.Header().Set(k, v)
					}
					w.Header().Set("X-Upstream-Private", "secret")
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						w.WriteHeader(http.StatusOK)
						_, _ = w.Write([]byte(sse))
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(jsonBody))
				}))
				defer upstream.Close()

				cfg := claudecode.Config{
					Enabled:               true,
					BaseURL:               upstream.URL,
					Accounts:              []claudecode.AccountConfig{{ID: "acc-cc", Token: "tok-cc", Enabled: true}},
					Allowlist:             []claudecode.ModelConfig{{ID: "claude-sonnet-5", Enabled: true}},
					Routing:               claudecode.DefaultRoutingConfig(),
					ForwardUnifiedHeaders: &forward,
				}
				store := ccr.NewCCRStore(1024 * 1024)
				srv := &Server{
					headroom: headroom.NewEngine(headroom.Config{Enabled: true, CCR: headroom.CCRConfig{Enabled: true}}, nil, ccr.NewStage(store)),
					ccrStore: store,
				}
				if !srv.isCCREnabled() {
					t.Fatal("test setup: CCR path not enabled")
				}

				reqBody := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
				if stream {
					reqBody = `{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
				}
				req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
				w := httptest.NewRecorder()
				srv.forwardToClaudeCode(w, req, cfg, []byte(reqBody), "claude-sonnet-5")

				if w.Code != http.StatusOK {
					t.Fatalf("status = %d: %s", w.Code, w.Body.String())
				}
				if !strings.Contains(w.Body.String(), "hi") {
					t.Fatalf("response body missing content: %s", w.Body.String())
				}
				for k, v := range upstreamUnifiedHeaders {
					got := w.Header().Get(k)
					if forward && got != v {
						t.Errorf("%s = %q, want %q", k, got, v)
					}
					if !forward && got != "" {
						t.Errorf("%s = %q forwarded with the switch off", k, got)
					}
				}
				if got := w.Header().Get("X-Upstream-Private"); got != "" {
					t.Errorf("unrelated header forwarded: %q", got)
				}
			})
		}
	}
}
