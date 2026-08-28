package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
	"antigravity-go-proxy/internal/openrouter"
)

func boolPtr(b bool) *bool {
	return &b
}

func setupOpenRouterCacheServer(t *testing.T, orConfig map[string]any, orHandler http.HandlerFunc) (*Server, *httptest.Server) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	mockOR := httptest.NewServer(orHandler)
	t.Cleanup(mockOR.Close)

	orConfig["baseUrl"] = mockOR.URL
	orConfig["enabled"] = true
	if _, hasKey := orConfig["apiKey"]; !hasKey {
		orConfig["apiKey"] = "sk-or-test-key"
	}
	if _, hasAllowlist := orConfig["allowlist"]; !hasAllowlist {
		orConfig["allowlist"] = []map[string]any{
			{
				"id":            "anthropic/claude-3.7-sonnet",
				"alias":         "claude-3-7-openrouter",
				"displayName":   "Claude 3.7 Sonnet (OpenRouter)",
				"contextLength": 200000,
				"enabled":       true,
			},
		}
	}

	_, err := config.Save(map[string]any{
		"openrouter": orConfig,
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

	return server, mockOR
}

func TestOpenRouterCache_RequestHeaderInjection(t *testing.T) {
	t.Run("caching enabled via proxy config injects headers", func(t *testing.T) {
		var receivedCache, receivedTTL, receivedClear string
		var mu sync.Mutex

		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/models" {
				_ = json.NewEncoder(w).Encode(openrouter.ModelsResponse{})
				return
			}
			if strings.HasSuffix(r.URL.Path, "/endpoints") {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"endpoints": []any{}}})
				return
			}
			mu.Lock()
			receivedCache = r.Header.Get(openrouter.HeaderCache)
			receivedTTL = r.Header.Get(openrouter.HeaderCacheTTL)
			receivedClear = r.Header.Get(openrouter.HeaderCacheClear)
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"cached hi"}],"model":"anthropic/claude-3.7-sonnet"}`))
		})

		server, _ := setupOpenRouterCacheServer(t, map[string]any{
			"responseCache": map[string]any{
				"enabled":    true,
				"ttlSeconds": 600,
			},
		}, handler)

		reqPayload := `{"model":"claude-3-7-openrouter","messages":[{"role":"user","content":"Hello"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", "test-proxy-key")

		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
		}

		mu.Lock()
		defer mu.Unlock()
		if receivedCache != "true" {
			t.Errorf("expected %s='true', got %q", openrouter.HeaderCache, receivedCache)
		}
		if receivedTTL != "600" {
			t.Errorf("expected %s='600', got %q", openrouter.HeaderCacheTTL, receivedTTL)
		}
		if receivedClear != "" {
			t.Errorf("expected %s to be empty, got %q", openrouter.HeaderCacheClear, receivedClear)
		}
	})

	t.Run("client override allowed passes client headers", func(t *testing.T) {
		var receivedCache, receivedTTL, receivedClear string
		var mu sync.Mutex

		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/models" || strings.HasSuffix(r.URL.Path, "/endpoints") {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
				return
			}
			mu.Lock()
			receivedCache = r.Header.Get(openrouter.HeaderCache)
			receivedTTL = r.Header.Get(openrouter.HeaderCacheTTL)
			receivedClear = r.Header.Get(openrouter.HeaderCacheClear)
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"model":"anthropic/claude-3.7-sonnet"}`))
		})

		server, _ := setupOpenRouterCacheServer(t, map[string]any{
			"responseCache": map[string]any{
				"enabled":             false,
				"allowClientOverride": true,
			},
		}, handler)

		reqPayload := `{"model":"claude-3-7-openrouter","messages":[{"role":"user","content":"Hello"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", "test-proxy-key")
		req.Header.Set(openrouter.HeaderCache, "true")
		req.Header.Set(openrouter.HeaderCacheTTL, "120")
		req.Header.Set(openrouter.HeaderCacheClear, "true")

		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
		}

		mu.Lock()
		defer mu.Unlock()
		if receivedCache != "true" {
			t.Errorf("expected %s='true', got %q", openrouter.HeaderCache, receivedCache)
		}
		if receivedTTL != "120" {
			t.Errorf("expected %s='120', got %q", openrouter.HeaderCacheTTL, receivedTTL)
		}
		if receivedClear != "true" {
			t.Errorf("expected %s='true', got %q", openrouter.HeaderCacheClear, receivedClear)
		}
	})

	t.Run("client override denied strips client headers when caching is off", func(t *testing.T) {
		var receivedCache, receivedTTL, receivedClear string
		var mu sync.Mutex

		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/models" || strings.HasSuffix(r.URL.Path, "/endpoints") {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
				return
			}
			mu.Lock()
			receivedCache = r.Header.Get(openrouter.HeaderCache)
			receivedTTL = r.Header.Get(openrouter.HeaderCacheTTL)
			receivedClear = r.Header.Get(openrouter.HeaderCacheClear)
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"model":"anthropic/claude-3.7-sonnet"}`))
		})

		server, _ := setupOpenRouterCacheServer(t, map[string]any{
			"responseCache": map[string]any{
				"enabled":             false,
				"allowClientOverride": false,
			},
		}, handler)

		reqPayload := `{"model":"claude-3-7-openrouter","messages":[{"role":"user","content":"Hello"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", "test-proxy-key")
		req.Header.Set(openrouter.HeaderCache, "true")
		req.Header.Set(openrouter.HeaderCacheTTL, "120")
		req.Header.Set(openrouter.HeaderCacheClear, "true")

		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
		}

		mu.Lock()
		defer mu.Unlock()
		if receivedCache != "" {
			t.Errorf("expected %s to be stripped, got %q", openrouter.HeaderCache, receivedCache)
		}
		if receivedTTL != "" {
			t.Errorf("expected %s to be stripped, got %q", openrouter.HeaderCacheTTL, receivedTTL)
		}
		if receivedClear != "" {
			t.Errorf("expected %s to be stripped, got %q", openrouter.HeaderCacheClear, receivedClear)
		}
	})

	t.Run("Clear forwarded with proxy cache enabled even when client override disallowed", func(t *testing.T) {
		var receivedCache, receivedClear string
		var mu sync.Mutex

		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/models" || strings.HasSuffix(r.URL.Path, "/endpoints") {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
				return
			}
			mu.Lock()
			receivedCache = r.Header.Get(openrouter.HeaderCache)
			receivedClear = r.Header.Get(openrouter.HeaderCacheClear)
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"model":"anthropic/claude-3.7-sonnet"}`))
		})

		server, _ := setupOpenRouterCacheServer(t, map[string]any{
			"responseCache": map[string]any{
				"enabled":             true,
				"allowClientOverride": false,
			},
		}, handler)

		reqPayload := `{"model":"claude-3-7-openrouter","messages":[{"role":"user","content":"Hello"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", "test-proxy-key")
		req.Header.Set(openrouter.HeaderCacheClear, "true")

		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
		}

		mu.Lock()
		defer mu.Unlock()
		if receivedCache != "true" {
			t.Errorf("expected %s='true', got %q", openrouter.HeaderCache, receivedCache)
		}
		if receivedClear != "true" {
			t.Errorf("expected %s='true' (forwarded when caching is on), got %q", openrouter.HeaderCacheClear, receivedClear)
		}
	})
}

func TestOpenRouterCache_ResponseHeaderPropagationAndHitHandling(t *testing.T) {
	t.Run("upstream cache HIT propagates headers to downstream client", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/models" || strings.HasSuffix(r.URL.Path, "/endpoints") {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set(openrouter.HeaderCacheStatus, "HIT")
			w.Header().Set(openrouter.HeaderCacheAge, "42")
			w.Header().Set(openrouter.HeaderCacheTTL, "258")
			w.Header().Set(openrouter.HeaderCacheSourceID, "gen-hit-source-123")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"id": "msg_hit_1",
				"type": "message",
				"role": "assistant",
				"content": [{"type":"text","text":"I am a cached response"}],
				"model": "anthropic/claude-3.7-sonnet",
				"usage": {
					"input_tokens": 0,
					"output_tokens": 0
				}
			}`))
		})

		server, _ := setupOpenRouterCacheServer(t, map[string]any{
			"responseCache": map[string]any{
				"enabled": true,
			},
		}, handler)

		reqPayload := `{"model":"claude-3-7-openrouter","messages":[{"role":"user","content":"Hello"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", "test-proxy-key")

		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
		}

		if got := rec.Header().Get(openrouter.HeaderCacheStatus); got != "HIT" {
			t.Errorf("expected %s='HIT', got %q", openrouter.HeaderCacheStatus, got)
		}
		if got := rec.Header().Get(openrouter.HeaderCacheAge); got != "42" {
			t.Errorf("expected %s='42', got %q", openrouter.HeaderCacheAge, got)
		}
		if got := rec.Header().Get(openrouter.HeaderCacheTTL); got != "258" {
			t.Errorf("expected %s='258', got %q", openrouter.HeaderCacheTTL, got)
		}
		if got := rec.Header().Get(openrouter.HeaderCacheSourceID); got != "gen-hit-source-123" {
			t.Errorf("expected %s='gen-hit-source-123', got %q", openrouter.HeaderCacheSourceID, got)
		}
	})

	t.Run("upstream cache MISS propagates status and TTL headers", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/models" || strings.HasSuffix(r.URL.Path, "/endpoints") {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set(openrouter.HeaderCacheStatus, "MISS")
			w.Header().Set(openrouter.HeaderCacheTTL, "300")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"id": "msg_miss_1",
				"type": "message",
				"role": "assistant",
				"content": [{"type":"text","text":"Fresh completion"}],
				"model": "anthropic/claude-3.7-sonnet",
				"usage": {
					"input_tokens": 150,
					"output_tokens": 50
				}
			}`))
		})

		server, _ := setupOpenRouterCacheServer(t, map[string]any{
			"responseCache": map[string]any{
				"enabled": true,
			},
		}, handler)

		reqPayload := `{"model":"claude-3-7-openrouter","messages":[{"role":"user","content":"Hello"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", "test-proxy-key")

		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
		}

		if got := rec.Header().Get(openrouter.HeaderCacheStatus); got != "MISS" {
			t.Errorf("expected %s='MISS', got %q", openrouter.HeaderCacheStatus, got)
		}
		if got := rec.Header().Get(openrouter.HeaderCacheTTL); got != "300" {
			t.Errorf("expected %s='300', got %q", openrouter.HeaderCacheTTL, got)
		}
	})

	t.Run("streaming SSE response with cache HIT propagates headers and stream", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/models" || strings.HasSuffix(r.URL.Path, "/endpoints") {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set(openrouter.HeaderCacheStatus, "HIT")
			w.Header().Set(openrouter.HeaderCacheAge, "15")
			w.Header().Set(openrouter.HeaderCacheTTL, "285")
			w.Header().Set(openrouter.HeaderCacheSourceID, "gen-sse-hit-src")
			w.WriteHeader(http.StatusOK)

			flusher, _ := w.(http.Flusher)
			_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_sse_hit\",\"usage\":{\"input_tokens\":0}}}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"streaming cached response\"}}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":0}}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		})

		server, _ := setupOpenRouterCacheServer(t, map[string]any{
			"responseCache": map[string]any{
				"enabled": true,
			},
		}, handler)

		reqPayload := `{"model":"claude-3-7-openrouter","stream":true,"messages":[{"role":"user","content":"Hello streaming"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqPayload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", "test-proxy-key")

		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
		}

		if got := rec.Header().Get(openrouter.HeaderCacheStatus); got != "HIT" {
			t.Errorf("expected %s='HIT', got %q", openrouter.HeaderCacheStatus, got)
		}
		if got := rec.Header().Get(openrouter.HeaderCacheAge); got != "15" {
			t.Errorf("expected %s='15', got %q", openrouter.HeaderCacheAge, got)
		}
		if got := rec.Header().Get(openrouter.HeaderCacheSourceID); got != "gen-sse-hit-src" {
			t.Errorf("expected %s='gen-sse-hit-src', got %q", openrouter.HeaderCacheSourceID, got)
		}

		bodyStr := rec.Body.String()
		if !strings.Contains(bodyStr, "streaming cached response") {
			t.Errorf("expected stream body to contain delta, got: %s", bodyStr)
		}
	})
}
