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

func TestUpdateClaudeConfig_BareSuffixValueTrimmed(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_PATH", filepath.Join(tmpDir, "settings.json"))

	updates := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_MODEL": "  [1m]  ",
		},
	}

	updated, err := UpdateClaudeConfig(updates)
	if err != nil {
		t.Fatalf("UpdateClaudeConfig failed: %v", err)
	}

	env := updated["env"].(map[string]any)
	if env["ANTHROPIC_MODEL"] != "[1m]" {
		t.Errorf("expected padded bare [1m] trimmed to [1m], got %q", env["ANTHROPIC_MODEL"])
	}
}

func TestUpdateClaudeConfig_DoesNotAliasExistingEnv(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_PATH", filepath.Join(tmpDir, "settings.json"))

	if err := ReplaceClaudeConfig(map[string]any{
		"env": map[string]any{"ANTHROPIC_MODEL": "old-model"},
	}); err != nil {
		t.Fatalf("failed to seed settings.json: %v", err)
	}

	before, err := ReadClaudeConfig()
	if err != nil {
		t.Fatalf("ReadClaudeConfig failed: %v", err)
	}
	heldEnv := before["env"].(map[string]any)

	updated, err := UpdateClaudeConfig(map[string]any{
		"env": map[string]any{"ANTHROPIC_MODEL": "new-model[1m]"},
	})
	if err != nil {
		t.Fatalf("UpdateClaudeConfig failed: %v", err)
	}

	if env := updated["env"].(map[string]any); env["ANTHROPIC_MODEL"] != "new-model" {
		t.Errorf("expected persisted config sanitized, got %v", env["ANTHROPIC_MODEL"])
	}
	if heldEnv["ANTHROPIC_MODEL"] != "old-model" {
		t.Errorf("prior ReadClaudeConfig env map was mutated via merge aliasing: got %v", heldEnv["ANTHROPIC_MODEL"])
	}
}

func TestRestoreClaudeConfig_CleansSmallFastModel(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_PATH", filepath.Join(tmpDir, "settings.json"))

	updates := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_SMALL_FAST_MODEL": "gemini-3.8-flash",
			"CUSTOM_USER_VAR":           "keep-me",
		},
	}

	if _, err := UpdateClaudeConfig(updates); err != nil {
		t.Fatalf("UpdateClaudeConfig failed: %v", err)
	}

	restored, err := RestoreClaudeConfig()
	if err != nil {
		t.Fatalf("RestoreClaudeConfig failed: %v", err)
	}

	env, ok := restored["env"].(map[string]any)
	if !ok {
		t.Fatalf("expected env map in restored config")
	}

	if _, exists := env["ANTHROPIC_SMALL_FAST_MODEL"]; exists {
		t.Errorf("expected ANTHROPIC_SMALL_FAST_MODEL to be deleted on restore, but it was kept: %v", env["ANTHROPIC_SMALL_FAST_MODEL"])
	}
	if env["CUSTOM_USER_VAR"] != "keep-me" {
		t.Errorf("expected CUSTOM_USER_VAR to be preserved, got %v", env["CUSTOM_USER_VAR"])
	}
}

func TestUpdateClaudeConfig_DoesNotMutateUpdatesInput(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_PATH", filepath.Join(tmpDir, "settings.json"))

	// Pre-existing settings.json without an "env" section forces UpdateClaudeConfig
	// through the new-section merge path (the one that mutates the caller's map).
	if err := ReplaceClaudeConfig(map[string]any{
		"other": map[string]any{"key": "value"},
	}); err != nil {
		t.Fatalf("failed to seed settings.json: %v", err)
	}

	updates := map[string]any{
		"env": map[string]any{
			"ANTHROPIC_MODEL": "gemini-3.8-flash-high[1m]",
		},
	}

	updated, err := UpdateClaudeConfig(updates)
	if err != nil {
		t.Fatalf("UpdateClaudeConfig failed: %v", err)
	}

	if env := updated["env"].(map[string]any); env["ANTHROPIC_MODEL"] != "gemini-3.8-flash-high" {
		t.Errorf("expected persisted config sanitized, got %v", env["ANTHROPIC_MODEL"])
	}

	// The caller's input map must not be modified: the persisted map is a copy.
	inputEnv := updates["env"].(map[string]any)
	if inputEnv["ANTHROPIC_MODEL"] != "gemini-3.8-flash-high[1m]" {
		t.Errorf("caller's updates map was mutated in place: got %v", inputEnv["ANTHROPIC_MODEL"])
	}
}
