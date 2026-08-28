package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
)

func TestModels_ClaudeCodeDiscoveryAndAliasing(t *testing.T) {
	// Enable ClaudeCode in config
	cfg := config.DefaultConfig()
	cfg.ClaudeCode.Enabled = true
	cfg.ClaudeCode.Accounts = []claudecode.AccountConfig{
		{
			ID:      "test-account-1",
			Name:    "Test Account",
			Token:   "sk-ant-test-token",
			Type:    "oauth",
			Enabled: true,
		},
	}
	cfg.ClaudeCode.Allowlist = claudecode.DefaultAllowlist()

	// Write config to in-memory config for testing
	origCfg := config.Get()
	config.SetForTest(cfg)
	defer config.SetForTest(origCfg)

	upstream := &fakeUpstream{
		modelsBody: []byte(`{"models":{"gemini-3.5-flash":{"displayName":"Gemini 3.5 Flash (High)","supportsThinking":true,"thinkingBudget":4000,"maxTokens":1048576,"maxOutputTokens":65536,"quotaInfo":{"remainingFraction":0.875,"resetTime":"2026-07-15T06:26:33Z"}}}}`),
	}
	handler := newTestHandler(t, upstream, "project")

	for _, path := range []string{"/v1/models", "/anthropic/v1/models"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer local-key")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}

		var payload map[string]any
		decodeBody(t, response.Body, &payload)
		models, ok := payload["data"].([]any)
		if !ok || len(models) == 0 {
			t.Fatalf("%s expected models list in data, got %#v", path, payload)
		}

		byID := make(map[string]map[string]any)
		seenCounts := make(map[string]int)
		for _, m := range models {
			mMap, ok := m.(map[string]any)
			if ok {
				id, _ := mMap["id"].(string)
				byID[id] = mMap
				seenCounts[id]++
			}
		}

		// Check deduplication
		for id, count := range seenCounts {
			if count > 1 {
				t.Errorf("%s model %q duplicated %d times", path, id, count)
			}
		}

		// Verify Claude 5 models exist
		expectedModels := []string{
			"claude-sonnet-5", "sonnet-5",
			"claude-opus-5", "opus-5",
			"claude-fable-5", "fable-5",
			"claude-haiku-4-5-20251001", "haiku-4-5",
			"claude-3-7-sonnet-20250219", "claude-3-7-sonnet",
			"claude-3-5-sonnet-20241022", "claude-3-5-sonnet",
			"claude-3-5-haiku-20241022", "claude-3-5-haiku",
			"claude-3-opus-20240229", "claude-3-opus",
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
