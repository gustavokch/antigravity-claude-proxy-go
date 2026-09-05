package openrouter

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestExtractRateLimits(t *testing.T) {
	t.Run("extracts all standard openrouter and standard ratelimit headers", func(t *testing.T) {
		h := http.Header{}
		h.Set("x-ratelimit-limit-requests", "100")
		h.Set("x-ratelimit-remaining-requests", "25")
		h.Set("x-ratelimit-reset-requests", "12.5s")
		h.Set("x-ratelimit-limit-tokens", "500000")
		h.Set("x-ratelimit-remaining-tokens", "120000")
		h.Set("x-ratelimit-reset-tokens", "30s")
		h.Set("retry-after", "15")

		rl := ExtractRateLimits(h)

		if rl.RequestsLimit != 100 {
			t.Errorf("RequestsLimit = %d, want 100", rl.RequestsLimit)
		}
		if rl.RequestsRemaining != 25 {
			t.Errorf("RequestsRemaining = %d, want 25", rl.RequestsRemaining)
		}
		if rl.TokensLimit != 500000 {
			t.Errorf("TokensLimit = %d, want 500000", rl.TokensLimit)
		}
		if rl.TokensRemaining != 120000 {
			t.Errorf("TokensRemaining = %d, want 120000", rl.TokensRemaining)
		}
		if rl.RetryAfter != 15 {
			t.Errorf("RetryAfter = %d, want 15", rl.RetryAfter)
		}
		if rl.RequestsReset.IsZero() {
			t.Errorf("RequestsReset should not be zero")
		}
		if rl.TokensReset.IsZero() {
			t.Errorf("TokensReset should not be zero")
		}
	})

	t.Run("extracts millisecond reset headers accurately", func(t *testing.T) {
		h := http.Header{}
		h.Set("x-ratelimit-reset-requests", "500ms")
		h.Set("x-ratelimit-reset-tokens", "250ms")

		rl := ExtractRateLimits(h)
		if rl.RequestsReset.IsZero() {
			t.Errorf("RequestsReset with ms duration should not be zero")
		}
		if rl.TokensReset.IsZero() {
			t.Errorf("TokensReset with ms duration should not be zero")
		}
		now := time.Now()
		if diff := rl.RequestsReset.Sub(now); diff < 200*time.Millisecond || diff > 800*time.Millisecond {
			t.Errorf("RequestsReset diff out of range: %v", diff)
		}
	})

	t.Run("parses epoch-seconds reset as absolute time", func(t *testing.T) {
		h := http.Header{}
		h.Set("x-ratelimit-reset-requests", "1757083200")

		rl := ExtractRateLimits(h)
		want := time.Unix(1757083200, 0)
		if !rl.RequestsReset.Equal(want) {
			t.Errorf("RequestsReset = %v, want epoch %v", rl.RequestsReset, want)
		}
	})

	t.Run("parses compound duration reset values", func(t *testing.T) {
		h := http.Header{}
		h.Set("x-ratelimit-reset-tokens", "6m12s")

		rl := ExtractRateLimits(h)
		if rl.TokensReset.IsZero() {
			t.Fatalf("TokensReset with compound duration should not be zero")
		}
		now := time.Now()
		if diff := rl.TokensReset.Sub(now); diff < 370*time.Second || diff > 374*time.Second {
			t.Errorf("TokensReset diff out of range: %v", diff)
		}
	})

	t.Run("extracts anthropic-style header aliases if present", func(t *testing.T) {
		h := http.Header{}
		h.Set("anthropic-ratelimit-requests-limit", "50")
		h.Set("anthropic-ratelimit-requests-remaining", "0")
		h.Set("anthropic-ratelimit-requests-reset", "2026-09-05T12:00:00Z")

		rl := ExtractRateLimits(h)
		if rl.RequestsLimit != 50 {
			t.Errorf("RequestsLimit = %d, want 50", rl.RequestsLimit)
		}
		if rl.RequestsRemaining != 0 {
			t.Errorf("RequestsRemaining = %d, want 0", rl.RequestsRemaining)
		}
	})

	t.Run("handles malformed and empty headers gracefully", func(t *testing.T) {
		h := http.Header{}
		h.Set("x-ratelimit-limit-requests", "not-a-number")
		h.Set("retry-after", "invalid-duration")

		rl := ExtractRateLimits(h)
		if rl.RequestsLimit != 0 {
			t.Errorf("RequestsLimit = %d, want 0", rl.RequestsLimit)
		}
		if rl.RetryAfter != 0 {
			t.Errorf("RetryAfter = %d, want 0", rl.RetryAfter)
		}
	})
}

func TestRateLimits_IsRateLimited(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	t.Run("requests exhausted with future reset", func(t *testing.T) {
		rl := RateLimits{
			RequestsLimit:     100,
			RequestsRemaining: 0,
			RequestsReset:     now.Add(10 * time.Second),
			LastUpdated:       now,
		}
		limited, wait := rl.IsRateLimited(now)
		if !limited {
			t.Errorf("expected IsRateLimited = true")
		}
		if wait < 9*time.Second || wait > 11*time.Second {
			t.Errorf("wait duration = %v, want ~10s", wait)
		}
	})

	t.Run("retry-after active", func(t *testing.T) {
		rl := RateLimits{
			RetryAfter:  30,
			LastUpdated: now,
		}
		limited, wait := rl.IsRateLimited(now)
		if !limited {
			t.Errorf("expected IsRateLimited = true")
		}
		if wait < 29*time.Second || wait > 31*time.Second {
			t.Errorf("wait duration = %v, want ~30s", wait)
		}
	})

	t.Run("retry-after expired", func(t *testing.T) {
		rl := RateLimits{
			RetryAfter:  10,
			LastUpdated: now.Add(-15 * time.Second),
		}
		limited, _ := rl.IsRateLimited(now)
		if limited {
			t.Errorf("expected IsRateLimited = false for expired retry-after")
		}
	})

	t.Run("healthy limits with remaining quota", func(t *testing.T) {
		rl := RateLimits{
			RequestsLimit:     100,
			RequestsRemaining: 50,
			RequestsReset:     now.Add(30 * time.Second),
			TokensLimit:       100000,
			TokensRemaining:   50000,
			TokensReset:       now.Add(30 * time.Second),
			LastUpdated:       now,
		}
		limited, _ := rl.IsRateLimited(now)
		if limited {
			t.Errorf("expected IsRateLimited = false when remaining > 0")
		}
	})
}

func TestRateLimiter_PacingAndRecording(t *testing.T) {
	limiter := NewRateLimiter()
	limiter.SetMinRequestInterval(20 * time.Millisecond)

	model := "minimax/minimax-m3"

	// Initial wait is immediate
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	if err := limiter.Wait(ctx, model); err != nil {
		t.Fatalf("first Wait failed: %v", err)
	}

	// Immediate next wait should be paced by MinRequestInterval
	if err := limiter.Wait(ctx, model); err != nil {
		t.Fatalf("second Wait failed: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 15*time.Millisecond {
		t.Errorf("expected pacing delay >= 15ms, got %v", elapsed)
	}

	// Record a rate limit
	now := time.Now()
	limiter.RecordRateLimit(model, RateLimits{
		RequestsRemaining: 0,
		RequestsReset:     now.Add(100 * time.Millisecond),
		LastUpdated:       now,
	}, 100*time.Millisecond)

	limited, wait := limiter.IsModelRateLimited(model, time.Now())
	if !limited {
		t.Errorf("expected model to be marked rate limited")
	}
	if wait <= 0 {
		t.Errorf("expected positive wait time, got %v", wait)
	}

	// Record success resets or updates rate limits
	limiter.RecordSuccess(model, RateLimits{
		RequestsLimit:     100,
		RequestsRemaining: 99,
		LastUpdated:       time.Now(),
	})

	limited, _ = limiter.IsModelRateLimited(model, time.Now())
	if limited {
		t.Errorf("expected model to not be rate limited after RecordSuccess")
	}

	// Reset should clear minInterval, cooldowns, limits, and timestamps
	limiter.Reset()
	if limiter.minInterval != 0 {
		t.Errorf("expected minInterval to be reset to 0, got %v", limiter.minInterval)
	}
}

func TestRateLimiter_GatewayWideMultiModelPacing(t *testing.T) {
	limiter := NewRateLimiter()
	limiter.SetMinRequestInterval(25 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	// Request model A
	if err := limiter.Wait(ctx, "model-a"); err != nil {
		t.Fatalf("Wait model-a failed: %v", err)
	}

	// Immediate next request for a different model (model-b) must still be paced globally
	if err := limiter.Wait(ctx, "model-b"); err != nil {
		t.Fatalf("Wait model-b failed: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 20*time.Millisecond {
		t.Errorf("expected gateway-wide pacing delay >= 20ms across models, got %v", elapsed)
	}
}
