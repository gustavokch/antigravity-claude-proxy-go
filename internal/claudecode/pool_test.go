package claudecode

import (
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
