package claudecode

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Standard Anthropic rate-limit header keys (case-insensitive in http.Header).
const (
	HeaderRequestsLimit     = "anthropic-ratelimit-requests-limit"
	HeaderRequestsRemaining = "anthropic-ratelimit-requests-remaining"
	HeaderRequestsReset     = "anthropic-ratelimit-requests-reset"
	HeaderTokensLimit       = "anthropic-ratelimit-tokens-limit"
	HeaderTokensRemaining   = "anthropic-ratelimit-tokens-remaining"
	HeaderTokensReset       = "anthropic-ratelimit-tokens-reset"
	HeaderRetryAfter        = "retry-after"
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

	if val := h.Get(HeaderRetryAfter); val != "" {
		rl.RetryAfter = parseRetryAfter(val)
	}

	return rl
}

// parseTimestamp attempts multiple common time formats (RFC3339, RFC3339Nano, ISO8601).
func parseTimestamp(val string) time.Time {
	val = strings.TrimSpace(val)
	if val == "" {
		return time.Time{}
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
