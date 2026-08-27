package config

import (
	"os"
	"path/filepath"
	"testing"

	"antigravity-go-proxy/internal/openrouter"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.LogLevel != "info" {
		t.Errorf("expected LogLevel info, got %s", cfg.LogLevel)
	}
	if cfg.MaxRetries != 5 {
		t.Errorf("expected MaxRetries 5, got %d", cfg.MaxRetries)
	}
	if cfg.AccountSelection.Strategy != "hybrid" {
		t.Errorf("expected Strategy hybrid, got %s", cfg.AccountSelection.Strategy)
	}
}

func TestClaudePresets(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	presets, err := ReadClaudePresets()
	if err != nil {
		t.Fatalf("ReadClaudePresets error: %v", err)
	}
	if len(presets) < 3 {
		t.Errorf("expected at least 3 default presets, got %d", len(presets))
	}

	// Save new custom preset
	customConfig := map[string]any{"ANTHROPIC_MODEL": "custom-model"}
	updated, err := SaveClaudePreset("My Preset", customConfig)
	if err != nil {
		t.Fatalf("SaveClaudePreset error: %v", err)
	}
	if len(updated) <= len(presets) {
		t.Errorf("expected preset count to increase")
	}

	// Delete custom preset
	deleted, err := DeleteClaudePreset("My Preset")
	if err != nil {
		t.Fatalf("DeleteClaudePreset error: %v", err)
	}
	if len(deleted) != len(presets) {
		t.Errorf("expected preset count to return to %d, got %d", len(presets), len(deleted))
	}
}

func TestServerPresets(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	presets, err := ReadServerPresets()
	if err != nil {
		t.Fatalf("ReadServerPresets error: %v", err)
	}
	if len(presets) < 3 {
		t.Errorf("expected at least 3 default server presets, got %d", len(presets))
	}

	// Cannot delete built-in preset
	_, err = DeleteServerPreset("Balanced")
	if err == nil {
		t.Errorf("expected error deleting built-in preset")
	}
}

func TestClaudeConfigOperations(t *testing.T) {
	tmpDir := t.TempDir()
	claudeDir := filepath.Join(tmpDir, ".claude")
	_ = os.MkdirAll(claudeDir, 0755)
	settingsPath := filepath.Join(claudeDir, "settings.json")
	t.Setenv("CLAUDE_CONFIG_PATH", settingsPath)

	// Update config
	_, err := UpdateClaudeConfig(map[string]any{
		"env": map[string]any{
			"ANTHROPIC_BASE_URL": "http://localhost:8080",
			"CUSTOM_VAR":         "value123",
		},
	})
	if err != nil {
		t.Fatalf("UpdateClaudeConfig error: %v", err)
	}

	read, err := ReadClaudeConfig()
	if err != nil {
		t.Fatalf("ReadClaudeConfig error: %v", err)
	}
	env, ok := read["env"].(map[string]any)
	if !ok || env["ANTHROPIC_BASE_URL"] != "http://localhost:8080" {
		t.Errorf("expected ANTHROPIC_BASE_URL http://localhost:8080")
	}

	// Restore config
	restored, err := RestoreClaudeConfig()
	if err != nil {
		t.Fatalf("RestoreClaudeConfig error: %v", err)
	}
	restoredEnv, _ := restored["env"].(map[string]any)
	if _, exists := restoredEnv["ANTHROPIC_BASE_URL"]; exists {
		t.Errorf("expected ANTHROPIC_BASE_URL to be removed")
	}
	if restoredEnv["CUSTOM_VAR"] != "value123" {
		t.Errorf("expected CUSTOM_VAR to be preserved")
	}
}

func TestCustomEndpointsConfig(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	updates := map[string]any{
		"customEndpoints": map[string]any{
			"claude-3-opus-20240229": map[string]any{
				"url":    "http://localhost:8080/mock",
				"apiKey": "secret-key-123",
			},
		},
	}

	saved, err := Save(updates)
	if err != nil {
		t.Fatalf("Save error: %v", err)
	}

	ep, ok := saved.CustomEndpoints["claude-3-opus-20240229"]
	if !ok {
		t.Fatalf("expected custom endpoint for claude-3-opus-20240229")
	}
	if ep.URL != "http://localhost:8080/mock" {
		t.Errorf("expected URL http://localhost:8080/mock, got %s", ep.URL)
	}
	if ep.APIKey != "secret-key-123" {
		t.Errorf("expected APIKey secret-key-123, got %s", ep.APIKey)
	}

	pub := GetPublicConfig()
	ceMap, ok := pub["customEndpoints"].(map[string]any)
	if !ok {
		t.Fatalf("expected customEndpoints map in GetPublicConfig")
	}
	opusMap, ok := ceMap["claude-3-opus-20240229"].(map[string]any)
	if !ok {
		t.Fatalf("expected opus map in public config")
	}
	if _, exists := opusMap["apiKey"]; exists {
		t.Errorf("apiKey secret should be redacted from public config")
	}
	if opusMap["hasApiKey"] != true {
		t.Errorf("expected hasApiKey = true in public config")
	}
	if opusMap["url"] != "http://localhost:8080/mock" {
		t.Errorf("expected url preserved in public config")
	}

	// Verify saving redacted public config back (as Web UI does) preserves secret API key
	saved2, err := Save(pub)
	if err != nil {
		t.Fatalf("Save error on public config: %v", err)
	}
	ep2, ok := saved2.CustomEndpoints["claude-3-opus-20240229"]
	if !ok {
		t.Fatalf("expected custom endpoint to remain after public config save")
	}
	if ep2.APIKey != "secret-key-123" {
		t.Errorf("expected secret APIKey secret-key-123 to be preserved when saving public config, got %s", ep2.APIKey)
	}
}

func TestGetConfigDirEnvOverrides(t *testing.T) {
	customDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", customDir)
	if got := GetConfigDir(); got != customDir {
		t.Errorf("expected %s, got %s", customDir, got)
	}

	t.Setenv("ANTIGRAVITY_CONFIG_DIR", "")
	t.Setenv("CONFIG_DIR", customDir)
	if got := GetConfigDir(); got != customDir {
		t.Errorf("expected %s from CONFIG_DIR, got %s", customDir, got)
	}
}

func TestOpenRouterConfig(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	cfg := DefaultConfig()
	if cfg.OpenRouter.BaseURL != "https://openrouter.ai/api" {
		t.Errorf("expected default BaseURL https://openrouter.ai/api, got %s", cfg.OpenRouter.BaseURL)
	}
	if cfg.OpenRouter.Enabled {
		t.Errorf("expected OpenRouter to be disabled by default")
	}

	updates := map[string]any{
		"openrouter": map[string]any{
			"enabled": true,
			"apiKey":  "sk-or-v1-secretkey123",
			"baseUrl": "https://openrouter.ai/api",
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
	}

	saved, err := Save(updates)
	if err != nil {
		t.Fatalf("Save error: %v", err)
	}
	if !saved.OpenRouter.Enabled {
		t.Errorf("expected OpenRouter.Enabled to be true")
	}
	if saved.OpenRouter.APIKey != "sk-or-v1-secretkey123" {
		t.Errorf("expected APIKey sk-or-v1-secretkey123, got %s", saved.OpenRouter.APIKey)
	}
	if len(saved.OpenRouter.Allowlist) != 1 {
		t.Fatalf("expected 1 allowlist item, got %d", len(saved.OpenRouter.Allowlist))
	}
	if saved.OpenRouter.Allowlist[0].Alias != "claude-3-7-openrouter" {
		t.Errorf("expected alias claude-3-7-openrouter, got %s", saved.OpenRouter.Allowlist[0].Alias)
	}

	pub := GetPublicConfig()
	orPub, ok := pub["openrouter"].(map[string]any)
	if !ok {
		t.Fatalf("expected openrouter in public config")
	}
	if _, exists := orPub["apiKey"]; exists {
		t.Errorf("apiKey secret should be redacted from public config")
	}
	if orPub["hasApiKey"] != true {
		t.Errorf("expected hasApiKey = true in public config")
	}

	// Verify saving redacted public config back preserves secret API key
	saved2, err := Save(pub)
	if err != nil {
		t.Fatalf("Save error on public config: %v", err)
	}
	if saved2.OpenRouter.APIKey != "sk-or-v1-secretkey123" {
		t.Errorf("expected secret APIKey to be preserved when saving public config, got %s", saved2.OpenRouter.APIKey)
	}

	// Verify explicitly clearing API key (apiKey: "", hasApiKey: false) works
	clearUpdates := map[string]any{
		"openrouter": map[string]any{
			"apiKey":    "",
			"hasApiKey": false,
		},
	}
	saved3, err := Save(clearUpdates)
	if err != nil {
		t.Fatalf("Save error clearing apiKey: %v", err)
	}
	if saved3.OpenRouter.APIKey != "" {
		t.Errorf("expected apiKey to be cleared, got %s", saved3.OpenRouter.APIKey)
	}
}

func TestModelsSettingsPersistenceAcrossReboots(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	// Step 1: Save model mappings, custom endpoints, and openrouter allowlist (from /#settings/models)
	initialUpdates := map[string]any{
		"modelMapping": map[string]any{
			"claude-opus-4-6": map[string]any{
				"mapping": "claude-3-opus-20240229",
				"pinned":  true,
			},
			"custom-alias-1": "gemini-3.7-flash-high",
		},
		"customEndpoints": map[string]any{
			"custom-target-model": map[string]any{
				"url":    "https://api.custom.com/v1",
				"apiKey": "custom-secret-key",
			},
		},
		"openrouter": map[string]any{
			"enabled": true,
			"apiKey":  "sk-or-secret-key-999",
			"baseUrl": "https://openrouter.ai/api",
			"allowlist": []map[string]any{
				{
					"id":              "anthropic/claude-3.7-sonnet",
					"alias":           "claude-3-7-openrouter",
					"displayName":     "Claude 3.7 Sonnet (OpenRouter)",
					"contextLength":   200000,
					"maxOutputTokens": 128000,
					"enabled":         true,
				},
			},
		},
	}

	if _, err := Save(initialUpdates); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// Step 2: Simulate reboot / rebuild by calling Load()
	loadedCfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed after restart: %v", err)
	}

	// Verify modelMapping
	if len(loadedCfg.ModelMapping) != 2 {
		t.Fatalf("expected 2 model mappings, got %d", len(loadedCfg.ModelMapping))
	}
	opusMap, ok := loadedCfg.ModelMapping["claude-opus-4-6"].(map[string]any)
	if !ok || opusMap["mapping"] != "claude-3-opus-20240229" || opusMap["pinned"] != true {
		t.Errorf("expected claude-opus-4-6 mapping object, got %#v", loadedCfg.ModelMapping["claude-opus-4-6"])
	}
	if loadedCfg.ModelMapping["custom-alias-1"] != "gemini-3.7-flash-high" {
		t.Errorf("expected string mapping for custom-alias-1, got %#v", loadedCfg.ModelMapping["custom-alias-1"])
	}

	// Verify customEndpoints
	ep, exists := loadedCfg.CustomEndpoints["custom-target-model"]
	if !exists || ep.URL != "https://api.custom.com/v1" || ep.APIKey != "custom-secret-key" {
		t.Errorf("expected custom endpoint preserved, got %#v", ep)
	}

	// Verify openrouter
	if !loadedCfg.OpenRouter.Enabled {
		t.Errorf("expected OpenRouter.Enabled true")
	}
	if loadedCfg.OpenRouter.APIKey != "sk-or-secret-key-999" {
		t.Errorf("expected OpenRouter.APIKey sk-or-secret-key-999, got %s", loadedCfg.OpenRouter.APIKey)
	}
	if len(loadedCfg.OpenRouter.Allowlist) != 1 || loadedCfg.OpenRouter.Allowlist[0].Alias != "claude-3-7-openrouter" {
		t.Errorf("expected OpenRouter.Allowlist preserved, got %#v", loadedCfg.OpenRouter.Allowlist)
	}

	// Step 3: Partial unrelated server config update (e.g. tuning maxRetries)
	if _, err := Save(map[string]any{"maxRetries": 12, "logLevel": "debug"}); err != nil {
		t.Fatalf("partial Save failed: %v", err)
	}

	// Step 4: Simulate second reboot / rebuild
	loadedCfg2, err := Load()
	if err != nil {
		t.Fatalf("second Load failed: %v", err)
	}

	if loadedCfg2.MaxRetries != 12 || loadedCfg2.LogLevel != "debug" {
		t.Errorf("expected updated server settings, got maxRetries=%d logLevel=%s", loadedCfg2.MaxRetries, loadedCfg2.LogLevel)
	}

	// Verify model settings still intact
	if len(loadedCfg2.ModelMapping) != 2 {
		t.Errorf("expected 2 model mappings after second reboot, got %d", len(loadedCfg2.ModelMapping))
	}
	if len(loadedCfg2.CustomEndpoints) != 1 {
		t.Errorf("expected 1 custom endpoint after second reboot, got %d", len(loadedCfg2.CustomEndpoints))
	}
	if len(loadedCfg2.OpenRouter.Allowlist) != 1 {
		t.Errorf("expected 1 OpenRouter allowlist model after second reboot, got %d", len(loadedCfg2.OpenRouter.Allowlist))
	}
}

func TestRankWeightsToOpenRouter_SingleSourceDefaults(t *testing.T) {
	// Zero-value config must yield the openrouter package defaults so the
	// weight blend has exactly one source of truth.
	got := OpenRouterRoutingConfig{}.RankWeightsToOpenRouter()
	want := openrouter.DefaultRankWeights()
	if got != want {
		t.Errorf("zero config weights = %+v, want package defaults %+v", got, want)
	}

	// A partial (non-zero) config is preserved as-is: the operator chose it.
	partial := OpenRouterRoutingConfig{}
	partial.RankWeights.Availability = 0.9
	if got := partial.RankWeightsToOpenRouter(); got != (openrouter.RankWeights{Availability: 0.9}) {
		t.Errorf("partial weights = %+v, want availability-only 0.9", got)
	}
}

func TestDefaultConfig_KimiBaseURL(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Kimi.BaseURL != "https://api.kimi.com/coding" {
		t.Fatalf("default Kimi base URL = %q, want %q", cfg.Kimi.BaseURL, "https://api.kimi.com/coding")
	}
	if cfg.Kimi.Enabled {
		t.Fatal("Kimi should be disabled by default")
	}
	if len(cfg.Kimi.Allowlist) != 0 {
		t.Fatalf("Kimi allowlist should be empty by default, got %d", len(cfg.Kimi.Allowlist))
	}
}

func TestGetPublicConfig_KimiRedactsAPIKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	updates := map[string]any{
		"kimi": map[string]any{
			"enabled": true,
			"apiKey":  "sk-kimi-secret-123",
			"baseUrl": "https://api.kimi.com/coding",
			"allowlist": []map[string]any{
				{"id": "kimi-k2-thinking", "alias": "k2", "enabled": true},
			},
		},
	}
	if _, err := Save(updates); err != nil {
		t.Fatalf("Save: %v", err)
	}

	pub := GetPublicConfig()
	kimi, ok := pub["kimi"].(map[string]any)
	if !ok {
		t.Fatalf("public config missing kimi map: %v", pub["kimi"])
	}
	if _, leaked := kimi["apiKey"]; leaked {
		t.Fatalf("apiKey must be redacted in public config, got %v", kimi["apiKey"])
	}
	if has, _ := kimi["hasApiKey"].(bool); !has {
		t.Fatal("public config should expose hasApiKey=true")
	}
	if kimi["baseUrl"] != "https://api.kimi.com/coding" {
		t.Fatalf("public config should preserve baseUrl, got %v", kimi["baseUrl"])
	}

	// Verify redacted public save preserves secret key
	saved2, err := Save(pub)
	if err != nil {
		t.Fatalf("Save on public config: %v", err)
	}
	if saved2.Kimi.APIKey != "sk-kimi-secret-123" {
		t.Fatalf("expected secret APIKey to be preserved, got %q", saved2.Kimi.APIKey)
	}
}

func TestDefaultConfig_HeadroomDisabledByDefault(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Headroom.Enabled {
		t.Error("headroom must default to disabled")
	}
	if cfg.Headroom.LiveTurns != 2 {
		t.Errorf("expected LiveTurns default 2, got %d", cfg.Headroom.LiveTurns)
	}
	if cfg.Headroom.CCR.MaxStoreMB != 64 || cfg.Headroom.CCR.MinChunkBytes != 2048 {
		t.Errorf("unexpected CCR defaults: %+v", cfg.Headroom.CCR)
	}
	if cfg.Headroom.OutputShaper.MechanicalThinkingBudget != 1024 {
		t.Errorf("unexpected shaper default: %+v", cfg.Headroom.OutputShaper)
	}
}

func TestSave_HeadroomRoundTrip(t *testing.T) {
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", t.TempDir())
	if _, err := Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	updated, err := Save(map[string]any{"headroom": map[string]any{
		"enabled":        true,
		"smartCrusher":   true,
		"codeCompressor": false,
	}})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !updated.Headroom.Enabled || !updated.Headroom.SmartCrusher || updated.Headroom.CodeCompressor {
		t.Errorf("unexpected persisted headroom config: %+v", updated.Headroom)
	}
}
