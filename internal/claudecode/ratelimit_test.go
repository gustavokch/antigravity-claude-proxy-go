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
