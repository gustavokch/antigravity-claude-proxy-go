package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
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
