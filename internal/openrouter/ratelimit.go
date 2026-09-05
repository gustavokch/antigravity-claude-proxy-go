package openrouter

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Header keys for rate limits in OpenRouter and Anthropic compatibility.
const (
	HeaderORRateLimitLimitRequests     = "x-ratelimit-limit-requests"
	HeaderORRateLimitRemainingRequests = "x-ratelimit-remaining-requests"
	HeaderORRateLimitResetRequests     = "x-ratelimit-reset-requests"
	HeaderORRateLimitLimitTokens       = "x-ratelimit-limit-tokens"
	HeaderORRateLimitRemainingTokens   = "x-ratelimit-remaining-tokens"
	HeaderORRateLimitResetTokens       = "x-ratelimit-reset-tokens"
	HeaderORRateLimitReset             = "x-ratelimit-reset"
	HeaderORRetryAfter                 = "retry-after"

	// Anthropic-style aliases
	HeaderAntRateLimitLimitRequests     = "anthropic-ratelimit-requests-limit"
	HeaderAntRateLimitRemainingRequests = "anthropic-ratelimit-requests-remaining"
	HeaderAntRateLimitResetRequests     = "anthropic-ratelimit-requests-reset"
	HeaderAntRateLimitLimitTokens       = "anthropic-ratelimit-tokens-limit"
	HeaderAntRateLimitRemainingTokens   = "anthropic-ratelimit-tokens-remaining"
	HeaderAntRateLimitResetTokens       = "anthropic-ratelimit-tokens-reset"
)

// RateLimits tracks rate limits observed from OpenRouter upstream response headers.
type RateLimits struct {
	RequestsLimit     int64     `json:"requestsLimit,omitempty"`
	RequestsRemaining int64     `json:"requestsRemaining,omitempty"`
	RequestsReset     time.Time `json:"requestsReset,omitempty"`
	TokensLimit       int64     `json:"tokensLimit,omitempty"`
	TokensRemaining   int64     `json:"tokensRemaining,omitempty"`
	TokensReset       time.Time `json:"tokensReset,omitempty"`
	RetryAfter        int       `json:"retryAfter,omitempty"`
	LastUpdated       time.Time `json:"lastUpdated,omitempty"`
}

// ExtractRateLimits parses rate limit headers from OpenRouter upstream response.
func ExtractRateLimits(h http.Header) RateLimits {
	rl := RateLimits{
		LastUpdated: time.Now(),
	}
	if h == nil {
		return rl
	}

	// Requests limit
	if val := getFirstHeader(h, HeaderORRateLimitLimitRequests, HeaderAntRateLimitLimitRequests); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.RequestsLimit = n
		}
	}
	// Requests remaining
	if val := getFirstHeader(h, HeaderORRateLimitRemainingRequests, HeaderAntRateLimitRemainingRequests); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.RequestsRemaining = n
		}
	}
	// Requests reset
	if val := getFirstHeader(h, HeaderORRateLimitResetRequests, HeaderAntRateLimitResetRequests, HeaderORRateLimitReset); val != "" {
		rl.RequestsReset = parseRateLimitTimestamp(val)
	}

	// Tokens limit
	if val := getFirstHeader(h, HeaderORRateLimitLimitTokens, HeaderAntRateLimitLimitTokens); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.TokensLimit = n
		}
	}
	// Tokens remaining
	if val := getFirstHeader(h, HeaderORRateLimitRemainingTokens, HeaderAntRateLimitRemainingTokens); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.TokensRemaining = n
		}
	}
	// Tokens reset
	if val := getFirstHeader(h, HeaderORRateLimitResetTokens, HeaderAntRateLimitResetTokens); val != "" {
		rl.TokensReset = parseRateLimitTimestamp(val)
	}

	// Retry-After
	if val := h.Get(HeaderORRetryAfter); val != "" {
		rl.RetryAfter = parseRetryAfterHeader(val)
	}

	return rl
}

// IsRateLimited returns true and the remaining wait duration if any rate limit is active.
func (rl RateLimits) IsRateLimited(now time.Time) (bool, time.Duration) {
	if rl.RetryAfter > 0 && !rl.LastUpdated.IsZero() {
		expire := rl.LastUpdated.Add(time.Duration(rl.RetryAfter) * time.Second)
		if expire.After(now) {
			return true, expire.Sub(now)
		}
	}
	if rl.RequestsLimit > 0 && rl.RequestsRemaining == 0 && rl.RequestsReset.After(now) {
		return true, rl.RequestsReset.Sub(now)
	}
	if rl.TokensLimit > 0 && rl.TokensRemaining == 0 && rl.TokensReset.After(now) {
		return true, rl.TokensReset.Sub(now)
	}
	return false, 0
}

// RateLimiter manages rate limiting and pacing for OpenRouter requests.
type RateLimiter struct {
	mu            sync.RWMutex
	limits        map[string]RateLimits
	cooldowns     map[string]time.Time
	lastRequestAt map[string]time.Time
	minInterval   time.Duration
}

// DefaultRateLimiter is the shared package-level instance.
var DefaultRateLimiter = NewRateLimiter()

// NewRateLimiter creates an in-memory RateLimiter.
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{
		limits:        make(map[string]RateLimits),
		cooldowns:     make(map[string]time.Time),
		lastRequestAt: make(map[string]time.Time),
	}
}

// SetMinRequestInterval configures the minimum pacing interval between requests.
func (l *RateLimiter) SetMinRequestInterval(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.minInterval = d
}

// Reset clears all tracked rate limits and timings.
func (l *RateLimiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.limits = make(map[string]RateLimits)
	l.cooldowns = make(map[string]time.Time)
	l.lastRequestAt = make(map[string]time.Time)
}

// RecordRateLimit registers a rate-limit event for a model with an optional cooldown.
func (l *RateLimiter) RecordRateLimit(model string, rl RateLimits, wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if model == "" {
		model = "*"
	}
	l.limits[model] = rl
	if wait > 0 {
		l.cooldowns[model] = time.Now().Add(wait)
	}
}

// RecordSuccess updates model rate limits on a successful request.
func (l *RateLimiter) RecordSuccess(model string, rl RateLimits) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if model == "" {
		model = "*"
	}
	l.limits[model] = rl
	delete(l.cooldowns, model)
}

// IsModelRateLimited checks if a specific model or global OpenRouter is currently in cooldown.
func (l *RateLimiter) IsModelRateLimited(model string, now time.Time) (bool, time.Duration) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	// Check model-specific cooldown
	if cd, ok := l.cooldowns[model]; ok && cd.After(now) {
		return true, cd.Sub(now)
	}
	if cd, ok := l.cooldowns["*"]; ok && cd.After(now) {
		return true, cd.Sub(now)
	}

	// Check model-specific rate limits
	if rl, ok := l.limits[model]; ok {
		if limited, wait := rl.IsRateLimited(now); limited {
			return true, wait
		}
	}
	// Check global rate limits
	if rl, ok := l.limits["*"]; ok {
		if limited, wait := rl.IsRateLimited(now); limited {
			return true, wait
		}
	}

	return false, 0
}

// Wait paces the request according to rate-limit cooldown and MinRequestInterval.
func (l *RateLimiter) Wait(ctx context.Context, model string) error {
	for {
		now := time.Now()
		limited, wait := l.IsModelRateLimited(model, now)
		if limited && wait > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
				continue
			}
		}

		l.mu.Lock()
		minInt := l.minInterval
		var delay time.Duration
		if minInt > 0 {
			if last, ok := l.lastRequestAt[model]; ok {
				elapsed := now.Sub(last)
				if elapsed < minInt {
					delay = minInt - elapsed
				}
			}
		}
		if delay <= 0 {
			l.lastRequestAt[model] = time.Now()
			l.mu.Unlock()
			return nil
		}
		l.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

func getFirstHeader(h http.Header, keys ...string) string {
	for _, k := range keys {
		if v := h.Get(k); v != "" {
			return v
		}
	}
	return ""
}

func parseRateLimitTimestamp(val string) time.Time {
	val = strings.TrimSpace(val)
	if val == "" {
		return time.Time{}
	}
	// Try seconds or duration string (e.g. "500ms", "12.5s", "12")
	clean := strings.TrimSuffix(strings.TrimSuffix(val, "ms"), "s")
	if sec, err := strconv.ParseFloat(clean, 64); err == nil && sec >= 0 {
		if strings.HasSuffix(val, "ms") {
			return time.Now().Add(time.Duration(sec * float64(time.Millisecond)))
		}
		return time.Now().Add(time.Duration(sec * float64(time.Second)))
	}

	// Try unix timestamp in seconds
	if unixSec, err := strconv.ParseInt(val, 10, 64); err == nil && unixSec > 1500000000 {
		return time.Unix(unixSec, 0)
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05Z",
		"2006-01-02T15:04:05",
		time.RFC1123,
		time.RFC1123Z,
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, val); err == nil {
			return t
		}
	}
	return time.Time{}
}

func parseRetryAfterHeader(val string) int {
	val = strings.TrimSpace(val)
	if val == "" {
		return 0
	}
	if sec, err := strconv.Atoi(val); err == nil && sec >= 0 {
		return sec
	}
	if fSec, err := strconv.ParseFloat(val, 64); err == nil && fSec >= 0 {
		return int(fSec + 0.999)
	}
	layouts := []string{time.RFC1123, time.RFC1123Z, time.RFC850, time.ANSIC}
	for _, l := range layouts {
		if t, err := time.Parse(l, val); err == nil {
			diff := time.Until(t)
			if diff > 0 {
				return int(diff.Seconds() + 0.999)
			}
			return 0
		}
	}
	return 0
}
