package claudecode

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverLocalCredentials(t *testing.T) {
	tempDir := t.TempDir()

	// Create a mock .claude.json
	claudeJSON := `{
		"sessionKey": "sk-ant-test-session-key",
		"autoUpdaterStatus": "disabled"
	}`
	if err := os.WriteFile(filepath.Join(tempDir, ".claude.json"), []byte(claudeJSON), 0600); err != nil {
		t.Fatalf("failed to write mock .claude.json: %v", err)
	}

	// Create a mock .claude/settings.json
	if err := os.Mkdir(filepath.Join(tempDir, ".claude"), 0700); err != nil {
		t.Fatalf("failed to mkdir: %v", err)
	}
	settingsJSON := `{
		"apiKey": "sk-ant-api03-test-key-999",
		"env": {
			"ANTHROPIC_API_KEY": "sk-ant-api03-test-key-env"
		}
	}`
	if err := os.WriteFile(filepath.Join(tempDir, ".claude", "settings.json"), []byte(settingsJSON), 0600); err != nil {
		t.Fatalf("failed to write mock settings.json: %v", err)
	}

	accounts, err := DiscoverLocalCredentials(tempDir)
	if err != nil {
		t.Fatalf("DiscoverLocalCredentials failed: %v", err)
	}

	if len(accounts) < 2 {
		t.Fatalf("expected at least 2 discovered accounts, got %d", len(accounts))
	}

	foundSessionKey := false
	foundAPIKey := false
	for _, acc := range accounts {
		if acc.Token == "sk-ant-test-session-key" {
			foundSessionKey = true
			if acc.Type != "setup_token" {
				t.Errorf("expected sessionKey to have type 'setup_token', got '%s'", acc.Type)
			}
		}
		if acc.Token == "sk-ant-api03-test-key-999" {
			foundAPIKey = true
			if acc.Type != "api_key" {
				t.Errorf("expected apiKey to have type 'api_key', got '%s'", acc.Type)
			}
		}
	}

	if !foundSessionKey {
		t.Errorf("expected to find sk-ant-test-session-key")
	}
	if !foundAPIKey {
		t.Errorf("expected to find sk-ant-api03-test-key-999")
	}
}
