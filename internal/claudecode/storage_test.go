package claudecode

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStorage_SaveAndLoad(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "claudecode_accounts.json")

	now := time.Now().Truncate(time.Second)
	accounts := []AccountConfig{
		{
			ID:               "cc-test-1",
			Name:             "Test Account 1",
			Token:            "test-token-1",
			RefreshToken:     "test-refresh-1",
			ExpiresAt:        &now,
			Email:            "user1@test.com",
			AccountUUID:      "uuid-1",
			OrganizationUUID: "org-1",
			Type:             "oauth",
			Priority:         1,
			Enabled:          true,
			Source:           "oauth",
		},
		{
			ID:       "cc-test-2",
			Name:     "Test Account 2",
			Token:    "test-token-2",
			Type:     "api_key",
			Priority: 2,
			Enabled:  false,
			Source:   "manual",
		},
	}

	if err := SaveStoredAccounts(path, accounts); err != nil {
		t.Fatalf("unexpected error saving accounts: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("failed to stat saved file: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("expected file mode 0600, got %v", info.Mode().Perm())
	}

	loaded, err := LoadStoredAccounts(path)
	if err != nil {
		t.Fatalf("unexpected error loading accounts: %v", err)
	}

	if len(loaded) != len(accounts) {
		t.Fatalf("expected %d accounts, got %d", len(accounts), len(loaded))
	}

	if loaded[0].Email != "user1@test.com" || loaded[0].RefreshToken != "test-refresh-1" {
		t.Errorf("loaded account 0 mismatch: %+v", loaded[0])
	}
	if loaded[1].ID != "cc-test-2" || loaded[1].Enabled != false {
		t.Errorf("loaded account 1 mismatch: %+v", loaded[1])
	}
}

func TestAccountPool_StorageAndTokenRefresh(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "claudecode_accounts.json")

	pool := NewAccountPool(nil)
	pool.SetStoragePath(path)

	// Set a mock token refresher
	refreshCalled := false
	pool.SetTokenRefresher(func(refreshToken string) (string, string, int, error) {
		if refreshToken != "old-refresh-token" {
			t.Errorf("expected old-refresh-token, got %s", refreshToken)
		}
		refreshCalled = true
		return "new-access-token", "new-refresh-token", 3600, nil
	})

	// Add an account with near-expiry token
	expiringSoon := time.Now().Add(2 * time.Minute)
	acc := pool.AddOrUpdateAccount(AccountConfig{
		ID:           "cc-refresh-test",
		Name:         "Refresh Test",
		Token:        "old-access-token",
		RefreshToken: "old-refresh-token",
		ExpiresAt:    &expiringSoon,
		Email:        "refresh@test.com",
		Type:         "oauth",
		Priority:     1,
		Enabled:      true,
		Source:       "oauth",
	})

	if err := pool.SaveStoredAccounts(); err != nil {
		t.Fatalf("failed to save stored accounts: %v", err)
	}

	// Trigger token refresh
	if err := pool.RefreshTokenIfNeeded(acc); err != nil {
		t.Fatalf("token refresh failed: %v", err)
	}

	if !refreshCalled {
		t.Fatal("token refresher was not called")
	}

	acc.mu.RLock()
	if acc.Token != "new-access-token" || acc.RefreshToken != "new-refresh-token" {
		t.Errorf("account tokens not updated: token=%s, refresh=%s", acc.Token, acc.RefreshToken)
	}
	acc.mu.RUnlock()

	// Verify persistence reloads the updated tokens
	newPool := NewAccountPool(nil)
	newPool.SetStoragePath(path)
	if err := newPool.LoadStoredAccounts(); err != nil {
		t.Fatalf("failed to load stored accounts in new pool: %v", err)
	}

	loadedAcc, ok := newPool.GetAccount("cc-refresh-test")
	if !ok {
		t.Fatal("expected account cc-refresh-test to exist in new pool")
	}

	loadedAcc.mu.RLock()
	if loadedAcc.Token != "new-access-token" || loadedAcc.RefreshToken != "new-refresh-token" {
		t.Errorf("reloaded tokens mismatch: token=%s, refresh=%s", loadedAcc.Token, loadedAcc.RefreshToken)
	}
	loadedAcc.mu.RUnlock()
}
