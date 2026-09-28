package claudecode

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Standard Anthropic rate-limit header keys (case-insensitive in http.Header).
const (
	HeaderRequestsLimit         = "anthropic-ratelimit-requests-limit"
	HeaderRequestsRemaining     = "anthropic-ratelimit-requests-remaining"
	HeaderRequestsReset         = "anthropic-ratelimit-requests-reset"
	HeaderTokensLimit           = "anthropic-ratelimit-tokens-limit"
	HeaderTokensRemaining       = "anthropic-ratelimit-tokens-remaining"
	HeaderTokensReset           = "anthropic-ratelimit-tokens-reset"
	HeaderInputTokensLimit      = "anthropic-ratelimit-input-tokens-limit"
	HeaderInputTokensRemaining  = "anthropic-ratelimit-input-tokens-remaining"
	HeaderInputTokensReset      = "anthropic-ratelimit-input-tokens-reset"
	HeaderOutputTokensLimit     = "anthropic-ratelimit-output-tokens-limit"
	HeaderOutputTokensRemaining = "anthropic-ratelimit-output-tokens-remaining"
	HeaderOutputTokensReset     = "anthropic-ratelimit-output-tokens-reset"
	HeaderRetryAfter            = "retry-after"
)

// Unified (subscription) rate-limit header keys. Resets are Unix epoch
// seconds and utilizations are 0-1 fractions, unlike the classic headers.
const (
	HeaderUnifiedPrefix                = "anthropic-ratelimit-unified-"
	HeaderUnifiedStatus                = "anthropic-ratelimit-unified-status"
	HeaderUnifiedReset                 = "anthropic-ratelimit-unified-reset"
	HeaderUnifiedRepresentativeClaim   = "anthropic-ratelimit-unified-representative-claim"
	HeaderUnifiedFallbackPercentage    = "anthropic-ratelimit-unified-fallback-percentage"
	HeaderUnifiedOverageStatus         = "anthropic-ratelimit-unified-overage-status"
	HeaderUnifiedOverageDisabledReason = "anthropic-ratelimit-unified-overage-disabled-reason"
	HeaderUnified5hStatus              = "anthropic-ratelimit-unified-5h-status"
	HeaderUnified5hReset               = "anthropic-ratelimit-unified-5h-reset"
	HeaderUnified5hUtilization         = "anthropic-ratelimit-unified-5h-utilization"
	HeaderUnified7dStatus              = "anthropic-ratelimit-unified-7d-status"
	HeaderUnified7dReset               = "anthropic-ratelimit-unified-7d-reset"
	HeaderUnified7dUtilization         = "anthropic-ratelimit-unified-7d-utilization"
)

// Representative-claim values that name a unified window.
const (
	UnifiedClaimFiveHour = "five_hour"
	UnifiedClaimSevenDay = "seven_day"
)

// ExtractRateLimits parses standard Anthropic rate-limit headers from an HTTP response header.
// LastUpdated is set only when at least one limit dimension was parsed;
// a response carrying no rate-limit headers yields a zero LastUpdated so
// callers can distinguish "no signal" from "full quota" and avoid clobbering
// the last good reading with an empty one.
func ExtractRateLimits(h http.Header) RateLimits {
	rl := RateLimits{}

	if val := h.Get(HeaderRequestsLimit); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.RequestsLimit = n
		}
	}
	if val := h.Get(HeaderRequestsRemaining); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.RequestsRemaining = n
		}
	}
	if val := h.Get(HeaderRequestsReset); val != "" {
		rl.RequestsReset = parseTimestamp(val)
	}

	if val := h.Get(HeaderTokensLimit); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.TokensLimit = n
		}
	}
	if val := h.Get(HeaderTokensRemaining); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.TokensRemaining = n
		}
	}
	if val := h.Get(HeaderTokensReset); val != "" {
		rl.TokensReset = parseTimestamp(val)
	}

	if val := h.Get(HeaderInputTokensLimit); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.InputTokensLimit = n
		}
	}
	if val := h.Get(HeaderInputTokensRemaining); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.InputTokensRemaining = n
		}
	}
	if val := h.Get(HeaderInputTokensReset); val != "" {
		rl.InputTokensReset = parseTimestamp(val)
	}

	if val := h.Get(HeaderOutputTokensLimit); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.OutputTokensLimit = n
		}
	}
	if val := h.Get(HeaderOutputTokensRemaining); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.OutputTokensRemaining = n
		}
	}
	if val := h.Get(HeaderOutputTokensReset); val != "" {
		rl.OutputTokensReset = parseTimestamp(val)
	}

	if val := h.Get(HeaderRetryAfter); val != "" {
		rl.RetryAfter = parseRetryAfter(val)
	}

	rl.Unified = extractUnified(h)

	if rl.HasLimits() || rl.RetryAfter > 0 {
		rl.LastUpdated = time.Now()
	}
	if rl.Unified != nil {
		rl.Unified.ObservedAt = rl.LastUpdated
	}

	return rl
}

// extractUnified parses the anthropic-ratelimit-unified-* headers. Names are
// matched case-insensitively even when the header map was not canonicalised,
// malformed values are skipped, and unknown unified headers are ignored.
// It returns nil when no unified value could be parsed.
func extractUnified(h http.Header) *Unified {
	var u Unified
	found := false
	for key, vals := range h {
		if len(vals) == 0 {
			continue
		}
		name := strings.ToLower(key)
		if !strings.HasPrefix(name, HeaderUnifiedPrefix) {
			continue
		}
		val := strings.TrimSpace(vals[0])
		if val == "" {
			continue
		}
		switch name {
		case HeaderUnifiedStatus:
			u.Status, found = val, true
		case HeaderUnifiedRepresentativeClaim:
			u.RepresentativeClaim, found = val, true
		case HeaderUnifiedOverageStatus:
			u.OverageStatus, found = val, true
		case HeaderUnifiedOverageDisabledReason:
			u.OverageDisabledReason, found = val, true
		case HeaderUnified5hStatus:
			u.FiveHour.Status, found = val, true
		case HeaderUnified7dStatus:
			u.SevenDay.Status, found = val, true
		case HeaderUnifiedReset:
			if t, ok := parseEpochSeconds(val); ok {
				u.Reset, found = t, true
			}
		case HeaderUnified5hReset:
			if t, ok := parseEpochSeconds(val); ok {
				u.FiveHour.Reset, found = t, true
			}
		case HeaderUnified7dReset:
			if t, ok := parseEpochSeconds(val); ok {
				u.SevenDay.Reset, found = t, true
			}
		case HeaderUnifiedFallbackPercentage:
			if f, ok := parseFraction(val); ok {
				u.FallbackPercentage, found = &f, true
			}
		case HeaderUnified5hUtilization:
			if f, ok := parseFraction(val); ok {
				u.FiveHour.Utilization, found = &f, true
			}
		case HeaderUnified7dUtilization:
			if f, ok := parseFraction(val); ok {
				u.SevenDay.Utilization, found = &f, true
			}
		}
	}
	if !found {
		return nil
	}
	return &u
}

// BindingReset returns the reset of the window the representative claim
// names, falling back to the overall unified reset when the claim names no
// known window or that window carried no reset.
func (u *Unified) BindingReset() time.Time {
	var reset time.Time
	switch u.RepresentativeClaim {
	case UnifiedClaimFiveHour:
		reset = u.FiveHour.Reset
	case UnifiedClaimSevenDay:
		reset = u.SevenDay.Reset
	}
	if reset.IsZero() {
		reset = u.Reset
	}
	return reset
}

// maxFreshUtilization returns the highest utilization among windows whose
// reset is after ref. Windows that have already reset (or carry no reset)
// describe a past period and are skipped.
func (u *Unified) maxFreshUtilization(ref time.Time) (float64, bool) {
	maxUtil, ok := 0.0, false
	for _, w := range []UnifiedWindow{u.FiveHour, u.SevenDay} {
		if w.Utilization == nil || !w.Reset.After(ref) {
			continue
		}
		if !ok || *w.Utilization > maxUtil {
			maxUtil = *w.Utilization
		}
		ok = true
	}
	return maxUtil, ok
}

// HasLimits reports whether any rate-limit dimension was parsed from headers.
// Empty responses (no limit headers, no Retry-After) carry no quota signal
// and must not overwrite the last good reading.
func (rl RateLimits) HasLimits() bool {
	return rl.RequestsLimit > 0 || rl.TokensLimit > 0 ||
		rl.InputTokensLimit > 0 || rl.OutputTokensLimit > 0 ||
		rl.Unified != nil
}

// IsRateLimited returns true if any limit has 0 remaining and reset timestamp is in the future,
// if RetryAfter duration is currently active, or if the unified status is
// "rejected" and the binding window's reset is in the future.
func (rl RateLimits) IsRateLimited(now time.Time) bool {
	if u := rl.Unified; u != nil && strings.EqualFold(u.Status, "rejected") && u.BindingReset().After(now) {
		return true
	}
	if rl.RetryAfter > 0 && !rl.LastUpdated.IsZero() && rl.LastUpdated.Add(time.Duration(rl.RetryAfter)*time.Second).After(now) {
		return true
	}
	if rl.RequestsLimit > 0 && rl.RequestsRemaining == 0 && rl.RequestsReset.After(now) {
		return true
	}
	if rl.TokensLimit > 0 && rl.TokensRemaining == 0 && rl.TokensReset.After(now) {
		return true
	}
	if rl.InputTokensLimit > 0 && rl.InputTokensRemaining == 0 && rl.InputTokensReset.After(now) {
		return true
	}
	if rl.OutputTokensLimit > 0 && rl.OutputTokensRemaining == 0 && rl.OutputTokensReset.After(now) {
		return true
	}
	return false
}

// MinRemainingFraction is MinRemainingFractionAt evaluated at time.Now().
func (rl RateLimits) MinRemainingFraction() (float64, bool) {
	return rl.MinRemainingFractionAt(time.Now())
}

// MinRemainingFractionAt computes the minimum remaining fraction (0.0 to 1.0) at now.
// Unified subscription windows whose reset is after now (after ObservedAt when now
// is zero) take precedence and yield 1 - max(5h, 7d utilization). Otherwise it is
// the minimum across the classic dimensions (requests, tokens, input tokens,
// output tokens). Returns (1.0, false) if neither applies.
func (rl RateLimits) MinRemainingFractionAt(now time.Time) (float64, bool) {
	if u := rl.Unified; u != nil {
		ref := now
		if ref.IsZero() {
			ref = u.ObservedAt
		}
		if maxUtil, ok := u.maxFreshUtilization(ref); ok {
			return math.Min(math.Max(1-maxUtil, 0), 1), true
		}
	}

	hasLimit := false
	minFrac := 1.0

	check := func(rem, lim int64) {
		if lim > 0 {
			hasLimit = true
			frac := float64(rem) / float64(lim)
			if frac < minFrac {
				minFrac = frac
			}
		}
	}

	check(rl.RequestsRemaining, rl.RequestsLimit)
	check(rl.TokensRemaining, rl.TokensLimit)
	check(rl.InputTokensRemaining, rl.InputTokensLimit)
	check(rl.OutputTokensRemaining, rl.OutputTokensLimit)

	if !hasLimit {
		return 1.0, false
	}
	if minFrac < 0 {
		minFrac = 0
	}
	if minFrac > 1.0 {
		minFrac = 1.0
	}
	return minFrac, true
}

// ResetTime determines the most relevant reset timestamp:
// 1. Active RetryAfter duration (relative to LastUpdated or now).
// 2. If any dimension is exhausted (Remaining == 0 with Limit > 0), the furthest reset time among exhausted dimensions.
// 3. Furthest reset time among all active dimensions with a future reset timestamp.
// 4. Fallback chain through configured reset timestamps.
func (rl RateLimits) ResetTime(now time.Time) time.Time {
	if rl.RetryAfter > 0 {
		ref := rl.LastUpdated
		if ref.IsZero() {
			ref = now
		}
		retryExpiry := ref.Add(time.Duration(rl.RetryAfter) * time.Second)
		if retryExpiry.After(now) {
			return retryExpiry
		}
	}

	// Check exhausted dimensions first
	var exhaustedReset time.Time
	checkExhausted := func(rem, lim int64, reset time.Time) {
		if lim > 0 && rem == 0 && reset.After(now) {
			if reset.After(exhaustedReset) {
				exhaustedReset = reset
			}
		}
	}
	checkExhausted(rl.RequestsRemaining, rl.RequestsLimit, rl.RequestsReset)
	checkExhausted(rl.TokensRemaining, rl.TokensLimit, rl.TokensReset)
	checkExhausted(rl.InputTokensRemaining, rl.InputTokensLimit, rl.InputTokensReset)
	checkExhausted(rl.OutputTokensRemaining, rl.OutputTokensLimit, rl.OutputTokensReset)

	if !exhaustedReset.IsZero() {
		return exhaustedReset
	}

	// Check any future reset timestamp among active dimensions
	var latestReset time.Time
	checkActive := func(lim int64, reset time.Time) {
		if lim > 0 && reset.After(now) {
			if reset.After(latestReset) {
				latestReset = reset
			}
		}
	}
	checkActive(rl.RequestsLimit, rl.RequestsReset)
	checkActive(rl.TokensLimit, rl.TokensReset)
	checkActive(rl.InputTokensLimit, rl.InputTokensReset)
	checkActive(rl.OutputTokensLimit, rl.OutputTokensReset)

	if !latestReset.IsZero() {
		return latestReset
	}

	// Fallback to any non-zero reset timestamp
	for _, t := range []time.Time{rl.InputTokensReset, rl.OutputTokensReset, rl.TokensReset, rl.RequestsReset} {
		if !t.IsZero() {
			return t
		}
	}

	return time.Time{}
}

// parseTimestamp attempts multiple common time formats (RFC3339, RFC3339Nano, ISO8601, or relative seconds).
func parseTimestamp(val string) time.Time {
	val = strings.TrimSpace(val)
	if val == "" {
		return time.Time{}
	}

	// Try relative seconds duration
	if sec, err := strconv.ParseFloat(val, 64); err == nil && sec >= 0 {
		return time.Now().Add(time.Duration(sec * float64(time.Second)))
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
		time.RFC1123,
		time.RFC1123Z,
	}

	for _, layout := range layouts {
		if t, err := time.Parse(layout, val); err == nil {
			return t
		}
	}
	return time.Time{}
}

// parseEpochSeconds parses a positive Unix timestamp in whole seconds.
func parseEpochSeconds(val string) (time.Time, bool) {
	sec, err := strconv.ParseInt(val, 10, 64)
	if err != nil || sec <= 0 {
		return time.Time{}, false
	}
	return time.Unix(sec, 0).UTC(), true
}

// parseFraction parses a finite, non-negative decimal such as "0.04".
func parseFraction(val string) (float64, bool) {
	f, err := strconv.ParseFloat(val, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, false
	}
	return f, true
}

// parseRetryAfter parses the Retry-After header as either seconds or an HTTP Date.
func parseRetryAfter(val string) int {
	val = strings.TrimSpace(val)
	if val == "" {
		return 0
	}

	// First try as integer seconds
	if sec, err := strconv.Atoi(val); err == nil && sec >= 0 {
		return sec
	}

	// Try as float seconds
	if fSec, err := strconv.ParseFloat(val, 64); err == nil && fSec >= 0 {
		return int(fSec + 0.999) // Round up
	}

	// Try as HTTP-Date
	layouts := []string{
		time.RFC1123,
		time.RFC1123Z,
		time.RFC850,
		time.ANSIC,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, val); err == nil {
			diff := time.Until(t)
			if diff > 0 {
				return int(diff.Seconds() + 0.999) // Round up
			}
			return 0
		}
	}

	return 0
}
