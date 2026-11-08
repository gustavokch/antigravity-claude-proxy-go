package accounts

import (
	"testing"
	"time"
)

// pickWith runs one strategy pick on a fresh three-account pool, so every
// call starts from the same rotation state and the picks are comparable.
func pickWith(t *testing.T, strategy string, exclude map[string]bool) Selection {
	t.Helper()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	manager, err := New(Options{
		Accounts: []*Account{testAccount("a@example.com"), testAccount("b@example.com"), testAccount("c@example.com")},
		Strategy: strategy,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.pickLocked("claude-sonnet-4-6", exclude)
}

func TestStrategiesSkipExcludedAccounts(t *testing.T) {
	t.Parallel()
	for _, strategy := range []string{StrategySticky, StrategyRoundRobin, StrategyHybrid} {
		t.Run(strategy, func(t *testing.T) {
			t.Parallel()
			first := pickWith(t, strategy, nil).Account
			if first == nil {
				t.Fatal("no account picked from a healthy pool")
			}
			if got := pickWith(t, strategy, map[string]bool{}).Account; got == nil || got.Email != first.Email {
				t.Fatalf("empty exclusion set picked %v, want %s", got, first.Email)
			}
			second := pickWith(t, strategy, map[string]bool{first.Email: true}).Account
			if second == nil || second.Email == first.Email {
				t.Fatalf("excluding %s picked %v, want another account", first.Email, second)
			}
			onlyC := map[string]bool{"a@example.com": true, "b@example.com": true}
			if got := pickWith(t, strategy, onlyC).Account; got == nil || got.Email != "c@example.com" {
				t.Fatalf("with a and b excluded picked %v, want c@example.com", got)
			}
			all := map[string]bool{"a@example.com": true, "b@example.com": true, "c@example.com": true}
			if got := pickWith(t, strategy, all); got != (Selection{}) {
				t.Fatalf("with every account excluded got %+v, want no account and no wait", got)
			}
		})
	}
}

// Sticky waits out a short limit on its current account. An excluded
// current account must not be waited for: the caller asked for anyone else.
func TestStickyDoesNotWaitForAnExcludedCurrentAccount(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	account := testAccount("sticky@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategySticky, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	manager.MarkRateLimited(account, "claude", 30*time.Second)
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if got := manager.pickLocked("claude", nil); got != (Selection{Wait: 30 * time.Second}) {
		t.Fatalf("plain sticky pick = %+v, want a 30s wait on the current account", got)
	}
	if got := manager.pickLocked("claude", map[string]bool{account.Email: true}); got != (Selection{}) {
		t.Fatalf("sticky pick with the current account excluded = %+v, want no account and no wait", got)
	}
}
