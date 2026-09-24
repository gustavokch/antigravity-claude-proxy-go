package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
	"antigravity-go-proxy/internal/zen"
)

type zenTestBackend struct{}

func (m *zenTestBackend) FetchAvailableModels(ctx context.Context) (cloudcode.Response, error) {
	// Minimal parseable Cloud Code catalog so GET /v1/models reaches the
	// gateway discovery blocks in tests.
	return cloudcode.Response{
		Body: []byte(`{
			"defaultAgentModelId":"gemini-test",
			"agentModelSorts":[{"displayName":"Recommended","groups":[{"modelIds":["gemini-test"]}]}],
			"models":{"gemini-test":{"displayName":"Gemini Test"}}
		}`),
	}, nil
}

func (m *zenTestBackend) StreamGenerateContent(ctx context.Context, req map[string]any, cb func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	return cloudcode.Response{Body: []byte(`{}`)}, nil
}

func newZenTestServer(t *testing.T) *Server {
	t.Helper()
	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &zenTestBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server
}

func saveZenTestConfig(t *testing.T, zenCfg map[string]any) {
	t.Helper()
	// Sequential tests run before the package's parallel tests; restore the
	// ambient config on cleanup so a Zen-enabled allowlist cannot leak into
	// tests that expect the default routing (e.g. server_test.go).
	orig := config.Get()
	t.Cleanup(func() { config.SetForTest(orig) })
	if _, err := config.Save(map[string]any{"zen": zenCfg}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
}

func zenUpstream(t *testing.T, gotBody *[]byte, gotAuth *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path = %q, want /v1/messages", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		*gotBody = body
		*gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":5}}`))
	}))
}

func postZenMessages(t *testing.T, server *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func TestServer_ForwardToZen_AliasMatch(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")

	var gotBody []byte
	var gotAuth string
	upstream := zenUpstream(t, &gotBody, &gotAuth)
	defer upstream.Close()

	saveZenTestConfig(t, map[string]any{
		"enabled": true,
		"apiKey":  "sk-zen-test",
		"baseUrl": upstream.URL,
		"allowlist": []map[string]any{
			{"id": "claude-sonnet-4-6", "alias": "sonnet", "enabled": true},
		},
	})

	rec := postZenMessages(t, newZenTestServer(t),
		`{"model":"sonnet","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)

	if rec.Code != 200 {
		t.Fatalf("client status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if gotAuth != "Bearer sk-zen-test" {
		t.Errorf("Authorization = %q, want Bearer sk-zen-test", gotAuth)
	}
	if !strings.Contains(string(gotBody), `"model":"claude-sonnet-4-6"`) {
		t.Errorf("upstream body should rewrite alias to zen id, got %s", gotBody)
	}
}

func TestServer_ForwardToZen_OpencodePrefixMatch(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")

	var gotBody []byte
	var gotAuth string
	upstream := zenUpstream(t, &gotBody, &gotAuth)
	defer upstream.Close()

	saveZenTestConfig(t, map[string]any{
		"enabled": true,
		"apiKey":  "sk-zen-test",
		"baseUrl": upstream.URL,
		"allowlist": []map[string]any{
			{"id": "claude-sonnet-4-6", "enabled": true},
		},
	})

	rec := postZenMessages(t, newZenTestServer(t),
		`{"model":"opencode/claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)

	if rec.Code != 200 {
		t.Fatalf("client status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(string(gotBody), `"model":"claude-sonnet-4-6"`) {
		t.Errorf("upstream body should strip opencode/ prefix, got %s", gotBody)
	}
}

func TestServer_ForwardToZen_SSE(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
	}))
	defer upstream.Close()

	saveZenTestConfig(t, map[string]any{
		"enabled": true,
		"apiKey":  "sk-zen-test",
		"baseUrl": upstream.URL,
		"allowlist": []map[string]any{
			{"id": "claude-sonnet-4-6", "enabled": true},
		},
	})

	rec := postZenMessages(t, newZenTestServer(t),
		`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`)

	if rec.Code != 200 {
		t.Fatalf("client status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "message_start") {
		t.Errorf("expected SSE passthrough, got %s", rec.Body.String())
	}
}

func TestServer_Messages_KeylessZenEntryFallsThrough(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")
	resetZenKeylessWarning()

	zenHit := false
	zenStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zenHit = true
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer zenStub.Close()

	var customHit bool
	customTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		customHit = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"type":"message","id":"msg_custom","content":[{"type":"text","text":"custom_ok"}]}`))
	}))
	defer customTarget.Close()

	saveZenTestConfig(t, map[string]any{
		"enabled": true,
		"baseUrl": zenStub.URL,
		"allowlist": []map[string]any{
			{"id": "claude-sonnet-4-6", "enabled": true},
		},
	})
	if _, err := config.Save(map[string]any{
		"customEndpoints": map[string]any{
			"claude-sonnet-4-6": map[string]any{
				"url":    customTarget.URL,
				"apiKey": "custom-secret",
			},
		},
	}); err != nil {
		t.Fatalf("config.Save customEndpoints: %v", err)
	}

	rec := postZenMessages(t, newZenTestServer(t),
		`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)

	if zenHit {
		t.Error("keyless Zen config must not claim the route (Zen upstream was hit)")
	}
	if !customHit {
		t.Fatalf("keyless Zen entry must fall through to the custom endpoint; status = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec.Code != 200 {
		t.Fatalf("client status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

func TestMatchZenModelEntry_SkipsWhenNoKey(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "")
	resetZenKeylessWarning()
	cfg := config.ZenConfig{Enabled: true, Allowlist: []config.ZenModelConfig{
		{ID: "claude-sonnet-4-6", Enabled: true},
	}}
	if _, ok := matchZenModelEntry(cfg, "claude-sonnet-4-6"); ok {
		t.Fatal("no key configured; Zen must not claim the route")
	}
}

func TestMatchZenModelEntry_KeylessWarnsOnceAndRearms(t *testing.T) {
	t.Setenv("OPENCODE_API_KEY", "")
	resetZenKeylessWarning()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev); resetZenKeylessWarning() })

	cfg := config.ZenConfig{Enabled: true, Allowlist: []config.ZenModelConfig{
		{ID: "claude-sonnet-4-6", Enabled: true},
	}}
	if _, ok := matchZenModelEntry(cfg, "claude-sonnet-4-6"); ok {
		t.Fatal("keyless config must not match")
	}
	if _, ok := matchZenModelEntry(cfg, "claude-sonnet-4-6"); ok {
		t.Fatal("keyless config must not match")
	}
	if got := bytes.Count(buf.Bytes(), []byte("fall through")); got != 1 {
		t.Fatalf("keyless warn fired %d times, want exactly once; log=%q", got, buf.String())
	}
	resetZenKeylessWarning()
	if _, ok := matchZenModelEntry(cfg, "claude-sonnet-4-6"); ok {
		t.Fatal("keyless config must not match")
	}
	if got := bytes.Count(buf.Bytes(), []byte("fall through")); got != 2 {
		t.Fatalf("keyless warn fired %d times after re-arm, want 2; log=%q", got, buf.String())
	}
}

func TestMatchZenModelEntry_SkipsNonAnthropicWire(t *testing.T) {
	cfg := config.ZenConfig{
		Enabled: true,
		APIKey:  "sk-test",
		Allowlist: []config.ZenModelConfig{
			{ID: "gpt-5", Enabled: true},
			{ID: "claude-sonnet-4-6", Enabled: true},
		},
	}
	if _, ok := matchZenModelEntry(cfg, "gpt-5"); ok {
		t.Fatal("gpt-5 is not Anthropic-wire; it must not claim the Zen route")
	}
	if _, ok := matchZenModelEntry(cfg, "claude-sonnet-4-6"); !ok {
		t.Fatal("claude-sonnet-4-6 must still match")
	}
}

func TestServer_Messages_NonWireZenEntryFallsThrough(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")

	zenHit := false
	zenStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zenHit = true
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer zenStub.Close()

	var customHit bool
	customTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		customHit = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"type":"message","id":"msg_custom","content":[{"type":"text","text":"custom_ok"}]}`))
	}))
	defer customTarget.Close()

	saveZenTestConfig(t, map[string]any{
		"enabled": true,
		"apiKey":  "sk-zen-test",
		"baseUrl": zenStub.URL,
		"allowlist": []map[string]any{
			{"id": "gpt-5", "enabled": true},
		},
	})
	if _, err := config.Save(map[string]any{
		"customEndpoints": map[string]any{
			"gpt-5": map[string]any{
				"url":    customTarget.URL,
				"apiKey": "custom-secret",
			},
		},
	}); err != nil {
		t.Fatalf("config.Save customEndpoints: %v", err)
	}

	rec := postZenMessages(t, newZenTestServer(t),
		`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)

	if zenHit {
		t.Error("non-wire Zen entry must not claim the route (Zen upstream was hit)")
	}
	if !customHit {
		t.Fatalf("non-wire Zen entry must fall through to the custom endpoint; status = %d body = %s", rec.Code, rec.Body.String())
	}
	if rec.Code != 200 {
		t.Fatalf("client status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

func TestServer_ForwardToZen_EnvKeyFallback(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "sk-zen-env")

	var gotBody []byte
	var gotAuth string
	upstream := zenUpstream(t, &gotBody, &gotAuth)
	defer upstream.Close()

	saveZenTestConfig(t, map[string]any{
		"enabled": true,
		"baseUrl": upstream.URL,
		"allowlist": []map[string]any{
			{"id": "claude-sonnet-4-6", "enabled": true},
		},
	})

	rec := postZenMessages(t, newZenTestServer(t),
		`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)

	if rec.Code != 200 {
		t.Fatalf("client status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if gotAuth != "Bearer sk-zen-env" {
		t.Errorf("Authorization = %q, want Bearer sk-zen-env", gotAuth)
	}
}

func TestServer_ForwardToZen_DefenceInDepthGuards(t *testing.T) {
	// Direct calls to forwardToZen bypass matchZenModelEntry (the routing
	// layer that now rejects keyless and non-wire configs). The forwarder
	// keeps 500 guards so a programming error fails loud instead of sending
	// an unauthenticated or misrouted request upstream.
	server := newZenTestServer(t)
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)
	reqMap := map[string]any{"model": "claude-sonnet-4-6", "max_tokens": 100}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	server.forwardToZen(rec, req, config.ZenConfig{Enabled: true}, body, reqMap,
		"claude-sonnet-4-6", config.ZenModelConfig{ID: "claude-sonnet-4-6", Enabled: true})
	if rec.Code != 500 {
		t.Errorf("keyless direct forward = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	server.forwardToZen(rec2, req, config.ZenConfig{Enabled: true, APIKey: "sk-zen-test"}, body, reqMap,
		"gpt-5.5", config.ZenModelConfig{ID: "gpt-5.5", Enabled: true})
	if rec2.Code != 500 {
		t.Errorf("non-wire direct forward = %d, want 500; body = %s", rec2.Code, rec2.Body.String())
	}
}

func TestServer_ForwardToZen_MaxTokensFillDefault(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")

	var gotBody []byte
	var gotAuth string
	upstream := zenUpstream(t, &gotBody, &gotAuth)
	defer upstream.Close()

	saveZenTestConfig(t, map[string]any{
		"enabled": true,
		"apiKey":  "sk-zen-test",
		"baseUrl": upstream.URL,
		"allowlist": []map[string]any{
			{"id": "claude-sonnet-4-6", "enabled": true},
		},
	})

	rec := postZenMessages(t, newZenTestServer(t),
		`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != 200 {
		t.Fatalf("client status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if maxTok, _ := sent["max_tokens"].(float64); int(maxTok) != zen.DefaultMaxOutputTokens {
		t.Errorf("max_tokens = %v, want default %d", sent["max_tokens"], zen.DefaultMaxOutputTokens)
	}
}

func TestServer_ForwardToZen_AllowlistCapWinsOverDefault(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")

	var gotBody []byte
	var gotAuth string
	upstream := zenUpstream(t, &gotBody, &gotAuth)
	defer upstream.Close()

	saveZenTestConfig(t, map[string]any{
		"enabled": true,
		"apiKey":  "sk-zen-test",
		"baseUrl": upstream.URL,
		"allowlist": []map[string]any{
			{"id": "claude-sonnet-4-6", "enabled": true, "maxOutputTokens": 8000},
		},
	})

	rec := postZenMessages(t, newZenTestServer(t),
		`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != 200 {
		t.Fatalf("client status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if maxTok, _ := sent["max_tokens"].(float64); int(maxTok) != 8000 {
		t.Errorf("max_tokens = %v, want allowlist cap 8000", sent["max_tokens"])
	}
}

func TestServer_ForwardToZen_ExplicitMaxTokensNotClampedToDefault(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")

	var gotBody []byte
	var gotAuth string
	upstream := zenUpstream(t, &gotBody, &gotAuth)
	defer upstream.Close()

	saveZenTestConfig(t, map[string]any{
		"enabled": true,
		"apiKey":  "sk-zen-test",
		"baseUrl": upstream.URL,
		"allowlist": []map[string]any{
			{"id": "claude-sonnet-4-6", "enabled": true},
		},
	})

	rec := postZenMessages(t, newZenTestServer(t),
		`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"max_tokens":64000}`)

	if rec.Code != 200 {
		t.Fatalf("client status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if maxTok, _ := sent["max_tokens"].(float64); int(maxTok) != 64000 {
		t.Errorf("max_tokens = %v, want explicit client value 64000 (not clamped to default)", sent["max_tokens"])
	}
}

func TestServer_Models_SkipsNonAnthropicWireZenEntries(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")

	saveZenTestConfig(t, map[string]any{
		"enabled": true,
		"apiKey":  "sk-zen-test",
		"baseUrl": "http://127.0.0.1:1",
		"allowlist": []map[string]any{
			{"id": "gpt-5", "enabled": true},
			{"id": "claude-sonnet-4-6", "enabled": true},
		},
	})

	server := newZenTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("x-api-key", "test-proxy-key")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range body.Data {
		if id, _ := m["id"].(string); id != "" {
			seen[id] = true
		}
	}
	if !seen["claude-sonnet-4-6"] {
		t.Errorf("claude-sonnet-4-6 missing from /v1/models: %v", seen)
	}
	if seen["gpt-5"] {
		t.Errorf("gpt-5 is not Anthropic-wire and must not be advertised in /v1/models")
	}
}

func TestServer_Models_ZenMaxOutputUsesPackageDefault(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")

	saveZenTestConfig(t, map[string]any{
		"enabled": true,
		"apiKey":  "sk-zen-test",
		"baseUrl": "http://127.0.0.1:1",
		"allowlist": []map[string]any{
			{"id": "claude-sonnet-4-6", "enabled": true},
			{"id": "claude-opus-4-5", "enabled": true, "maxOutputTokens": 64000},
		},
	})

	server := newZenTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("x-api-key", "test-proxy-key")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]map[string]any{}
	for _, m := range body.Data {
		if id, _ := m["id"].(string); id != "" {
			byID[id] = m
		}
	}
	def, ok := byID["claude-sonnet-4-6"]
	if !ok {
		t.Fatalf("claude-sonnet-4-6 missing from /v1/models")
	}
	if maxOut, _ := def["max_output_tokens"].(float64); int(maxOut) != zen.DefaultMaxOutputTokens {
		t.Errorf("default max_output_tokens = %v, want package default %d", def["max_output_tokens"], zen.DefaultMaxOutputTokens)
	}
	if ctxWin, _ := def["context_window"].(float64); int(ctxWin) != defaultDiscoveryContextWindow {
		t.Errorf("context_window = %v, want discovery default %d", def["context_window"], defaultDiscoveryContextWindow)
	}
	capped, ok := byID["claude-opus-4-5"]
	if !ok {
		t.Fatalf("claude-opus-4-5 missing from /v1/models")
	}
	if maxOut, _ := capped["max_output_tokens"].(float64); int(maxOut) != 64000 {
		t.Errorf("explicit max_output_tokens = %v, want 64000", capped["max_output_tokens"])
	}
}

func TestMatchZenModelEntry(t *testing.T) {
	cfg := config.ZenConfig{APIKey: "sk-test", Allowlist: []config.ZenModelConfig{
		{ID: "claude-sonnet-4-6", Alias: "sonnet", Enabled: true},
		{ID: "gpt-5.5", Enabled: false},
		{ID: "gpt-5", Enabled: true},
	}}
	if _, ok := matchZenModelEntry(cfg, "sonnet"); !ok {
		t.Error("alias match failed")
	}
	if _, ok := matchZenModelEntry(cfg, "opencode/claude-sonnet-4-6"); !ok {
		t.Error("opencode/ prefix match failed")
	}
	if _, ok := matchZenModelEntry(cfg, "CLAUDE-SONNET-4-6"); !ok {
		t.Error("case-insensitive match failed")
	}
	if _, ok := matchZenModelEntry(cfg, "gpt-5.5"); ok {
		t.Error("disabled entry should not match")
	}
	if _, ok := matchZenModelEntry(cfg, "gpt-5"); ok {
		t.Error("enabled but non-Anthropic-wire entry should not match")
	}
	if _, ok := matchZenModelEntry(cfg, ""); ok {
		t.Error("empty model should not match")
	}
	if got := zenTargetModel(config.ZenModelConfig{ID: "opencode/claude-sonnet-4-6"}); got != "claude-sonnet-4-6" {
		t.Errorf("zenTargetModel = %q, want claude-sonnet-4-6", got)
	}
}

// Chat-wire allowlist entries must be claimed by the Zen route, canonicalized,
// forwarded to /v1/chat/completions, and the client must receive an
// Anthropic-shaped answer. Regression pin for the whole chat-wire dispatch:
// matchZenModelEntry → zenTargetModel → forwardToZen wire branch → SendChat.
func TestServer_ForwardToZen_ChatWireRouting(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")

	var gotPath, gotAuth string
	var gotReq map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","choices":[{"message":{"content":"yo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}))
	defer upstream.Close()

	saveZenTestConfig(t, map[string]any{
		"enabled": true,
		"apiKey":  "sk-zen-test",
		"baseUrl": upstream.URL,
		"allowlist": []map[string]any{
			{"id": "glm-5.3", "alias": "glm", "enabled": true},
		},
	})

	rec := postZenMessages(t, newZenTestServer(t),
		`{"model":"glm","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)

	if rec.Code != 200 {
		t.Fatalf("client status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-zen-test" {
		t.Errorf("Authorization = %q, want Bearer sk-zen-test", gotAuth)
	}
	if gotReq["model"] != "glm-5.3" {
		t.Errorf("upstream model = %v, want glm-5.3 (alias resolved to canonical)", gotReq["model"])
	}
	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("response not JSON: %v; body = %s", err, rec.Body.String())
	}
	if msg["type"] != "message" || msg["stop_reason"] != "end_turn" {
		t.Errorf("response not Anthropic-shaped: %s", rec.Body.String())
	}
	content, _ := msg["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "yo" {
		t.Errorf("content = %v", content)
	}
	usage, _ := msg["usage"].(map[string]any)
	if usage["input_tokens"] != 5.0 || usage["output_tokens"] != 2.0 {
		t.Errorf("usage = %v", usage)
	}
}
