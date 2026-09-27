package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
)

func ccStoredFiveHour(t *testing.T, id string) float64 {
	t.Helper()
	stored, err := claudecode.LoadStoredAccounts(claudecode.DefaultStoragePath())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range stored {
		if a.ID == id && a.Unified != nil && a.Unified.FiveHour.Utilization != nil {
			return *a.Unified.FiveHour.Utilization
		}
	}
	return -1
}

func closeTestUnified(util float64) *claudecode.Unified {
	now := time.Now()
	return &claudecode.Unified{
		Status:     "allowed",
		Reset:      now.Add(3 * time.Hour),
		FiveHour:   claudecode.UnifiedWindow{Utilization: &util, Reset: now.Add(3 * time.Hour), Status: "allowed"},
		ObservedAt: now,
	}
}

// A unified snapshot change held back by the save throttle must reach disk
// when the server closes, and the pool's background saves must be drained.
func TestServerClose_FlushesThrottledUnifiedSnapshot(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	seedCCConfig(t)
	const id = "cc-oauth@example.com"

	pool, _ := srv.getOrCreateCCPool(config.Get().ClaudeCode)
	pool.UpdateAccountRateLimits(id, claudecode.RateLimits{Unified: closeTestUnified(0.1)})
	deadline := time.Now().Add(5 * time.Second)
	for ccStoredFiveHour(t, id) != 0.1 {
		if time.Now().After(deadline) {
			t.Fatalf("first snapshot was never saved")
		}
		time.Sleep(10 * time.Millisecond)
	}

	pool.UpdateAccountRateLimits(id, claudecode.RateLimits{Unified: closeTestUnified(0.2)})
	if got := ccStoredFiveHour(t, id); got != 0.1 {
		t.Fatalf("throttled change reached disk before Close: utilization = %v", got)
	}

	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if got := ccStoredFiveHour(t, id); got != 0.2 {
		t.Errorf("stored utilization after Close = %v, want 0.2", got)
	}
}

// A usage section or usageLimits of the wrong type is refused before it is
// stored, since it would otherwise make the whole config fail to decode.
func TestClaudeCodeManagement_RejectsInvalidUsageConfig(t *testing.T) {
	tests := []struct {
		name, path, body, field string
	}{
		{"config usage not an object", "/api/claudecode/config", `{"enabled":true,"usage":"on"}`, "usage"},
		{"config usage field wrong type", "/api/claudecode/config", `{"enabled":true,"usage":{"retentionDays":"sixty"}}`, "usage"},
		{"account usageLimits not an object", "/api/claudecode/accounts", `{"id":"cc-manual","usageLimits":5}`, "usageLimits"},
		{"account usageLimits field wrong type", "/api/claudecode/accounts", `{"id":"cc-manual","usageLimits":{"tokens5h":"many"}}`, "usageLimits"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, _ := newTestServerWithManager(t)
			seedCCConfig(t)
			rec := doJSON(t, srv, http.MethodPost, tt.path, tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if !strings.Contains(body, `"status":"error"`) || !strings.Contains(body, tt.field) {
				t.Errorf("error body %s does not name %s", body, tt.field)
			}
			if acc := findAccount(t, readCCAccountsFromDisk(t), "cc-manual"); acc["usageLimits"] != nil {
				t.Errorf("rejected usageLimits was stored: %v", acc["usageLimits"])
			}
		})
	}

	t.Run("valid values are accepted", func(t *testing.T) {
		srv, _, _ := newTestServerWithManager(t)
		seedCCConfig(t)
		if rec := doJSON(t, srv, http.MethodPost, "/api/claudecode/accounts", `{"id":"cc-manual","usageLimits":{"tokens5h":1000}}`); rec.Code != http.StatusOK {
			t.Fatalf("accounts: status = %d: %s", rec.Code, rec.Body.String())
		}
		if rec := doJSON(t, srv, http.MethodPost, "/api/claudecode/config", `{"enabled":true,"usage":{"retentionDays":30}}`); rec.Code != http.StatusOK {
			t.Fatalf("config: status = %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// Accounts without usage limits are written without a usageLimits key.
func TestCCAccountToMap_OmitsNilUsageLimits(t *testing.T) {
	if _, ok := ccAccountToMap(claudecode.AccountConfig{ID: "a"})["usageLimits"]; ok {
		t.Errorf("usageLimits present for an account without limits")
	}
	m := ccAccountToMap(claudecode.AccountConfig{ID: "b", UsageLimits: &claudecode.UsageLimits{Tokens5h: 1}})
	if m["usageLimits"] == nil {
		t.Errorf("usageLimits missing for an account with limits")
	}
}
