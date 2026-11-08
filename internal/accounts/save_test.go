package accounts

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// Every UpdateSubscription saves. Concurrent saves must neither tear the file
// nor let an older snapshot land after a newer one: once all of them return,
// the file holds the tier the manager holds.
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
