package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

func TestServer_ForwardToKimi_ClampsMaxTokensToAllowlistFloor(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				{"id": "kimi-k2-thinking", "maxOutputTokens": 16, "enabled": true},
			},
		},
	})
	if err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	server := newKimiTestServer(t)

	body := `{"model":"kimi-k2-thinking","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	var upstreamReq map[string]any
	if err := json.Unmarshal(gotBody, &upstreamReq); err != nil {
		t.Fatalf("unmarshal upstream body %s: %v", gotBody, err)
	}
	if got, want := upstreamReq["max_tokens"], float64(16); got != want {
		t.Errorf("upstream max_tokens = %v, want %v (allowlist floor)", got, want)
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
			"baseUrl": "https://api.moonshot.ai/anthropic",
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
	if got := matchKimiModel(cfg, "kimi-k2-thinking[1m]"); got != "kimi-k2-thinking" {
		t.Errorf("matchKimiModel(id[1m]) = %q, want kimi-k2-thinking", got)
	}
	if got := matchKimiModel(cfg, "k2[1m]"); got != "kimi-k2-thinking" {
		t.Errorf("matchKimiModel(k2[1m]) = %q, want kimi-k2-thinking", got)
	}
	if got := matchKimiModel(cfg, "k2[1M]"); got != "kimi-k2-thinking" {
		t.Errorf("matchKimiModel(k2[1M]) = %q, want kimi-k2-thinking", got)
	}
	if got := matchKimiModel(cfg, "K2"); got != "kimi-k2-thinking" {
		t.Errorf("matchKimiModel(K2) = %q, want kimi-k2-thinking", got)
	}
	if got := matchKimiModel(cfg, "KIMI-K2-THINKING"); got != "kimi-k2-thinking" {
		t.Errorf("matchKimiModel(KIMI-K2-THINKING) = %q, want kimi-k2-thinking", got)
	}
	if got := matchKimiModel(cfg, "kimi-k2-thinking[1M]"); got != "kimi-k2-thinking" {
		t.Errorf("matchKimiModel(id[1M]) = %q, want kimi-k2-thinking", got)
	}
	if got := matchKimiModel(cfg, "[1m]"); got != "" {
		t.Errorf("matchKimiModel([1m]) = %q, want empty", got)
	}
	if got := matchKimiModel(cfg, "   [1m]   "); got != "" {
		t.Errorf("matchKimiModel(padded [1m]) = %q, want empty", got)
	}
	if got := matchKimiModel(cfg, "unknown"); got != "" {
		t.Errorf("matchKimiModel(unknown) = %q, want empty", got)
	}

	// Allowlist item configured with [1m] in ID/Alias
	cfgWith1m := config.KimiConfig{
		Enabled: true,
		Allowlist: []config.KimiModelConfig{
			{ID: "kimi-k2-thinking[1m]", Alias: "k2[1m]", Enabled: true},
		},
	}
	if got := matchKimiModel(cfgWith1m, "kimi-k2-thinking"); got != "kimi-k2-thinking" {
		t.Errorf("matchKimiModel(id against [1m] item) = %q, want kimi-k2-thinking", got)
	}
	if got := matchKimiModel(cfgWith1m, "k2"); got != "kimi-k2-thinking" {
		t.Errorf("matchKimiModel(alias against [1m] item) = %q, want kimi-k2-thinking", got)
	}
	if got := matchKimiModel(cfgWith1m, "[1m]"); got != "" {
		t.Errorf("matchKimiModel([1m] against [1m] item) = %q, want empty", got)
	}
}

func TestServer_ForwardToKimi_Strips1mInPayload(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var gotBody []byte
	kimid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
	}))
	defer kimid.Close()

	_, err := config.Save(map[string]any{
		"kimi": map[string]any{
			"enabled": true,
			"apiKey":  "sk-kimi-test",
			"baseUrl": kimid.URL,
			"allowlist": []map[string]any{
				{"id": "kimi-k2-thinking[1m]", "alias": "k2", "enabled": true},
			},
		},
	})
	if err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	server := newKimiTestServer(t)

	// Send request with [1m] suffix in requested model
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"kimi-k2-thinking[1m]","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var forwarded map[string]any
	if err := json.Unmarshal(gotBody, &forwarded); err != nil {
		t.Fatalf("unmarshal forwarded body: %v", err)
	}
	if forwarded["model"] != "kimi-k2-thinking" {
		t.Errorf("forwarded model = %v, want kimi-k2-thinking (stripped)", forwarded["model"])
	}
}

func TestServer_ForwardToKimi_OmitsMaxTokensWhenNothingKnown(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				{"id": "kimi-k2-thinking", "enabled": true},
			},
		},
	})
	if err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	server := newKimiTestServer(t)

	body := `{"model":"kimi-k2-thinking","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	var upstreamReq map[string]any
	if err := json.Unmarshal(gotBody, &upstreamReq); err != nil {
		t.Fatalf("unmarshal upstream body %s: %v", gotBody, err)
	}
	if _, present := upstreamReq["max_tokens"]; present {
		t.Errorf("expected upstream body to omit max_tokens, got %v", upstreamReq["max_tokens"])
	}
	if rec.Code != 200 {
		t.Errorf("client status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

// Kimi Code maps the Claude Code effort levels itself (medium->high, xhigh->max,
// docs: kimi.com/code/docs/en/kimi-code/models.html "Effort mapping in
// third-party tools"), and rejects unknown spellings with a 400. The gateway is
// a transparent forwarder (ADR-0001), so every reasoning field must reach
// Kimi exactly as the client sent it: normalizing here would hide the
// vendor's own mapping and change which level the vendor actually runs.
func TestServer_ForwardToKimi_PassesReasoningFieldsThrough(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
	}))
	defer upstream.Close()

	if _, err := config.Save(map[string]any{
		"kimi": map[string]any{
			"enabled": true, "apiKey": "sk-kimi-test", "baseUrl": upstream.URL,
			"allowlist": []map[string]any{{"id": "k3", "enabled": true}},
		},
	}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	server := newKimiTestServer(t)
	body := `{"model":"k3[1m]","max_tokens":4096,` +
		`"thinking":{"type":"enabled","budget_tokens":2048,"display":"summarized"},` +
		`"output_config":{"effort":"xhigh"},"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	var sent map[string]any
	if err := json.Unmarshal(gotBody, &sent); err != nil {
		t.Fatalf("unmarshal upstream body %s: %v", gotBody, err)
	}
	if sent["model"] != "k3" {
		t.Errorf("model = %v, want k3 (the [1m] suffix is a Claude Code convention, not a Kimi id)", sent["model"])
	}
	if got := sent["output_config"]; fmt.Sprint(got) != "map[effort:xhigh]" {
		t.Errorf("output_config = %v, want it forwarded untouched", got)
	}
	if got := sent["thinking"]; fmt.Sprint(got) != "map[budget_tokens:2048 display:summarized type:enabled]" {
		t.Errorf("thinking = %v, want it forwarded untouched", got)
	}
	if sent["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens = %v, want 4096", sent["max_tokens"])
	}
}
