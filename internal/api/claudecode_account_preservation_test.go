package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"antigravity-go-proxy/internal/config"
)

// seedCCConfig writes a claudecode section with one fully-populated OAuth
// account (plus an extra manual account) to the test config.json and reloads
// the in-memory config. It mirrors the on-disk state left behind by
// registerAuthenticatedClaudeCodeAccount after a successful OAuth login.
func seedCCConfig(t *testing.T) {
	t.Helper()
	path, err := config.ConfigFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{
		"claudecode": map[string]any{
			"enabled": true,
			"baseUrl": "https://api.anthropic.com",
			"mode":    "pool",
			"accounts": []any{
				map[string]any{
					"id":               "cc-oauth@example.com",
					"name":             "Claude Code (oauth@example.com)",
					"token":            "sk-ant-oat01-oauth-token",
					"refreshToken":     "sk-ant-ort01-refresh-token",
					"expiresAt":        "2026-08-31T02:23:35Z",
					"email":            "oauth@example.com",
					"accountUuid":      "acc-uuid-1",
					"organizationUuid": "org-uuid-1",
					"type":             "oauth",
					"priority":         1,
					"enabled":          true,
					"source":           "oauth",
				},
				map[string]any{
					"id":       "cc-manual",
					"name":     "Manual account",
					"token":    "sk-ant-oat01-manual-token",
					"type":     "setup_token",
					"priority": 2,
					"enabled":  true,
					"source":   "manual",
				},
			},
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(); err != nil {
		t.Fatal(err)
	}
}

// readCCAccountsFromDisk returns the claudecode accounts as persisted in
// config.json — the state a proxy restart would load.
func readCCAccountsFromDisk(t *testing.T) []map[string]any {
	t.Helper()
	path, err := config.ConfigFilePath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	cc, _ := raw["claudecode"].(map[string]any)
	accs, _ := cc["accounts"].([]any)
	out := make([]map[string]any, 0, len(accs))
	for _, a := range accs {
		if m, ok := a.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func findAccount(t *testing.T, accs []map[string]any, id string) map[string]any {
	t.Helper()
	for _, a := range accs {
		if a["id"] == id {
			return a
		}
	}
	t.Fatalf("account %q not found in config.json; got %v", id, accs)
	return nil
}

func doJSON(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer test-api-key")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// A settings save (POST /api/claudecode/config) must never modify the
// accounts list. The UI round-trips a stale, token-redacted snapshot; saving
// it used to delete accounts and strip refresh tokens, which surfaced as
// lost Claude Code logins after a proxy restart.
func TestClaudeCodeConfigPost_PreservesAccounts(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	seedCCConfig(t)

	t.Run("body without accounts key", func(t *testing.T) {
		rec := doJSON(t, srv, http.MethodPost, "/api/claudecode/config", `{"enabled":true,"mode":"single"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		accs := readCCAccountsFromDisk(t)
		acc := findAccount(t, accs, "cc-oauth@example.com")
		if acc["refreshToken"] != "sk-ant-ort01-refresh-token" {
			t.Errorf("refreshToken lost on settings save, got %v", acc["refreshToken"])
		}
		if acc["token"] != "sk-ant-oat01-oauth-token" {
			t.Errorf("token lost on settings save, got %v", acc["token"])
		}
		findAccount(t, accs, "cc-manual")
	})

	t.Run("body with stale redacted accounts snapshot", func(t *testing.T) {
		stale := `{"enabled":true,"mode":"pool","accounts":[{"id":"cc-oauth@example.com","hasToken":true,"maskedToken":"sk-ant...oken","hasRefreshToken":true,"email":"oauth@example.com","enabled":true}]}`
		rec := doJSON(t, srv, http.MethodPost, "/api/claudecode/config", stale)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		accs := readCCAccountsFromDisk(t)
		acc := findAccount(t, accs, "cc-oauth@example.com")
		if acc["refreshToken"] != "sk-ant-ort01-refresh-token" {
			t.Errorf("refreshToken lost on settings save, got %v", acc["refreshToken"])
		}
		if acc["token"] != "sk-ant-oat01-oauth-token" {
			t.Errorf("token lost on settings save, got %v", acc["token"])
		}
		findAccount(t, accs, "cc-manual")
	})
}

// Mutating one account through the dedicated endpoints must preserve the
// fields of the surviving accounts (refresh token, expiry, email, UUIDs and
// source), not rebuild them as bare 6-field stubs.
func TestClaudeCodeAccountMutations_PreserveSiblingFields(t *testing.T) {
	t.Run("toggle via accounts POST", func(t *testing.T) {
		srv, _, _ := newTestServerWithManager(t)
		seedCCConfig(t)
		rec := doJSON(t, srv, http.MethodPost, "/api/claudecode/accounts", `{"id":"cc-manual","enabled":false}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		acc := findAccount(t, readCCAccountsFromDisk(t), "cc-oauth@example.com")
		if acc["refreshToken"] != "sk-ant-ort01-refresh-token" {
			t.Errorf("sibling refreshToken lost, got %v", acc["refreshToken"])
		}
		if acc["expiresAt"] != "2026-08-31T02:23:35Z" {
			t.Errorf("sibling expiresAt lost, got %v", acc["expiresAt"])
		}
		if acc["email"] != "oauth@example.com" {
			t.Errorf("sibling email lost, got %v", acc["email"])
		}
		if acc["source"] != "oauth" {
			t.Errorf("sibling source lost, got %v", acc["source"])
		}
	})

	t.Run("delete sibling", func(t *testing.T) {
		srv, _, _ := newTestServerWithManager(t)
		seedCCConfig(t)
		rec := doJSON(t, srv, http.MethodDelete, "/api/claudecode/accounts/cc-manual", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		accs := readCCAccountsFromDisk(t)
		acc := findAccount(t, accs, "cc-oauth@example.com")
		if acc["refreshToken"] != "sk-ant-ort01-refresh-token" {
			t.Errorf("surviving refreshToken lost on delete, got %v", acc["refreshToken"])
		}
		if acc["expiresAt"] != "2026-08-31T02:23:35Z" {
			t.Errorf("surviving expiresAt lost on delete, got %v", acc["expiresAt"])
		}
	})

	t.Run("auto import", func(t *testing.T) {
		srv, _, _ := newTestServerWithManager(t)
		seedCCConfig(t)
		// Seed a Claude CLI config in HOME so discovery finds a token.
		claudeJSON := filepath.Join(t.TempDir(), "unused")
		_ = claudeJSON
		home := os.Getenv("HOME")
		if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"cli@example.com"},"refreshToken":"sk-ant-ort01-cli-refresh","token":"sk-ant-oat01-cli-token"}`), 0600); err != nil {
			t.Fatal(err)
		}
		rec := doJSON(t, srv, http.MethodPost, "/api/claudecode/import", `{}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		acc := findAccount(t, readCCAccountsFromDisk(t), "cc-oauth@example.com")
		if acc["refreshToken"] != "sk-ant-ort01-refresh-token" {
			t.Errorf("sibling refreshToken lost on import, got %v", acc["refreshToken"])
		}
		if acc["expiresAt"] != "2026-08-31T02:23:35Z" {
			t.Errorf("sibling expiresAt lost on import, got %v", acc["expiresAt"])
		}
	})
}
