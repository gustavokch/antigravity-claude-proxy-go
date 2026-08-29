package claudecode

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Standard Anthropic rate-limit header keys (case-insensitive in http.Header).
const (
	HeaderRequestsLimit        = "anthropic-ratelimit-requests-limit"
	HeaderRequestsRemaining    = "anthropic-ratelimit-requests-remaining"
	HeaderRequestsReset        = "anthropic-ratelimit-requests-reset"
	HeaderTokensLimit          = "anthropic-ratelimit-tokens-limit"
	HeaderTokensRemaining      = "anthropic-ratelimit-tokens-remaining"
	HeaderTokensReset          = "anthropic-ratelimit-tokens-reset"
	HeaderInputTokensLimit     = "anthropic-ratelimit-input-tokens-limit"
	HeaderInputTokensRemaining = "anthropic-ratelimit-input-tokens-remaining"
	HeaderInputTokensReset     = "anthropic-ratelimit-input-tokens-reset"
	HeaderOutputTokensLimit    = "anthropic-ratelimit-output-tokens-limit"
	HeaderOutputTokensRemaining = "anthropic-ratelimit-output-tokens-remaining"
	HeaderOutputTokensReset    = "anthropic-ratelimit-output-tokens-reset"
	HeaderRetryAfter           = "retry-after"
)

// ExtractRateLimits parses standard Anthropic rate-limit headers from an HTTP response header.
func ExtractRateLimits(h http.Header) RateLimits {
	rl := RateLimits{
		LastUpdated: time.Now(),
	}

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

	return rl
}

// IsRateLimited returns true if any limit has 0 remaining and reset timestamp is in the future,
// or if RetryAfter duration is currently active.
func (rl RateLimits) IsRateLimited(now time.Time) bool {
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
