package api

import (
	"bytes"
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
)

type kimiTestBackend struct{}

func (m *kimiTestBackend) FetchAvailableModels(ctx context.Context) (cloudcode.Response, error) {
	return cloudcode.Response{
		Body: []byte(`{
			"defaultAgentModelId":"kimi-k2-thinking",
			"agentModelSorts":[{"displayName":"Recommended","groups":[{"modelIds":["kimi-k2-thinking"]}]}],
			"models":{"kimi-k2-thinking":{"displayName":"Kimi K2"}}
		}`),
	}, nil
}
func (m *kimiTestBackend) StreamGenerateContent(ctx context.Context, req map[string]any, cb func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	return cloudcode.Response{Body: []byte(`{}`)}, nil
}

func newKimiTestServer(t *testing.T) *Server {
	t.Helper()
	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &kimiTestBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server
}

func TestServer_ForwardToKimi_AllowsAliasMatch(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var (
		gotPath string
		gotAuth string
		gotBody []byte
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
	}))
	defer upstream.Close()

	_, err := config.Save(map[string]any{
		"kimi": map[string]any{
			"enabled": true,
			"apiKey":  "sk-kimi-test",
			"baseUrl": upstream.URL,
			"allowlist": []map[string]any{
				{"id": "kimi-k2-thinking", "alias": "k2", "enabled": true},
			},
		},
	})
	if err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	server := newKimiTestServer(t)

	body := `{"model":"k2","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if gotPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", gotPath)
	}
	if gotAuth != "Bearer sk-kimi-test" {
		t.Errorf("Authorization = %q, want Bearer sk-kimi-test", gotAuth)
	}
	if !strings.Contains(string(gotBody), `"model":"kimi-k2-thinking"`) {
		t.Errorf("upstream body should rewrite alias to kimi id, got %s", gotBody)
	}
	if rec.Code != 200 {
		t.Errorf("client status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

func TestServer_KimiEndToEnd_StreamingResponse(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	kimid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer kimi-key" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer kimid.Close()

	_, err := config.Save(map[string]any{
		"kimi": map[string]any{
			"enabled": true,
			"apiKey":  "kimi-key",
			"baseUrl": kimid.URL,
			"allowlist": []map[string]any{
				{"id": "kimi-k2-thinking", "enabled": true},
			},
		},
	})
	if err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	server := newKimiTestServer(t)
	body := []byte(`{"model":"kimi-k2-thinking","stream":true,"messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "message_start") {
		t.Fatalf("missing message_start in stream: %s", rec.Body.String())
	}
}

func TestServer_ModelsList_IncludesKimi(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	_, err := config.Save(map[string]any{
		"kimi": map[string]any{
			"enabled": true,
			"apiKey":  "sk-kimi-test",
			"baseUrl": "https://api.kimi.com/coding",
			"allowlist": []map[string]any{
				{"id": "kimi-k2-thinking", "alias": "k2", "displayName": "Kimi K2", "contextLength": 200000, "maxOutputTokens": 8000, "enabled": true},
			},
		},
	})
	if err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	server := newKimiTestServer(t)

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
	var found, aliasFound, kimiOwned bool
	for _, m := range body.Data {
		switch m["id"] {
		case "kimi-k2-thinking":
			found = true
			if m["owned_by"] == "kimi" {
				kimiOwned = true
			}
		case "k2":
			aliasFound = true
		}
	}
	if !found {
		t.Fatalf("kimi-k2-thinking not in /v1/models response: %+v", body.Data)
	}
	if !kimiOwned {
		t.Errorf("expected at least one kimi-k2-thinking entry with owned_by=kimi, got: %+v", body.Data)
	}
	if !aliasFound {
		t.Errorf("alias k2 not in /v1/models response: %+v", body.Data)
	}
}

func TestMatchKimiModel_EdgeCases(t *testing.T) {
	cfg := config.KimiConfig{
		Enabled: true,
		Allowlist: []config.KimiModelConfig{
			{ID: "kimi-k2-thinking", Alias: "k2", Enabled: true},
			{ID: "", Alias: "", Enabled: true},
			{ID: "disabled-model", Alias: "dis", Enabled: false},
		},
	}

	if got := matchKimiModel(cfg, ""); got != "" {
		t.Errorf("matchKimiModel(empty) = %q, want empty", got)
	}
	if got := matchKimiModel(cfg, "dis"); got != "" {
		t.Errorf("matchKimiModel(disabled) = %q, want empty", got)
	}
	if got := matchKimiModel(cfg, "k2"); got != "kimi-k2-thinking" {
		t.Errorf("matchKimiModel(k2) = %q, want kimi-k2-thinking", got)
	}
	if got := matchKimiModel(cfg, "kimi-k2-thinking"); got != "kimi-k2-thinking" {
		t.Errorf("matchKimiModel(id) = %q, want kimi-k2-thinking", got)
	}
	if got := matchKimiModel(cfg, "unknown"); got != "" {
		t.Errorf("matchKimiModel(unknown) = %q, want empty", got)
	}
}
