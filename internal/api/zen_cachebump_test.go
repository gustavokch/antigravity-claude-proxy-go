package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/cachebump"
	"antigravity-go-proxy/internal/config"
)

func TestCacheBump_ZenRecordsAndBumps(t *testing.T) {
	upstream := &cacheBumpUpstream{
		status:   http.StatusOK,
		respBody: `{"id":"m1","type":"message","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":800,"cache_creation_input_tokens":0}}`,
	}
	ts := httptest.NewServer(upstream.handler())
	defer ts.Close()

	cfg := cacheBumpTestConfig(t, ts.URL)
	cfg.ClaudeCode.Enabled = false // route the turn to Zen, not Claude Code
	cfg.Zen = config.ZenConfig{
		Enabled: true,
		BaseURL: ts.URL,
		APIKey:  "zen-key",
		Allowlist: []config.ZenModelConfig{
			{ID: "claude-sonnet-4-6", Enabled: true},
		},
	}
	cfg.CacheBump.Routes.Zen = true
	persistTestConfig(t, cfg)
	// persistTestConfig swaps the global config; restore defaults on cleanup
	// so the Zen route cannot leak into the package's parallel tests.
	t.Cleanup(func() { config.SetForTest(config.DefaultConfig()) })
	resetCCPoolForTest()

	srv, store, sched := newCacheBumpServer(t)

	zenBody := strings.Replace(cacheBumpTurnBody, `"claude-sonnet-5"`, `"claude-sonnet-4-6"`, 1)
	zenReqMap := map[string]any{
		"model":      "claude-sonnet-4-6",
		"max_tokens": 64000,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	}
	zenEntry := config.ZenModelConfig{ID: "claude-sonnet-4-6", Enabled: true}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(zenBody))
	req.Header.Set("x-session-id", "sess-zen")
	w := httptest.NewRecorder()
	srv.forwardToZen(w, req, config.Get().Zen, []byte(zenBody), zenReqMap, "claude-sonnet-4-6", zenEntry)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	key := cachebump.RecordKey(cachebump.RouteZen, "sess-zen")
	rec, ok := store.Get(key)
	if !ok {
		t.Fatal("expected zen session recorded")
	}
	if rec.Route != cachebump.RouteZen || rec.Model != "claude-sonnet-4-6" {
		t.Errorf("unexpected record: %+v", rec)
	}

	future := time.Now().Add(10 * time.Minute)
	sched.Now = func() time.Time { return future }
	sched.Tick(context.Background())

	if upstream.lenRequests() != 2 {
		t.Fatalf("expected bump request, got %d upstream requests", upstream.lenRequests())
	}
	bump := upstream.lastRequest()
	if bump.authToken != "zen-key" {
		t.Errorf("expected Bearer zen-key bump, got %q", bump.authToken)
	}
}
