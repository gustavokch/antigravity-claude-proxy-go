package accounts

import (
	"sync"
	"time"
)

// rateWindow is the lookback behind PriorMinuteRequests. The calibrator derives
// a per-minute ceiling, so the counter has to cover exactly a minute.
const rateWindow = time.Minute

// throttleMeter tracks, per account, how many requests are in flight and how
// many were sent inside rateWindow. The 429 journal records both numbers so a
// calibrator can recover concurrency and rate ceilings from production traffic
// instead of from a benchmark that has to throttle a real account to learn
// anything.
//
// It also remembers that an account was rejected, so the next success on that
// account can be journalled as a recovery. The gap between the two is the only
// measurement of how long the throttle actually held.
type throttleMeter struct {
	mu       sync.Mutex
	now      func() time.Time
	inFlight map[string]int
	sends    map[string][]time.Time
	rejected map[string]bool
}

func newThrottleMeter(now func() time.Time) *throttleMeter {
	if now == nil {
		now = time.Now
	}
	return &throttleMeter{
		now:      now,
		inFlight: make(map[string]int),
		sends:    make(map[string][]time.Time),
		rejected: make(map[string]bool),
	}
}

// Begin records a request leaving for account. It returns the in-flight count
// including this request and the number of sends inside rateWindow including
// this one, which are the two numbers the journal needs at rejection time.
func (meter *throttleMeter) Begin(account string) (int, int) {
	meter.mu.Lock()
	defer meter.mu.Unlock()
	now := meter.now()
	meter.inFlight[account]++
	sends := append(trimSendsBefore(meter.sends[account], now.Add(-rateWindow)), now)
	meter.sends[account] = sends
	return meter.inFlight[account], len(sends)
}

// End records a request returning for account. It leaves the send history
// alone: a completed request was still sent, and still counts against a rate.
func (meter *throttleMeter) End(account string) {
	meter.mu.Lock()
	defer meter.mu.Unlock()
	if meter.inFlight[account] > 0 {
		meter.inFlight[account]--
	}
}

// Observe reports the current counts without recording a send, trimming the
// send history to the window as it goes.
func (meter *throttleMeter) Observe(account string) (int, int) {
	meter.mu.Lock()
	defer meter.mu.Unlock()
	sends := trimSendsBefore(meter.sends[account], meter.now().Add(-rateWindow))
	meter.sends[account] = sends
	return meter.inFlight[account], len(sends)
}

// ObserveRejection reports the counts to journal against a rejection whose
// Begin values the caller no longer holds. It floors both at one: a rejection
// is handled after the request has already been released, and a zero on a
// journal line is indistinguishable from a field an older build never wrote, so
// the calibrator would drop the record entirely.
func (meter *throttleMeter) ObserveRejection(account string) (int, int) {
	inFlight, priorMinute := meter.Observe(account)
	return max(1, inFlight), max(1, priorMinute)
}

// MarkRejected notes that account was throttled, arming the next success on it
// to be journalled as a recovery.
func (meter *throttleMeter) MarkRejected(account string) {
	meter.mu.Lock()
	defer meter.mu.Unlock()
	meter.rejected[account] = true
}

// TakeRecovery reports whether this success is the first one after a rejection,
// and disarms so the successes behind it are not journalled too.
func (meter *throttleMeter) TakeRecovery(account string) bool {
	meter.mu.Lock()
	defer meter.mu.Unlock()
	if !meter.rejected[account] {
		return false
	}
	delete(meter.rejected, account)
	return true
}

// trimSendsBefore drops timestamps at or before cutoff, keeping the history
// bounded by the request rate rather than by process uptime.
func trimSendsBefore(stamps []time.Time, cutoff time.Time) []time.Time {
	index := 0
	for index < len(stamps) && !stamps[index].After(cutoff) {
		index++
	}
	return stamps[index:]
}
