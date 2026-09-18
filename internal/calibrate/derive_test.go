package calibrate

import (
	"strings"
	"testing"
	"time"
)

var journalStart = time.Date(2026, 9, 17, 21, 0, 0, 0, time.UTC)

// rejects builds n reject entries on one account, each with the given counts.
func rejects(account string, n, inFlight, priorMinute int) []Entry {
	entries := make([]Entry, 0, n)
	for index := range n {
		entries = append(entries, Entry{
			Timestamp:           journalStart.Add(time.Duration(index) * time.Hour),
			Account:             account,
			Status:              429,
			Outcome:             OutcomeReject,
			InFlight:            inFlight,
			PriorMinuteRequests: priorMinute,
		})
	}
	return entries
}

// rejectRecoverPair builds one reject and the recovery that followed it after
// gap, with failures set so the pair counts as a retried gap.
func rejectRecoverPair(account string, at time.Time, gap time.Duration) []Entry {
	return []Entry{
		{Timestamp: at, Account: account, Status: 429, Outcome: OutcomeReject, InFlight: 4, PriorMinuteRequests: 40, Failures: 1},
		{Timestamp: at.Add(gap), Account: account, Status: 200, Outcome: OutcomeRecover},
	}
}

func TestConcurrencySafeIsOneBelowTheLowestRejectingInFlight(t *testing.T) {
	entries := append(rejects("a@example.com", 3, 6, 40), rejects("a@example.com", 2, 4, 40)...)

	result := Derive(Journal{Entries: entries})

	if !result.ConcurrencySafe.OK {
		t.Fatalf("guard: got not OK with n=%d", result.ConcurrencySafe.N)
	}
	if result.ConcurrencySafe.Value != 3 {
		t.Fatalf("ConcurrencySafe: got %d, want 3 — one below the lowest rejecting in-flight of 4", result.ConcurrencySafe.Value)
	}
}

func TestConcurrencySafeNeedsFiveRejects(t *testing.T) {
	result := Derive(Journal{Entries: rejects("a@example.com", 4, 4, 40)})

	if result.ConcurrencySafe.OK {
		t.Fatal("guard: got OK with only four reject records")
	}
	if result.ConcurrencySafe.N != 4 || result.ConcurrencySafe.Need != 5 {
		t.Fatalf("guard counts: got n=%d need=%d, want 4 and 5", result.ConcurrencySafe.N, result.ConcurrencySafe.Need)
	}
}

func TestConcurrencySafeIgnoresRecordsWithoutTheField(t *testing.T) {
	entries := append(rejects("a@example.com", 5, 0, 40), rejects("a@example.com", 5, 4, 40)...)

	result := Derive(Journal{Entries: entries})

	if result.ConcurrencySafe.Value != 3 {
		t.Fatalf("ConcurrencySafe: got %d, want 3 — zero means the field was absent, not that one request rejected", result.ConcurrencySafe.Value)
	}
	if result.ConcurrencySafe.N != 5 {
		t.Fatalf("guard n: got %d, want 5 — only records carrying the field count", result.ConcurrencySafe.N)
	}
}

func TestConcurrencySafeFloorsAtOne(t *testing.T) {
	result := Derive(Journal{Entries: rejects("a@example.com", 5, 1, 40)})

	if result.ConcurrencySafe.Value != 1 {
		t.Fatalf("ConcurrencySafe: got %d, want 1 — never advise zero concurrency", result.ConcurrencySafe.Value)
	}
}

func TestRPMSafeIsOneBelowTheLowestRejectingRate(t *testing.T) {
	entries := append(rejects("a@example.com", 3, 4, 60), rejects("a@example.com", 2, 4, 37)...)

	result := Derive(Journal{Entries: entries})

	if !result.RPMSafe.OK {
		t.Fatalf("guard: got not OK with n=%d", result.RPMSafe.N)
	}
	if result.RPMSafe.Value != 36 {
		t.Fatalf("RPMSafe: got %d, want 36", result.RPMSafe.Value)
	}
}

func TestRecoverIsTheMedianRetriedGap(t *testing.T) {
	var entries []Entry
	entries = append(entries, rejectRecoverPair("a@example.com", journalStart, 10*time.Minute)...)
	entries = append(entries, rejectRecoverPair("a@example.com", journalStart.Add(2*time.Hour), 20*time.Minute)...)
	entries = append(entries, rejectRecoverPair("a@example.com", journalStart.Add(4*time.Hour), 30*time.Minute)...)

	result := Derive(Journal{Entries: entries})

	if !result.Recover.OK {
		t.Fatalf("guard: got not OK with n=%d", result.Recover.N)
	}
	if result.Recover.Value != 20*time.Minute {
		t.Fatalf("Recover: got %s, want 20m", result.Recover.Value)
	}
}

func TestRecoverNeedsThreePairs(t *testing.T) {
	var entries []Entry
	entries = append(entries, rejectRecoverPair("a@example.com", journalStart, 10*time.Minute)...)
	entries = append(entries, rejectRecoverPair("a@example.com", journalStart.Add(2*time.Hour), 20*time.Minute)...)

	result := Derive(Journal{Entries: entries})

	if result.Recover.OK {
		t.Fatal("guard: got OK with only two pairs")
	}
	if result.Recover.Need != 3 {
		t.Fatalf("guard need: got %d, want 3", result.Recover.Need)
	}
}

func TestRecoverSkipsGapsWithNoRetryAttempted(t *testing.T) {
	var entries []Entry
	for index, gap := range []time.Duration{10 * time.Minute, 20 * time.Minute, 30 * time.Minute} {
		pair := rejectRecoverPair("a@example.com", journalStart.Add(time.Duration(index)*2*time.Hour), gap)
		pair[0].Failures = 0
		entries = append(entries, pair...)
	}

	result := Derive(Journal{Entries: entries})

	if result.Recover.OK {
		t.Fatal("guard: got OK from gaps where no retry was attempted — those measure operator idleness, not the throttle")
	}
}

func TestRecoverPairsPerAccount(t *testing.T) {
	entries := []Entry{
		{Timestamp: journalStart, Account: "a@example.com", Status: 429, Outcome: OutcomeReject, Failures: 1},
		{Timestamp: journalStart.Add(1 * time.Minute), Account: "b@example.com", Status: 200, Outcome: OutcomeRecover},
	}

	result := Derive(Journal{Entries: entries})

	if result.Recover.N != 0 {
		t.Fatalf("pairs: got %d, want 0 — a recovery on another account is not this account's window", result.Recover.N)
	}
}

func TestFragmentOmitsKeysWhoseGuardFailed(t *testing.T) {
	result := Derive(Journal{Entries: rejects("a@example.com", 4, 4, 40)})

	fragment := result.Fragment()

	if _, present := fragment["requestDelayMs"]; present {
		t.Fatal("fragment carried requestDelayMs despite a failed guard")
	}
	if _, present := fragment["capacityBackoffTiersMs"]; present {
		t.Fatal("fragment carried capacityBackoffTiersMs despite a failed guard")
	}
}

func TestFragmentDerivesEveryKeyFromAFullSample(t *testing.T) {
	entries := rejects("a@example.com", 5, 4, 37)
	for index, gap := range []time.Duration{20 * time.Minute, 20 * time.Minute, 20 * time.Minute} {
		entries = append(entries, rejectRecoverPair("a@example.com", journalStart.Add(time.Duration(index+10)*time.Hour), gap)...)
	}

	fragment := Derive(Journal{Entries: entries}).Fragment()

	if fragment["requestDelayMs"] != 1667 {
		t.Fatalf("requestDelayMs: got %v, want 1667 — ceil(60000/36)", fragment["requestDelayMs"])
	}
	if fragment["sharedThrottleWindowMs"] != 1_200_000 {
		t.Fatalf("sharedThrottleWindowMs: got %v, want 1200000", fragment["sharedThrottleWindowMs"])
	}
	tiers, ok := fragment["capacityBackoffTiersMs"].([]int)
	if !ok {
		t.Fatalf("capacityBackoffTiersMs: got %T, want []int", fragment["capacityBackoffTiersMs"])
	}
	want := []int{10_000, 1_200_000, 1_800_000}
	if len(tiers) != len(want) {
		t.Fatalf("tiers: got %v, want %v", tiers, want)
	}
	for index := range want {
		if tiers[index] != want[index] {
			t.Fatalf("tiers: got %v, want %v — the top tier is capped at 1800s", tiers, want)
		}
	}
	bucket, ok := fragment["accountSelection.tokenBucket"].(map[string]any)
	if !ok {
		t.Fatalf("accountSelection.tokenBucket: got %T, want map", fragment["accountSelection.tokenBucket"])
	}
	if bucket["tokensPerMinute"] != 36 {
		t.Fatalf("tokensPerMinute: got %v, want 36", bucket["tokensPerMinute"])
	}
	if bucket["maxTokens"] != 9 {
		t.Fatalf("maxTokens: got %v, want 9 — min(20, 3*3)", bucket["maxTokens"])
	}
}

func TestRequestDelayNeverDropsBelowTheFloor(t *testing.T) {
	entries := rejects("a@example.com", 5, 4, 1000)

	fragment := Derive(Journal{Entries: entries}).Fragment()

	if fragment["requestDelayMs"] != 200 {
		t.Fatalf("requestDelayMs: got %v, want the 200ms floor", fragment["requestDelayMs"])
	}
}

func TestMaxTokensIsCappedAtTwenty(t *testing.T) {
	entries := rejects("a@example.com", 5, 40, 37)

	fragment := Derive(Journal{Entries: entries}).Fragment()

	bucket := fragment["accountSelection.tokenBucket"].(map[string]any)
	if bucket["maxTokens"] != 20 {
		t.Fatalf("maxTokens: got %v, want the cap of 20", bucket["maxTokens"])
	}
}

func TestReportNamesTheMissingSampleSize(t *testing.T) {
	report := Derive(Journal{Entries: rejects("a@example.com", 4, 4, 40)}).Report()

	if !strings.Contains(report, "insufficient data (n=4, need 5)") {
		t.Fatalf("report did not state the shortfall:\n%s", report)
	}
}

func TestReportFlagsDisagreementWithTheDailyProbe(t *testing.T) {
	var entries []Entry
	for index := range 3 {
		entries = append(entries, rejectRecoverPair("a@example.com", journalStart.Add(time.Duration(index)*2*time.Hour), 20*time.Minute)...)
	}
	result := Derive(Journal{Entries: entries})
	result.RecoverDaily = 60 * time.Minute

	report := result.Report()

	if !strings.Contains(report, "disagree") {
		t.Fatalf("report did not flag a 200%% disagreement with the daily probe:\n%s", report)
	}
}

func TestRecoverFallsBackToTheDailyProbeWhenTheGuardFails(t *testing.T) {
	result := Derive(Journal{})
	result.RecoverDaily = 17 * time.Minute

	fragment := result.Fragment()

	if fragment["sharedThrottleWindowMs"] != 1_020_000 {
		t.Fatalf("sharedThrottleWindowMs: got %v, want 1020000 from the daily probe fallback", fragment["sharedThrottleWindowMs"])
	}
}
