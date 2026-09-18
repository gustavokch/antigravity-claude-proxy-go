package calibrate

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Sample sizes below which a value is reported as insufficient rather than
// emitted. A ceiling read off one or two rejections is an anecdote.
const (
	needRejects = 5
	needPairs   = 3
)

// Bounds on the emitted configuration.
const (
	minRequestDelayMs = 200
	maxBucketTokens   = 20
	maxBackoffTierMs  = 1_800_000
	firstBackoffTier  = 10_000
)

// dailyDisagreementRatio is how far the journal's window and the daily probe's
// may diverge before the report says they describe different buckets.
const dailyDisagreementRatio = 0.25

// IntResult is a derived count and the evidence behind it. OK false means the
// guard failed and Value must not be used.
type IntResult struct {
	Value int
	OK    bool
	N     int
	Need  int
}

// DurationResult is a derived duration and the evidence behind it.
type DurationResult struct {
	Value time.Duration
	OK    bool
	N     int
	Need  int
}

// Result is everything derived from one journal. RecoverDaily is filled by the
// active probe when it runs, and is zero otherwise.
type Result struct {
	ConcurrencySafe IntResult
	RPMSafe         IntResult
	Recover         DurationResult
	RecoverDaily    time.Duration
	Skipped         int
}

// Derive computes the safe ceilings from a journal. Every output carries its
// own guard; nothing is inferred from an empty sample.
func Derive(journal Journal) Result {
	result := Result{Skipped: journal.Skipped}

	var inFlights, rates []int
	for _, entry := range journal.Entries {
		if entry.Outcome != OutcomeReject {
			continue
		}
		// A zero means the field was absent on a pre-migration line, not that
		// the request rejected with nothing in flight — the rejected request
		// itself was always in flight.
		if entry.InFlight > 0 {
			inFlights = append(inFlights, entry.InFlight)
		}
		if entry.PriorMinuteRequests > 0 {
			rates = append(rates, entry.PriorMinuteRequests)
		}
	}

	result.ConcurrencySafe = oneBelowMinimum(inFlights)
	result.RPMSafe = oneBelowMinimum(rates)
	result.Recover = medianRetriedGap(journal.Entries)
	return result
}

// oneBelowMinimum takes the lowest observed rejecting value and steps one below
// it, which is the highest value never seen to reject.
func oneBelowMinimum(values []int) IntResult {
	outcome := IntResult{N: len(values), Need: needRejects}
	if len(values) < needRejects {
		return outcome
	}
	lowest := values[0]
	for _, value := range values[1:] {
		if value < lowest {
			lowest = value
		}
	}
	outcome.Value = max(1, lowest-1)
	outcome.OK = true
	return outcome
}

// medianRetriedGap pairs each reject with the next recovery on the same
// account and takes the median gap. Pairs where no retry was attempted are
// dropped: that gap measures how long the account was left alone, not how long
// the throttle held.
func medianRetriedGap(entries []Entry) DurationResult {
	ordered := make([]Entry, len(entries))
	copy(ordered, entries)
	sort.SliceStable(ordered, func(first, second int) bool {
		return ordered[first].Timestamp.Before(ordered[second].Timestamp)
	})

	type pending struct {
		at      time.Time
		retried bool
	}
	open := make(map[string]pending)
	var gaps []time.Duration

	for _, entry := range ordered {
		switch entry.Outcome {
		case OutcomeReject:
			open[entry.Account] = pending{at: entry.Timestamp, retried: entry.Failures > 0}
		case OutcomeRecover:
			start, ok := open[entry.Account]
			if !ok {
				continue
			}
			delete(open, entry.Account)
			if !start.retried {
				continue
			}
			if gap := entry.Timestamp.Sub(start.at); gap > 0 {
				gaps = append(gaps, gap)
			}
		}
	}

	outcome := DurationResult{N: len(gaps), Need: needPairs}
	if len(gaps) < needPairs {
		return outcome
	}
	sort.Slice(gaps, func(first, second int) bool { return gaps[first] < gaps[second] })
	outcome.Value = gaps[len(gaps)/2]
	outcome.OK = true
	return outcome
}

// window returns the recovery window to emit: the journal's measurement when
// its guard passed, otherwise the daily probe's value, otherwise zero.
func (result Result) window() time.Duration {
	if result.Recover.OK {
		return result.Recover.Value
	}
	return result.RecoverDaily
}

// Fragment renders the derived settings as config keys, in the shape
// config.Config declares, so the printed JSON can be pasted into config.json as
// it stands. A key whose guard failed is absent: an omitted key leaves the
// operator's current value alone, which is the safe outcome for a thin sample.
func (result Result) Fragment() map[string]any {
	fragment := map[string]any{}

	if result.RPMSafe.OK {
		fragment["requestDelayMs"] = max(minRequestDelayMs, int(math.Ceil(60000/float64(result.RPMSafe.Value))))
	}
	if result.RPMSafe.OK || result.ConcurrencySafe.OK {
		bucket := map[string]any{}
		if result.RPMSafe.OK {
			bucket["tokensPerMinute"] = result.RPMSafe.Value
		}
		if result.ConcurrencySafe.OK {
			bucket["maxTokens"] = min(maxBucketTokens, result.ConcurrencySafe.Value*3)
		}
		fragment["accountSelection"] = map[string]any{"tokenBucket": bucket}
	}
	if window := result.window(); window > 0 {
		windowMs := int(window / time.Millisecond)
		fragment["sharedThrottleWindowMs"] = windowMs
		fragment["capacityBackoffTiersMs"] = []int{
			firstBackoffTier,
			min(maxBackoffTierMs, windowMs),
			min(maxBackoffTierMs, windowMs*2),
		}
	}
	return fragment
}

// Report is the human-readable summary. It states what was derived, what was
// not, and how far short the evidence fell.
func (result Result) Report() string {
	var builder strings.Builder

	builder.WriteString("throttle calibration\n")
	if result.Skipped > 0 {
		fmt.Fprintf(&builder, "  %d journal line(s) did not parse and were skipped\n", result.Skipped)
	}

	writeInt := func(label string, value IntResult) {
		if value.OK {
			fmt.Fprintf(&builder, "  %-18s %d  (n=%d)\n", label, value.Value, value.N)
			return
		}
		fmt.Fprintf(&builder, "  %-18s insufficient data (n=%d, need %d)\n", label, value.N, value.Need)
	}
	writeInt("concurrency safe", result.ConcurrencySafe)
	writeInt("requests/min safe", result.RPMSafe)

	if result.Recover.OK {
		fmt.Fprintf(&builder, "  %-18s %s  (n=%d, median)\n", "recovery window", result.Recover.Value.Round(time.Second), result.Recover.N)
	} else {
		fmt.Fprintf(&builder, "  %-18s insufficient data (n=%d, need %d)\n", "recovery window", result.Recover.N, result.Recover.Need)
	}

	if result.RecoverDaily > 0 {
		fmt.Fprintf(&builder, "  %-18s %s  (daily endpoint)\n", "recovery window", result.RecoverDaily.Round(time.Second))
		if result.Recover.OK && disagree(result.Recover.Value, result.RecoverDaily) {
			builder.WriteString("  the journal and the daily endpoint disagree by more than 25%: they may describe different buckets\n")
		}
		if !result.Recover.OK {
			builder.WriteString("  using the daily endpoint value; it is a fallback, not a measurement of the bucket cloudcode-pa enforces\n")
		}
	}
	return builder.String()
}

// disagree reports whether two window measurements differ by more than
// dailyDisagreementRatio of the larger one.
func disagree(first, second time.Duration) bool {
	larger := math.Max(float64(first), float64(second))
	if larger == 0 {
		return false
	}
	return math.Abs(float64(first)-float64(second))/larger > dailyDisagreementRatio
}
