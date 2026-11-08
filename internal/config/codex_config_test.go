package config

import (
	"testing"

	"antigravity-go-proxy/internal/codex"
)

// The public view of a Codex account carries no credential: the access token
// becomes hasToken and maskedToken, the refresh token hasRefreshToken, and the
// id token is dropped. Posting that view straight back must keep all three, or
// the account needs a fresh login.
func TestCodexAccountRoundTripKeepsCredentials(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)

	updates := map[string]any{
		"codex": map[string]any{
			"enabled": true,
			"accounts": []any{
				map[string]any{
					"id":           "codex-a@b.test",
					"email":        "a@b.test",
					"accountId":    "acct-1",
					"token":        "codex-access-token-secret",
					"refreshToken": "codex-refresh-token-secret",
					"idToken":      "codex-id-token-secret",
					"enabled":      true,
				},
			},
		},
	}
	if _, err := Save(updates); err != nil {
		t.Fatalf("first Save: %v", err)
	}

	section, ok := GetPublicConfig()["codex"].(map[string]any)
	if !ok {
		t.Fatal("codex missing from the public config")
	}
	accounts, _ := section["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("public accounts = %v", section["accounts"])
	}
	acc, _ := accounts[0].(map[string]any)
	for _, secret := range []string{"token", "refreshToken", "idToken"} {
		if _, present := acc[secret]; present {
			t.Errorf("public view still carries %s: %v", secret, acc)
		}
	}
	if acc["hasToken"] != true || acc["hasRefreshToken"] != true {
		t.Errorf("public view missing the redaction markers: %v", acc)
	}
	if acc["maskedToken"] != "codex-...cret" {
		t.Errorf("maskedToken = %v, want codex-...cret", acc["maskedToken"])
	}

	if _, err := Save(GetPublicConfig()); err != nil {
		t.Fatalf("Save of the public view: %v", err)
	}
	saved := Get().Codex
	if len(saved.Accounts) != 1 {
		t.Fatalf("accounts after the round-trip = %d, want 1", len(saved.Accounts))
	}
	got := saved.Accounts[0]
	if got.Token != "codex-access-token-secret" || got.RefreshToken != "codex-refresh-token-secret" || got.IDToken != "codex-id-token-secret" {
		t.Errorf("credentials after the round-trip = %+v, want all three preserved", got)
	}
}

// A settings-only save names no accounts, so the persisted ones must survive it.
func TestCodexSettingsSaveKeepsAccounts(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)

	if _, err := Save(map[string]any{"codex": map[string]any{
		"accounts": []any{map[string]any{"id": "codex-a", "token": "t", "refreshToken": "r", "enabled": true}},
	}}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	if _, err := Save(map[string]any{"codex": map[string]any{"enabled": true, "baseUrl": "https://example.test/codex"}}); err != nil {
		t.Fatalf("settings Save: %v", err)
	}
	got := Get().Codex
	if !got.Enabled || got.BaseURL != "https://example.test/codex" {
		t.Errorf("settings not applied: %+v", got)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].RefreshToken != "r" {
		t.Errorf("accounts after a settings save = %+v, want the seeded account", got.Accounts)
	}
}

func TestDefaultConfigCodex(t *testing.T) {
	got := DefaultConfig().Codex
	if got.Enabled {
		t.Error("codex gateway must be off by default")
	}
	if got.BaseURL != codex.DefaultBaseURL {
		t.Errorf("BaseURL = %q, want %q", got.BaseURL, codex.DefaultBaseURL)
	}
	if got.Accounts == nil || len(got.Accounts) != 0 {
		t.Errorf("Accounts = %v, want an empty list", got.Accounts)
	}
	if len(got.Allowlist) != len(codex.DefaultAllowlist()) {
		t.Errorf("Allowlist has %d entries, want the %d defaults", len(got.Allowlist), len(codex.DefaultAllowlist()))
	}
}
