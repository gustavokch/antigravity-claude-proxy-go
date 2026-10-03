package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPIKeysRedactedFromPublicConfig(t *testing.T) {
	orig := Get()
	t.Cleanup(func() { SetForTest(orig) })

	cfg := DefaultConfig()
	cfg.APIKey = "legacy-secret"
	cfg.APIKeys = []APIKeyEntry{
		{ID: "gus", Label: "gus", Key: "gus-secret", Enabled: true},
		{ID: "friend", Label: "friend", Key: "friend-secret", Enabled: true},
	}
	SetForTest(cfg)

	pub := GetPublicConfig()
	if _, exists := pub["apiKey"]; exists {
		t.Errorf("legacy apiKey must not appear in public config")
	}
	if _, exists := pub["apiKeys"]; exists {
		t.Errorf("apiKeys must not appear in public config")
	}
	if pub["hasApiKey"] != true {
		t.Errorf("hasApiKey = %v, want true", pub["hasApiKey"])
	}
	if pub["hasApiKeys"] != true {
		t.Errorf("hasApiKeys = %v, want true", pub["hasApiKeys"])
	}
	raw, _ := json.Marshal(pub)
	for _, secret := range []string{"legacy-secret", "gus-secret", "friend-secret"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("public config leaks %q", secret)
		}
	}
}

func TestPublicConfigWithoutKeys(t *testing.T) {
	orig := Get()
	t.Cleanup(func() { SetForTest(orig) })
	SetForTest(DefaultConfig())

	pub := GetPublicConfig()
	if _, exists := pub["apiKey"]; exists {
		t.Errorf("apiKey must not appear in public config")
	}
	if _, exists := pub["apiKeys"]; exists {
		t.Errorf("apiKeys must not appear in public config")
	}
	if pub["hasApiKey"] != false {
		t.Errorf("hasApiKey = %v, want false", pub["hasApiKey"])
	}
	if pub["hasApiKeys"] != false {
		t.Errorf("hasApiKeys = %v, want false", pub["hasApiKeys"])
	}
}

func TestSaveDropsTopLevelHasKeyEchoes(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	if _, err := Save(map[string]any{"apiKey": "real-secret"}); err != nil {
		t.Fatalf("Save error: %v", err)
	}
	pub := GetPublicConfig()
	if _, err := Save(pub); err != nil {
		t.Fatalf("Save public config error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tmpDir, ".config", "antigravity-proxy", "config.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var stored map[string]any
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("parse stored config: %v", err)
	}
	if _, exists := stored["hasApiKey"]; exists {
		t.Errorf("hasApiKey echo must not persist to config.json")
	}
	if _, exists := stored["hasApiKeys"]; exists {
		t.Errorf("hasApiKeys echo must not persist to config.json")
	}
	if stored["apiKey"] != "real-secret" {
		t.Errorf("real apiKey must survive a public-config save-back, got %v", stored["apiKey"])
	}
}
