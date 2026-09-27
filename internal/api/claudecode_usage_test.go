package api

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/cachebump"
	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/claudecode/ccusage"
)

// isolateClaudeDirs points every directory the usage engine could read at
// fresh temp dirs, so the real ~/.claude and ~/.config are never touched.
func isolateClaudeDirs(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
}

// newTestCCUsage builds a usage engine with its ledger in a temp dir. The
// shared pricer it installs is reset when the test ends.
func newTestCCUsage(t *testing.T, usage claudecode.UsageConfig) *ccusage.Engine {
	t.Helper()
	isolateClaudeDirs(t)
	if usage.LedgerDir == "" {
		usage.LedgerDir = t.TempDir()
	}
	en, err := NewClaudeCodeUsage(context.Background(), claudecode.Config{Usage: usage}, nil)
	if err != nil {
		t.Fatalf("NewClaudeCodeUsage: %v", err)
	}
	t.Cleanup(func() {
		en.Close()
		claudecode.SetPricer(nil)
	})
	return en
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestClaudeCodeUsage_DefaultLedgerDirAndDisabled(t *testing.T) {
	isolateClaudeDirs(t)
	configDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", configDir)
	t.Cleanup(func() { claudecode.SetPricer(nil) })

	en, err := NewClaudeCodeUsage(context.Background(), claudecode.Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer en.Close()
	if want := filepath.Join(configDir, claudecode.UsageLedgerDirName); en.Root() != want {
		t.Errorf("default ledger root = %q, want %q", en.Root(), want)
	}
	if _, err := os.Stat(filepath.Join(configDir, claudecode.UsageLedgerDirName, "projects")); err != nil {
		t.Errorf("ledger projects dir: %v", err)
	}

	off := false
	disabled, err := NewClaudeCodeUsage(context.Background(), claudecode.Config{Usage: claudecode.UsageConfig{Enabled: &off}}, nil)
	if err != nil || disabled != nil {
		t.Fatalf("disabled usage: engine %v err %v", disabled, err)
	}
}

func TestClaudeCodeUsage_InvalidSettingsFallBack(t *testing.T) {
	en := newTestCCUsage(t, claudecode.UsageConfig{SessionHours: 2.5, CostMode: "bogus", Timezone: "Not/AZone"})
	if en == nil {
		t.Fatal("invalid optional settings disabled the engine")
	}
}

func TestClaudeCodeUsage_SharedPricer(t *testing.T) {
	newTestCCUsage(t, claudecode.UsageConfig{})

	// Models the curated table knows keep their prices when there are no
	// 1-hour cache writes.
	for _, model := range []string{"claude-opus-5", "claude-sonnet-5-20260101", "claude-haiku-4-5", "claude-3-opus-20240229"} {
		u := claudecode.Usage{Input: 1000, Output: 500, CacheRead: 20000, CacheCreate: 3000, CacheCreate5m: 3000}
		if got, want := claudecode.UsageCost(model, u), claudecode.CalculateCost(model, 1000, 500, 3000, 20000); !approx(got, want) {
			t.Errorf("%s: cost %v, want %v", model, got, want)
		}
	}

	// A 1-hour cache write costs twice the input rate.
	p := claudecode.GetModelPricing("claude-sonnet-5")
	u := claudecode.Usage{CacheCreate: 300, CacheCreate5m: 100, CacheCreate1h: 200}
	if got, want := claudecode.UsageCost("claude-sonnet-5", u), 100*p.CacheWrite+200*2*p.Prompt; !approx(got, want) {
		t.Errorf("1h cache write cost %v, want %v", got, want)
	}
	// Cache writes without a split count as 5-minute writes.
	u = claudecode.Usage{CacheCreate: 300}
	if got, want := claudecode.UsageCost("claude-sonnet-5", u), 300*p.CacheWrite; !approx(got, want) {
		t.Errorf("unsplit cache write cost %v, want %v", got, want)
	}

	// A model only LiteLLM knows is priced from LiteLLM.
	lite, ok := ccusage.NewLiteLLMPricer().Find("anthropic.claude-v1")
	if !ok {
		t.Fatal("embedded LiteLLM snapshot lacks the test model")
	}
	u = claudecode.Usage{Input: 1000, Output: 100}
	if got, want := claudecode.UsageCost("anthropic.claude-v1", u), 1000*lite.Input+100*lite.Output; !approx(got, want) {
		t.Errorf("LiteLLM model cost %v, want %v", got, want)
	}

	// A model nobody knows keeps the curated default.
	if got, want := claudecode.UsageCost("unknown-model", u), claudecode.DefaultPricer("unknown-model", u); !approx(got, want) {
		t.Errorf("unknown model cost %v, want %v", got, want)
	}
}

func TestClaudeCodeUsage_NilEngine(t *testing.T) {
	srv := &Server{}
	srv.StartClaudeCodeUsage(context.Background())
	srv.recordClaudeCodeUsage("acc", "s", "claude-sonnet-5", ccusage.OriginProxy, claudecode.Usage{Input: 1})
	if srv.ClaudeCodeUsage() != nil {
		t.Error("nil engine reported")
	}
	if err := srv.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestClaudeCodeUsageAnchors(t *testing.T) {
	reset := time.Date(2026, 9, 23, 12, 10, 0, 0, time.UTC)
	got := claudeCodeUsageAnchors(&claudecode.Unified{FiveHour: claudecode.UnifiedWindow{Reset: reset}})
	if len(got) != 1 || !got[0].Equal(time.Date(2026, 9, 23, 7, 10, 0, 0, time.UTC)) {
		t.Errorf("anchors = %v", got)
	}
	if claudeCodeUsageAnchors(nil) != nil || claudeCodeUsageAnchors(&claudecode.Unified{}) != nil {
		t.Error("anchors without a 5h reset")
	}
}

// TestClaudeCodeUsage_ProxiedResponseReachesLedger is the end-to-end path: a
// Claude Code response served by the proxy becomes a ledger line and shows
// up in the account's snapshot.
func TestClaudeCodeUsage_ProxiedResponseReachesLedger(t *testing.T) {
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", t.TempDir())
	ledgerDir := t.TempDir()
	en := newTestCCUsage(t, claudecode.UsageConfig{LedgerDir: ledgerDir})

	body := `{"id":"msg_01E2E","type":"message","model":"claude-sonnet-5-20260101","content":[{"type":"text","text":"hi"}],` +
		`"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":1000,"cache_creation_input_tokens":300,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200},` +
		`"iterations":[{"type":"message","input_tokens":100,"output_tokens":50},{"type":"advisor_message","model":"claude-opus-5","input_tokens":7,"output_tokens":3}]}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "req_011E2E")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	cfg := ccTestConfig(upstream.URL)
	ccResetPool()
	defer ccResetPool()
	getOrCreateCCPool(cfg)

	reqBody := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv := &Server{ccUsage: en}
	srv.forwardToClaudeCode(w, req, cfg, []byte(reqBody), "claude-sonnet-5")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	p := claudecode.GetModelPricing("claude-sonnet-5")
	wantCost := 100*p.Prompt + 50*p.Completion + 100*p.CacheWrite + 200*2*p.Prompt + 1000*p.CacheRead

	now := time.Now()
	anchors := claudeCodeUsageAnchors(&claudecode.Unified{FiveHour: claudecode.UnifiedWindow{Reset: now.Add(4 * time.Hour)}})
	snap := en.Snapshot("acc1", now, anchors)
	if snap.Active == nil || !snap.Active.Anchored {
		t.Fatalf("no anchored active block: %+v", snap.Active)
	}
	if snap.Window5h.Entries != 2 || snap.Window5h.Tokens != 1450+10 {
		t.Errorf("5h window = %+v", snap.Window5h)
	}
	opus := claudecode.GetModelPricing("claude-opus-5")
	if want := wantCost + 7*opus.Prompt + 3*opus.Completion; !approx(snap.Window5h.CostUSD, want) {
		t.Errorf("5h cost = %v, want %v", snap.Window5h.CostUSD, want)
	}

	if err := en.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	path := filepath.Join(ledgerDir, "projects", "acc1", now.UTC().Format("2006-01-02")+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ledger file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("ledger lines = %d:\n%s", len(lines), data)
	}
	for _, want := range []string{`"requestId":"req_011E2E"`, `"id":"msg_01E2E"`, `"model":"claude-sonnet-5-20260101"`, `"accountId":"acc1"`, `"source":"proxy"`, `"ephemeral_1h_input_tokens":200`} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("ledger line lacks %s: %s", want, lines[0])
		}
	}
	if !strings.Contains(lines[1], `"id":"msg_01E2E:advisor:0"`) {
		t.Errorf("advisor line = %s", lines[1])
	}
	entries, err := ccusage.ReadLedgerFile(path)
	if err != nil || len(entries) != 2 || entries[0].CostUSD == nil || !approx(*entries[0].CostUSD, wantCost) {
		t.Fatalf("ledger read back: %+v, %v", entries, err)
	}
}

func TestClaudeCodeUsage_CacheBumpRecorded(t *testing.T) {
	var withID bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if withID {
			w.Header().Set("request-id", "req_011Bump")
			_, _ = w.Write([]byte(`{"id":"msg_01Bump","type":"message","model":"claude-sonnet-5","usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":5000}}`))
			return
		}
		_, _ = w.Write([]byte(`{"type":"message","usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":4000}}`))
	}))
	defer upstream.Close()
	cacheBumpTestConfig(t, upstream.URL)
	resetCCPoolForTest()
	defer resetCCPoolForTest()
	en := newTestCCUsage(t, claudecode.UsageConfig{})
	srv := &Server{ccUsage: en}

	rec := cachebump.Record{SessionID: "sess", AccountID: "acc-a", Model: "claude-sonnet-5", Body: []byte(`{"model":"claude-sonnet-5","max_tokens":1,"messages":[]}`)}
	for _, id := range []bool{true, false} {
		withID = id
		res, err := srv.sendClaudeCodeBump(context.Background(), rec)
		if err != nil {
			t.Fatalf("bump: %v", err)
		}
		if res.CacheReadTokens == 0 {
			t.Errorf("bump result = %+v", res)
		}
	}

	got := en.Entries()
	if len(got) != 2 {
		t.Fatalf("entries = %+v", got)
	}
	for _, e := range got {
		if e.Origin != ccusage.OriginCacheBump || e.AccountID != "acc-a" || e.SessionID != "sess" {
			t.Errorf("bump entry = %+v", e)
		}
	}
	if got[0].MessageID != "msg_01Bump" || got[0].RequestID != "req_011Bump" {
		t.Errorf("bump ids = %q %q", got[0].MessageID, got[0].RequestID)
	}
	if !strings.HasPrefix(got[1].MessageID, "bump:acc-a:") || got[1].Model != "claude-sonnet-5" {
		t.Errorf("synthetic bump entry = %q model %q", got[1].MessageID, got[1].Model)
	}
}
