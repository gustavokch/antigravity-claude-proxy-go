package claudecode

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestExtractRateLimits(t *testing.T) {
	h := make(http.Header)
	h.Set(HeaderRequestsLimit, "100")
	h.Set(HeaderRequestsRemaining, "95")
	h.Set(HeaderRequestsReset, "2026-08-27T12:30:00Z")
	h.Set(HeaderTokensLimit, "400000")
	h.Set(HeaderTokensRemaining, "350000")
	h.Set(HeaderTokensReset, "2026-08-27T12:35:00.123Z")
	h.Set(HeaderRetryAfter, "15")

	rl := ExtractRateLimits(h)

	if rl.RequestsLimit != 100 {
		t.Errorf("expected RequestsLimit 100, got %d", rl.RequestsLimit)
	}
	if rl.RequestsRemaining != 95 {
		t.Errorf("expected RequestsRemaining 95, got %d", rl.RequestsRemaining)
	}
	if expected := time.Date(2026, 8, 27, 12, 30, 0, 0, time.UTC); !rl.RequestsReset.Equal(expected) {
		t.Errorf("expected RequestsReset %v, got %v", expected, rl.RequestsReset)
	}
	if rl.TokensLimit != 400000 {
		t.Errorf("expected TokensLimit 400000, got %d", rl.TokensLimit)
	}
	if rl.TokensRemaining != 350000 {
		t.Errorf("expected TokensRemaining 350000, got %d", rl.TokensRemaining)
	}
	if rl.RetryAfter != 15 {
		t.Errorf("expected RetryAfter 15, got %d", rl.RetryAfter)
	}
	if rl.LastUpdated.IsZero() {
		t.Errorf("expected non-zero LastUpdated")
	}
}

func TestExtractRateLimits_MalformedAndEmpty(t *testing.T) {
	h := make(http.Header)
	h.Set(HeaderRequestsLimit, "invalid")
	h.Set(HeaderRequestsReset, "not-a-date")
	h.Set(HeaderRetryAfter, "-5")

	rl := ExtractRateLimits(h)
	if rl.RequestsLimit != 0 {
		t.Errorf("expected 0 for malformed RequestsLimit, got %d", rl.RequestsLimit)
	}
	if !rl.RequestsReset.IsZero() {
		t.Errorf("expected zero time for malformed RequestsReset, got %v", rl.RequestsReset)
	}
	if rl.RetryAfter != 0 {
		t.Errorf("expected 0 for negative/malformed RetryAfter, got %d", rl.RetryAfter)
	}
}

func TestParseRetryAfter_HTTPDate(t *testing.T) {
	future := time.Now().Add(10 * time.Second).UTC().Format(time.RFC1123)
	sec := parseRetryAfter(future)
	if sec < 9 || sec > 11 {
		t.Errorf("expected approx 10s from HTTP Date, got %d", sec)
	}

	past := time.Now().Add(-10 * time.Second).UTC().Format(time.RFC1123)
	secPast := parseRetryAfter(past)
	if secPast != 0 {
		t.Errorf("expected 0 for past HTTP Date, got %d", secPast)
	}
}

func TestExtractRateLimits_FloatingPointSeconds(t *testing.T) {
	h := make(http.Header)
	h.Set(HeaderRetryAfter, "1.5")
	h.Set(HeaderRequestsReset, "2.5")

	rl := ExtractRateLimits(h)
	if rl.RetryAfter != 2 {
		t.Errorf("expected RetryAfter 2 (rounded up from 1.5), got %d", rl.RetryAfter)
	}
	if rl.RequestsReset.IsZero() {
		t.Errorf("expected non-zero RequestsReset parsed from relative float seconds")
	}
}

func TestExtractRateLimits_GranularTokens(t *testing.T) {
	h := make(http.Header)
	h.Set("anthropic-ratelimit-requests-limit", "1000")
	h.Set("anthropic-ratelimit-requests-remaining", "990")
	h.Set("anthropic-ratelimit-requests-reset", "2026-08-29T15:04:05Z")
	h.Set("anthropic-ratelimit-input-tokens-limit", "500000")
	h.Set("anthropic-ratelimit-input-tokens-remaining", "450000")
	h.Set("anthropic-ratelimit-input-tokens-reset", "2026-08-29T15:05:00Z")
	h.Set("anthropic-ratelimit-output-tokens-limit", "100000")
	h.Set("anthropic-ratelimit-output-tokens-remaining", "95000")
	h.Set("anthropic-ratelimit-output-tokens-reset", "2026-08-29T15:06:00Z")
	h.Set("anthropic-ratelimit-tokens-limit", "600000")
	h.Set("anthropic-ratelimit-tokens-remaining", "545000")
	h.Set("anthropic-ratelimit-tokens-reset", "2026-08-29T15:06:00Z")

	rl := ExtractRateLimits(h)

	if rl.InputTokensLimit != 500000 || rl.InputTokensRemaining != 450000 {
		t.Errorf("input tokens mismatch: limit=%d, rem=%d", rl.InputTokensLimit, rl.InputTokensRemaining)
	}
	if rl.OutputTokensLimit != 100000 || rl.OutputTokensRemaining != 95000 {
		t.Errorf("output tokens mismatch: limit=%d, rem=%d", rl.OutputTokensLimit, rl.OutputTokensRemaining)
	}
	if rl.TokensLimit != 600000 || rl.TokensRemaining != 545000 {
		t.Errorf("unified tokens mismatch: limit=%d, rem=%d", rl.TokensLimit, rl.TokensRemaining)
	}
	if rl.IsRateLimited(time.Now()) {
		t.Errorf("expected not rate limited")
	}
}

func TestRateLimits_IsRateLimited(t *testing.T) {
	now := time.Now()
	rl := RateLimits{
		RequestsLimit:     100,
		RequestsRemaining: 0,
		RequestsReset:     now.Add(30 * time.Second),
	}
	if !rl.IsRateLimited(now) {
		t.Errorf("expected IsRateLimited=true when RequestsRemaining is 0 and Reset in future")
	}

	rl2 := RateLimits{
		InputTokensLimit:     500,
		InputTokensRemaining: 0,
		InputTokensReset:     now.Add(30 * time.Second),
	}
	if !rl2.IsRateLimited(now) {
		t.Errorf("expected IsRateLimited=true when InputTokensRemaining is 0 and Reset in future")
	}

	// RetryAfter test
	rl3 := RateLimits{
		RetryAfter:  45,
		LastUpdated: now.Add(-10 * time.Second),
	}
	if !rl3.IsRateLimited(now) {
		t.Errorf("expected IsRateLimited=true when RetryAfter is active (45s - 10s = 35s remaining)")
	}

	rl4 := RateLimits{
		RetryAfter:  30,
		LastUpdated: now.Add(-40 * time.Second),
	}
	if rl4.IsRateLimited(now) {
		t.Errorf("expected IsRateLimited=false when RetryAfter has expired")
	}

	// Granular output tokens rate limit
	rl5 := RateLimits{
		OutputTokensLimit:     100,
		OutputTokensRemaining: 0,
		OutputTokensReset:     now.Add(30 * time.Second),
	}
	if !rl5.IsRateLimited(now) {
		t.Errorf("expected IsRateLimited=true when OutputTokensRemaining is 0 and Reset in future")
	}
}

func TestRateLimits_MinRemainingFraction(t *testing.T) {
	// No limits set
	empty := RateLimits{}
	if frac, ok := empty.MinRemainingFraction(); ok || frac != 1.0 {
		t.Errorf("expected (1.0, false), got (%f, %v)", frac, ok)
	}

	// Requests limit lower
	rl1 := RateLimits{
		RequestsLimit:     100,
		RequestsRemaining: 20, // 0.2
		TokensLimit:       1000,
		TokensRemaining:   500, // 0.5
	}
	if frac, ok := rl1.MinRemainingFraction(); !ok || frac != 0.2 {
		t.Errorf("expected (0.2, true), got (%f, %v)", frac, ok)
	}

	// Granular input token limit lowest
	rl2 := RateLimits{
		RequestsLimit:         100,
		RequestsRemaining:     90, // 0.9
		InputTokensLimit:      1000,
		InputTokensRemaining:  50, // 0.05
		OutputTokensLimit:     500,
		OutputTokensRemaining: 400, // 0.8
	}
	if frac, ok := rl2.MinRemainingFraction(); !ok || frac != 0.05 {
		t.Errorf("expected (0.05, true), got (%f, %v)", frac, ok)
	}
}

func TestExtractRateLimits_EmptyHeadersHaveNoSignal(t *testing.T) {
	rl := ExtractRateLimits(make(http.Header))
	if rl.HasLimits() {
		t.Errorf("expected HasLimits=false for empty headers, got %+v", rl)
	}
	if !rl.LastUpdated.IsZero() {
		t.Errorf("expected zero LastUpdated for empty headers, got %v", rl.LastUpdated)
	}
	if frac, ok := rl.MinRemainingFraction(); ok || frac != 1.0 {
		t.Errorf("expected (1.0, false), got (%f, %v)", frac, ok)
	}
}

func TestExtractRateLimits_PartialHeadersCountAsSignal(t *testing.T) {
	h := make(http.Header)
	h.Set(HeaderRequestsLimit, "100")
	h.Set(HeaderRequestsRemaining, "90")
	rl := ExtractRateLimits(h)
	if !rl.HasLimits() {
		t.Errorf("expected HasLimits=true, got %+v", rl)
	}
	if rl.LastUpdated.IsZero() {
		t.Errorf("expected non-zero LastUpdated when limits parsed")
	}
}

func TestRateLimits_HasLimits(t *testing.T) {
	if (RateLimits{}).HasLimits() {
		t.Errorf("zero value must have no limits")
	}
	if !(RateLimits{OutputTokensLimit: 10}).HasLimits() {
		t.Errorf("single dimension must count")
	}
}

func TestRateLimits_ResetTime(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	// Case 1: Active RetryAfter takes precedence
	rlRetry := RateLimits{
		RetryAfter:    30,
		LastUpdated:   now,
		RequestsReset: now.Add(10 * time.Second),
	}
	expectedRetry := now.Add(30 * time.Second)
	if got := rlRetry.ResetTime(now); !got.Equal(expectedRetry) {
		t.Errorf("expected ResetTime %v from RetryAfter, got %v", expectedRetry, got)
	}

	// Case 2: Exhausted granular dimension (Remaining == 0) selected over non-exhausted dimension
	inputReset := now.Add(45 * time.Second)
	reqReset := now.Add(10 * time.Second)
	rlExhausted := RateLimits{
		RequestsLimit:        100,
		RequestsRemaining:    50,
		RequestsReset:        reqReset,
		InputTokensLimit:     1000,
		InputTokensRemaining: 0,
		InputTokensReset:     inputReset,
	}
	if got := rlExhausted.ResetTime(now); !got.Equal(inputReset) {
		t.Errorf("expected exhausted InputTokensReset %v, got %v", inputReset, got)
	}

	// Case 3: Multiple active dimensions - fallback chain selects latest active reset
	outReset := now.Add(25 * time.Second)
	rlActive := RateLimits{
		TokensLimit:           500,
		TokensRemaining:       200,
		TokensReset:           reqReset,
		OutputTokensLimit:     100,
		OutputTokensRemaining: 80,
		OutputTokensReset:     outReset,
	}
	if got := rlActive.ResetTime(now); !got.Equal(outReset) {
		t.Errorf("expected latest active reset %v, got %v", outReset, got)
	}
}

// unifiedHeadersRun1 and unifiedHeadersRun2 are the response headers of the
// first record carrying unified headers (line 3) in
// .reference/claude-code-headers-20260923.jsonl and
// .reference/claude-code-headers-20260923-run2.jsonl, in capture order. They
// are duplicated here so the test does not depend on the capture files.
var unifiedHeadersRun1 = [][2]string{
	{"anthropic-ratelimit-unified-5h-status", "allowed"},
	{"anthropic-ratelimit-unified-representative-claim", "five_hour"},
	{"anthropic-ratelimit-unified-overage-status", "rejected"},
	{"anthropic-ratelimit-unified-reset", "1790165400"},
	{"anthropic-ratelimit-unified-5h-utilization", "0.04"},
	{"anthropic-ratelimit-unified-7d-reset", "1790154000"},
	{"anthropic-ratelimit-unified-5h-reset", "1790165400"},
	{"anthropic-ratelimit-unified-7d-status", "allowed"},
	{"anthropic-ratelimit-unified-fallback-percentage", "0.5"},
	{"anthropic-ratelimit-unified-overage-disabled-reason", "org_level_disabled"},
	{"anthropic-ratelimit-unified-7d-utilization", "0.22"},
	{"anthropic-ratelimit-unified-status", "allowed"},
}

var unifiedHeadersRun2 = [][2]string{
	{"anthropic-ratelimit-unified-5h-status", "allowed"},
	{"anthropic-ratelimit-unified-representative-claim", "five_hour"},
	{"anthropic-ratelimit-unified-overage-status", "rejected"},
	{"anthropic-ratelimit-unified-reset", "1790202000"},
	{"anthropic-ratelimit-unified-5h-utilization", "0.12"},
	{"anthropic-ratelimit-unified-7d-reset", "1790758800"},
	{"anthropic-ratelimit-unified-5h-reset", "1790202000"},
	{"anthropic-ratelimit-unified-7d-status", "allowed"},
	{"anthropic-ratelimit-unified-fallback-percentage", "0.5"},
	{"anthropic-ratelimit-unified-overage-disabled-reason", "org_level_disabled"},
	{"anthropic-ratelimit-unified-7d-utilization", "0.02"},
	{"anthropic-ratelimit-unified-status", "allowed"},
}

func headerFromPairs(pairs [][2]string) http.Header {
	h := make(http.Header)
	for _, p := range pairs {
		h.Add(p[0], p[1])
	}
	return h
}

func TestExtractRateLimits_UnifiedCaptures(t *testing.T) {
	cases := []struct {
		name               string
		pairs              [][2]string
		util5h, util7d     float64
		reset5h, reset7d   time.Time
		unifiedReset       time.Time
		observed           time.Time // capture time, used as "now"
		wantFracAtObserved float64
	}{
		{
			name:               "run1",
			pairs:              unifiedHeadersRun1,
			util5h:             0.04,
			util7d:             0.22,
			reset5h:            time.Date(2026, 9, 23, 12, 10, 0, 0, time.UTC),
			reset7d:            time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC),
			unifiedReset:       time.Date(2026, 9, 23, 12, 10, 0, 0, time.UTC),
			observed:           time.Date(2026, 9, 23, 8, 48, 42, 0, time.UTC),
			wantFracAtObserved: 1 - 0.22,
		},
		{
			name:               "run2",
			pairs:              unifiedHeadersRun2,
			util5h:             0.12,
			util7d:             0.02,
			reset5h:            time.Date(2026, 9, 23, 22, 20, 0, 0, time.UTC),
			reset7d:            time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC),
			unifiedReset:       time.Date(2026, 9, 23, 22, 20, 0, 0, time.UTC),
			observed:           time.Date(2026, 9, 23, 18, 0, 0, 0, time.UTC),
			wantFracAtObserved: 1 - 0.12,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rl := ExtractRateLimits(headerFromPairs(tc.pairs))
			u := rl.Unified
			if u == nil {
				t.Fatalf("expected Unified to be parsed, got nil")
			}
			if !rl.HasLimits() {
				t.Errorf("expected HasLimits=true with unified headers")
			}
			if rl.LastUpdated.IsZero() || u.ObservedAt.IsZero() {
				t.Errorf("expected non-zero LastUpdated and ObservedAt, got %v / %v", rl.LastUpdated, u.ObservedAt)
			}
			if u.Status != "allowed" || u.RepresentativeClaim != "five_hour" {
				t.Errorf("unexpected status/claim %q/%q", u.Status, u.RepresentativeClaim)
			}
			if u.OverageStatus != "rejected" || u.OverageDisabledReason != "org_level_disabled" {
				t.Errorf("unexpected overage %q/%q", u.OverageStatus, u.OverageDisabledReason)
			}
			if u.FallbackPercentage == nil || *u.FallbackPercentage != 0.5 {
				t.Errorf("expected FallbackPercentage 0.5, got %v", u.FallbackPercentage)
			}
			if !u.Reset.Equal(tc.unifiedReset) {
				t.Errorf("expected Reset %v, got %v", tc.unifiedReset, u.Reset)
			}
			if u.FiveHour.Utilization == nil || *u.FiveHour.Utilization != tc.util5h {
				t.Errorf("expected 5h utilization %v, got %v", tc.util5h, u.FiveHour.Utilization)
			}
			if !u.FiveHour.Reset.Equal(tc.reset5h) || u.FiveHour.Status != "allowed" {
				t.Errorf("unexpected 5h window %+v", u.FiveHour)
			}
			if u.SevenDay.Utilization == nil || *u.SevenDay.Utilization != tc.util7d {
				t.Errorf("expected 7d utilization %v, got %v", tc.util7d, u.SevenDay.Utilization)
			}
			if !u.SevenDay.Reset.Equal(tc.reset7d) || u.SevenDay.Status != "allowed" {
				t.Errorf("unexpected 7d window %+v", u.SevenDay)
			}
			if frac, ok := rl.MinRemainingFractionAt(tc.observed); !ok || !approxEqual(frac, tc.wantFracAtObserved) {
				t.Errorf("expected (%v, true), got (%v, %v)", tc.wantFracAtObserved, frac, ok)
			}
			if rl.IsRateLimited(tc.observed) {
				t.Errorf("allowed status must not be rate limited")
			}
		})
	}
}

func approxEqual(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}

func TestExtractRateLimits_UnifiedCaseInsensitive(t *testing.T) {
	// A header map built without canonicalisation must still be read.
	h := http.Header{
		"anthropic-ratelimit-unified-5h-utilization": {"0.30"},
		"ANTHROPIC-RATELIMIT-UNIFIED-5H-RESET":       {"1790165400"},
		"Anthropic-Ratelimit-Unified-Status":         {"allowed"},
	}
	rl := ExtractRateLimits(h)
	if rl.Unified == nil {
		t.Fatalf("expected Unified to be parsed")
	}
	if rl.Unified.FiveHour.Utilization == nil || *rl.Unified.FiveHour.Utilization != 0.30 {
		t.Errorf("expected 5h utilization 0.30, got %v", rl.Unified.FiveHour.Utilization)
	}
	if !rl.Unified.FiveHour.Reset.Equal(time.Unix(1790165400, 0)) {
		t.Errorf("unexpected 5h reset %v", rl.Unified.FiveHour.Reset)
	}
	if rl.Unified.Status != "allowed" {
		t.Errorf("expected status allowed, got %q", rl.Unified.Status)
	}
}

func TestExtractRateLimits_UnifiedMalformedIgnored(t *testing.T) {
	h := make(http.Header)
	h.Set("anthropic-ratelimit-unified-5h-utilization", "lots")
	h.Set("anthropic-ratelimit-unified-5h-reset", "2026-09-23T12:10:00Z")
	h.Set("anthropic-ratelimit-unified-7d-utilization", "NaN")
	h.Set("anthropic-ratelimit-unified-7d-reset", "-5")
	h.Set("anthropic-ratelimit-unified-reset", "soon")
	h.Set("anthropic-ratelimit-unified-fallback-percentage", "half")
	h.Set("anthropic-ratelimit-unified-some-future-header", "whatever")

	rl := ExtractRateLimits(h)
	if rl.Unified != nil {
		t.Errorf("expected no unified data from malformed values, got %+v", *rl.Unified)
	}
	if rl.HasLimits() || !rl.LastUpdated.IsZero() {
		t.Errorf("malformed unified headers must carry no signal, got %+v", rl)
	}

	// A valid value alongside malformed ones is kept; the malformed ones are dropped.
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.5")
	rl = ExtractRateLimits(h)
	if rl.Unified == nil {
		t.Fatalf("expected Unified to be parsed")
	}
	u := rl.Unified
	if u.SevenDay.Utilization == nil || *u.SevenDay.Utilization != 0.5 {
		t.Errorf("expected 7d utilization 0.5, got %v", u.SevenDay.Utilization)
	}
	if u.FiveHour.Utilization != nil || !u.FiveHour.Reset.IsZero() || !u.SevenDay.Reset.IsZero() ||
		!u.Reset.IsZero() || u.FallbackPercentage != nil {
		t.Errorf("malformed values must be ignored, got %+v", *u)
	}
}

func TestRateLimits_UnifiedMinRemainingFractionFreshness(t *testing.T) {
	rl := ExtractRateLimits(headerFromPairs(unifiedHeadersRun1))
	cases := []struct {
		name   string
		now    time.Time
		want   float64
		wantOK bool
	}{
		{"both windows fresh", time.Date(2026, 9, 23, 8, 50, 0, 0, time.UTC), 1 - 0.22, true},
		{"7d window reset", time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC), 1 - 0.04, true},
		{"both windows reset", time.Date(2026, 9, 23, 12, 10, 0, 0, time.UTC), 1.0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frac, ok := rl.MinRemainingFractionAt(tc.now)
			if ok != tc.wantOK || !approxEqual(frac, tc.want) {
				t.Errorf("expected (%v, %v), got (%v, %v)", tc.want, tc.wantOK, frac, ok)
			}
		})
	}

	// A zero now falls back to ObservedAt.
	observed := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	rl.Unified.ObservedAt = observed
	if frac, ok := rl.MinRemainingFractionAt(time.Time{}); !ok || !approxEqual(frac, 1-0.04) {
		t.Errorf("expected (0.96, true) relative to ObservedAt, got (%v, %v)", frac, ok)
	}
}

func TestRateLimits_UnifiedPreferredOverClassic(t *testing.T) {
	now := time.Date(2026, 9, 23, 8, 50, 0, 0, time.UTC)
	util := 0.10
	rl := RateLimits{
		RequestsLimit:     100,
		RequestsRemaining: 5, // 0.05 classic
		Unified: &Unified{
			FiveHour:   UnifiedWindow{Utilization: &util, Reset: now.Add(time.Hour)},
			ObservedAt: now,
		},
	}
	if frac, ok := rl.MinRemainingFractionAt(now); !ok || !approxEqual(frac, 0.9) {
		t.Errorf("expected unified (0.9, true), got (%v, %v)", frac, ok)
	}
	// Once the unified window has reset, the classic dimensions apply again.
	if frac, ok := rl.MinRemainingFractionAt(now.Add(2 * time.Hour)); !ok || !approxEqual(frac, 0.05) {
		t.Errorf("expected classic (0.05, true), got (%v, %v)", frac, ok)
	}
	// Utilization above 1 clamps to zero remaining.
	over := 1.3
	rl.Unified.FiveHour.Utilization = &over
	if frac, ok := rl.MinRemainingFractionAt(now); !ok || frac != 0 {
		t.Errorf("expected (0, true), got (%v, %v)", frac, ok)
	}
}

func TestRateLimits_UnifiedRejectedIsRateLimited(t *testing.T) {
	now := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	reset5h := now.Add(2 * time.Hour)
	reset7d := now.Add(72 * time.Hour)
	resetUnified := now.Add(30 * time.Minute)
	cases := []struct {
		name  string
		claim string
		reset time.Time // binding reset
	}{
		{"five_hour binds 5h reset", "five_hour", reset5h},
		{"seven_day binds 7d reset", "seven_day", reset7d},
		{"unknown claim binds unified reset", "opus_weekly", resetUnified},
		{"missing claim binds unified reset", "", resetUnified},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rl := RateLimits{Unified: &Unified{
				Status:              "rejected",
				Reset:               resetUnified,
				RepresentativeClaim: tc.claim,
				FiveHour:            UnifiedWindow{Reset: reset5h, Status: "rejected"},
				SevenDay:            UnifiedWindow{Reset: reset7d},
				ObservedAt:          now,
			}}
			if got := rl.Unified.BindingReset(); !got.Equal(tc.reset) {
				t.Errorf("expected BindingReset %v, got %v", tc.reset, got)
			}
			if !rl.IsRateLimited(tc.reset.Add(-time.Second)) {
				t.Errorf("expected rate limited before the binding reset")
			}
			if rl.IsRateLimited(tc.reset) {
				t.Errorf("expected not rate limited at the binding reset")
			}
		})
	}

	// A binding window without a reset falls back to the unified reset.
	rl := RateLimits{Unified: &Unified{Status: "rejected", Reset: resetUnified, RepresentativeClaim: "seven_day"}}
	if got := rl.Unified.BindingReset(); !got.Equal(resetUnified) {
		t.Errorf("expected fallback to unified reset %v, got %v", resetUnified, got)
	}

	// Allowed status is never rate limited by unified data.
	allowed := RateLimits{Unified: &Unified{Status: "allowed", Reset: resetUnified}}
	if allowed.IsRateLimited(now) {
		t.Errorf("allowed status must not be rate limited")
	}
}

func TestRateLimits_UnifiedJSONOmittedWhenAbsent(t *testing.T) {
	b, err := json.Marshal(RateLimits{RequestsLimit: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "unified") {
		t.Errorf("classic-only RateLimits must not serialise a unified key: %s", b)
	}
}
