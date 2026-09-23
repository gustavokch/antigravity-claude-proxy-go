package claudecode

import (
	"net/http"
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
