package config

import (
	"path/filepath"
	"testing"
)

func TestUpdateClaudeConfig_CleansLegacy1mSuffix(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_PATH", filepath.Join(tmpDir, "settings.json"))

	updates := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_MODEL":            "gemini-3.8-flash-high[1m]",
			"CLAUDE_CODE_SUBAGENT_MODEL": "gemini-3.7-flash-high[1M]",
			"ANTHROPIC_SMALL_FAST_MODEL": "gemini-3.8-flash[1m]",
			"ANTHROPIC_BASE_URL":         "http://localhost:8080",
		},
	}

	updated, err := UpdateClaudeConfig(updates)
	if err != nil {
		t.Fatalf("UpdateClaudeConfig failed: %v", err)
	}

	env, ok := updated["env"].(map[string]any)
	if !ok {
		t.Fatalf("expected env map in updated config")
	}

	if env["ANTHROPIC_MODEL"] != "gemini-3.8-flash-high" {
		t.Errorf("expected ANTHROPIC_MODEL sanitized to gemini-3.8-flash-high, got %v", env["ANTHROPIC_MODEL"])
	}
	if env["CLAUDE_CODE_SUBAGENT_MODEL"] != "gemini-3.7-flash-high" {
		t.Errorf("expected CLAUDE_CODE_SUBAGENT_MODEL sanitized to gemini-3.7-flash-high, got %v", env["CLAUDE_CODE_SUBAGENT_MODEL"])
	}
	if env["ANTHROPIC_SMALL_FAST_MODEL"] != "gemini-3.8-flash" {
		t.Errorf("expected ANTHROPIC_SMALL_FAST_MODEL sanitized to gemini-3.8-flash, got %v", env["ANTHROPIC_SMALL_FAST_MODEL"])
	}
}

func TestUpdateClaudeConfig_BareSuffixValueLeftAsIs(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_PATH", filepath.Join(tmpDir, "settings.json"))

	updates := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_MODEL": "[1m]",
		},
	}

	updated, err := UpdateClaudeConfig(updates)
	if err != nil {
		t.Fatalf("UpdateClaudeConfig failed: %v", err)
	}

	env := updated["env"].(map[string]any)
	// A value that is only the suffix must be left untouched, not emptied.
	if env["ANTHROPIC_MODEL"] != "[1m]" {
		t.Errorf("expected bare [1m] value left as-is, got %v", env["ANTHROPIC_MODEL"])
	}
}
