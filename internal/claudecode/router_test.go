package claudecode

import (
	"testing"
)

func TestRouter_ResolveModel(t *testing.T) {
	r := NewRouter(nil) // Use default allowlist

	tests := []struct {
		input       string
		expectedID  string
		expectFound bool
	}{
		{"claude-sonnet-5", "claude-sonnet-5", true},
		{"sonnet-5", "claude-sonnet-5", true},
		{"claude-opus-5", "claude-opus-5", true},
		{"opus-5", "claude-opus-5", true},
		{"claude-fable-5", "claude-fable-5", true},
		{"fable-5", "claude-fable-5", true},
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001", true},
		{"claude-haiku-4-5", "claude-haiku-4-5-20251001", true},
		{"haiku-4-5", "claude-haiku-4-5-20251001", true},
		{"claude-haiku-4.5", "claude-haiku-4-5-20251001", true},
		{"haiku-4.5", "claude-haiku-4-5-20251001", true},
		{"claude-3-7-sonnet-20250219", "claude-3-7-sonnet-20250219", true},
		{"claude-3-7-sonnet", "claude-3-7-sonnet-20250219", true},
		{"claude-3.7-sonnet", "claude-3-7-sonnet-20250219", true},
		{"sonnet-3-7", "claude-3-7-sonnet-20250219", true},
		{"sonnet-3.7", "claude-3-7-sonnet-20250219", true},
		{"claude-3-5-sonnet", "claude-3-5-sonnet-20241022", true},
		{"claude-3.5-sonnet", "claude-3-5-sonnet-20241022", true},
		{"sonnet-3-5", "claude-3-5-sonnet-20241022", true},
		{"sonnet-3.5", "claude-3-5-sonnet-20241022", true},
		{"CLAUDE-FABLE-5", "claude-fable-5", true},
		{"claude-3-5-haiku", "claude-3-5-haiku-20241022", true},
		{"claude-3.5-haiku", "claude-3-5-haiku-20241022", true},
		{"haiku-3-5", "claude-3-5-haiku-20241022", true},
		{"haiku-3.5", "claude-3-5-haiku-20241022", true},
		{"claude-3-opus", "claude-3-opus-20240229", true},
		{"opus-3", "claude-3-opus-20240229", true},
		{"sonnet-3-5-custom-build", "claude-3-5-sonnet-20241022", true},
		{"sonnet-3.5-custom-build", "claude-3-5-sonnet-20241022", true},
		{"sonnet-3-custom-build", "claude-3-sonnet-20240229", true},
		{"haiku-3-5-custom-build", "claude-3-5-haiku-20241022", true},
		{"haiku-3-custom-build", "claude-3-haiku-20240307", true},
		{"claude", "", false},
		{"c", "", false},
		{"claude-", "", false},
		{"non-existent-model", "", false},
		{"", "", false},
	}

	for _, tt := range tests {
		canonical, found := r.ResolveModel(tt.input)
		if found != tt.expectFound {
			t.Errorf("ResolveModel(%q): expected found=%v, got %v", tt.input, tt.expectFound, found)
		}
		if found && canonical != tt.expectedID {
			t.Errorf("ResolveModel(%q): expected ID %q, got %q", tt.input, tt.expectedID, canonical)
		}
		if r.IsModelAllowed(tt.input) != tt.expectFound {
			t.Errorf("IsModelAllowed(%q): expected %v, got %v", tt.input, tt.expectFound, r.IsModelAllowed(tt.input))
		}
	}
}

func TestRouter_UpdateAllowlist(t *testing.T) {
	custom := []ModelConfig{
		{
			ID:          "custom-claude-model",
			Alias:       "custom-alias",
			Aliases:     []string{"custom-alias-2"},
			DisplayName: "Custom",
			Enabled:     true,
		},
		{
			ID:      "disabled-model",
			Enabled: false,
		},
	}

	r := NewRouter(custom)
	if !r.IsModelAllowed("custom-claude-model") {
		t.Errorf("expected custom-claude-model to be allowed")
	}
	if !r.IsModelAllowed("custom-alias") {
		t.Errorf("expected custom-alias to be allowed")
	}
	if !r.IsModelAllowed("custom-alias-2") {
		t.Errorf("expected custom-alias-2 to be allowed")
	}
	if r.IsModelAllowed("disabled-model") {
		t.Errorf("expected disabled-model to NOT be allowed")
	}
	if r.IsModelAllowed("claude-sonnet-5") {
		t.Errorf("expected default models to not be present after custom allowlist")
	}

	models := r.GetAllowedModels()
	if len(models) != 1 {
		t.Errorf("expected 1 enabled model, got %d", len(models))
	}
}
