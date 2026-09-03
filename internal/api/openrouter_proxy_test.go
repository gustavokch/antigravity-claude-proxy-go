package api

import (
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
	"antigravity-go-proxy/internal/openrouter"
)

type mockCloudCodeBackend struct{}

func (m *mockCloudCodeBackend) FetchAvailableModels(ctx context.Context) (cloudcode.Response, error) {
	return cloudcode.Response{
		Body: []byte(`{"models":{"gemini-2.5-pro":{"displayName":"Gemini 2.5 Pro"}}}`),
	}, nil
}

func (m *mockCloudCodeBackend) StreamGenerateContent(ctx context.Context, req map[string]any, cb func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	return cloudcode.Response{Body: []byte(`{}`)}, nil
}

func TestOpenRouterForwarding_Unary(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var receivedAuth, receivedKey, receivedVer, receivedBeta string
	var receivedBody map[string]any

	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(openrouter.ModelsResponse{Data: []openrouter.ModelItem{
				{
					ID:   "anthropic/claude-3.7-sonnet",
					Name: "Claude 3.7 Sonnet",
					Pricing: &openrouter.Pricing{
						Prompt:     0.000003,
						Completion: 0.000015,
					},
				},
			}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/endpoints") {
			// Async provider-endpoints warmup — no providers in this fixture.
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"endpoints": []any{}}})
			return
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("expected path /v1/messages, got %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		receivedAuth = r.Header.Get("Authorization")
		receivedKey = r.Header.Get("x-api-key")
		receivedVer = r.Header.Get("anthropic-version")
		receivedBeta = r.Header.Get("anthropic-beta")

		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &receivedBody)

		_, _ = w.Write([]byte(`{"id":"msg_123","type":"message","role":"assistant","content":[{"type":"text","text":"Hello from OpenRouter!"}],"model":"anthropic/claude-3.7-sonnet"}`))
	}))
	defer mockOR.Close()

	_, err := config.Save(map[string]any{
		"openrouter": map[string]any{
			"enabled": true,
			"apiKey":  "sk-or-v1-secret-123",
			"baseUrl": mockOR.URL,
			"allowlist": []map[string]any{
				{
					"id":            "anthropic/claude-3.7-sonnet",
					"alias":         "claude-3-7-openrouter",
					"displayName":   "Claude 3.7 Sonnet (OpenRouter)",
					"contextLength": 200000,
					"enabled":       true,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("config save error: %v", err)
	}

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	// 1. Test request using alias claude-3-7-openrouter
	reqPayload := `{"model":"claude-3-7-openrouter","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "output-128k-2025-02-19")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	if receivedAuth != "Bearer sk-or-v1-secret-123" {
		t.Errorf("expected Bearer sk-or-v1-secret-123, got %s", receivedAuth)
	}
	if receivedKey != "sk-or-v1-secret-123" {
		t.Errorf("expected x-api-key sk-or-v1-secret-123, got %s", receivedKey)
	}
	if receivedVer != "2023-06-01" {
		t.Errorf("expected anthropic-version 2023-06-01, got %s", receivedVer)
	}
	if receivedBeta != "output-128k-2025-02-19" {
		t.Errorf("expected anthropic-beta output-128k-2025-02-19, got %s", receivedBeta)
	}
	if receivedBody["model"] != "anthropic/claude-3.7-sonnet" {
		t.Errorf("expected rewritten model anthropic/claude-3.7-sonnet, got %v", receivedBody["model"])
	}

	var respBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &respBody); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if respBody["id"] != "msg_123" {
		t.Errorf("expected response id msg_123, got %v", respBody["id"])
	}
}

func TestOpenRouterForwarding_SSE(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_sse\"}}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"streaming text\"}}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer mockOR.Close()

	_, err := config.Save(map[string]any{
		"openrouter": map[string]any{
			"enabled": true,
			"apiKey":  "sk-or-v1-secret-123",
			"baseUrl": mockOR.URL,
			"allowlist": []map[string]any{
				{
					"id":      "anthropic/claude-3.7-sonnet",
					"enabled": true,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("config save error: %v", err)
	}

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	reqPayload := `{"model":"anthropic/claude-3.7-sonnet","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	bodyStr := rec.Body.String()
	if !strings.Contains(bodyStr, "message_start") || !strings.Contains(bodyStr, "streaming text") {
		t.Errorf("expected SSE chunks in response, got %s", bodyStr)
	}
}

func TestMergedModelsEndpoint(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	_, err := config.Save(map[string]any{
		"openrouter": map[string]any{
			"enabled": true,
			"apiKey":  "sk-or-v1-secret-123",
			"allowlist": []map[string]any{
				{
					"id":              "anthropic/claude-3.7-sonnet",
					"alias":           "claude-3-7-openrouter",
					"displayName":     "Claude 3.7 Sonnet (OpenRouter)",
					"contextLength":   200000,
					"maxOutputTokens": 128000,
					"enabled":         true,
				},
				{
					"id":      "disabled/model",
					"enabled": false,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("config save error: %v", err)
	}

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("x-api-key", "test-proxy-key")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode models response: %v", err)
	}

	foundCloudCode := false
	foundORModel := false
	foundORAlias := false
	foundDisabled := false

	for _, m := range resp.Data {
		id, _ := m["id"].(string)
		if id == "gemini-2.5-pro" {
			foundCloudCode = true
		}
		if id == "anthropic/claude-3.7-sonnet" {
			foundORModel = true
			if m["owned_by"] != "openrouter" {
				t.Errorf("expected owned_by openrouter, got %v", m["owned_by"])
			}
			if m["max_output_tokens"] != float64(128000) {
				t.Errorf("expected max_output_tokens 128000, got %v", m["max_output_tokens"])
			}
		}
		if id == "claude-3-7-openrouter" {
			foundORAlias = true
		}
		if id == "disabled/model" {
			foundDisabled = true
		}
	}

	if !foundCloudCode {
		t.Errorf("expected Cloud Code model gemini-2.5-pro in models list")
	}
	if !foundORModel {
		t.Errorf("expected OpenRouter model anthropic/claude-3.7-sonnet in models list")
	}
	if !foundORAlias {
		t.Errorf("expected OpenRouter alias claude-3-7-openrouter in models list")
	}
	if foundDisabled {
		t.Errorf("disabled model should not be in models list")
	}
}

// startOpenRouterMock starts a mock OpenRouter upstream that serves the
// models catalog, an empty endpoints list, and captures max_tokens from any
// chat request body into *received.
func startOpenRouterMock(t *testing.T, received *float64) *httptest.Server {
	t.Helper()
	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(openrouter.ModelsResponse{Data: []openrouter.ModelItem{
				{ID: "muse-spark-1.3-contributor", Name: "Muse Spark 1.3",
					Pricing: &openrouter.Pricing{Prompt: 0.000001, Completion: 0.000002}},
			}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/endpoints") {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"endpoints": []any{}}})
			return
		}
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		if v, ok := parsed["max_tokens"].(float64); ok {
			*received = v
		}
		_, _ = w.Write([]byte(`{"id":"msg_x","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"muse-spark-1.3-contributor"}`))
	}))
	t.Cleanup(mockOR.Close)
	return mockOR
}

// TestOpenRouterForwarding_RaisesMaxTokensToMinimumFloor covers the original
// muse-spark failure mode: providers reject tiny max_tokens values, so a value
// we do send is raised to the 16 minimum. The allowlist override is set high
// (64000) so only the minimum floor explains the result.
func TestOpenRouterForwarding_RaisesMaxTokensToMinimumFloor(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var receivedMaxTokens float64
	mockOR := startOpenRouterMock(t, &receivedMaxTokens)

	_, err := config.Save(map[string]any{
		"openrouter": map[string]any{
			"enabled": true,
			"apiKey":  "sk-or-v1-secret-123",
			"baseUrl": mockOR.URL,
			"allowlist": []map[string]any{
				{
					"id":              "muse-spark-1.3-contributor",
					"displayName":     "Muse Spark 1.3 (OpenRouter)",
					"contextLength":   200000,
					"maxOutputTokens": 64000,
					"enabled":         true,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("config save error: %v", err)
	}

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	reqPayload := `{"model":"muse-spark-1.3-contributor","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	req.Header.Set("anthropic-version", "2023-06-01")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	if receivedMaxTokens != 16 {
		t.Errorf("expected upstream max_tokens raised to minimum floor 16, got %v", receivedMaxTokens)
	}
}

// TestOpenRouterForwarding_PassesThroughMaxTokensBelowLimit: a request value
// under the known limit is sent unchanged — never raised.
func TestOpenRouterForwarding_PassesThroughMaxTokensBelowLimit(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var receivedMaxTokens float64
	mockOR := startOpenRouterMock(t, &receivedMaxTokens)

	_, err := config.Save(map[string]any{
		"openrouter": map[string]any{
			"enabled": true,
			"apiKey":  "sk-or-v1-secret-123",
			"baseUrl": mockOR.URL,
			"allowlist": []map[string]any{
				{
					"id":              "muse-spark-1.3-contributor",
					"displayName":     "Muse Spark 1.3 (OpenRouter)",
					"contextLength":   200000,
					"maxOutputTokens": 64000,
					"enabled":         true,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("config save error: %v", err)
	}

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	reqPayload := `{"model":"muse-spark-1.3-contributor","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	req.Header.Set("anthropic-version", "2023-06-01")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	if receivedMaxTokens != 4096 {
		t.Errorf("expected upstream max_tokens passthrough 4096, got %v", receivedMaxTokens)
	}
}

// TestOpenRouterForwarding_ClampsMaxTokensDownToManualOverride: a request
// value above the webUI manual override is clamped down to the override.
func TestOpenRouterForwarding_ClampsMaxTokensDownToManualOverride(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var receivedMaxTokens float64
	mockOR := startOpenRouterMock(t, &receivedMaxTokens)

	_, err := config.Save(map[string]any{
		"openrouter": map[string]any{
			"enabled": true,
			"apiKey":  "sk-or-v1-secret-123",
			"baseUrl": mockOR.URL,
			"allowlist": []map[string]any{
				{
					"id":              "muse-spark-1.3-contributor",
					"displayName":     "Muse Spark 1.3 (OpenRouter)",
					"contextLength":   200000,
					"maxOutputTokens": 1024,
					"enabled":         true,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("config save error: %v", err)
	}

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	reqPayload := `{"model":"muse-spark-1.3-contributor","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	req.Header.Set("anthropic-version", "2023-06-01")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	if receivedMaxTokens != 1024 {
		t.Errorf("expected upstream max_tokens clamped down to override 1024, got %v", receivedMaxTokens)
	}
}

// TestOpenRouterForwarding_OverrideBelowFloorWins: an admin-set cap below
// the provider floor (override 15 < floor 16) still wins — the client value
// clamps down to the override and the floor never raises past a known limit.
func TestOpenRouterForwarding_OverrideBelowFloorWins(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var receivedMaxTokens float64
	mockOR := startOpenRouterMock(t, &receivedMaxTokens)

	_, err := config.Save(map[string]any{
		"openrouter": map[string]any{
			"enabled": true,
			"apiKey":  "sk-or-v1-secret-123",
			"baseUrl": mockOR.URL,
			"allowlist": []map[string]any{
				{
					"id":              "muse-spark-1.3-contributor",
					"displayName":     "Muse Spark 1.3 (OpenRouter)",
					"contextLength":   200000,
					"maxOutputTokens": 15,
					"enabled":         true,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("config save error: %v", err)
	}

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	reqPayload := `{"model":"muse-spark-1.3-contributor","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	req.Header.Set("anthropic-version", "2023-06-01")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	if receivedMaxTokens != 15 {
		t.Errorf("expected upstream max_tokens clamped to override 15 (floor must not exceed a known limit), got %v", receivedMaxTokens)
	}
}

// TestOpenRouterForwarding_MaxOutputTokensFromWebUISaveRoundTrip locks the
// contract behind the WebUI limits panel: a POST /api/openrouter/config save
// carrying maxOutputTokens must be honored by the running forwarder on the
// next request, without a restart. The WebUI writes the whole allowlist
// through this endpoint, so this is the path the max_tokens policy depends on.
func TestOpenRouterForwarding_MaxOutputTokensFromWebUISaveRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var receivedMaxTokens float64
	mockOR := startOpenRouterMock(t, &receivedMaxTokens)

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	// Save via the management endpoint the WebUI settings page uses — same
	// shape the limits panel's debounced save sends (allowlist replace).
	payload := fmt.Sprintf(`{
		"enabled": true,
		"apiKey": "sk-or-v1-secret-123",
		"baseUrl": %q,
		"allowlist": [
			{
				"id": "muse-spark-1.3-contributor",
				"displayName": "Muse Spark 1.3 (OpenRouter)",
				"contextLength": 200000,
				"maxOutputTokens": 16,
				"enabled": true
			}
		]
	}`, mockOR.URL)

	saveReq := httptest.NewRequest(http.MethodPost, "/api/openrouter/config", strings.NewReader(payload))
	saveReq.Header.Set("Content-Type", "application/json")
	saveRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(saveRec, saveReq)
	if saveRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on config save, got %d: %s", saveRec.Code, saveRec.Body.String())
	}

	reqPayload := `{"model":"muse-spark-1.3-contributor","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	req.Header.Set("anthropic-version", "2023-06-01")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}
	if receivedMaxTokens != 16 {
		t.Errorf("expected upstream max_tokens clamped to 16 after WebUI save, got %v", receivedMaxTokens)
	}
}

// TestOpenRouterForwarding_RejectsWhenNothingKnown: no max_tokens in the
// client request, no webUI override (0), and the mock catalog advertises no
// max completion tokens — the upstream /v1/messages schema requires the
// field, so the proxy must reject early with a clear 400 instead of
// forwarding a request that is guaranteed to fail.
func TestOpenRouterForwarding_RejectsWhenNothingKnown(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	// Isolate from the shared process-wide catalog cache: other tests warm it
	// with fixtures that do advertise max completion tokens.
	openrouter.DefaultClient.SaveCache(nil)
	t.Cleanup(func() { openrouter.DefaultClient.SaveCache(nil) })

	upstreamCalled := false
	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(openrouter.ModelsResponse{Data: []openrouter.ModelItem{
				{ID: "muse-spark-1.3-contributor", Name: "Muse Spark 1.3",
					Pricing: &openrouter.Pricing{Prompt: 0.000001, Completion: 0.000002}},
			}})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/endpoints") {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"endpoints": []any{}}})
			return
		}
		upstreamCalled = true
		_, _ = w.Write([]byte(`{"id":"msg_x","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"muse-spark-1.3-contributor"}`))
	}))
	t.Cleanup(mockOR.Close)

	_, err := config.Save(map[string]any{
		"openrouter": map[string]any{
			"enabled": true,
			"apiKey":  "sk-or-v1-secret-123",
			"baseUrl": mockOR.URL,
			"allowlist": []map[string]any{
				{
					"id":            "muse-spark-1.3-contributor",
					"displayName":   "Muse Spark 1.3 (OpenRouter)",
					"contextLength": 200000,
					"enabled":       true,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("config save error: %v", err)
	}

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	reqPayload := `{"model":"muse-spark-1.3-contributor","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	req.Header.Set("anthropic-version", "2023-06-01")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "max_tokens") {
		t.Errorf("expected error body to mention max_tokens, got %s", rec.Body.String())
	}
	if upstreamCalled {
		t.Error("expected upstream never to be called when max_tokens cannot be determined")
	}
}

func TestModelMappingToOpenRouterAndForwarded(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var receivedORBody map[string]any
	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_ = json.NewEncoder(w).Encode(openrouter.ModelsResponse{Data: []openrouter.ModelItem{
				{
					ID:   "anthropic/claude-3.7-sonnet",
					Name: "Claude 3.7 Sonnet",
					Pricing: &openrouter.Pricing{
						Prompt:     0.000003,
						Completion: 0.000015,
					},
				},
			}})
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &receivedORBody)
		_, _ = w.Write([]byte(`{"id":"msg_or","type":"message","role":"assistant","content":[{"type":"text","text":"From OR"}],"model":"anthropic/claude-3.7-sonnet"}`))
	}))
	defer mockOR.Close()

	var receivedCustomBody map[string]any
	mockCustom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &receivedCustomBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_custom","type":"message","role":"assistant","content":[{"type":"text","text":"From Custom"}],"model":"custom-forwarded-model"}`))
	}))
	defer mockCustom.Close()

	_, err := config.Save(map[string]any{
		"openrouter": map[string]any{
			"enabled": true,
			"apiKey":  "sk-or-test-key",
			"baseUrl": mockOR.URL,
			"allowlist": []map[string]any{
				{
					"id":          "anthropic/claude-3.7-sonnet",
					"alias":       "or-sonnet",
					"displayName": "Claude 3.7 Sonnet (OpenRouter)",
					"enabled":     true,
				},
			},
		},
		"customEndpoints": map[string]any{
			"custom-forwarded-model": map[string]any{
				"url":    mockCustom.URL,
				"apiKey": "custom-api-key",
			},
		},
		"modelMapping": map[string]any{
			// Map alias directly to OpenRouter Model ID
			"alias-to-or-id": map[string]any{
				"mapping": "anthropic/claude-3.7-sonnet",
			},
			// Map alias directly to OpenRouter Local Alias
			"alias-to-or-alias": map[string]any{
				"mapping": "or-sonnet",
			},
			// Map alias to Custom Forwarding Endpoint
			"alias-to-custom": map[string]any{
				"mapping": "custom-forwarded-model",
			},
			// Multi-hop mapping: hop1 -> hop2 -> or-sonnet
			"hop-1": map[string]any{
				"mapping": "hop-2",
			},
			"hop-2": map[string]any{
				"mapping": "or-sonnet",
			},
		},
	})
	if err != nil {
		t.Fatalf("config save error: %v", err)
	}

	server, err := New(Options{
		APIKey:  "test-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	// 1. Test mapping to OpenRouter ID
	reqPayload := `{"model":"alias-to-or-id","max_tokens":1024,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-key")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("alias-to-or-id expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if receivedORBody["model"] != "anthropic/claude-3.7-sonnet" {
		t.Errorf("expected OpenRouter target model anthropic/claude-3.7-sonnet, got %v", receivedORBody["model"])
	}

	// 2. Test mapping to OpenRouter Alias
	reqPayload = `{"model":"alias-to-or-alias","max_tokens":1024,"messages":[{"role":"user","content":"Hi"}]}`
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-key")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("alias-to-or-alias expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if receivedORBody["model"] != "anthropic/claude-3.7-sonnet" {
		t.Errorf("expected OpenRouter target model anthropic/claude-3.7-sonnet, got %v", receivedORBody["model"])
	}

	// 3. Test mapping to Custom Forwarding Endpoint
	reqPayload = `{"model":"alias-to-custom","max_tokens":1024,"messages":[{"role":"user","content":"Hi"}]}`
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-key")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("alias-to-custom expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if receivedCustomBody["model"] != "custom-forwarded-model" {
		t.Errorf("expected Custom Endpoint target model custom-forwarded-model, got %v", receivedCustomBody["model"])
	}

	// 4. Test multi-hop chained mapping
	reqPayload = `{"model":"hop-1","max_tokens":1024,"messages":[{"role":"user","content":"Hi"}]}`
	req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-key")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("hop-1 expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if receivedORBody["model"] != "anthropic/claude-3.7-sonnet" {
		t.Errorf("expected chained mapping to resolve to anthropic/claude-3.7-sonnet, got %v", receivedORBody["model"])
	}
}
