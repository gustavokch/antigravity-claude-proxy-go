package calibrate

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

// MaxProbeRequests caps the active probe. Its only job is to draw one
// rejection carrying a delay; more requests buy no more information and cost
// the operator real capacity.
const MaxProbeRequests = 10

// ErrAccountProtected stops the probe on anything that looks like account
// jeopardy rather than an ordinary throttle. The caller exits 3.
var ErrAccountProtected = errors.New("account protection triggered")

var (
	// retryDelayPattern matches google.rpc.RetryInfo's retryDelay, which the
	// daily endpoint returns as a bare seconds string.
	retryDelayPattern = regexp.MustCompile(`"retryDelay"\s*:\s*"(\d+(?:\.\d+)?)s"`)
	// quotaResetDelayPattern matches the Go-style duration in ErrorInfo
	// metadata, e.g. "17m31.337247485s".
	quotaResetDelayPattern = regexp.MustCompile(`"quotaResetDelay"\s*:\s*"([^"]+)"`)
)

// ParseRetryDelay extracts the upstream's own statement of how long the caller
// must wait. It reports found/not-found rather than a sentinel: a bare
// cloudcode-pa rejection carries no delay at all, and treating that as a short
// delay would emit a backoff ladder far below the measured window.
func ParseRetryDelay(body string) (time.Duration, bool) {
	if match := retryDelayPattern.FindStringSubmatch(body); match != nil {
		if delay, err := time.ParseDuration(match[1] + "s"); err == nil && delay > 0 {
			return delay, true
		}
	}
	if match := quotaResetDelayPattern.FindStringSubmatch(body); match != nil {
		if delay, err := time.ParseDuration(match[1]); err == nil && delay > 0 {
			return delay, true
		}
	}
	return 0, false
}

// protectionMarkers are the substrings that mean stop probing. Account
// jeopardy is never worth another request.
var protectionMarkers = []string{
	"validation_required",
	"validation_url",
	"accounts.google.com/signin/continue",
	"has been disabled",
	"violation of terms of service",
	"invalid_grant",
	"unauthorized_client",
}

// GuardBody returns ErrAccountProtected when a response body signals account
// jeopardy rather than an ordinary throttle.
func GuardBody(body string) error {
	lower := strings.ToLower(body)
	for _, marker := range protectionMarkers {
		if strings.Contains(lower, marker) {
			return ErrAccountProtected
		}
	}
	return nil
}
