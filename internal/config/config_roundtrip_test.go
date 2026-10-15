package config

import "testing"

// GetPublicConfig replaces account credentials with hasToken / hasRefreshToken
// markers. Posting that public view straight back must not erase the values: an
// OAuth refresh token is not re-derivable, so a dropped one means the account
// needs a fresh login.
func TestClaudeCodeAccountRoundTripKeepsCredentials(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)

	updates := map[string]any{
		"claudecode": map[string]any{
			"enabled": true,
			"accounts": []any{
				map[string]any{
					"email":        "a@b.test",
					"type":         "oauth",
					"token":        "access-token-secret",
					"refreshToken": "refresh-token-secret",
				},
			},
		},
	}
	if _, err := Save(updates); err != nil {
		t.Fatalf("first Save: %v", err)
	}

	acc := publicAccount(t, 0)
	if _, present := acc["token"]; present {
		t.Errorf("public view still carries the token: %v", acc)
	}
	if _, present := acc["refreshToken"]; present {
		t.Errorf("public view still carries the refresh token: %v", acc)
	}
	if acc["hasToken"] != true || acc["hasRefreshToken"] != true {
		t.Errorf("public view missing the redaction markers: %v", acc)
	}

	if _, err := Save(GetPublicConfig()); err != nil {
		t.Fatalf("Save of the public view: %v", err)
	}

	saved := Get()
	if len(saved.ClaudeCode.Accounts) != 1 {
		t.Fatalf("accounts after the round-trip = %d, want 1", len(saved.ClaudeCode.Accounts))
	}
	got := saved.ClaudeCode.Accounts[0]
	if got.Token != "access-token-secret" {
		t.Errorf("token after the round-trip = %q, want it preserved", got.Token)
	}
	if got.RefreshToken != "refresh-token-secret" {
		t.Errorf("refreshToken after the round-trip = %q, want it preserved", got.RefreshToken)
	}
}

// Two accounts written without any id key used to match each other on two empty
// ids, so each could borrow the other's credential. Matching falls back to email.
func TestClaudeCodeAccountRoundTripKeepsAccountsApart(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)

	updates := map[string]any{
		"claudecode": map[string]any{
			"enabled": true,
			"accounts": []any{
				map[string]any{"email": "one@b.test", "type": "oauth", "token": "tok-one", "refreshToken": "rt-one"},
				map[string]any{"email": "two@b.test", "type": "oauth", "token": "tok-two", "refreshToken": "rt-two"},
			},
		},
	}
	if _, err := Save(updates); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if _, err := Save(GetPublicConfig()); err != nil {
		t.Fatalf("Save of the public view: %v", err)
	}

	saved := Get()
	if len(saved.ClaudeCode.Accounts) != 2 {
		t.Fatalf("accounts after the round-trip = %d, want 2", len(saved.ClaudeCode.Accounts))
	}
	want := map[string][2]string{
		"one@b.test": {"tok-one", "rt-one"},
		"two@b.test": {"tok-two", "rt-two"},
	}
	for _, acc := range saved.ClaudeCode.Accounts {
		expected, ok := want[acc.Email]
		if !ok {
			t.Errorf("unexpected account %q after the round-trip", acc.Email)
			continue
		}
		if acc.Token != expected[0] {
			t.Errorf("%s token = %q, want %q", acc.Email, acc.Token, expected[0])
		}
		if acc.RefreshToken != expected[1] {
			t.Errorf("%s refreshToken = %q, want %q", acc.Email, acc.RefreshToken, expected[1])
		}
	}
}

func publicAccount(t *testing.T, index int) map[string]any {
	t.Helper()
	cc, ok := GetPublicConfig()["claudecode"].(map[string]any)
	if !ok {
		t.Fatal("claudecode missing from the public config")
	}
	accounts, ok := cc["accounts"].([]any)
	if !ok || index >= len(accounts) {
		t.Fatalf("public accounts = %v", cc["accounts"])
	}
	acc, ok := accounts[index].(map[string]any)
	if !ok {
		t.Fatalf("public account entry = %T", accounts[index])
	}
	return acc
}
