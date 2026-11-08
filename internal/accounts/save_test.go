package accounts

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// UpdateSubscription saves when the tier changes, and each call here sets a
// new tier. Concurrent saves must neither tear the file nor let an older
// snapshot land after a newer one: once all of them return, the file holds
// the tier the manager holds.
func TestConcurrentSavesKeepTheNewestSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	manager, err := New(Options{Accounts: []*Account{testAccount("a@example.com")}, ConfigPath: path})
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := range 200 {
		group.Go(func() {
			if err := manager.UpdateSubscription("a@example.com", Subscription{Tier: fmt.Sprintf("tier-%03d", i)}); err != nil {
				t.Errorf("UpdateSubscription: %v", err)
			}
		})
	}
	group.Wait()

	file, err := Load(path)
	if err != nil {
		t.Fatalf("load after concurrent saves: %v", err)
	}
	if len(file.Accounts) != 1 {
		t.Fatalf("saved %d accounts, want 1", len(file.Accounts))
	}
	want := manager.GetAllAccounts()[0].Subscription.Tier
	if got := file.Accounts[0].Subscription.Tier; got != want {
		t.Fatalf("saved tier %q, in-memory tier %q: an older snapshot landed last", got, want)
	}
}

// A loadCodeAssist answer that repeats the account's tier and project, as
// every new prompt's does, leaves accounts.json alone: only DetectedAt, which
// changes on every answer, would differ.
func TestUnchangedTierAndProjectDoNotRewriteTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	manager, err := New(Options{Accounts: []*Account{testAccount("a@example.com")}, ConfigPath: path})
	if err != nil {
		t.Fatal(err)
	}
	account := manager.accounts[0]
	manager.CacheProject(account, "proj-live")
	if err := manager.UpdateSubscription(account.Email, Subscription{Tier: "pro", ProjectID: "proj-live", DetectedAt: "2026-11-08T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	manager.CacheProject(account, "proj-live")
	if err := manager.UpdateSubscription(account.Email, Subscription{Tier: "pro", ProjectID: "proj-live", DetectedAt: "2026-11-08T10:05:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("an unchanged tier and project rewrote accounts.json (stat error %v)", err)
	}
	if got := manager.GetAllAccounts()[0].Subscription.DetectedAt; got != "2026-11-08T10:05:00Z" {
		t.Fatalf("DetectedAt = %q, want the newest answer's", got)
	}
}
