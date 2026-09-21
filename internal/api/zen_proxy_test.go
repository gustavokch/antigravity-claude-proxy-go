package api

import (
	"context"
	"encoding/json"
	"io"
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
	return cloudcode.Response{Body: []byte(`{}`)}, nil
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

func TestServer_ForwardToZen_NoKey400(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")

	saveZenTestConfig(t, map[string]any{
		"enabled": true,
		"baseUrl": "http://127.0.0.1:1",
		"allowlist": []map[string]any{
			{"id": "claude-sonnet-4-6", "enabled": true},
		},
	})

	rec := postZenMessages(t, newZenTestServer(t),
		`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)

	if rec.Code != 400 {
		t.Fatalf("client status = %d, want 400; body = %s", rec.Code, rec.Body.String())
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

func TestServer_ForwardToZen_NonAnthropicID400(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")

	saveZenTestConfig(t, map[string]any{
		"enabled": true,
		"apiKey":  "sk-zen-test",
		"baseUrl": "http://127.0.0.1:1",
		"allowlist": []map[string]any{
			{"id": "gpt-5.5", "enabled": true},
		},
	})

	rec := postZenMessages(t, newZenTestServer(t),
		`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`)

	if rec.Code != 400 {
		t.Fatalf("client status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "gpt-5.5") {
		t.Errorf("error should name the model, got %s", rec.Body.String())
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

func TestMatchZenModelEntry(t *testing.T) {
	cfg := config.ZenConfig{Allowlist: []config.ZenModelConfig{
		{ID: "claude-sonnet-4-6", Alias: "sonnet", Enabled: true},
		{ID: "gpt-5.5", Enabled: false},
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
	if _, ok := matchZenModelEntry(cfg, ""); ok {
		t.Error("empty model should not match")
	}
	if got := zenTargetModel(config.ZenModelConfig{ID: "opencode/claude-sonnet-4-6"}); got != "claude-sonnet-4-6" {
		t.Errorf("zenTargetModel = %q, want claude-sonnet-4-6", got)
	}
}
