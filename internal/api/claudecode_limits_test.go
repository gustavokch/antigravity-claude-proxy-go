package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/claudecode/ccusage"
	"antigravity-go-proxy/internal/config"
)

const ccLimitsModel = "cc-limits-model"

// ccLimitsFixture is a test server with Claude Code accounts and,
// optionally, a usage engine on the server's clock.
type ccLimitsFixture struct {
	server *Server
	pool   *claudecode.AccountPool
	engine *ccusage.Engine
	now    time.Time
}

func newCCLimitsFixture(t *testing.T, withEngine bool, accts ...claudecode.AccountConfig) *ccLimitsFixture {
	t.Helper()
	server, _, _ := newTestServerWithManager(t)
	isolateClaudeDirs(t)
	resetCCPoolForTest()
	t.Cleanup(resetCCPoolForTest)

	cfg := config.Get()
	cfg.ClaudeCode.Enabled = true
	cfg.ClaudeCode.Accounts = accts
	cfg.ClaudeCode.Allowlist = []claudecode.ModelConfig{{ID: ccLimitsModel}}
	config.SetForTest(cfg)

	f := &ccLimitsFixture{server: server, now: server.now()}
	f.pool, _ = server.getOrCreateCCPool(cfg.ClaudeCode)
	if f.pool == nil {
		t.Fatal("expected claude code pool to be created")
	}
	if withEngine {
		en, err := ccusage.NewEngine(ccusage.EngineOptions{
			LedgerRoot: t.TempDir(),
			Now:        server.now,
			Location:   time.UTC,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { en.Close() })
		server.ccUsage = en
		f.engine = en
	}
	return f
}

var ccLimitsSeq int

// record adds one usage entry with a logged cost for acct, ago before now.
func (f *ccLimitsFixture) record(acct string, ago time.Duration, tokens int64, cost float64) {
	ccLimitsSeq++
	f.engine.Record(ccusage.Entry{
		Timestamp: f.now.Add(-ago),
		SessionID: "s",
		RequestID: fmt.Sprintf("req_limits%d", ccLimitsSeq),
		MessageID: fmt.Sprintf("msg_limits%d", ccLimitsSeq),
		Model:     "claude-sonnet-5",
		Input:     tokens,
		CostUSD:   &cost,
		AccountID: acct,
	})
}

// rows fetches /account-limits and returns the Claude Code rows by ID.
func (f *ccLimitsFixture) rows(t *testing.T) map[string]map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/account-limits", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, a := range res["accounts"].([]any) {
		am := a.(map[string]any)
		if am["provider"] == "claudecode" {
			out[am["id"].(string)] = am
		}
	}
	return out
}

func ccOAuth(id string) claudecode.AccountConfig {
	return claudecode.AccountConfig{ID: id, Email: id + "@example.com", Token: "t-" + id, Type: "oauth", Priority: 1, Enabled: true, Source: "config"}
}

func ccPools(t *testing.T, row map[string]any) map[string]any {
	t.Helper()
	q, ok := row["quota"].(map[string]any)
	if !ok {
		t.Fatalf("row %v has no quota object", row["id"])
	}
	pools, _ := q["pools"].(map[string]any)
	return pools
}

func ccUsageWindow(t *testing.T, row map[string]any, name string) map[string]any {
	t.Helper()
	u, ok := row["usage"].(map[string]any)
	if !ok {
		t.Fatalf("row %v has no usage object", row["id"])
	}
	w, ok := u[name].(map[string]any)
	if !ok {
		t.Fatalf("usage.%s missing: %v", name, u)
	}
	return w
}

func near(a any, b float64) bool {
	f, ok := a.(float64)
	return ok && math.Abs(f-b) < 1e-9
}

func rfc3339(ts time.Time) string { return ts.UTC().Format(time.RFC3339) }

func fptr(v float64) *float64 { return &v }

func TestClaudeCodeLimits_HeadersCalibrateAndProject(t *testing.T) {
	f := newCCLimitsFixture(t, true, ccOAuth("cc-h"), ccOAuth("cc-low"))
	reset5h := f.now.Add(3 * time.Hour)
	reset7d := f.now.Add(48 * time.Hour)
	for _, id := range []string{"cc-h", "cc-low"} {
		util5h := 0.25
		if id == "cc-low" {
			util5h = 0.19
		}
		f.pool.UpdateAccountRateLimits(id, claudecode.RateLimits{
			LastUpdated: f.now,
			Unified: &claudecode.Unified{
				Status:     "allowed",
				FiveHour:   claudecode.UnifiedWindow{Utilization: fptr(util5h), Reset: reset5h},
				SevenDay:   claudecode.UnifiedWindow{Utilization: fptr(0.10), Reset: reset7d},
				ObservedAt: f.now,
			},
		})
		// $5 inside the header 5h window [now-2h, now+3h), $1 before it.
		f.record(id, 3*time.Hour, 50, 1)
		f.record(id, 90*time.Minute, 100, 2)
		f.record(id, 30*time.Minute, 200, 3)
	}

	rows := f.rows(t)
	row := rows["cc-h"]
	pools := ccPools(t, row)
	p5 := pools["claude-5h"].(map[string]any)
	if p5["source"] != "headers" || p5["remainingFraction"] != 0.75 || p5["resetTime"] != rfc3339(reset5h) {
		t.Errorf("5h pool = %v", p5)
	}
	if p7 := pools["claude-weekly"].(map[string]any); p7["source"] != "headers" || p7["remainingFraction"] != 0.9 {
		t.Errorf("weekly pool = %v", p7)
	}

	w5 := ccUsageWindow(t, row, "window5h")
	if w5["source"] != "headers" || w5["utilization"] != 0.25 || !near(w5["costUSD"], 5) || w5["tokens"] != float64(300) {
		t.Errorf("usage.window5h = %v", w5)
	}
	if w5["start"] != rfc3339(reset5h.Add(-5*time.Hour)) || w5["end"] != rfc3339(reset5h) {
		t.Errorf("usage.window5h bounds = %v..%v", w5["start"], w5["end"])
	}
	// The block burns $5 an hour for three more hours: $5 now, $20 at the
	// end, so the projection is four times the utilization.
	proj, _ := w5["projectedUtilization"].(float64)
	if math.Abs(proj-1.0) > 1e-6 {
		t.Errorf("projectedUtilization = %v, want 1.0", w5["projectedUtilization"])
	}
	if w5["status"] != "warning" && w5["status"] != "exceeds" {
		t.Errorf("status = %v for projection %v", w5["status"], proj)
	}
	// The projection never reaches remainingFraction.
	l := row["limits"].(map[string]any)[ccLimitsModel].(map[string]any)
	if l["remainingFraction"] != 0.75 {
		t.Errorf("limits remainingFraction = %v, want 0.75", l["remainingFraction"])
	}

	u := row["usage"].(map[string]any)
	if u["costBasis"] != "api-equivalent" || !near(u["todayCostUSD"], 6) {
		t.Errorf("usage = %v", u)
	}
	if br, ok := u["burnRate"].(map[string]any); !ok || !near(br["costPerHour"], 5) {
		t.Errorf("burnRate = %v", u["burnRate"])
	}

	// Calibration: 5h at 0.25 implies $5/0.25 = $20; 7d at 0.10 is below
	// the calibration threshold. The 7d reset is remembered.
	s := f.engine.Summary("cc-h")
	if !near(s.CalibratedCostUSD5h, 20) || s.CalibratedCostUSD7d != 0 || !s.CalibratedAt.Equal(f.now) {
		t.Errorf("summary calibration = %+v", s)
	}
	if !s.Reset7d.Equal(reset7d) {
		t.Errorf("summary Reset7d = %v, want %v", s.Reset7d, reset7d)
	}
	if s := f.engine.Summary("cc-low"); s.CalibratedCostUSD5h != 0 || !s.CalibratedAt.IsZero() {
		t.Errorf("calibrated below 0.20: %+v", s)
	}
}

func TestClaudeCodeLimits_CalibratedFallback(t *testing.T) {
	f := newCCLimitsFixture(t, true, ccOAuth("cc-cal"), ccOAuth("cc-clamp"), ccOAuth("cc-noanchor"))
	now := f.now
	// Stale headers: both windows reset an hour ago.
	for _, id := range []string{"cc-cal", "cc-clamp"} {
		f.pool.UpdateAccountRateLimits(id, claudecode.RateLimits{
			LastUpdated: now.Add(-6 * time.Hour),
			Unified: &claudecode.Unified{
				Status:     "allowed",
				FiveHour:   claudecode.UnifiedWindow{Utilization: fptr(0.5), Reset: now.Add(-7 * time.Hour)},
				SevenDay:   claudecode.UnifiedWindow{Utilization: fptr(0.5), Reset: now.Add(-24 * time.Hour)},
				ObservedAt: now.Add(-8 * time.Hour),
			},
		})
		f.engine.UpdateSummary(id, func(s *ccusage.Summary) {
			s.CalibratedCostUSD5h, s.CalibratedCostUSD7d = 20, 100
		})
	}
	f.engine.UpdateSummary("cc-noanchor", func(s *ccusage.Summary) {
		s.CalibratedCostUSD5h, s.CalibratedCostUSD7d = 20, 100
	})

	// The 5h anchor now-12h steps to [now-2h, now+3h); the 7d reset of a
	// day ago steps to [now-1d, now+6d).
	f.record("cc-cal", 3*time.Hour, 10, 7) // before the stepped 5h window
	f.record("cc-cal", 90*time.Minute, 10, 2)
	f.record("cc-cal", 30*time.Minute, 10, 3)
	f.record("cc-clamp", 30*time.Minute, 10, 45)
	f.record("cc-noanchor", 30*time.Minute, 10, 4)

	rows := f.rows(t)

	pools := ccPools(t, rows["cc-cal"])
	p5 := pools["claude-5h"].(map[string]any)
	if p5["source"] != "calibrated" || !near(p5["remainingFraction"], 1-5.0/20) || p5["resetTime"] != rfc3339(now.Add(3*time.Hour)) {
		t.Errorf("calibrated 5h pool = %v", p5)
	}
	p7 := pools["claude-weekly"].(map[string]any)
	if p7["source"] != "calibrated" || !near(p7["remainingFraction"], 1-12.0/100) || p7["resetTime"] != rfc3339(now.Add(6*24*time.Hour)) {
		t.Errorf("calibrated weekly pool = %v", p7)
	}
	w7 := ccUsageWindow(t, rows["cc-cal"], "window7d")
	if w7["start"] != rfc3339(now.Add(-24*time.Hour)) || w7["source"] != "calibrated" || !near(w7["utilization"], 0.12) {
		t.Errorf("usage.window7d = %v", w7)
	}

	// $45 against an implied $20 clamps to zero, not below.
	if p := ccPools(t, rows["cc-clamp"])["claude-5h"].(map[string]any); p["remainingFraction"] != 0.0 {
		t.Errorf("clamped pool = %v", p)
	}
	if w := ccUsageWindow(t, rows["cc-clamp"], "window5h"); !near(w["utilization"], 2.25) || w["status"] != "exceeds" {
		t.Errorf("clamped usage window = %v", w)
	}

	// Without any anchor: the floored ccusage block for 5h, and a rolling
	// seven days without a reset for 7d.
	pools = ccPools(t, rows["cc-noanchor"])
	p5 = pools["claude-5h"].(map[string]any)
	blockStart := now.Add(-30 * time.Minute).Truncate(time.Hour)
	if p5["source"] != "calibrated" || !near(p5["remainingFraction"], 0.8) || p5["resetTime"] != rfc3339(blockStart.Add(5*time.Hour)) {
		t.Errorf("unanchored 5h pool = %v", p5)
	}
	p7 = pools["claude-weekly"].(map[string]any)
	if _, has := p7["resetTime"]; has || !near(p7["remainingFraction"], 0.96) {
		t.Errorf("rolling weekly pool = %v", p7)
	}
	if w := ccUsageWindow(t, rows["cc-noanchor"], "window7d"); w["start"] != rfc3339(now.Add(-7*24*time.Hour)) || w["end"] != rfc3339(now) {
		t.Errorf("rolling usage window = %v", w)
	}
}

func TestClaudeCodeLimits_ConfigMaxAndOmitted(t *testing.T) {
	cfgAcct := ccOAuth("cc-config")
	cfgAcct.UsageLimits = &claudecode.UsageLimits{CostUSD5h: 10, Tokens5h: 5, Tokens7d: 1000}
	key := claudecode.AccountConfig{ID: "cc-key", Email: "key@example.com", Token: "sk", Type: "api_key", Priority: 1, Enabled: true, Source: "config",
		UsageLimits: &claudecode.UsageLimits{CostUSD7d: 50}}
	keyBare := claudecode.AccountConfig{ID: "cc-key-bare", Email: "bare@example.com", Token: "sk2", Type: "api_key", Priority: 1, Enabled: true, Source: "config"}
	f := newCCLimitsFixture(t, true, cfgAcct, ccOAuth("cc-max"), ccOAuth("cc-none"), key, keyBare)

	f.record("cc-config", 30*time.Minute, 250, 4)
	f.record("cc-key", 30*time.Minute, 100, 5)
	f.record("cc-key-bare", 30*time.Minute, 100, 5)
	// A completed $20 block yesterday makes the max; the active block is $5.
	f.record("cc-max", 30*time.Hour, 1000, 12)
	f.record("cc-max", 29*time.Hour, 1000, 8)
	f.record("cc-max", 30*time.Minute, 100, 5)

	rows := f.rows(t)

	// Configured limits: cost wins over tokens for 5h; tokens for 7d.
	pools := ccPools(t, rows["cc-config"])
	if p := pools["claude-5h"].(map[string]any); p["source"] != "config" || !near(p["remainingFraction"], 0.6) {
		t.Errorf("config 5h pool = %v", p)
	}
	if p := pools["claude-weekly"].(map[string]any); p["source"] != "config" || !near(p["remainingFraction"], 0.75) {
		t.Errorf("config weekly pool = %v", p)
	}

	// The max block, 5h only.
	pools = ccPools(t, rows["cc-max"])
	if p, ok := pools["claude-5h"].(map[string]any); !ok || p["source"] != "max" || !near(p["remainingFraction"], 0.75) {
		t.Errorf("max 5h pool = %v", pools["claude-5h"])
	}
	if _, ok := pools["claude-weekly"]; ok {
		t.Errorf("max must not give a weekly pool: %v", pools)
	}
	if w := ccUsageWindow(t, rows["cc-max"], "window7d"); w["source"] != nil || w["utilization"] != nil || w["status"] != nil || !near(w["costUSD"], 25) {
		t.Errorf("weekly usage without a limit = %v", w)
	}

	// Nothing to go on: pools are omitted, usage still reports the cost.
	if pools := ccPools(t, rows["cc-none"]); len(pools) != 0 {
		t.Errorf("pools without any source = %v", pools)
	}
	if _, ok := rows["cc-none"]["usage"].(map[string]any); !ok {
		t.Errorf("usage missing for an account without data")
	}

	// API-key accounts: configured limits give pools, nothing else does,
	// and usage is reported either way.
	pools = ccPools(t, rows["cc-key"])
	if p, ok := pools["claude-weekly"].(map[string]any); !ok || p["source"] != "config" || !near(p["remainingFraction"], 0.9) || len(pools) != 1 {
		t.Errorf("api key pools = %v", pools)
	}
	if _, ok := rows["cc-key-bare"]["quota"]; ok {
		t.Errorf("api key without limits has a quota: %v", rows["cc-key-bare"]["quota"])
	}
	if w := ccUsageWindow(t, rows["cc-key-bare"], "window5h"); !near(w["costUSD"], 5) || w["source"] != nil {
		t.Errorf("api key usage window = %v", w)
	}
	if l := rows["cc-key"]["limits"].(map[string]any)[ccLimitsModel].(map[string]any); !near(l["remainingFraction"], 0.9) {
		t.Errorf("api key limits = %v", l)
	}
}

func TestClaudeCodeLimits_NilEngineUnchanged(t *testing.T) {
	f := newCCLimitsFixture(t, false, ccOAuth("cc-n"), claudecode.AccountConfig{ID: "cc-nk", Email: "nk@example.com", Token: "sk", Type: "api_key", Priority: 1, Enabled: true,
		UsageLimits: &claudecode.UsageLimits{CostUSD5h: 1}})
	f.pool.UpdateAccountRateLimits("cc-n", claudecode.RateLimits{
		LastUpdated: f.now,
		Unified: &claudecode.Unified{
			FiveHour:   claudecode.UnifiedWindow{Utilization: fptr(0.3), Reset: f.now.Add(-time.Minute)},
			SevenDay:   claudecode.UnifiedWindow{Utilization: fptr(0.3), Reset: f.now.Add(time.Hour)},
			ObservedAt: f.now,
		},
	})
	rows := f.rows(t)
	for id, row := range rows {
		if _, ok := row["usage"]; ok {
			t.Errorf("%s: usage without an engine", id)
		}
	}
	pools := ccPools(t, rows["cc-n"])
	if len(pools) != 1 {
		t.Fatalf("pools = %v, want the fresh weekly window only", pools)
	}
	p := pools["claude-weekly"].(map[string]any)
	if len(p) != 3 || p["source"] != "headers" || p["remainingFraction"] != 0.7 || p["resetTime"] != rfc3339(f.now.Add(time.Hour)) {
		t.Errorf("weekly pool = %v", p)
	}
	if _, ok := rows["cc-nk"]["quota"]; ok {
		t.Errorf("api key got a quota without an engine")
	}
}

func TestClaudeCodeStepWindow(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		anchor time.Time
		dur    time.Duration
		want   time.Time
	}{
		{"current", now.Add(-time.Hour), 5 * time.Hour, now.Add(-time.Hour)},
		{"two periods back", now.Add(-12 * time.Hour), 5 * time.Hour, now.Add(-2 * time.Hour)},
		{"on the boundary", now.Add(-10 * time.Hour), 5 * time.Hour, now},
		{"anchor ahead", now.Add(time.Hour), 5 * time.Hour, now.Add(-4 * time.Hour)},
		{"anchor far ahead", now.Add(11 * time.Hour), 5 * time.Hour, now.Add(-4 * time.Hour)},
		{"weekly", now.Add(-8 * 24 * time.Hour), sevenDays, now.Add(-24 * time.Hour)},
	}
	for _, tc := range tests {
		got := claudeCodeStepWindow(tc.anchor, tc.dur, now)
		if !got.Equal(tc.want) {
			t.Errorf("%s: start = %v, want %v", tc.name, got, tc.want)
		}
		if got.After(now) || !now.Before(got.Add(tc.dur)) {
			t.Errorf("%s: window %v+%v does not contain now", tc.name, got, tc.dur)
		}
	}
}
