package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/claudecode/ccusage"
	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/modelcatalog"
)

const usageCatalogBody = `{
	"agentModelSorts":[{"groups":[{"modelIds":["gemini-3.5-flash-low","gpt-oss"]}]}],
	"models":{
		"gemini-3.5-flash-low":{"displayName":"Gemini Flash","quotaInfo":{"remainingFraction":0.875,"resetTime":"2026-07-15T18:00:00Z"}},
		"gpt-oss":{"displayName":"GPT OSS","quotaInfo":{"remainingFraction":0.5,"resetTime":"2026-07-16T00:00:00Z"}}
	}
}`

// newUsageWindowsServer is a Cloud Code test server whose catalog fetch
// returns modelsBody, with an isolated config and Claude Code pool.
func newUsageWindowsServer(t *testing.T, modelsBody string) *Server {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", dir)
	isolateClaudeDirs(t)
	_, _ = config.Load()
	resetCCPoolForTest()
	t.Cleanup(resetCCPoolForTest)
	return newTestServer(t, &fakeUpstream{modelsBody: []byte(modelsBody)}, "project")
}

// enableClaudeCode configures accts and returns the live pool.
func enableClaudeCode(t *testing.T, server *Server, accts ...claudecode.AccountConfig) *claudecode.AccountPool {
	t.Helper()
	cfg := config.Get()
	cfg.ClaudeCode.Enabled = true
	cfg.ClaudeCode.Accounts = accts
	cfg.ClaudeCode.Allowlist = []claudecode.ModelConfig{{ID: "cc-usage-model", Alias: "ccu"}}
	config.SetForTest(cfg)
	resetCCPoolForTest()
	pool, _ := server.getOrCreateCCPool(cfg.ClaudeCode)
	if pool == nil {
		t.Fatal("expected claude code pool to be created")
	}
	return pool
}

func getUsage(t *testing.T, server *Server) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	req.Header.Set("Authorization", "Bearer local-key")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

type rawUsage struct {
	Windows []json.RawMessage `json:"windows"`
	Models  json.RawMessage   `json:"models"`
}

func setUnified(pool *claudecode.AccountPool, id string, now time.Time, util5h, util7d float64) {
	pool.UpdateAccountRateLimits(id, claudecode.RateLimits{
		LastUpdated: now,
		Unified: &claudecode.Unified{
			Status:     "allowed",
			FiveHour:   claudecode.UnifiedWindow{Utilization: fptr(util5h), Reset: now.Add(2 * time.Hour)},
			SevenDay:   claudecode.UnifiedWindow{Utilization: fptr(util7d), Reset: now.Add(72 * time.Hour)},
			ObservedAt: now,
		},
	})
}

func TestUsage_ClaudeWindowsWhenCatalogFails(t *testing.T) {
	server := newUsageWindowsServer(t, "not json")
	now := server.now()
	named := ccOAuth("cc-w")
	named.Name = "Work"
	disabled := ccOAuth("cc-off")
	disabled.Enabled = false
	key := claudecode.AccountConfig{ID: "cc-key", Token: "sk", Type: "api_key", Priority: 1, Enabled: true, Source: "config"}
	pool := enableClaudeCode(t, server, named, ccOAuth("cc-nodata"), disabled, key)
	setUnified(pool, "cc-w", now, 0.25, 0.5)
	setUnified(pool, "cc-off", now, 0.25, 0.5)
	setUnified(pool, "cc-key", now, 0.25, 0.5)

	rec := getUsage(t, server)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeBody(t, rec.Body, &body)
	if models, ok := body["models"].([]any); !ok || len(models) != 0 {
		t.Errorf("models = %#v, want []", body["models"])
	}
	windows := body["windows"].([]any)
	if len(windows) != 2 {
		t.Fatalf("windows = %#v", windows)
	}
	want := []map[string]any{
		{"label": "Claude 5h (Work)", "remaining_fraction": 0.75, "used_percent": 25.0, "reset_at": rfc3339(now.Add(2 * time.Hour))},
		{"label": "Claude weekly (Work)", "remaining_fraction": 0.5, "used_percent": 50.0, "reset_at": rfc3339(now.Add(72 * time.Hour))},
	}
	for i, w := range windows {
		got := w.(map[string]any)
		for k, v := range want[i] {
			if got[k] != v {
				t.Errorf("window %d %s = %v, want %v", i, k, got[k], v)
			}
		}
		if got["provider"] != "claudecode" || got["account_id"] != "cc-w" {
			t.Errorf("window %d provider/account = %v/%v", i, got["provider"], got["account_id"])
		}
		ids, _ := got["model_ids"].([]any)
		if len(ids) != 2 || ids[0] != "cc-usage-model" || ids[1] != "ccu" {
			t.Errorf("window %d model_ids = %v", i, got["model_ids"])
		}
		if len(got) != 7 {
			t.Errorf("window %d has fields %v", i, got)
		}
	}
}

func TestUsage_BothFailKeepsError(t *testing.T) {
	server := newUsageWindowsServer(t, "not json")
	baseline := getUsage(t, server)
	if baseline.Code == http.StatusOK {
		t.Fatalf("catalog failure without Claude Code: status 200")
	}

	// Accounts with no pool data are no Claude windows either.
	enableClaudeCode(t, server, ccOAuth("cc-nodata"))
	rec := getUsage(t, server)
	if rec.Code != baseline.Code || rec.Body.String() != baseline.Body.String() {
		t.Errorf("got %d %s, want %d %s", rec.Code, rec.Body.String(), baseline.Code, baseline.Body.String())
	}
}

func TestUsage_GoogleWindowsUnchangedWithClaude(t *testing.T) {
	server := newUsageWindowsServer(t, usageCatalogBody)
	baseline := getUsage(t, server)
	if baseline.Code != http.StatusOK {
		t.Fatalf("baseline status=%d", baseline.Code)
	}
	var before rawUsage
	decodeBody(t, bytes.NewReader(baseline.Body.Bytes()), &before)

	// The baseline is groupQuotaWindows' output, as before this change.
	catalog, err := modelcatalog.Parse([]byte(usageCatalogBody))
	if err != nil {
		t.Fatal(err)
	}
	expected := groupQuotaWindows(catalog.Selectable())
	if len(before.Windows) != len(expected) {
		t.Fatalf("baseline windows = %d, want %d", len(before.Windows), len(expected))
	}
	for i, w := range expected {
		b, _ := json.Marshal(w)
		if !bytes.Equal(b, before.Windows[i]) {
			t.Errorf("baseline window %d = %s, want %s", i, before.Windows[i], b)
		}
	}

	pool := enableClaudeCode(t, server, ccOAuth("cc-w"))
	setUnified(pool, "cc-w", server.now(), 0.1, 0.2)
	rec := getUsage(t, server)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var after rawUsage
	decodeBody(t, rec.Body, &after)
	if !bytes.Equal(after.Models, before.Models) {
		t.Errorf("models changed: %s, was %s", after.Models, before.Models)
	}
	if len(after.Windows) != len(before.Windows)+2 {
		t.Fatalf("windows = %d, want %d", len(after.Windows), len(before.Windows)+2)
	}
	for i := range before.Windows {
		if !bytes.Equal(after.Windows[i], before.Windows[i]) {
			t.Errorf("google window %d = %s, was %s", i, after.Windows[i], before.Windows[i])
		}
	}
	var last map[string]any
	_ = json.Unmarshal(after.Windows[len(after.Windows)-1], &last)
	if last["label"] != "Claude weekly (cc-w@example.com)" || last["provider"] != "claudecode" {
		t.Errorf("claude window = %v", last)
	}
}

func TestUsage_ClaudeWindowsFromEngineDoNotCalibrate(t *testing.T) {
	server := newUsageWindowsServer(t, "not json")
	now := server.now()
	en, err := ccusage.NewEngine(ccusage.EngineOptions{LedgerRoot: t.TempDir(), Now: server.now, Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { en.Close() })
	server.ccUsage = en
	pool := enableClaudeCode(t, server, ccOAuth("cc-cal"), ccOAuth("cc-roll"))
	setUnified(pool, "cc-cal", now, 0.5, 0.5)
	en.UpdateSummary("cc-roll", func(s *ccusage.Summary) { s.CalibratedCostUSD5h, s.CalibratedCostUSD7d = 20, 100 })
	for i, acct := range []string{"cc-cal", "cc-roll"} {
		cost := 5.0
		en.Record(ccusage.Entry{
			Timestamp: now.Add(-30 * time.Minute), SessionID: "s", RequestID: "req_w" + acct, MessageID: "msg_w" + acct,
			Model: "claude-sonnet-5", Input: int64(10 * (i + 1)), CostUSD: &cost, AccountID: acct,
		})
	}

	rec := getUsage(t, server)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	decodeBody(t, rec.Body, &body)
	byLabel := map[string]map[string]any{}
	for _, w := range body["windows"].([]any) {
		byLabel[w.(map[string]any)["label"].(string)] = w.(map[string]any)
	}
	if w := byLabel["Claude weekly (cc-roll@example.com)"]; w == nil || w["reset_at"] != "" || !near(w["remaining_fraction"], 0.95) {
		t.Errorf("rolling weekly window = %v", w)
	}
	if w := byLabel["Claude 5h (cc-roll@example.com)"]; w == nil || !near(w["remaining_fraction"], 0.75) {
		t.Errorf("calibrated 5h window = %v", w)
	}
	if s := en.Summary("cc-cal"); s.CalibratedCostUSD5h != 0 || s.CalibratedCostUSD7d != 0 || !s.Reset7d.IsZero() {
		t.Errorf("/v1/usage saved a calibration: %+v", s)
	}
}

// ccReportFixture records usage for two accounts and unattributed usage:
// cc-a $1 30h ago and $2 1h ago, cc-b $4 1h ago and $16 five days ago,
// unattributed $8 2h ago. The clock is 2026-08-14 12:00 UTC, a Friday.
func ccReportFixture(t *testing.T) *ccLimitsFixture {
	t.Helper()
	f := newCCLimitsFixture(t, true, ccOAuth("cc-a"), ccOAuth("cc-b"))
	f.record("cc-a", 30*time.Hour, 100, 1)
	f.record("cc-a", time.Hour, 200, 2)
	f.record("cc-b", time.Hour, 400, 4)
	f.record("cc-b", 5*24*time.Hour, 1600, 16)
	f.record("", 2*time.Hour, 800, 8)
	return f
}

func getReport(t *testing.T, server *Server, query string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/claudecode/usage"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status=%d body=%s", query, rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// reportRows keys a report's rows by their period key and account.
func reportRows(t *testing.T, body map[string]any, list, key string) map[string]float64 {
	t.Helper()
	rows, ok := body[list].([]any)
	if !ok {
		t.Fatalf("no %s rows in %v", list, body)
	}
	out := map[string]float64{}
	for _, r := range rows {
		row := r.(map[string]any)
		out[row[key].(string)+"/"+row["accountId"].(string)] = row["totalCost"].(float64)
	}
	return out
}

// sameCosts reports whether got has exactly want's keys with near costs.
func sameCosts[K comparable](got, want map[K]float64) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if c, ok := got[k]; !ok || !near(c, v) {
			return false
		}
	}
	return true
}

func TestClaudeCodeUsageReport_Summaries(t *testing.T) {
	f := ccReportFixture(t)
	tests := []struct {
		query, list, key string
		want             map[string]float64
		total            float64
	}{
		{"", "daily", "date", map[string]float64{
			"2026-08-09/cc-b": 16, "2026-08-13/cc-a": 1, "2026-08-14/cc-a": 2, "2026-08-14/cc-b": 4, "2026-08-14/unattributed": 8}, 31},
		{"?report=daily&account=cc-a", "daily", "date", map[string]float64{"2026-08-13/cc-a": 1, "2026-08-14/cc-a": 2}, 3},
		{"?report=daily&account=unattributed", "daily", "date", map[string]float64{"2026-08-14/unattributed": 8}, 8},
		{"?report=daily&since=20260814", "daily", "date", map[string]float64{
			"2026-08-14/cc-a": 2, "2026-08-14/cc-b": 4, "2026-08-14/unattributed": 8}, 14},
		{"?report=daily&since=2026-08-10&until=2026-08-13", "daily", "date", map[string]float64{"2026-08-13/cc-a": 1}, 1},
		{"?report=daily&timezone=America/Los_Angeles&account=cc-a", "daily", "date", map[string]float64{"2026-08-12/cc-a": 1, "2026-08-14/cc-a": 2}, 3},
		{"?report=weekly", "weekly", "week", map[string]float64{
			"2026-08-09/cc-a": 3, "2026-08-09/cc-b": 20, "2026-08-09/unattributed": 8}, 31},
		{"?report=weekly&startOfWeek=friday&account=cc-b", "weekly", "week", map[string]float64{"2026-08-07/cc-b": 16, "2026-08-14/cc-b": 4}, 20},
		{"?report=monthly", "monthly", "month", map[string]float64{
			"2026-08/cc-a": 3, "2026-08/cc-b": 20, "2026-08/unattributed": 8}, 31},
		{"?report=session&account=cc-b", "sessions", "sessionId", map[string]float64{"s/cc-b": 20}, 20},
	}
	for _, tt := range tests {
		body := getReport(t, f.server, tt.query)
		if body["enabled"] != true {
			t.Errorf("%s: enabled = %v", tt.query, body["enabled"])
		}
		if got := reportRows(t, body, tt.list, tt.key); !sameCosts(got, tt.want) {
			t.Errorf("%s: rows = %v, want %v", tt.query, got, tt.want)
		}
		if totals := body["totals"].(map[string]any); !near(totals["totalCost"], tt.total) {
			t.Errorf("%s: totals = %v, want cost %v", tt.query, totals, tt.total)
		}
	}

	body := getReport(t, f.server, "?report=daily&timezone=America/Los_Angeles")
	if body["report"] != "daily" || body["timezone"] != "America/Los_Angeles" || body["since"] != "" || body["until"] != "" {
		t.Errorf("report metadata = %v", body)
	}
	if body := getReport(t, f.server, "?since=2026-08-14"); body["timezone"] != "UTC" || body["since"] != "20260814" {
		t.Errorf("default timezone and since = %v, %v", body["timezone"], body["since"])
	}
	if rows := getReport(t, f.server, "?account=nobody")["daily"].([]any); len(rows) != 0 {
		t.Errorf("unknown account rows = %v", rows)
	}
}

func TestClaudeCodeUsageReport_Blocks(t *testing.T) {
	f := ccReportFixture(t)
	// A second entry gives cc-b's active block a burn rate.
	f.record("cc-b", 20*time.Minute, 100, 1)
	type blockKey struct {
		account string
		active  bool
		gap     bool
	}
	blocks := func(query string) []map[string]any {
		t.Helper()
		body := getReport(t, f.server, query)
		if body["report"] != "blocks" {
			t.Fatalf("%s: report = %v", query, body["report"])
		}
		var out []map[string]any
		for _, b := range body["blocks"].([]any) {
			out = append(out, b.(map[string]any))
		}
		return out
	}
	summarize := func(rows []map[string]any) map[blockKey]float64 {
		out := map[blockKey]float64{}
		for _, r := range rows {
			k := blockKey{r["accountId"].(string), r["isActive"].(bool), r["isGap"].(bool)}
			out[k] += r["costUSD"].(float64)
		}
		return out
	}

	all := blocks("?report=blocks")
	for i := 1; i < len(all); i++ {
		if all[i-1]["startTime"].(string) > all[i]["startTime"].(string) {
			t.Errorf("blocks not in start order: %v then %v", all[i-1]["startTime"], all[i]["startTime"])
		}
	}
	got := summarize(all)
	for k, v := range map[blockKey]float64{
		{"cc-a", false, false}: 1, {"cc-a", true, false}: 2, {"cc-a", false, true}: 0,
		{"cc-b", false, false}: 16, {"cc-b", true, false}: 5, {"cc-b", false, true}: 0,
		{"unattributed", true, false}: 8,
	} {
		if c, ok := got[k]; !ok || !near(c, v) {
			t.Errorf("blocks %+v cost = %v (present %v), want %v; all %v", k, c, ok, v, got)
		}
	}
	for _, r := range all {
		if r["accountId"] == "cc-b" && r["isActive"] == true && (r["burnRate"] == nil || r["projection"] == nil) {
			t.Errorf("active block without burn rate or projection: %v", r)
		}
	}

	active := blocks("?report=blocks&active=true")
	if got := summarize(active); len(active) != 3 || !sameCosts(got, map[blockKey]float64{
		{"cc-a", true, false}: 2, {"cc-b", true, false}: 5, {"unattributed", true, false}: 8}) {
		t.Errorf("active blocks = %v", got)
	}

	// Recent drops cc-b's block from five days ago and the gap after it.
	for _, r := range blocks("?report=blocks&recent") {
		if near(r["costUSD"], 16) || r["accountId"] == "cc-b" && r["isGap"] == true {
			t.Errorf("recent kept an old block: %v", r)
		}
	}

	for _, r := range blocks("?report=blocks&account=cc-a&since=20260814") {
		if r["accountId"] != "cc-a" || r["startTime"].(string) < "2026-08-14" {
			t.Errorf("filtered block = %v", r)
		}
	}

	// A "max" token limit comes from all blocks before the active filter:
	// cc-b's 1600-token block.
	maxRows := blocks("?report=blocks&active&account=cc-b&tokenLimit=max")
	if len(maxRows) != 1 {
		t.Fatalf("active cc-b blocks = %v", maxRows)
	}
	for _, r := range maxRows {
		tls, ok := r["tokenLimitStatus"].(map[string]any)
		if !ok || tls["limit"] != float64(1600) {
			t.Errorf("tokenLimitStatus = %v", r["tokenLimitStatus"])
		}
	}
}

func TestClaudeCodeUsageReport_BadParams(t *testing.T) {
	f := ccReportFixture(t)
	for _, query := range []string{
		"?report=hourly",
		"?since=yesterday",
		"?until=2026-13-45",
		"?since=20260814&until=20260801",
		"?timezone=Mars/Olympus_Mons",
		"?startOfWeek=funday",
		"?report=blocks&recent=maybe",
		"?report=blocks&active=2",
		"?report=blocks&tokenLimit=-5",
	} {
		rec := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/claudecode/usage"+query, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%s", query, rec.Code, rec.Body.String())
		}
	}
}

func TestClaudeCodeUsageReport_AuthRequired(t *testing.T) {
	f := ccReportFixture(t)
	cfg := config.Get()
	cfg.WebUIPassword = "secret"
	config.SetForTest(cfg)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/claudecode/usage"},
		{http.MethodPost, "/api/claudecode/usage/reload"},
	} {
		rec := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without password: %d", tc.method, tc.path, rec.Code)
		}
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("x-webui-password", "secret")
		rec = httptest.NewRecorder()
		f.server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s with password: %d %s", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
}

func TestClaudeCodeUsageReport_NilEngine(t *testing.T) {
	f := newCCLimitsFixture(t, false, ccOAuth("cc-a"))
	rec := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/claudecode/usage?report=blocks", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"enabled\":false}\n" {
		t.Errorf("GET without engine: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	f.server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/claudecode/usage/reload", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "{\"enabled\":false,\"status\":\"ok\"}\n" {
		t.Errorf("reload without engine: %d %s", rec.Code, rec.Body.String())
	}
	// Bad parameters are still rejected.
	rec = httptest.NewRecorder()
	f.server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/claudecode/usage?report=nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad report without engine: %d", rec.Code)
	}
}

func TestClaudeCodeUsageReload(t *testing.T) {
	f := ccReportFixture(t)
	rec := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/claudecode/usage/reload", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Status  string `json:"status"`
		Enabled bool   `json:"enabled"`
		Stats   struct {
			Entries int `json:"entries"`
			Files   int `json:"files"`
			Ledger  struct {
				Written uint64 `json:"written"`
				Dropped uint64 `json:"dropped"`
				Errors  uint64 `json:"errors"`
			} `json:"ledger"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || !body.Enabled || body.Stats.Entries != 5 {
		t.Errorf("reload = %s", rec.Body.String())
	}
}

// writeLedgerDay writes entries to acct's ledger file for day, as the
// ledger lays them out.
func writeLedgerDay(t *testing.T, root, acct, day string, entries ...ccusage.Entry) {
	t.Helper()
	var data []byte
	for _, e := range entries {
		line, err := ccusage.MarshalLedgerLine(e)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, line...)
	}
	dir := filepath.Join(root, "projects", ccusage.LedgerAccountDir(acct))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, day+".jsonl"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeCodeUsageReport_LedgerHistory(t *testing.T) {
	f := newCCLimitsFixture(t, true, ccOAuth("cc-a"))
	cutoff := f.now.Add(-ccusage.DefaultEngineWindow) // 2026-08-06 12:00 UTC
	entry := func(ts time.Time, n int) ccusage.Entry {
		cost := float64(n)
		return ccusage.Entry{Timestamp: ts, SessionID: "s", RequestID: fmt.Sprintf("req_hist%d", n), MessageID: fmt.Sprintf("msg_hist%d", n),
			Model: "claude-sonnet-5", Input: int64(n), CostUSD: &cost, AccountID: "cc-a", Origin: ccusage.OriginProxy}
	}
	old := entry(f.now.AddDate(0, 0, -20), 32)
	before := entry(cutoff.Add(-time.Minute), 64)
	after := entry(cutoff.Add(time.Minute), 128)
	writeLedgerDay(t, f.engine.Root(), "cc-a", "2026-07-25", old)
	writeLedgerDay(t, f.engine.Root(), "cc-a", "2026-08-06", before, after)
	// The in-window entry is also in memory; it must count once.
	f.engine.Record(after)

	tests := []struct {
		query, list, key string
		want             map[string]float64
	}{
		{"?report=daily", "daily", "date", map[string]float64{"2026-07-25/cc-a": 32, "2026-08-06/cc-a": 192}},
		{"?report=monthly", "monthly", "month", map[string]float64{"2026-07/cc-a": 32, "2026-08/cc-a": 192}},
		{"?report=daily&since=20260801", "daily", "date", map[string]float64{"2026-08-06/cc-a": 192}},
		{"?report=daily&since=20260807", "daily", "date", map[string]float64{}},
	}
	for _, tt := range tests {
		body := getReport(t, f.server, tt.query)
		if got := reportRows(t, body, tt.list, tt.key); !sameCosts(got, tt.want) {
			t.Errorf("%s: rows = %v, want %v", tt.query, got, tt.want)
		}
		if body["windowStart"] != "2026-08-06T12:00:00Z" || body["partialLocal"] != false {
			t.Errorf("%s: windowStart %v partialLocal %v", tt.query, body["windowStart"], body["partialLocal"])
		}
	}
	var blockCost float64
	for _, b := range getReport(t, f.server, "?report=blocks")["blocks"].([]any) {
		blockCost += b.(map[string]any)["costUSD"].(float64)
	}
	if !near(blockCost, 224) {
		t.Errorf("blocks cost %v, want 224", blockCost)
	}
}

func TestClaudeCodeUsageReport_PartialLocal(t *testing.T) {
	f := newCCLimitsFixture(t, false, ccOAuth("cc-a"))
	en, err := ccusage.NewEngine(ccusage.EngineOptions{
		LedgerRoot: t.TempDir(), Now: f.server.now, Location: time.UTC,
		ScanLocalLogs: true, LocalPaths: func() []string { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { en.Close() })
	f.server.ccUsage = en
	if body := getReport(t, f.server, ""); body["partialLocal"] != true {
		t.Errorf("unbounded report partialLocal = %v", body["partialLocal"])
	}
	if body := getReport(t, f.server, "?since=20260810"); body["partialLocal"] != false {
		t.Errorf("in-window report partialLocal = %v", body["partialLocal"])
	}
}

func TestClaudeCodeUsageReload_Concurrent(t *testing.T) {
	f := ccReportFixture(t)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			f.server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/claudecode/usage/reload", nil))
			codes[i] = rec.Code
		}()
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("reload %d: status %d", i, c)
		}
	}
}

func TestClaudeCodeAccountsPost_RejectsUnattributedID(t *testing.T) {
	f := newCCLimitsFixture(t, false)
	rec := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/claudecode/accounts",
		strings.NewReader(`{"id":"unattributed","token":"t","type":"oauth","enabled":true}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d body %s", rec.Code, rec.Body.String())
	}
	for _, a := range config.Get().ClaudeCode.Accounts {
		if a.ID == "unattributed" {
			t.Error("reserved account ID was saved")
		}
	}
}
