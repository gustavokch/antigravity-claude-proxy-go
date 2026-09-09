package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/openrouter"
)

type discoveryTestBackend struct{}

func (m *discoveryTestBackend) FetchAvailableModels(ctx context.Context) (cloudcode.Response, error) {
	return cloudcode.Response{
		Body: []byte(`{"models":{"gemini-2.5-pro":{"displayName":"Gemini 2.5 Pro"}},"agentModelSorts":[{"groups":[{"modelIds":["gemini-2.5-pro"]}]}]}`),
	}, nil
}

func (m *discoveryTestBackend) StreamGenerateContent(ctx context.Context, req map[string]any, cb func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	return cloudcode.Response{Body: []byte(`{}`)}, nil
}

func TestClaudeCodeModelDiscovery(t *testing.T) {
	// Enable ClaudeCode in global config
	origCfg := config.Get()
	defer config.SetForTest(origCfg)

	testCfg := origCfg
	testCfg.ClaudeCode.Enabled = true
	testCfg.ClaudeCode.Allowlist = nil // triggers DefaultAllowlist
	config.SetForTest(testCfg)

	server := &Server{
		backend: &discoveryTestBackend{},
		logger:  slog.Default(),
		now:     time.Now,
	}

	for _, path := range []string{"/v1/models", "/models"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()

		server.models(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s returned status %d, expected 200", path, rec.Code)
		}

		var resp struct {
			Object string           `json:"object"`
			Data   []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s failed to unmarshal response: %v", path, err)
		}

		if resp.Object != "list" {
			t.Errorf("%s expected object=list, got %q", path, resp.Object)
		}

		byID := make(map[string]map[string]any)
		seenCounts := make(map[string]int)
		for _, m := range resp.Data {
			id, _ := m["id"].(string)
			if id != "" {
				byID[id] = m
				seenCounts[id]++
			}
		}

		// Ensure no duplicates
		for id, count := range seenCounts {
			if count > 1 {
				t.Errorf("%s model %q duplicated %d times", path, id, count)
			}
		}

		// Verify Claude models and all aliases exist
		expectedModels := []string{
			"claude-sonnet-5", "sonnet-5",
			"claude-opus-5", "opus-5",
			"claude-fable-5", "fable-5",
			"claude-haiku-4-5-20251001", "haiku-4-5", "claude-haiku-4-5", "claude-haiku-4.5", "haiku-4.5",
			"claude-3-7-sonnet-20250219", "claude-3-7-sonnet", "sonnet-3-7", "claude-3.7-sonnet", "sonnet-3.7",
			"claude-3-5-sonnet-20241022", "claude-3-5-sonnet", "sonnet-3-5", "claude-3.5-sonnet", "sonnet-3.5",
			"claude-3-5-haiku-20241022", "claude-3-5-haiku", "haiku-3-5", "claude-3.5-haiku", "haiku-3.5",
			"claude-3-opus-20240229", "claude-3-opus", "opus-3", "claude-3.0-opus",
			"claude-3-haiku-20240307", "claude-3-haiku", "haiku-3", "claude-3.0-haiku",
			"claude-3-sonnet-20240229", "claude-3-sonnet", "sonnet-3", "claude-3.0-sonnet",
		}

		for _, id := range expectedModels {
			m, found := byID[id]
			if !found {
				t.Errorf("%s model %q not found in response", path, id)
				continue
			}
			if m["owned_by"] != "anthropic" {
				t.Errorf("%s model %q: expected owned_by=anthropic, got %v", path, id, m["owned_by"])
			}
			if m["object"] != "model" {
				t.Errorf("%s model %q: expected object=model, got %v", path, id, m["object"])
			}
		}

		// Verify aliases metadata field on canonical entry
		sonnet5 := byID["claude-sonnet-5"]
		aliases, hasAliases := sonnet5["aliases"].([]any)
		if !hasAliases || len(aliases) == 0 {
			t.Errorf("%s expected aliases slice on claude-sonnet-5, got %#v", path, sonnet5["aliases"])
		}
		if sonnet5["supports_thinking"] != true {
			t.Errorf("%s expected supports_thinking=true on claude-sonnet-5, got %v", path, sonnet5["supports_thinking"])
		}
	}
}

func TestClaudeCodeCustomAllowlistDiscovery(t *testing.T) {
	origCfg := config.Get()
	defer config.SetForTest(origCfg)

	testCfg := origCfg
	testCfg.ClaudeCode.Enabled = true
	testCfg.ClaudeCode.Allowlist = []claudecode.ModelConfig{
		{
			ID:              "custom-claude-preview",
			Alias:           "custom-alias",
			Aliases:         []string{"custom-alias-1", "custom-alias-2"},
			DisplayName:     "Custom Claude Preview",
			ContextLen:      128000,
			MaxOutputTokens: 4096,
			Thinking:        true,
			Enabled:         true,
		},
	}
	config.SetForTest(testCfg)

	server := &Server{
		backend: &discoveryTestBackend{},
		logger:  slog.Default(),
		now:     time.Now,
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()

	server.models(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("returned status %d, expected 200", rec.Code)
	}

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	byID := make(map[string]map[string]any)
	for _, m := range resp.Data {
		if id, ok := m["id"].(string); ok {
			byID[id] = m
		}
	}

	for _, expectedID := range []string{"custom-claude-preview", "custom-alias", "custom-alias-1", "custom-alias-2"} {
		m, ok := byID[expectedID]
		if !ok {
			t.Errorf("expected model %q in response", expectedID)
			continue
		}
		if m["owned_by"] != "anthropic" {
			t.Errorf("expected owned_by=anthropic for %q, got %v", expectedID, m["owned_by"])
		}
		if m["supports_thinking"] != true {
			t.Errorf("expected supports_thinking=true for %q", expectedID)
		}
	}
}

type geminiDiscoveryTestBackend struct{}

func (m *geminiDiscoveryTestBackend) FetchAvailableModels(ctx context.Context) (cloudcode.Response, error) {
	return cloudcode.Response{
		Body: []byte(`{
			"models":{
				"gemini-3.8-flash-high":{"displayName":"Gemini 3.8 Flash (High)","supportsThinking":true,"thinkingBudget":16000,"maxTokens":1048576,"maxOutputTokens":65536},
				"gemini-2.5-pro":{"displayName":"Gemini 2.5 Pro","supportsThinking":true,"thinkingBudget":10000,"maxTokens":1048576,"maxOutputTokens":65536},
				"claude-opus-4-6-thinking":{"displayName":"Claude Opus 4.6 (Thinking)","supportsThinking":true,"thinkingBudget":1024,"maxTokens":250000,"maxOutputTokens":64000}
			},
			"agentModelSorts":[{"groups":[{"modelIds":["gemini-3.8-flash-high","gemini-2.5-pro","claude-opus-4-6-thinking"]}]}]
		}`),
	}, nil
}

func (m *geminiDiscoveryTestBackend) StreamGenerateContent(ctx context.Context, req map[string]any, cb func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	return cloudcode.Response{Body: []byte(`{}`)}, nil
}

func TestGeminiModels_AdvertiseMaxContextWindow(t *testing.T) {
	server := &Server{
		backend: &geminiDiscoveryTestBackend{},
		logger:  slog.Default(),
		now:     time.Now,
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()

	server.models(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("returned status %d, expected 200", rec.Code)
	}

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	for _, m := range resp.Data {
		id, _ := m["id"].(string)
		if strings.HasPrefix(id, "gemini-") {
			// Ensure no [1m] suffix in advertised model IDs
			if strings.Contains(id, "[1m]") || strings.Contains(id, "[1M]") {
				t.Errorf("model ID %q contains [1m] suffix", id)
			}
			// Verify context window is reported >= 1M. encoding/json decodes
			// numbers into float64 inside map[string]any, so assert that type.
			cw, ok := m["context_window"].(float64)
			if !ok {
				t.Errorf("gemini model %q missing or non-numeric context_window", id)
				continue
			}
			if cw < 1000000 {
				t.Errorf("gemini model %q context_window = %v, expected >= 1M", id, cw)
			}
		}
	}
}

// withOpenRouterCatalog swaps the package-global OpenRouter catalog cache for
// the duration of one test and restores the previous contents afterwards.
//
// The cache timestamp cannot be restored exactly — SaveCache always stamps
// time.Now() — but validity survives the round trip, because IsCacheValid
// requires a non-empty cache: an empty cache stays invalid, and a populated
// one that was valid stays valid. Tests using this helper must not call
// t.Parallel: the cache is process-wide state.
func withOpenRouterCatalog(t *testing.T, models []openrouter.ModelItem) {
	t.Helper()
	prev := openrouter.DefaultClient.GetCachedModels()
	t.Cleanup(func() { openrouter.DefaultClient.SaveCache(prev) })
	openrouter.DefaultClient.SaveCache(models)
}

// TestOpenRouterModels_MaxOutputFallbackDoesNotEqualContextWindow guards the
// discovery fallback that fires when the live catalog knows a model's context
// window but not its max completion tokens. Equating the two advertises a
// 1M-token max output for a 1M-context model, and clients that trust
// /v1/models then send a max_tokens the provider rejects. The context window
// must still report the real value.
func TestOpenRouterModels_MaxOutputFallbackDoesNotEqualContextWindow(t *testing.T) {
	withOpenRouterCatalog(t, []openrouter.ModelItem{
		// Context known, max completion tokens unknown: the exact shape that
		// drives the fallback.
		{ID: "vendor/huge-context", ContextLength: 1048576},
	})

	origCfg := config.Get()
	t.Cleanup(func() { config.SetForTest(origCfg) })
	testCfg := origCfg
	testCfg.OpenRouter.Enabled = true
	testCfg.OpenRouter.Allowlist = []config.OpenRouterModelConfig{
		{ID: "vendor/huge-context", Enabled: true},
	}
	config.SetForTest(testCfg)

	server := &Server{backend: &discoveryTestBackend{}, logger: slog.Default(), now: time.Now}
	rec := httptest.NewRecorder()
	server.models(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("returned status %d, expected 200", rec.Code)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal: %v", err)
	}

	var entry map[string]any
	for _, m := range resp.Data {
		if m["id"] == "vendor/huge-context" {
			entry = m
			break
		}
	}
	if entry == nil {
		t.Fatalf("allowlist model missing from discovery response")
	}
	if cw, _ := entry["context_window"].(float64); cw != 1048576 {
		t.Errorf("context_window = %v, expected 1048576 from the live catalog", entry["context_window"])
	}
	if mo, _ := entry["max_output_tokens"].(float64); mo != float64(defaultDiscoveryMaxOutputTokens) {
		t.Errorf("max_output_tokens = %v, expected %d (fallback must not equal the context window)",
			entry["max_output_tokens"], defaultDiscoveryMaxOutputTokens)
	}
}

// TestOpenRouterModels_WarmsColdCatalogCache pins that discovery repairs an
// empty or expired catalog cache instead of silently serving the conservative
// defaults forever. The startup warmup is asynchronous and silent on failure,
// so a /v1/models request that arrives first — or after a failed startup
// fetch — is the only chance to notice the cache is not there.
func TestOpenRouterModels_WarmsColdCatalogCache(t *testing.T) {
	withOpenRouterCatalog(t, nil)

	var hits int32
	catalog := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"vendor/cold","context_length":1048576,"max_completion_tokens":65536}]}`))
	}))
	t.Cleanup(catalog.Close)

	origCfg := config.Get()
	t.Cleanup(func() { config.SetForTest(origCfg) })
	testCfg := origCfg
	testCfg.OpenRouter.Enabled = true
	testCfg.OpenRouter.APIKey = "test-key"
	testCfg.OpenRouter.BaseURL = catalog.URL
	testCfg.OpenRouter.Allowlist = []config.OpenRouterModelConfig{
		{ID: "vendor/cold", Enabled: true},
	}
	config.SetForTest(testCfg)

	server := &Server{backend: &discoveryTestBackend{}, logger: slog.Default(), now: time.Now}
	rec := httptest.NewRecorder()
	server.models(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("returned status %d, expected 200", rec.Code)
	}

	// The fetch is asynchronous by design: this request may still answer from
	// the defaults, but the cache must be filled for the next one.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, maxOut, ok := openrouter.DefaultClient.GetModelLimits("vendor/cold"); ok && maxOut == 65536 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("catalog cache still cold after discovery request (%d upstream fetches)", atomic.LoadInt32(&hits))
}
