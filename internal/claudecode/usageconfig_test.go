package claudecode

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestUsageConfig_Defaults(t *testing.T) {
	var u UsageConfig
	if !u.UsageEnabled() || !u.LocalLogsEnabled() {
		t.Error("usage tracking and local logs should default on")
	}
	if d, err := u.SessionDuration(); err != nil || d != 5*time.Hour {
		t.Errorf("default session = %v, %v", d, err)
	}
	if got := u.RetentionDaysOrDefault(); got != 60 {
		t.Errorf("default retention = %d", got)
	}
}

func TestUsageConfig_SessionHours(t *testing.T) {
	cases := []struct {
		hours float64
		want  time.Duration
		ok    bool
	}{
		{0, 5 * time.Hour, true},
		{1, time.Hour, true},
		{8, 8 * time.Hour, true},
		{2.5, 0, false},
		{-5, 0, false},
		{169, 0, false},
	}
	for _, tc := range cases {
		got, err := UsageConfig{SessionHours: tc.hours}.SessionDuration()
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("sessionHours %v: %v, %v", tc.hours, got, err)
		}
	}
}

func TestUsageConfig_JSON(t *testing.T) {
	var cfg Config
	data := `{"usage":{"enabled":false,"ledgerDir":"/x","scanLocalLogs":false,"localAccountId":"a","sessionHours":5,"costMode":"calculate","timezone":"UTC","onlinePricing":true,"retentionDays":30},
		"accounts":[{"id":"a","usageLimits":{"costUsd5h":10,"costUsd7d":50,"tokens5h":1000,"tokens7d":9000}}]}`
	if err := json.Unmarshal([]byte(data), &cfg); err != nil {
		t.Fatal(err)
	}
	u := cfg.Usage
	if u.UsageEnabled() || u.LocalLogsEnabled() || u.LedgerDir != "/x" || u.LocalAccountID != "a" || u.CostMode != "calculate" ||
		u.Timezone != "UTC" || !u.OnlinePricing || u.RetentionDaysOrDefault() != 30 {
		t.Errorf("usage = %+v", u)
	}
	l := cfg.Accounts[0].UsageLimits
	if l == nil || l.CostUSD5h != 10 || l.CostUSD7d != 50 || l.Tokens5h != 1000 || l.Tokens7d != 9000 {
		t.Errorf("usage limits = %+v", l)
	}

	out, err := json.Marshal(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"usage"`) {
		t.Errorf("zero usage config is written: %s", out)
	}
}
