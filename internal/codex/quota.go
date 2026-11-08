package codex

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Window is one subscription usage window as the Codex backend reports it:
// the 5-hour window (300 minutes) is primary, the weekly one (10080) secondary.
type Window struct {
	UsedPercent   float64   `json:"usedPercent"`
	WindowMinutes int       `json:"windowMinutes"`
	ResetAt       time.Time `json:"resetAt"`
}

// Quota is the last usage reading for an account.
type Quota struct {
	Primary    *Window   `json:"primary,omitempty"`
	Secondary  *Window   `json:"secondary,omitempty"`
	PlanType   string    `json:"planType,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
}

// ParseQuota reads the x-codex-* usage headers of a Codex response. It reports
// false when neither window is complete, so a response without the headers
// cannot blank the last good reading.
func ParseQuota(h http.Header, now time.Time) (Quota, bool) {
	q := Quota{
		Primary:    parseWindow(h, "x-codex-primary-", now),
		Secondary:  parseWindow(h, "x-codex-secondary-", now),
		PlanType:   strings.TrimSpace(h.Get("x-codex-plan-type")),
		ObservedAt: now,
	}
	if q.Primary == nil && q.Secondary == nil {
		return Quota{}, false
	}
	return q, true
}

// parseWindow reads one window. It needs the used percentage, the window length
// and a reset time. The relative reset-after-seconds is preferred over the
// absolute reset-at: the reset is compared against this host's clock, and a
// relative value carries no skew between the two clocks.
func parseWindow(h http.Header, prefix string, now time.Time) *Window {
	used, err := strconv.ParseFloat(strings.TrimSpace(h.Get(prefix+"used-percent")), 64)
	if err != nil || math.IsNaN(used) || math.IsInf(used, 0) {
		return nil
	}
	minutes, err := strconv.Atoi(strings.TrimSpace(h.Get(prefix + "window-minutes")))
	if err != nil || minutes <= 0 {
		return nil
	}
	var reset time.Time
	if after, err := strconv.ParseFloat(strings.TrimSpace(h.Get(prefix+"reset-after-seconds")), 64); err == nil && after >= 0 && after < math.MaxInt32 {
		reset = now.Add(time.Duration(after * float64(time.Second)))
	} else if at, err := strconv.ParseInt(strings.TrimSpace(h.Get(prefix+"reset-at")), 10, 64); err == nil && at > 0 {
		reset = time.Unix(at, 0)
	} else {
		return nil
	}
	return &Window{UsedPercent: used, WindowMinutes: minutes, ResetAt: reset}
}

// ParseUsageLimit reads the reset time from a usage_limit_reached error body,
// the reply the backend sends when an account's subscription window is spent.
// The error type is matched in the error object and at the top level. resets_at
// (unix seconds) is used when it lies in the future, else resets_in_seconds.
// ok is false for any other body, and for a usage-limit body without a usable
// reset time.
func ParseUsageLimit(body []byte, now time.Time) (until time.Time, ok bool) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return time.Time{}, false
	}
	errObj, _ := doc["error"].(map[string]any)
	for _, obj := range []map[string]any{errObj, doc} {
		if obj == nil {
			continue
		}
		if t, _ := obj["type"].(string); !strings.EqualFold(strings.TrimSpace(t), "usage_limit_reached") {
			continue
		}
		if at, ok := obj["resets_at"].(float64); ok && at > 0 && at < math.MaxInt64 {
			if reset := time.Unix(int64(at), 0); reset.After(now) {
				return reset, true
			}
		}
		if in, ok := obj["resets_in_seconds"].(float64); ok && in > 0 && in < math.MaxInt32 {
			return now.Add(time.Duration(in * float64(time.Second))), true
		}
	}
	return time.Time{}, false
}
