package accounts

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type ErrorReason string

const (
	ReasonRateLimit ErrorReason = "RATE_LIMIT_EXCEEDED"
	ReasonQuota     ErrorReason = "QUOTA_EXHAUSTED"
	ReasonCapacity  ErrorReason = "MODEL_CAPACITY_EXHAUSTED"
	ReasonServer    ErrorReason = "SERVER_ERROR"
	ReasonUnknown   ErrorReason = "UNKNOWN"
)

var (
	quotaDelayPattern   = regexp.MustCompile(`(?i)quotaResetDelay[:\s"]+(\d+(?:\.\d+)?)(ms|s)`)
	retrySecondPattern  = regexp.MustCompile(`(?i)retry\s+(?:after\s+)?(\d+)\s*(?:sec|s\b)`)
	durationPattern     = regexp.MustCompile(`(?i)(?:(\d+)h)?(?:(\d+)m)?(\d+)s`)
	verificationPattern = regexp.MustCompile(`https://accounts\.google\.com/signin/continue\?[^\s"\\]+`)
)

func ParseResetTime(header http.Header, body string, now time.Time) time.Duration {
	if value := header.Get("Retry-After"); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil {
			return sanitizeReset(time.Duration(seconds) * time.Second)
		}
		if date, err := http.ParseTime(value); err == nil && date.After(now) {
			return sanitizeReset(date.Sub(now))
		}
	}
	if value := header.Get("x-ratelimit-reset"); value != "" {
		if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
			return sanitizeReset(time.Unix(seconds, 0).Sub(now))
		}
	}
	if value := header.Get("x-ratelimit-reset-after"); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil {
			return sanitizeReset(time.Duration(seconds) * time.Second)
		}
	}
	if match := quotaDelayPattern.FindStringSubmatch(body); match != nil {
		value, _ := strconv.ParseFloat(match[1], 64)
		if strings.EqualFold(match[2], "s") {
			value *= 1000
		}
		return sanitizeReset(time.Duration(value) * time.Millisecond)
	}
	if match := retrySecondPattern.FindStringSubmatch(body); match != nil {
		seconds, _ := strconv.Atoi(match[1])
		return sanitizeReset(time.Duration(seconds) * time.Second)
	}
	if match := durationPattern.FindStringSubmatch(body); match != nil {
		hours, _ := strconv.Atoi(match[1])
		minutes, _ := strconv.Atoi(match[2])
		seconds, _ := strconv.Atoi(match[3])
		return sanitizeReset(time.Duration(hours)*time.Hour + time.Duration(minutes)*time.Minute + time.Duration(seconds)*time.Second)
	}
	return 0
}

func ClassifyError(body string, status int) ErrorReason {
	if status == 529 || status == http.StatusServiceUnavailable {
		return ReasonCapacity
	}
	if status == http.StatusInternalServerError {
		return ReasonServer
	}
	lower := strings.ToLower(body)
	// Bare RESOURCE_EXHAUSTED / "check quota" is Google's RPM/capacity 429.
	// Daily quota actually carries quotaResetDelay / daily-limit language.
	// Treating the bare status as quota invented 5–120 minute locks while agy
	// still showed remainingFraction ≈ 1.
	if containsAny(lower, "quotaresetdelay", "quotaresettimestamp", "daily limit", "quota exceeded", "quota_exhausted") {
		return ReasonQuota
	}
	if IsCapacityExhausted(body) {
		return ReasonCapacity
	}
	if status == http.StatusTooManyRequests || containsAny(lower, "rate_limit_exceeded", "rate limit", "too many requests", "throttl", "resource_exhausted") {
		return ReasonRateLimit
	}
	if containsAny(lower, "internal server error", "server error", "503", "502", "504") {
		return ReasonServer
	}
	return ReasonUnknown
}

func IsPermanentAuthFailure(body string) bool {
	lower := strings.ToLower(body)
	return containsAny(lower, "invalid_grant", "token revoked", "token has been expired or revoked", "token_revoked", "invalid_client", "credentials are invalid")
}

func IsValidationRequired(body string) bool {
	lower := strings.ToLower(body)
	return containsAny(lower, "validation_required", "account_disabled", "user_disabled")
}

func IsAccountBanned(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "has been disabled") && strings.Contains(lower, "violation of terms of service")
}

func IsCapacityExhausted(body string) bool {
	lower := strings.ToLower(body)
	return containsAny(lower, "model_capacity_exhausted", "capacity_exhausted", "model is currently overloaded", "service temporarily unavailable")
}

func ExtractVerificationURL(body string) string {
	var response struct {
		Error struct {
			Details []struct {
				Metadata map[string]any `json:"metadata"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &response) == nil {
		for _, detail := range response.Error.Details {
			if value, ok := detail.Metadata["validation_url"].(string); ok && value != "" {
				return value
			}
		}
	}
	match := verificationPattern.FindString(body)
	return strings.TrimRight(match, ",.)}>]")
}

// rateLimitTiers escalates the guessed cooldown when Google returns a 429
// carrying no Retry-After and no quotaResetDelay. The old flat 30 s produced
// roughly 38 doomed upstream calls per account across the 19-minute wave of
// 2026-09-17, and on a sliding-window throttle every rejected call can extend
// the window. The first tier stays above 10 s deliberately: the dispatcher
// fast-retries the same account when the computed wait is <= 10 s.
var rateLimitTiers = []time.Duration{
	30 * time.Second,
	time.Minute,
	2 * time.Minute,
	5 * time.Minute,
	10 * time.Minute,
}

// jitterFraction is how much Decorrelate may add, as a fraction of the wait.
const jitterFraction = 0.25

// Decorrelate spreads a cooldown by adding 0–25% of it. Two accounts rejected
// in the same second are otherwise marked for the same duration and wake in
// lockstep, re-entering the same throttle together. Jitter is additive only —
// subtracting could retry before a server-specified reset. A nil source
// returns the input unchanged so tests stay deterministic.
func Decorrelate(wait time.Duration, random func() float64) time.Duration {
	if wait <= 0 || random == nil {
		return wait
	}
	return wait + time.Duration(float64(wait)*jitterFraction*random())
}

func SmartBackoff(reason ErrorReason, serverReset time.Duration, failures int) time.Duration {
	if serverReset > 0 {
		return max(serverReset, 2*time.Second)
	}
	switch reason {
	case ReasonQuota:
		tiers := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}
		return tiers[min(failures, len(tiers)-1)]
	case ReasonRateLimit:
		return rateLimitTiers[min(failures, len(rateLimitTiers)-1)]
	case ReasonCapacity:
		return 15 * time.Second
	case ReasonServer:
		return 20 * time.Second
	default:
		return time.Minute
	}
}

func sanitizeReset(value time.Duration) time.Duration {
	if value <= 0 {
		return 500 * time.Millisecond
	}
	if value < 500*time.Millisecond {
		return value + 200*time.Millisecond
	}
	return value
}

func containsAny(value string, candidates ...string) bool {
	for _, candidate := range candidates {
		if strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

// RateLimitError reports that the pool cannot serve a model right now and
// cannot wait the throttle out inside maxWait. It carries the reset so the
// API layer can answer 429 + Retry-After: a bare 400 tells Claude Code the
// request is permanently invalid and it never retries.
type RateLimitError struct {
	Model      string
	RetryAfter time.Duration
	Shared     bool
}

func (err *RateLimitError) Error() string {
	scope := "account"
	if err.Shared {
		scope = "pool-wide"
	}
	return fmt.Sprintf("RESOURCE_EXHAUSTED: %s rate limit on %s; retry after %s",
		scope, err.Model, err.RetryAfter.Round(time.Second))
}
