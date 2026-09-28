package claudecode

import (
	"testing"
	"time"
)

// Flush saves a snapshot change the throttle held back, and only then.
func TestAccountPool_FlushSavesThrottledChange(t *testing.T) {
	pool, clock, saves := throttlePool(t, "oauth")
	base := *clock

	if err := pool.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := saves.Load(); got != 0 {
		t.Fatalf("flush with nothing held back saved %d times, want 0", got)
	}

	pool.RecordSuccess("cc-t", 10, 0, RateLimits{LastUpdated: *clock, Unified: unifiedFixture(base, "allowed", 0.1, 0.1)})
	pool.waitSaves()
	if err := pool.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := saves.Load(); got != 1 {
		t.Fatalf("flush after a saved change: saves = %d, want 1", got)
	}

	*clock = base.Add(10 * time.Second)
	pool.RecordSuccess("cc-t", 10, 0, RateLimits{LastUpdated: *clock, Unified: unifiedFixture(base, "allowed", 0.2, 0.1)})
	pool.waitSaves()
	if got := saves.Load(); got != 1 {
		t.Fatalf("throttled change saved: saves = %d, want 1", got)
	}
	if err := pool.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := saves.Load(); got != 2 {
		t.Fatalf("flush of a throttled change: saves = %d, want 2", got)
	}
	if err := pool.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := saves.Load(); got != 2 {
		t.Fatalf("second flush saved again: saves = %d, want 2", got)
	}

	*clock = base.Add(20 * time.Second)
	pool.RecordSuccess("cc-t", 10, 0, RateLimits{LastUpdated: *clock, Unified: unifiedFixture(base, "allowed", 0.3, 0.1)})
	pool.Retire()
	if err := pool.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := saves.Load(); got != 2 {
		t.Fatalf("flush on a retired pool saved: saves = %d, want 2", got)
	}
}
