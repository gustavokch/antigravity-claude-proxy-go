package codex

import (
	"net/http"
	"testing"
	"time"
)

func TestParseQuota(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	h := http.Header{}
	h.Set("x-codex-primary-used-percent", "42.5")
	h.Set("x-codex-primary-window-minutes", "300")
	h.Set("x-codex-primary-reset-after-seconds", "600")
	h.Set("x-codex-primary-reset-at", "1")
	h.Set("x-codex-secondary-used-percent", "7")
	h.Set("x-codex-secondary-window-minutes", "10080")
	h.Set("x-codex-secondary-reset-at", "1800086400")
	h.Set("x-codex-plan-type", "plus")

	q, ok := ParseQuota(h, now)
	if !ok {
		t.Fatal("ParseQuota reported no quota")
	}
	if q.Primary == nil || q.Primary.UsedPercent != 42.5 || q.Primary.WindowMinutes != 300 {
		t.Fatalf("primary = %+v", q.Primary)
	}
	if want := now.Add(600 * time.Second); !q.Primary.ResetAt.Equal(want) {
		t.Errorf("primary reset = %v, want the relative reset %v over reset-at", q.Primary.ResetAt, want)
	}
	if q.Secondary == nil || q.Secondary.WindowMinutes != 10080 || !q.Secondary.ResetAt.Equal(time.Unix(1800086400, 0)) {
		t.Errorf("secondary = %+v", q.Secondary)
	}
	if q.PlanType != "plus" || !q.ObservedAt.Equal(now) {
		t.Errorf("plan/observed = %q/%v", q.PlanType, q.ObservedAt)
	}
}

func TestParseQuotaNeedsACompleteWindow(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for name, h := range map[string]http.Header{
		"no headers":        {},
		"plan type only":    {"X-Codex-Plan-Type": {"pro"}},
		"no reset":          {"X-Codex-Primary-Used-Percent": {"10"}, "X-Codex-Primary-Window-Minutes": {"300"}},
		"no window minutes": {"X-Codex-Primary-Used-Percent": {"10"}, "X-Codex-Primary-Reset-At": {"1800000600"}},
		"bad percent":       {"X-Codex-Primary-Used-Percent": {"ten"}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-At": {"1800000600"}},
	} {
		if q, ok := ParseQuota(h, now); ok {
			t.Errorf("%s: ParseQuota = %+v, true; want false", name, q)
		}
	}

	// One complete window is enough; the other stays nil.
	h := http.Header{"X-Codex-Secondary-Used-Percent": {"1"}, "X-Codex-Secondary-Window-Minutes": {"10080"}, "X-Codex-Secondary-Reset-After-Seconds": {"0"}}
	q, ok := ParseQuota(h, now)
	if !ok || q.Primary != nil || q.Secondary == nil || !q.Secondary.ResetAt.Equal(now) {
		t.Errorf("secondary only: %+v, %v", q, ok)
	}
}

func TestParseUsageLimit(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, tc := range []struct {
		name string
		body string
		want time.Time
		ok   bool
	}{
		{"resets_at in the future", `{"error":{"type":"usage_limit_reached","resets_at":1800003600,"resets_in_seconds":60}}`, time.Unix(1800003600, 0), true},
		{"resets_at in the past falls back", `{"error":{"type":"usage_limit_reached","resets_at":1700000000,"resets_in_seconds":60}}`, now.Add(time.Minute), true},
		{"top-level type", `{"type":"usage_limit_reached","resets_in_seconds":120}`, now.Add(2 * time.Minute), true},
		{"case and spaces", `{"error":{"type":" Usage_Limit_Reached ","resets_in_seconds":5}}`, now.Add(5 * time.Second), true},
		{"no reset", `{"error":{"type":"usage_limit_reached"}}`, time.Time{}, false},
		{"other 429", `{"error":{"type":"rate_limit_error","resets_in_seconds":60}}`, time.Time{}, false},
		{"not json", `rate limited`, time.Time{}, false},
	} {
		got, ok := ParseUsageLimit([]byte(tc.body), now)
		if ok != tc.ok || !got.Equal(tc.want) {
			t.Errorf("%s: ParseUsageLimit = %v, %v; want %v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
