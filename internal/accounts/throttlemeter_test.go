package accounts

import (
	"sync"
	"testing"
	"time"
)

// fakeClock returns a clock function whose value the test advances by hand, so
// the sixty-second window can be crossed without sleeping.
func fakeClock(start time.Time) (func() time.Time, func(time.Duration)) {
	var mu sync.Mutex
	current := start
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	advance := func(delta time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		current = current.Add(delta)
	}
	return now, advance
}

func TestBeginCountsInFlightAndPriorMinute(t *testing.T) {
	now, _ := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)

	inFlight, priorMinute := meter.Begin("a@example.com")
	if inFlight != 1 || priorMinute != 1 {
		t.Fatalf("first Begin: got inFlight=%d priorMinute=%d, want 1 and 1", inFlight, priorMinute)
	}

	inFlight, priorMinute = meter.Begin("a@example.com")
	if inFlight != 2 || priorMinute != 2 {
		t.Fatalf("second Begin: got inFlight=%d priorMinute=%d, want 2 and 2", inFlight, priorMinute)
	}
}

func TestEndDecrementsInFlightButNotPriorMinute(t *testing.T) {
	now, _ := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)

	meter.Begin("a@example.com")
	meter.Begin("a@example.com")
	meter.End("a@example.com")

	inFlight, priorMinute := meter.Observe("a@example.com")
	if inFlight != 1 {
		t.Fatalf("inFlight after End: got %d, want 1", inFlight)
	}
	if priorMinute != 2 {
		t.Fatalf("priorMinute after End: got %d, want 2 — a completed request was still sent", priorMinute)
	}
}

func TestEndDoesNotGoNegative(t *testing.T) {
	now, _ := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)

	meter.End("a@example.com")

	inFlight, _ := meter.Observe("a@example.com")
	if inFlight != 0 {
		t.Fatalf("inFlight after unmatched End: got %d, want 0", inFlight)
	}
}

func TestPriorMinuteDropsSendsOlderThanTheWindow(t *testing.T) {
	now, advance := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)

	meter.Begin("a@example.com")
	meter.End("a@example.com")
	advance(61 * time.Second)

	_, priorMinute := meter.Begin("a@example.com")
	if priorMinute != 1 {
		t.Fatalf("priorMinute after the window passed: got %d, want 1", priorMinute)
	}
}

func TestPriorMinuteKeepsSendsInsideTheWindow(t *testing.T) {
	now, advance := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)

	meter.Begin("a@example.com")
	meter.End("a@example.com")
	advance(59 * time.Second)

	_, priorMinute := meter.Begin("a@example.com")
	if priorMinute != 2 {
		t.Fatalf("priorMinute inside the window: got %d, want 2", priorMinute)
	}
}

func TestAccountsAreTrackedIndependently(t *testing.T) {
	now, _ := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)

	meter.Begin("a@example.com")
	meter.Begin("a@example.com")
	meter.Begin("b@example.com")

	inFlight, _ := meter.Observe("b@example.com")
	if inFlight != 1 {
		t.Fatalf("second account inFlight: got %d, want 1", inFlight)
	}
}

func TestTakeRecoveryOnlyFiresOncePerRejection(t *testing.T) {
	now, _ := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)

	if meter.TakeRecovery("a@example.com") {
		t.Fatal("TakeRecovery before any rejection: got true, want false")
	}

	meter.MarkRejected("a@example.com")
	if !meter.TakeRecovery("a@example.com") {
		t.Fatal("first TakeRecovery after a rejection: got false, want true")
	}
	if meter.TakeRecovery("a@example.com") {
		t.Fatal("second TakeRecovery after one rejection: got true, want false")
	}
}

func TestRecoveryIsPerAccount(t *testing.T) {
	now, _ := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)

	meter.MarkRejected("a@example.com")
	if meter.TakeRecovery("b@example.com") {
		t.Fatal("TakeRecovery for an unrejected account: got true, want false")
	}
}

func TestNilClockDefaultsToTimeNow(t *testing.T) {
	meter := newThrottleMeter(nil)
	if _, priorMinute := meter.Begin("a@example.com"); priorMinute != 1 {
		t.Fatalf("Begin with a nil clock: got priorMinute=%d, want 1", priorMinute)
	}
}

func TestMeterIsSafeUnderConcurrentUse(t *testing.T) {
	now, _ := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)

	var group sync.WaitGroup
	for range 50 {
		group.Add(1)
		go func() {
			defer group.Done()
			meter.Begin("a@example.com")
			meter.Observe("a@example.com")
			meter.End("a@example.com")
		}()
	}
	group.Wait()

	inFlight, priorMinute := meter.Observe("a@example.com")
	if inFlight != 0 {
		t.Fatalf("inFlight after balanced concurrent use: got %d, want 0", inFlight)
	}
	if priorMinute != 50 {
		t.Fatalf("priorMinute after 50 sends: got %d, want 50", priorMinute)
	}
}

func TestObserveRejectionCountsTheRejectedRequestItself(t *testing.T) {
	now, _ := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)

	// The rejection is handled after the request returned, so the meter has
	// already released it. A zero here would be read as a missing field and the
	// whole record dropped from the sample.
	inFlight, priorMinute := meter.ObserveRejection("a@example.com")

	if inFlight != 1 {
		t.Fatalf("inFlight: got %d, want 1 — the rejected request was itself in flight", inFlight)
	}
	if priorMinute != 1 {
		t.Fatalf("priorMinute: got %d, want 1 — the rejected request was itself sent", priorMinute)
	}
}

func TestObserveRejectionKeepsALiveCount(t *testing.T) {
	now, _ := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)
	meter.Begin("a@example.com")
	meter.Begin("a@example.com")
	meter.Begin("a@example.com")

	inFlight, priorMinute := meter.ObserveRejection("a@example.com")

	if inFlight != 3 {
		t.Fatalf("inFlight: got %d, want 3", inFlight)
	}
	if priorMinute != 3 {
		t.Fatalf("priorMinute: got %d, want 3", priorMinute)
	}
}
