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
	"sync/atomic"
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

func TestServer_ForwardToKimi_CCRHydration_Streaming(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var chunkID string
	var callCount int32
	mockKimi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		curr := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		if curr == 1 {
			// Turn 1: headroom_retrieve
			fmt.Fprintf(w, "event: message_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_k1\",\"role\":\"assistant\",\"model\":\"kimi-k2-thinking\",\"usage\":{\"input_tokens\":40,\"output_tokens\":8}}}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_k1\",\"name\":\"headroom_retrieve\",\"input\":{}}}\n\n")

			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"chunk_id\\\":\\\"%s\\\"}\"}}\n\n", chunkID)

			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

			fmt.Fprintf(w, "event: message_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":10}}\n\n")

			fmt.Fprintf(w, "event: message_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
		} else {
			// Turn 2: Hydrated text answer
			fmt.Fprintf(w, "event: message_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_k2\",\"role\":\"assistant\",\"model\":\"kimi-k2-thinking\",\"usage\":{\"input_tokens\":100,\"output_tokens\":15}}}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")

			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Kimi answered with hydrated knowledge.\"}}\n\n")

			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

			fmt.Fprintf(w, "event: message_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":15}}\n\n")

			fmt.Fprintf(w, "event: message_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
		}
	}))
	defer mockKimi.Close()

	_, err := config.Save(map[string]any{
		"kimi": map[string]any{
			"enabled": true,
			"apiKey":  "kimi-secret-key",
			"baseUrl": mockKimi.URL,
			"allowlist": []map[string]any{
				{"id": "kimi-k2-thinking", "alias": "k2", "enabled": true},
			},
		},
		"headroom": map[string]any{
			"enabled": true,
			"ccr": map[string]any{
				"enabled": true,
			},
		},
	})
	if err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	server := newKimiTestServer(t)
	var ok bool
	chunkID, ok = server.ccrStore.Put("Secret Kimi context payload")
	if !ok {
		t.Fatalf("Failed to put chunk into CCRStore")
	}

	reqBody := `{"model":"kimi-k2-thinking","stream":true,"messages":[{"role":"user","content":"Fetch chunk"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("x-api-key", "test-proxy-key")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if atomic.LoadInt32(&callCount) != 2 {
		t.Fatalf("Expected 2 calls to Kimi backend, got %d", callCount)
	}

	respBody := w.Body.String()
	if strings.Contains(respBody, "headroom_retrieve") {
		t.Fatalf("Downstream client leaked headroom_retrieve tool_use: %s", respBody)
	}
	if !strings.Contains(respBody, "Kimi answered with hydrated knowledge.") {
		t.Fatalf("Downstream client missing hydrated text: %s", respBody)
	}
	if !strings.Contains(respBody, "\"output_tokens\":25") { // 10 + 15 = 25
		t.Fatalf("Expected patched output_tokens 25, got: %s", respBody)
	}
}

func TestServer_ForwardToKimi_CCRHydration_Unary(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var chunkID string
	var callCount int32
	mockKimi := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		curr := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if curr == 1 {
			resp := map[string]any{
				"id":    "msg_ku_1",
				"type":  "message",
				"role":  "assistant",
				"model": "kimi-k2-thinking",
				"content": []any{
					map[string]any{
						"type":  "tool_use",
						"id":    "toolu_ku1",
						"name":  "headroom_retrieve",
						"input": map[string]any{"chunk_id": chunkID},
					},
				},
				"stop_reason": "tool_use",
				"usage": map[string]any{
					"input_tokens":  60,
					"output_tokens": 10,
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		} else {
			resp := map[string]any{
				"id":    "msg_ku_2",
				"type":  "message",
				"role":  "assistant",
				"model": "kimi-k2-thinking",
				"content": []any{
					map[string]any{
						"type": "text",
						"text": "Unary Kimi hydrated response",
					},
				},
				"stop_reason": "end_turn",
				"usage": map[string]any{
					"input_tokens":  120,
					"output_tokens": 14,
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		}
	}))
	defer mockKimi.Close()

	_, err := config.Save(map[string]any{
		"kimi": map[string]any{
			"enabled": true,
			"apiKey":  "kimi-secret-key",
			"baseUrl": mockKimi.URL,
			"allowlist": []map[string]any{
				{"id": "kimi-k2-thinking", "alias": "k2", "enabled": true},
			},
		},
		"headroom": map[string]any{
			"enabled": true,
			"ccr": map[string]any{
				"enabled": true,
			},
		},
	})
	if err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	server := newKimiTestServer(t)
	var ok bool
	chunkID, ok = server.ccrStore.Put("Secret Kimi Unary payload")
	if !ok {
		t.Fatalf("Failed to put chunk into CCRStore")
	}

	reqBody := `{"model":"kimi-k2-thinking","stream":false,"messages":[{"role":"user","content":"Fetch chunk"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("x-api-key", "test-proxy-key")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if atomic.LoadInt32(&callCount) != 2 {
		t.Fatalf("Expected 2 calls to Kimi backend, got %d", callCount)
	}

	var respMap map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &respMap); err != nil {
		t.Fatalf("Failed to unmarshal unary response: %v", err)
	}

	content, _ := respMap["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("Expected 1 content block, got: %v", content)
	}
	firstBlock, _ := content[0].(map[string]any)
	if firstBlock["text"] != "Unary Kimi hydrated response" {
		t.Fatalf("Unexpected content text: %v", firstBlock["text"])
	}

	usage, _ := respMap["usage"].(map[string]any)
	if usage["output_tokens"].(float64) != 24 { // 10 + 14 = 24
		t.Fatalf("Expected output_tokens 24, got %v", usage["output_tokens"])
	}
}
