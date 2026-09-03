package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
	"antigravity-go-proxy/internal/stats"
)

// captureBackend records the request the proxy actually dispatched.
type captureBackend struct {
	mu   sync.Mutex
	last map[string]any
}

func (b *captureBackend) StreamGenerateContent(ctx context.Context, req map[string]any, cb func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	b.mu.Lock()
	raw, _ := json.Marshal(req)
	_ = json.Unmarshal(raw, &b.last)
	b.mu.Unlock()
	return cloudcode.Response{}, nil
}

func (b *captureBackend) FetchAvailableModels(ctx context.Context) (cloudcode.Response, error) {
	return cloudcode.Response{Body: []byte(`{"defaultAgentModelId":"gemini-3.5-flash-low","models":{}}`)}, nil
}

func (b *captureBackend) seen() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last
}

func newHeadroomTestServer(t *testing.T, headroom map[string]any) (*Server, *captureBackend) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmp)
	t.Setenv("HOME", tmp)
	if _, err := config.Load(); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if _, err := config.Save(map[string]any{"headroom": headroom}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	backend := &captureBackend{}
	tracker, _ := stats.NewTracker("")
	srv, err := New(Options{
		APIKey:  "test-key",
		Backend: backend,
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
		Tracker: tracker,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, backend
}

func postMessages(t *testing.T, srv *Server, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
	req.Header.Set("x-api-key", "test-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func toolResultBody(payload string) map[string]any {
	return map[string]any{
		"model": "gemini-3.5-flash-low",
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "content": payload},
			}},
		},
	}
}

func firstToolResult(t *testing.T, req map[string]any) string {
	t.Helper()
	msgs, ok := req["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("no messages in dispatched request: %#v", req)
	}
	blocks := msgs[0].(map[string]any)["content"].([]any)
	return blocks[0].(map[string]any)["content"].(string)
}

func TestHeadroom_CompressesCloudCodeDispatch(t *testing.T) {
	srv, backend := newHeadroomTestServer(t, map[string]any{
		"enabled": true, "smartCrusher": true, "codeCompressor": true,
	})
	if rec := postMessages(t, srv, toolResultBody("{\n  \"a\": 1\n}")); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := firstToolResult(t, backend.seen()); got != `{"a":1}` {
		t.Errorf("backend received uncompressed payload: %q", got)
	}
}

func TestHeadroom_DisabledLeavesRequestIntact(t *testing.T) {
	original := "{\n  \"a\": 1\n}"
	srv, backend := newHeadroomTestServer(t, map[string]any{"enabled": false, "smartCrusher": true})
	if rec := postMessages(t, srv, toolResultBody(original)); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := firstToolResult(t, backend.seen()); got != original {
		t.Errorf("expected untouched payload, got %q", got)
	}
}

func TestHeadroom_CompressesKimiDispatch(t *testing.T) {
	var received []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{}}`))
	}))
	defer upstream.Close()

	tmp := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmp)
	t.Setenv("HOME", tmp)
	if _, err := config.Load(); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if _, err := config.Save(map[string]any{
		"headroom": map[string]any{"enabled": true, "smartCrusher": true},
		"kimi": map[string]any{
			"enabled": true, "baseUrl": upstream.URL, "apiKey": "k",
			"allowlist": []map[string]any{
				{"id": "kimi-k2", "alias": "k2", "enabled": true},
			},
		},
	}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	srv, err := New(Options{APIKey: "test-key", Backend: &captureBackend{}, Builder: proxyformat.NewBuilder(), Now: time.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := toolResultBody("{\n  \"a\": 1\n}")
	body["model"] = "kimi-k2"
	if rec := postMessages(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var dispatched map[string]any
	if err := json.Unmarshal(received, &dispatched); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if got := firstToolResult(t, dispatched); got != `{"a":1}` {
		t.Errorf("Kimi upstream received uncompressed payload: %q", got)
	}
}

func TestHeadroom_CompressesOpenRouterDispatch(t *testing.T) {
	var received []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{}}`))
	}))
	defer upstream.Close()

	tmp := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmp)
	t.Setenv("HOME", tmp)
	if _, err := config.Load(); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if _, err := config.Save(map[string]any{
		"headroom": map[string]any{"enabled": true, "smartCrusher": true},
		"openrouter": map[string]any{
			"enabled": true, "baseUrl": upstream.URL, "apiKey": "sk-or-test",
			"allowlist": []map[string]any{
				{"id": "anthropic/claude-3.5-sonnet", "alias": "claude-3-5-sonnet", "enabled": true},
			},
		},
	}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	srv, err := New(Options{APIKey: "test-key", Backend: &captureBackend{}, Builder: proxyformat.NewBuilder(), Now: time.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := toolResultBody("{\n  \"a\": 1\n}")
	body["model"] = "claude-3-5-sonnet"
	body["max_tokens"] = 1024
	if rec := postMessages(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var dispatched map[string]any
	if err := json.Unmarshal(received, &dispatched); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if got := firstToolResult(t, dispatched); got != `{"a":1}` {
		t.Errorf("OpenRouter upstream received uncompressed payload: %q", got)
	}
}

func TestHeadroom_CompressesCustomEndpointDispatch(t *testing.T) {
	var received []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[],"usage":{}}`))
	}))
	defer upstream.Close()

	tmp := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmp)
	t.Setenv("HOME", tmp)
	if _, err := config.Load(); err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if _, err := config.Save(map[string]any{
		"headroom": map[string]any{"enabled": true, "smartCrusher": true},
		"customEndpoints": map[string]any{
			"custom-model": map[string]any{
				"url": upstream.URL,
			},
		},
	}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	srv, err := New(Options{APIKey: "test-key", Backend: &captureBackend{}, Builder: proxyformat.NewBuilder(), Now: time.Now})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := toolResultBody("{\n  \"a\": 1\n}")
	body["model"] = "custom-model"
	if rec := postMessages(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var dispatched map[string]any
	if err := json.Unmarshal(received, &dispatched); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if got := firstToolResult(t, dispatched); got != `{"a":1}` {
		t.Errorf("Custom endpoint upstream received uncompressed payload: %q", got)
	}
}
