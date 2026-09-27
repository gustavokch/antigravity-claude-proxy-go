package claudecode

import (
	"sync/atomic"
	"testing"
	"time"
)

// A retired pool must not save, even when a change that is always saved (a
// flip to rejected) arrives after retirement.
func TestAccountPool_RetireStopsSaves(t *testing.T) {
	pool, clock, saves := throttlePool(t, "oauth")
	pool.Retire()
	pool.Retire() // idempotent

	pool.RecordSuccess("cc-t", 10, 0, RateLimits{LastUpdated: *clock, Unified: unifiedFixture(*clock, "rejected", 1, 0.1)})
	pool.waitSaves()
	if got := saves.Load(); got != 0 {
		t.Fatalf("retired pool saved %d times, want 0", got)
	}
}

// Retire waits for a save already running, and drops a pass queued behind
// it, so nothing the old pool does can land on disk after Retire returns.
func TestAccountPool_RetireWaitsAndDropsPending(t *testing.T) {
	pool, clock, _ := throttlePool(t, "oauth")
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var saves atomic.Int32
	pool.persist = func() error {
		started <- struct{}{}
		<-release
		saves.Add(1)
		return nil
	}

	pool.RecordSuccess("cc-t", 10, 0, RateLimits{LastUpdated: *clock, Unified: unifiedFixture(*clock, "allowed", 0.1, 0.1)})
	<-started
	// Queue one more pass behind the running save.
	pool.RecordSuccess("cc-t", 10, 0, RateLimits{LastUpdated: *clock, Unified: unifiedFixture(*clock, "rejected", 1, 0.1)})

	done := make(chan struct{})
	go func() {
		pool.Retire()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Retire returned while a save was still running")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Retire did not return after the running save finished")
	}
	if got := saves.Load(); got != 1 {
		t.Fatalf("saves = %d, want 1 (the queued pass must be dropped)", got)
	}
}

func TestAccountPool_InheritUnified(t *testing.T) {
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	oldA := unifiedFixture(base, "allowed", 0.2, 0.1)
	oldB := unifiedFixture(base, "allowed", 0.3, 0.1)
	oldC := unifiedFixture(base, "allowed", 0.4, 0.1)
	newC := unifiedFixture(base.Add(time.Minute), "allowed", 0.5, 0.1)

	old := NewAccountPool([]AccountConfig{
		{ID: "a", Token: "t", Enabled: true},
		{ID: "b", Token: "t", Enabled: true},
		{ID: "c", Token: "t", Enabled: true},
	})
	for id, u := range map[string]*Unified{"a": oldA, "b": oldB, "c": oldC} {
		acc, _ := old.GetAccount(id)
		acc.RateLimits.Unified = u
	}

	next := NewAccountPool([]AccountConfig{
		{ID: "a", Token: "t2", Enabled: true},
		{ID: "c", Token: "t", Enabled: true},
		{ID: "d", Token: "t", Enabled: true},
	})
	accC, _ := next.GetAccount("c")
	accC.RateLimits.Unified = newC

	next.InheritUnified(old)
	next.InheritUnified(nil)

	accA, _ := next.GetAccount("a")
	if accA.RateLimits.Unified != oldA {
		t.Errorf("a: unified not inherited by pointer")
	}
	if !accA.RateLimits.LastUpdated.Equal(base) {
		t.Errorf("a: LastUpdated = %v, want %v", accA.RateLimits.LastUpdated, base)
	}
	if accC.RateLimits.Unified != newC {
		t.Errorf("c: existing unified data was overwritten")
	}
	if accD, _ := next.GetAccount("d"); accD.RateLimits.Unified != nil {
		t.Errorf("d: got unified data it never had")
	}
	if _, ok := next.GetAccount("b"); ok {
		t.Errorf("b: inheritance must not add accounts")
	}
	if u := *oldA.FiveHour.Utilization; u != 0.2 {
		t.Errorf("old snapshot mutated: utilization %v", u)
	}
}
