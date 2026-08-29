package claudecode

import (
	"errors"
	"testing"
	"time"
)

func TestAccountPool_CRUD(t *testing.T) {
	pool := NewAccountPool(nil)

	acc1 := pool.AddOrUpdateAccount(AccountConfig{
		ID:       "acc-1",
		Name:     "Main Account",
		Token:    "sk-ant-test-1",
		Type:     "api_key",
		Priority: 1,
		Enabled:  true,
		Source:   "manual",
	})

	if acc1 == nil || acc1.ID != "acc-1" {
		t.Fatalf("expected account acc-1 created")
	}

	got, found := pool.GetAccount("acc-1")
	if !found || got.Name != "Main Account" {
		t.Fatalf("expected to get acc-1")
	}

	snaps := pool.Snapshots()
	if len(snaps) != 1 || snaps[0].ID != "acc-1" {
		t.Fatalf("expected 1 snapshot, got %d", len(snaps))
	}

	// Update account
	pool.AddOrUpdateAccount(AccountConfig{
		ID:       "acc-1",
		Name:     "Updated Main",
		Token:    "sk-ant-test-1-updated",
		Type:     "api_key",
		Priority: 2,
		Enabled:  false,
	})

	gotUpdated, _ := pool.GetAccount("acc-1")
	if gotUpdated.Name != "Updated Main" || gotUpdated.Enabled != false || gotUpdated.Priority != 2 {
		t.Errorf("account update did not apply correctly")
	}

	// Delete
	if !pool.DeleteAccount("acc-1") {
		t.Errorf("expected DeleteAccount to return true")
	}
	if _, found := pool.GetAccount("acc-1"); found {
		t.Errorf("expected acc-1 to be deleted")
	}
}

func TestAccountPool_SelectAccount_PriorityAndSticky(t *testing.T) {
	pool := NewAccountPool([]AccountConfig{
		{
			ID:       "acc-p2",
			Name:     "Low Priority",
			Token:    "token-2",
			Priority: 2,
			Enabled:  true,
		},
		{
			ID:       "acc-p1",
			Name:     "High Priority",
			Token:    "token-1",
			Priority: 1,
			Enabled:  true,
		},
	})

	// First selection should pick priority 1 (acc-p1)
	selected, err := pool.SelectAccount("session-123", nil)
	if err != nil {
		t.Fatalf("SelectAccount failed: %v", err)
	}
	if selected.ID != "acc-p1" {
		t.Errorf("expected acc-p1 to be selected, got %s", selected.ID)
	}

	// Next selection with same session should stick to acc-p1
	selected2, err := pool.SelectAccount("session-123", nil)
	if err != nil {
		t.Fatalf("SelectAccount 2 failed: %v", err)
	}
	if selected2.ID != "acc-p1" {
		t.Errorf("expected sticky session to return acc-p1, got %s", selected2.ID)
	}

	// If acc-p1 is excluded (e.g. failed in this request), should fall back to acc-p2
	selected3, err := pool.SelectAccount("session-123", map[string]bool{"acc-p1": true})
	if err != nil {
		t.Fatalf("SelectAccount with exclusion failed: %v", err)
	}
	if selected3.ID != "acc-p2" {
		t.Errorf("expected fallback to acc-p2, got %s", selected3.ID)
	}
}

func TestAccountPool_RateLimitAndCooldown(t *testing.T) {
	pool := NewAccountPool([]AccountConfig{
		{
			ID:       "acc-1",
			Name:     "Account 1",
			Token:    "token-1",
			Priority: 1,
			Enabled:  true,
		},
	})

	// Record rate limit
	rl := RateLimits{
		RequestsRemaining: 0,
		RequestsReset:     time.Now().Add(5 * time.Second),
		LastUpdated:       time.Now(),
	}
	pool.RecordRateLimit("acc-1", rl, 5*time.Second)

	// Should not be selectable
	_, err := pool.SelectAccount("", nil)
	if err != ErrNoAvailableAccounts {
		t.Errorf("expected ErrNoAvailableAccounts when in cooldown, got %v", err)
	}

	// Verify Snapshot status
	snaps := pool.Snapshots()
	if len(snaps) != 1 || snaps[0].Status != "cooldown" {
		t.Errorf("expected status 'cooldown', got %s", snaps[0].Status)
	}
}

func TestAccountPool_AcquireReleaseSuccess(t *testing.T) {
	pool := NewAccountPool([]AccountConfig{
		{
			ID:       "acc-1",
			Name:     "Account 1",
			Token:    "token-1",
			Priority: 1,
			Enabled:  true,
		},
	})

	pool.Acquire("acc-1")
	acc, _ := pool.GetAccount("acc-1")
	if acc.InFlight != 1 || acc.TotalRequests != 1 {
		t.Errorf("expected InFlight 1 and TotalRequests 1, got InFlight %d Total %d", acc.InFlight, acc.TotalRequests)
	}

	pool.RecordSuccess("acc-1", 1500, 0.005, RateLimits{})
	pool.Release("acc-1")

	if acc.InFlight != 0 {
		t.Errorf("expected InFlight 0 after release, got %d", acc.InFlight)
	}
	if acc.TotalTokens != 1500 || acc.TotalCost != 0.005 {
		t.Errorf("expected tokens 1500 and cost 0.005, got tokens %d cost %f", acc.TotalTokens, acc.TotalCost)
	}
}

func TestAccountPool_StickyCapacityBounds(t *testing.T) {
	pool := NewAccountPool([]AccountConfig{
		{
			ID:       "acc-1",
			Name:     "Account 1",
			Token:    "token-1",
			Priority: 1,
			Enabled:  true,
		},
	})

	// Fill sticky sessions past maxStickyEntries
	for i := 0; i < maxStickyEntries+50; i++ {
		sessionID := "session-" + string(rune('a'+(i%26))) + "-" + time.Now().Format(time.RFC3339Nano)
		_, err := pool.SelectAccount(sessionID, nil)
		if err != nil {
			t.Fatalf("unexpected select error: %v", err)
		}
	}

	pool.mu.RLock()
	stickyLen := len(pool.sticky)
	pool.mu.RUnlock()

	if stickyLen > maxStickyEntries {
		t.Errorf("expected sticky map size <= %d, got %d", maxStickyEntries, stickyLen)
	}
}

func TestAccountPool_RefreshTokenIfNeeded_AndForceRefresh(t *testing.T) {
	exp := time.Now().Add(2 * time.Minute) // Expiring in 2m (within 5m window)
	pool := NewAccountPool([]AccountConfig{
		{
			ID:           "acc-oauth",
			Name:         "OAuth Acc",
			Token:        "old-access-tok",
			RefreshToken: "valid-refresh-tok",
			ExpiresAt:    &exp,
			Type:         "oauth",
			Enabled:      true,
		},
	})

	refreshedCount := 0
	pool.SetTokenRefresher(func(refreshToken string) (string, string, int, error) {
		refreshedCount++
		if refreshedCount == 1 && refreshToken != "valid-refresh-tok" {
			t.Errorf("unexpected refresh token 1: %s", refreshToken)
		}
		if refreshedCount == 2 && refreshToken != "new-refresh-tok" {
			t.Errorf("unexpected refresh token 2: %s", refreshToken)
		}
		return "new-access-tok", "new-refresh-tok", 3600, nil
	})

	acc, _ := pool.GetAccount("acc-oauth")
	err := pool.RefreshTokenIfNeeded(acc)
	if err != nil {
		t.Fatalf("RefreshTokenIfNeeded failed: %v", err)
	}

	if refreshedCount != 1 {
		t.Errorf("expected 1 refresh, got %d", refreshedCount)
	}

	acc.mu.RLock()
	if acc.Token != "new-access-tok" || acc.RefreshToken != "new-refresh-tok" {
		t.Errorf("tokens not updated: token=%s, refresh=%s", acc.Token, acc.RefreshToken)
	}
	acc.mu.RUnlock()

	// Force refresh test
	err = pool.RefreshAccountToken("acc-oauth")
	if err != nil {
		t.Fatalf("RefreshAccountToken failed: %v", err)
	}
	if refreshedCount != 2 {
		t.Errorf("expected 2 refreshes, got %d", refreshedCount)
	}
}

func TestAccountPool_RefreshAllExpiringTokens(t *testing.T) {
	expiringSoon := time.Now().Add(10 * time.Minute)
	fresh := time.Now().Add(5 * time.Hour)

	pool := NewAccountPool([]AccountConfig{
		{
			ID:           "acc-1",
			Token:        "tok-1",
			RefreshToken: "ref-1",
			ExpiresAt:    &expiringSoon,
			Enabled:      true,
		},
		{
			ID:           "acc-2",
			Token:        "tok-2",
			RefreshToken: "ref-2",
			ExpiresAt:    &fresh,
			Enabled:      true,
		},
	})

	pool.SetTokenRefresher(func(refreshToken string) (string, string, int, error) {
		return "refreshed-" + refreshToken, "new-" + refreshToken, 7200, nil
	})

	refreshedIDs, err := pool.RefreshAllExpiringTokens(15 * time.Minute)
	if err != nil {
		t.Fatalf("RefreshAllExpiringTokens failed: %v", err)
	}

	if len(refreshedIDs) != 1 || refreshedIDs[0] != "acc-1" {
		t.Errorf("expected only acc-1 refreshed, got: %v", refreshedIDs)
	}
}

func TestAccountPool_RefreshAllExpiringTokens_Errors(t *testing.T) {
	expiringSoon := time.Now().Add(5 * time.Minute)

	pool := NewAccountPool([]AccountConfig{
		{
			ID:           "acc-fail",
			Token:        "tok-fail",
			RefreshToken: "bad-ref",
			ExpiresAt:    &expiringSoon,
			Enabled:      true,
		},
	})

	pool.SetTokenRefresher(func(refreshToken string) (string, string, int, error) {
		return "", "", 0, errors.New("upstream oauth error")
	})

	refreshedIDs, err := pool.RefreshAllExpiringTokens(15 * time.Minute)
	if err == nil {
		t.Fatalf("expected error from RefreshAllExpiringTokens when account refresh fails, got nil")
	}
	if len(refreshedIDs) != 0 {
		t.Errorf("expected 0 refreshed IDs, got %d", len(refreshedIDs))
	}
}

