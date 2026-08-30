package api

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
	"antigravity-go-proxy/internal/openrouter"
	"antigravity-go-proxy/internal/stats"
)

func TestOpenRouterObservability_Unary(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":    "msg_unary_test",
			"type":  "message",
			"role":  "assistant",
			"model": "anthropic/claude-3.7-sonnet",
			"content": []map[string]any{
				{"type": "text", "text": "Hello world from OpenRouter with observability!"},
			},
			"usage": map[string]any{
				"input_tokens":                1200,
				"output_tokens":               300,
				"cache_read_input_tokens":     800,
				"cache_creation_input_tokens": 100,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockOR.Close()

	// Seed OpenRouter pricing in cache
	openrouter.DefaultClient.SaveCache([]openrouter.ModelItem{
		{
			ID:   "anthropic/claude-3.7-sonnet",
			Name: "Claude 3.7 Sonnet",
			Pricing: &openrouter.Pricing{
				Prompt:          0.000003,  // $3/M
				Completion:      0.000015,  // $15/M
				InputCacheRead:  0.0000003, // $0.30/M
				InputCacheWrite: 0.00000375,
			},
		},
	})

	_, err := config.Save(map[string]any{
		"openrouter": map[string]any{
			"enabled": true,
			"apiKey":  "sk-or-test-key",
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

	var logBuf bytes.Buffer
	testLogger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	tracker, _ := stats.NewTracker("")
	openrouter.DefaultSessionTracker.Reset()

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
		Logger:  testLogger,
		Tracker: tracker,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	reqPayload := `{"model":"anthropic/claude-3.7-sonnet","messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	req.Header.Set("x-session-id", "session-unary-1")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "[OpenRouter]") {
		t.Errorf("expected [OpenRouter] log tag, got: %s", logs)
	}
	if !strings.Contains(logs, "gateway=openrouter") {
		t.Errorf("expected gateway=openrouter attribute, got: %s", logs)
	}
	if !strings.Contains(logs, "session-unary-1") {
		t.Errorf("expected session-unary-1 in logs, got: %s", logs)
	}
	if !strings.Contains(logs, "input_tokens=1200") {
		t.Errorf("expected input_tokens=1200 in logs, got: %s", logs)
	}
	if !strings.Contains(logs, "output_tokens=300") {
		t.Errorf("expected output_tokens=300 in logs, got: %s", logs)
	}
	if !strings.Contains(logs, "cache_read_tokens=800") {
		t.Errorf("expected cache_read_tokens=800 in logs, got: %s", logs)
	}
	if !strings.Contains(logs, "level_tag=SUCCESS") {
		t.Errorf("expected level_tag=SUCCESS in logs, got: %s", logs)
	}

	// Verify session cost was recorded in DefaultSessionTracker
	sessionStats, ok := openrouter.DefaultSessionTracker.Get("session-unary-1")
	if !ok {
		t.Fatalf("expected session-unary-1 to exist in session tracker")
	}
	if sessionStats.RequestCount != 1 {
		t.Errorf("expected request count 1, got %d", sessionStats.RequestCount)
	}
	if sessionStats.InputTokens != 1200 {
		t.Errorf("expected 1200 input tokens in session, got %d", sessionStats.InputTokens)
	}
	if sessionStats.OutputTokens != 300 {
		t.Errorf("expected 300 output tokens in session, got %d", sessionStats.OutputTokens)
	}
	if sessionStats.TotalCost <= 0 {
		t.Errorf("expected positive session cost, got %f", sessionStats.TotalCost)
	}

	// Verify stats tracker recorded the request
	history := tracker.GetHistory()
	if len(history) == 0 {
		t.Errorf("expected history in stats tracker")
	}
}

func TestOpenRouterObservability_StreamingSSE(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		// 1. message_start
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_sse_obs\",\"model\":\"anthropic/claude-3.7-sonnet\",\"usage\":{\"input_tokens\":1000,\"cache_read_input_tokens\":600,\"cache_creation_input_tokens\":50}}}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}

		// 2. content_block_delta
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"streamed response\"}}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}

		// 3. message_delta
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":200}}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer mockOR.Close()

	openrouter.DefaultClient.SaveCache([]openrouter.ModelItem{
		{
			ID:   "anthropic/claude-3.7-sonnet",
			Name: "Claude 3.7 Sonnet",
			Pricing: &openrouter.Pricing{
				Prompt:         0.000003,
				Completion:     0.000015,
				InputCacheRead: 0.0000003,
			},
		},
	})

	_, err := config.Save(map[string]any{
		"openrouter": map[string]any{
			"enabled": true,
			"apiKey":  "sk-or-test-key",
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

	var logBuf bytes.Buffer
	testLogger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	tracker, _ := stats.NewTracker("")
	openrouter.DefaultSessionTracker.Reset()

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
		Logger:  testLogger,
		Tracker: tracker,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	reqPayload := `{"model":"anthropic/claude-3.7-sonnet","stream":true,"messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	req.Header.Set("x-session-id", "session-stream-1")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	bodyStr := rec.Body.String()
	if !strings.Contains(bodyStr, "message_start") || !strings.Contains(bodyStr, "streamed response") {
		t.Errorf("expected SSE body chunks, got: %s", bodyStr)
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "[OpenRouter]") {
		t.Errorf("expected [OpenRouter] log tag, got: %s", logs)
	}
	if !strings.Contains(logs, "session-stream-1") {
		t.Errorf("expected session-stream-1 in logs, got: %s", logs)
	}
	if !strings.Contains(logs, "input_tokens=1000") {
		t.Errorf("expected input_tokens=1000 in logs, got: %s", logs)
	}
	if !strings.Contains(logs, "output_tokens=200") {
		t.Errorf("expected output_tokens=200 in logs, got: %s", logs)
	}
	if !strings.Contains(logs, "cache_read_tokens=600") {
		t.Errorf("expected cache_read_tokens=600 in logs, got: %s", logs)
	}
	if !strings.Contains(logs, "level_tag=SUCCESS") {
		t.Errorf("expected level_tag=SUCCESS in logs, got: %s", logs)
	}

	// Verify session stats
	sessionStats, ok := openrouter.DefaultSessionTracker.Get("session-stream-1")
	if !ok {
		t.Fatalf("expected session-stream-1 in session tracker")
	}
	if sessionStats.InputTokens != 1000 || sessionStats.OutputTokens != 200 || sessionStats.CacheReadTokens != 600 {
		t.Errorf("unexpected session stats: %+v", sessionStats)
	}
	if sessionStats.TotalCost <= 0 {
		t.Errorf("expected positive session cost, got %f", sessionStats.TotalCost)
	}
}

func TestOpenRouterObservability_MultiCallSessionProgression(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":    "msg_multi",
			"type":  "message",
			"role":  "assistant",
			"model": "anthropic/claude-3.7-sonnet",
			"content": []map[string]any{
				{"type": "text", "text": "Turn response"},
			},
			"usage": map[string]any{
				"input_tokens":            1000,
				"output_tokens":           200,
				"cache_read_input_tokens": 500,
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer mockOR.Close()

	openrouter.DefaultClient.SaveCache([]openrouter.ModelItem{
		{
			ID:   "anthropic/claude-3.7-sonnet",
			Name: "Claude 3.7 Sonnet",
			Pricing: &openrouter.Pricing{
				Prompt:         0.000003,
				Completion:     0.000015,
				InputCacheRead: 0.0000003,
			},
		},
	})

	_, err := config.Save(map[string]any{
		"openrouter": map[string]any{
			"enabled": true,
			"apiKey":  "sk-or-test-key",
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

	openrouter.DefaultSessionTracker.Reset()

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	sessionID := "progression-session-xyz"

	// Turn 1
	req1 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"anthropic/claude-3.7-sonnet","messages":[{"role":"user","content":"turn 1"}]}`))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("x-api-key", "test-proxy-key")
	req1.Header.Set("x-session-id", sessionID)
	rec1 := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec1, req1)

	s1, _ := openrouter.DefaultSessionTracker.Get(sessionID)
	if s1.RequestCount != 1 {
		t.Errorf("turn 1: expected request count 1, got %d", s1.RequestCount)
	}

	// Turn 2
	req2 := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"anthropic/claude-3.7-sonnet","messages":[{"role":"user","content":"turn 2"}]}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("x-api-key", "test-proxy-key")
	req2.Header.Set("x-session-id", sessionID)
	rec2 := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec2, req2)

	s2, _ := openrouter.DefaultSessionTracker.Get(sessionID)
	if s2.RequestCount != 2 {
		t.Errorf("turn 2: expected request count 2, got %d", s2.RequestCount)
	}
	if s2.InputTokens != 2000 {
		t.Errorf("turn 2: expected 2000 total input tokens, got %d", s2.InputTokens)
	}
	if s2.OutputTokens != 400 {
		t.Errorf("turn 2: expected 400 total output tokens, got %d", s2.OutputTokens)
	}
	if s2.CacheReadTokens != 1000 {
		t.Errorf("turn 2: expected 1000 total cache read tokens, got %d", s2.CacheReadTokens)
	}
	if s2.TotalCost != s1.TotalCost*2 {
		t.Errorf("turn 2: expected total cost to double, got %f vs %f", s2.TotalCost, s1.TotalCost*2)
	}
}

func TestOpenRouterObservability_ColdCacheAutoPricing(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			// Catalog discovery response
			resp := openrouter.ModelsResponse{
				Data: []openrouter.ModelItem{
					{
						ID:   "qwen/qwen3.8-max",
						Name: "Qwen 3.8 Max",
						Pricing: &openrouter.Pricing{
							Prompt:          0.000002,
							Completion:      0.000006,
							InputCacheRead:  0.00000025,
							InputCacheWrite: 0.0000025,
						},
					},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		if r.URL.Path == "/v1/messages" {
			// Messages API response matching user log: 18249 in, 35975 cache read, 383 out
			resp := map[string]any{
				"id":    "msg_qwen_test",
				"type":  "message",
				"role":  "assistant",
				"model": "qwen/qwen3.8-max",
				"content": []map[string]any{
					{"type": "text", "text": "Qwen response"},
				},
				"usage": map[string]any{
					"input_tokens":            18249,
					"output_tokens":           383,
					"cache_read_input_tokens": 35975,
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		http.NotFound(w, r)
	}))
	defer mockOR.Close()

	// Clear cache to simulate cold start
	openrouter.DefaultClient.SaveCache(nil)

	var logBuf bytes.Buffer
	testLogger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	_, err := config.Save(map[string]any{
		"openrouter": map[string]any{
			"enabled": true,
			"apiKey":  "sk-or-test-key",
			"baseUrl": mockOR.URL,
			"allowlist": []map[string]any{
				{
					"id":      "qwen/qwen3.8-max",
					"enabled": true,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("config save error: %v", err)
	}

	openrouter.DefaultSessionTracker.Reset()

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Logger:  testLogger,
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	reqPayload := `{"model":"qwen/qwen3.8-max","messages":[{"role":"user","content":"Hello Qwen"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	req.Header.Set("x-session-id", "session-qwen-1")
	rec := httptest.NewRecorder()

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "[OpenRouter]") {
		t.Errorf("expected [OpenRouter] log tag, got: %s", logs)
	}
	if !strings.Contains(logs, "gateway=openrouter") {
		t.Errorf("expected gateway=openrouter attribute, got: %s", logs)
	}

	// Verify cost is non-zero
	// Expected cost: (18249 * 0.000002) + (383 * 0.000006) + (35975 * 0.00000025)
	// = 0.036498 + 0.002298 + 0.00899375 = 0.04778975 => ~$0.0478
	if strings.Contains(logs, "$0.0000 ($0.0000 session)") {
		t.Errorf("expected non-zero cost calculation in log, got: %s", logs)
	}
	if !strings.Contains(logs, "$0.0478 ($0.0478 session)") {
		t.Errorf("expected $0.0478 in log, got: %s", logs)
	}

	sessionStats, ok := openrouter.DefaultSessionTracker.Get("session-qwen-1")
	if !ok {
		t.Fatalf("expected session stats for session-qwen-1")
	}
	if sessionStats.TotalCost <= 0.04 {
		t.Errorf("expected total cost ~0.0478, got %f", sessionStats.TotalCost)
	}
}

func TestResolveEffectivePricing(t *testing.T) {
	// Case 1: Initial pricing already has non-zero values -> keep initial
	p1 := openrouter.Pricing{Prompt: 0.00001, Completion: 0.00003}
	res1 := resolveEffectivePricing(p1, "nonexistent-model")
	if res1.Prompt != 0.00001 || res1.Completion != 0.00003 {
		t.Errorf("expected original pricing to be preserved, got %+v", res1)
	}

	// Case 2: Initial pricing zero, but cache has pricing -> fallback to cache
	openrouter.DefaultClient.SaveCache([]openrouter.ModelItem{
		{
			ID: "meta-llama/llama-3-70b",
			Pricing: &openrouter.Pricing{
				Prompt:     0.0000007,
				Completion: 0.0000008,
			},
		},
	})
	res2 := resolveEffectivePricing(openrouter.Pricing{}, "meta-llama/llama-3-70b")
	if res2.Prompt != 0.0000007 || res2.Completion != 0.0000008 {
		t.Errorf("expected cached pricing to be resolved, got %+v", res2)
	}
}

func TestOpenRouterObservability_GLMStreamingWithHeaderProviderAndJSONSession(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("OpenRouter-Provider", "Z-AI")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		// 1. message_start with prompt_tokens
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_glm_1\",\"model\":\"z-ai/glm-5.3\",\"usage\":{\"prompt_tokens\":850,\"completion_tokens\":0}}}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}

		// 2. content_block_delta
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"GLM generation\"}}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}

		// 3. message_delta with completion_tokens
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"usage\":{\"completion_tokens\":120}}}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer mockOR.Close()

	openrouter.DefaultClient.SaveCache([]openrouter.ModelItem{
		{
			ID:   "z-ai/glm-5.3",
			Name: "GLM 5.3",
			Pricing: &openrouter.Pricing{
				Prompt:     0.000001,
				Completion: 0.000002,
			},
		},
	})

	_, err := config.Save(map[string]any{
		"openrouter": map[string]any{
			"enabled": true,
			"apiKey":  "sk-or-test-key",
			"baseUrl": mockOR.URL,
			"allowlist": []map[string]any{
				{
					"id":      "z-ai/glm-5.3",
					"enabled": true,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("config save error: %v", err)
	}

	var logBuf bytes.Buffer
	testLogger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
		Logger:  testLogger,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	reqPayload := `{"model":"z-ai/glm-5.3","stream":true,"messages":[{"role":"user","content":"Hello"}],"metadata":{"user_id":"{\"device_id\":\"6a0a24c3\",\"account_uuid\":\"aabe0580\",\"session_id\":\"6c03114f-4472-472b-8abc-b64396665e4d\"}"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "[OpenRouter]") {
		t.Errorf("expected [OpenRouter] log tag, got: %s", logs)
	}
	if !strings.Contains(logs, "z-ai/glm-5.3") {
		t.Errorf("expected z-ai/glm-5.3 in log, got: %s", logs)
	}
	if !strings.Contains(logs, "session_id=6c03114f-4472-472b-8abc-b64396665e4d") {
		t.Errorf("expected clean session_id in log, got: %s", logs)
	}
	if !strings.Contains(logs, "provider=Z-AI") {
		t.Errorf("expected provider=Z-AI in log, got: %s", logs)
	}
	if !strings.Contains(logs, "input_tokens=850") {
		t.Errorf("expected input_tokens=850 in log, got: %s", logs)
	}
	if !strings.Contains(logs, "output_tokens=120") {
		t.Errorf("expected output_tokens=120 in log, got: %s", logs)
	}
}
