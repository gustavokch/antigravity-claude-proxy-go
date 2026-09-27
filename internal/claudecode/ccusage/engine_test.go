package ccusage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// engineFixture is a ledger root and a Claude config dir under t.TempDir.
type engineFixture struct {
	ledgerRoot string
	claudeDir  string
	clock      *testClock
}

func newEngineFixture(t *testing.T) *engineFixture {
	t.Helper()
	dir := t.TempDir()
	f := &engineFixture{
		ledgerRoot: filepath.Join(dir, "ledger"),
		claudeDir:  filepath.Join(dir, "claude"),
		clock:      &testClock{t: time.Now().UTC().Truncate(time.Millisecond)},
	}
	if err := os.MkdirAll(filepath.Join(f.claudeDir, "projects"), 0o700); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *engineFixture) options() EngineOptions {
	return EngineOptions{
		LedgerRoot:    f.ledgerRoot,
		ScanLocalLogs: true,
		LocalPaths:    func() []string { return []string{f.claudeDir} },
		Now:           f.clock.Now,
		Location:      time.UTC,
	}
}

func (f *engineFixture) engine(t *testing.T, mutate func(*EngineOptions)) *Engine {
	t.Helper()
	opts := f.options()
	if mutate != nil {
		mutate(&opts)
	}
	en, err := NewEngine(opts)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { en.Close() })
	return en
}

// transcriptLine renders a Claude Code transcript line. Empty requestID or
// model leave the field out.
func transcriptLine(ts time.Time, requestID, messageID, model string, input, output int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"type":"assistant","timestamp":%q,"sessionId":"sess-1"`, ts.UTC().Format("2006-01-02T15:04:05.000Z07:00"))
	if requestID != "" {
		fmt.Fprintf(&b, `,"requestId":%q`, requestID)
	}
	fmt.Fprintf(&b, `,"message":{"id":%q`, messageID)
	if model != "" {
		fmt.Fprintf(&b, `,"model":%q`, model)
	}
	fmt.Fprintf(&b, `,"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`, input, output)
	return b.String() + "\n"
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func (f *engineFixture) transcript(name string) string {
	return filepath.Join(f.claudeDir, "projects", "proj", name+".jsonl")
}

func (f *engineFixture) ledgerFile(t *testing.T, accountID string, ts time.Time, entries ...Entry) {
	t.Helper()
	var b strings.Builder
	for _, e := range entries {
		line, err := MarshalLedgerLine(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
	}
	writeFile(t, filepath.Join(f.ledgerRoot, "projects", LedgerAccountDir(accountID), ts.UTC().Format("2006-01-02")+".jsonl"), b.String())
}

func messageIDs(entries []Entry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.MessageID)
	}
	return out
}

func TestAdmitLocal(t *testing.T) {
	hex32 := strings.Repeat("0123456789abcdef", 2)
	cases := []struct {
		name  string
		entry Entry
		want  bool
	}{
		{"anthropic", Entry{RequestID: "req_011CX", MessageID: "msg_01ABCdef", Model: "claude-sonnet-5"}, true},
		{"no request id", Entry{MessageID: "msg_01ABCdef", Model: "claude-sonnet-5"}, false},
		{"foreign request id", Entry{RequestID: "gen-123", MessageID: "msg_01ABCdef", Model: "claude-sonnet-5"}, false},
		{"translated id", Entry{RequestID: "req_011CX", MessageID: "msg_" + hex32, Model: "claude-sonnet-5"}, false},
		{"hex id of other length", Entry{RequestID: "req_011CX", MessageID: "msg_" + hex32 + "0", Model: "claude-sonnet-5"}, true},
		{"zen id", Entry{RequestID: "req_011CX", MessageID: "msg_zen_abc", Model: "claude-sonnet-5"}, false},
		{"classifier id", Entry{RequestID: "req_011CX", MessageID: "msg_clf_abc", Model: "claude-sonnet-5"}, false},
		{"stub id", Entry{RequestID: "req_011CX", MessageID: "msg_stub_abc", Model: "claude-sonnet-5"}, false},
		{"other model", Entry{RequestID: "req_011CX", MessageID: "msg_01ABCdef", Model: "kimi-k2"}, false},
		{"synthetic model", Entry{RequestID: "req_011CX", MessageID: "msg_01ABCdef", Model: "<synthetic>"}, false},
	}
	for _, tc := range cases {
		if got := AdmitLocal(tc.entry); got != tc.want {
			t.Errorf("%s: AdmitLocal = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestEngine_LocalAdmission mixes Anthropic-served transcript lines with the
// lines Claude Code writes for turns this proxy translated from another
// gateway: proxy-made msg_<32 hex> IDs without a requestId.
func TestEngine_LocalAdmission(t *testing.T) {
	f := newEngineFixture(t)
	ts := f.clock.Now().Add(-time.Hour)
	hex32 := strings.Repeat("a1b2c3d4", 4)
	writeFile(t, f.transcript("sess-1"),
		transcriptLine(ts, "req_011CAnthropicA", "msg_01AnthropicA", "claude-sonnet-5", 10, 20)+
			transcriptLine(ts.Add(time.Second), "", "msg_"+hex32, "claude-sonnet-5", 1000, 2000)+
			transcriptLine(ts.Add(2*time.Second), "", "msg_zen_k1", "claude-sonnet-5", 1000, 2000)+
			`{"not":"usage"}`+"\n"+
			transcriptLine(ts.Add(3*time.Second), "req_011CAnthropicB", "msg_01AnthropicB", "claude-opus-5", 30, 40)+
			transcriptLine(ts.Add(4*time.Second), "req_011CKimi", "msg_01Kimi", "kimi-k2", 1000, 2000))
	en := f.engine(t, nil)
	en.Refresh()

	got := en.Entries()
	if ids := messageIDs(got); len(ids) != 2 || ids[0] != "msg_01AnthropicA" || ids[1] != "msg_01AnthropicB" {
		t.Fatalf("admitted = %v", ids)
	}
	for _, e := range got {
		if e.Source != SourceLocal || e.AccountID != "" || e.Inferred {
			t.Errorf("entry %s: source=%q account=%q inferred=%v", e.MessageID, e.Source, e.AccountID, e.Inferred)
		}
		if e.Project != "proj" {
			t.Errorf("entry %s: project %q", e.MessageID, e.Project)
		}
	}
}

func TestEngine_LocalScanDisabled(t *testing.T) {
	f := newEngineFixture(t)
	writeFile(t, f.transcript("sess-1"), transcriptLine(f.clock.Now().Add(-time.Hour), "req_011CA", "msg_01A", "claude-sonnet-5", 1, 2))
	en := f.engine(t, func(o *EngineOptions) { o.ScanLocalLogs = false })
	en.Refresh()
	if n := len(en.Entries()); n != 0 {
		t.Errorf("entries = %d with local scanning off", n)
	}
}

func TestEngine_LedgerBeatsLocal(t *testing.T) {
	for _, order := range []string{"ledger file first", "local first then Record"} {
		t.Run(order, func(t *testing.T) {
			f := newEngineFixture(t)
			ts := f.clock.Now().Add(-time.Hour)
			// Claude Code writes a partial output count; the ledger has the
			// final usage and the account.
			writeFile(t, f.transcript("sess-1"), transcriptLine(ts, "req_011CShared", "msg_01Shared", "claude-sonnet-5", 10, 5))
			ledgerEntry := Entry{
				Timestamp: ts, SessionID: "sess-1", RequestID: "req_011CShared", MessageID: "msg_01Shared",
				Model: "claude-sonnet-5", Input: 10, Output: 90, AccountID: "acct-a",
			}
			en := f.engine(t, func(o *EngineOptions) {
				o.Accounts = func() []AccountRef { return []AccountRef{{ID: "auto", AutoImport: true}} }
			})
			if order == "ledger file first" {
				f.ledgerFile(t, "acct-a", ts, ledgerEntry)
				en.Refresh()
			} else {
				en.Refresh()
				en.Record(ledgerEntry)
				en.Refresh()
			}
			got := en.Entries()
			if len(got) != 1 {
				t.Fatalf("entries = %+v", got)
			}
			e := got[0]
			if e.Source != SourceLedger || e.AccountID != "acct-a" || e.Output != 90 || e.Inferred {
				t.Errorf("survivor = source %q account %q output %d inferred %v", e.Source, e.AccountID, e.Output, e.Inferred)
			}
		})
	}
}

func TestEngine_LocalAttribution(t *testing.T) {
	cases := []struct {
		name         string
		localAccount string
		accounts     []AccountRef
		want         string
		inferred     bool
	}{
		{"single auto import", "", []AccountRef{{ID: "manual"}, {ID: "auto", AutoImport: true}}, "auto", true},
		{"configured account wins", "configured", []AccountRef{{ID: "auto", AutoImport: true}}, "configured", false},
		{"two auto imports", "", []AccountRef{{ID: "a", AutoImport: true}, {ID: "b", AutoImport: true}}, "", false},
		{"no auto import", "", []AccountRef{{ID: "manual"}}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEngineFixture(t)
			now := f.clock.Now()
			writeFile(t, f.transcript("sess-1"), transcriptLine(now.Add(-time.Hour), "req_011CDirect", "msg_01Direct", "claude-sonnet-5", 100, 200))
			en := f.engine(t, func(o *EngineOptions) {
				o.LocalAccountID = tc.localAccount
				o.Accounts = func() []AccountRef { return tc.accounts }
			})
			en.Refresh()
			got := en.Entries()
			if len(got) != 1 || got[0].AccountID != tc.want || got[0].Inferred != tc.inferred {
				t.Fatalf("entries = %+v, want account %q inferred %v", got, tc.want, tc.inferred)
			}
			snap := en.Snapshot(tc.want, now, nil)
			if snap.Window5h.Tokens != 300 || snap.Inferred != tc.inferred {
				t.Errorf("snapshot 5h tokens %d inferred %v", snap.Window5h.Tokens, snap.Inferred)
			}
			if tc.want != "" {
				if other := en.Snapshot("", now, nil); other.Window7d.Entries != 0 {
					t.Errorf("unattributed snapshot has %d entries", other.Window7d.Entries)
				}
			}
		})
	}
}

func TestEngine_TailPartialLinesAndTruncation(t *testing.T) {
	f := newEngineFixture(t)
	ts := f.clock.Now().Add(-time.Hour)
	path := f.transcript("sess-1")
	first := transcriptLine(ts, "req_011C1", "msg_011", "claude-sonnet-5", 1, 1)
	second := transcriptLine(ts.Add(time.Second), "req_011C2", "msg_012", "claude-sonnet-5", 2, 2)
	writeFile(t, path, first+second[:20])

	en := f.engine(t, nil)
	en.Refresh()
	if ids := messageIDs(en.Entries()); len(ids) != 1 || ids[0] != "msg_011" {
		t.Fatalf("after partial write: %v", ids)
	}

	appendFile(t, path, second[20:])
	en.Refresh()
	if ids := messageIDs(en.Entries()); len(ids) != 2 {
		t.Fatalf("after completing the line: %v", ids)
	}
	en.Refresh()
	if n := len(en.Entries()); n != 2 {
		t.Fatalf("idle refresh changed entries: %d", n)
	}

	// The file is rewritten shorter: it is read again from the start.
	third := transcriptLine(ts.Add(2*time.Second), "req_011C3", "msg_013", "claude-sonnet-5", 3, 3)
	writeFile(t, path, third)
	en.Refresh()
	if ids := messageIDs(en.Entries()); len(ids) != 3 || ids[2] != "msg_013" {
		t.Fatalf("after truncation: %v", ids)
	}
	if st := en.Stats(); st.Files != 1 || st.Entries != 3 {
		t.Errorf("stats = %+v", st)
	}
}

func TestEngine_WindowPrune(t *testing.T) {
	f := newEngineFixture(t)
	now := f.clock.Now()
	old := now.Add(-DefaultEngineWindow - time.Hour)
	writeFile(t, f.transcript("sess-1"),
		transcriptLine(old, "req_011COld", "msg_01Old", "claude-sonnet-5", 1, 1)+
			transcriptLine(now.Add(-time.Hour), "req_011CNew", "msg_01New", "claude-sonnet-5", 1, 1))
	en := f.engine(t, nil)
	en.Refresh()
	if ids := messageIDs(en.Entries()); len(ids) != 1 || ids[0] != "msg_01New" {
		t.Fatalf("loaded %v", ids)
	}

	// Time moves on: the kept entry ages out and a reread does not bring it
	// back.
	f.clock.Set(now.Add(DefaultEngineWindow))
	appendFile(t, f.transcript("sess-1"), transcriptLine(now.Add(DefaultEngineWindow-time.Minute), "req_011CLater", "msg_01Later", "claude-sonnet-5", 1, 1))
	en.Refresh()
	writeFile(t, f.transcript("sess-1"), transcriptLine(now.Add(-time.Hour), "req_011CNew", "msg_01New", "claude-sonnet-5", 1, 1))
	en.Refresh()
	if ids := messageIDs(en.Entries()); len(ids) != 1 || ids[0] != "msg_01Later" {
		t.Fatalf("after the window moved: %v", ids)
	}
}

func TestEngine_RecordWritesLedgerAndSnapshot(t *testing.T) {
	f := newEngineFixture(t)
	now := f.clock.Now()
	en := f.engine(t, func(o *EngineOptions) {
		o.Pricer = PricerFunc(func(string) (ModelPrice, bool) {
			return ModelPrice{Input: 1e-6, Output: 2e-6}, true
		})
		o.CostMode = CostModeCalculate
	})
	en.Record(Entry{Timestamp: now.Add(-10 * time.Minute), SessionID: "s", RequestID: "req_011CR", MessageID: "msg_01R", Model: "claude-sonnet-5", Input: 100, Output: 50, AccountID: "acct-a"})
	en.Record(Entry{Timestamp: now.Add(-5 * time.Minute), Model: "claude-sonnet-5", CacheRead: 1000, AccountID: "acct-a", Origin: OriginCacheBump})

	snap := en.Snapshot("acct-a", now, nil)
	if snap.Active == nil || snap.Window5h.Entries != 2 || snap.Window5h.Tokens != 1150 {
		t.Fatalf("snapshot 5h = %+v active=%v", snap.Window5h, snap.Active != nil)
	}
	if want := 100e-6 + 100e-6; !approxEqual(snap.Window5h.CostUSD, want) || !approxEqual(snap.Today.CostUSD, want) {
		t.Errorf("costs 5h %v today %v, want %v", snap.Window5h.CostUSD, snap.Today.CostUSD, want)
	}
	if len(snap.Models) != 1 || snap.Models[0].TotalTokens != 1150 {
		t.Errorf("models = %+v", snap.Models)
	}

	// Reading the ledger back does not double count.
	if err := en.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	en2 := f.engine(t, nil)
	en2.Refresh()
	got := en2.Entries()
	if len(got) != 2 {
		t.Fatalf("ledger read back %d entries: %+v", len(got), got)
	}
	if got[1].Origin != OriginCacheBump || !strings.HasPrefix(got[1].MessageID, "bump:acct-a:") {
		t.Errorf("bump entry = origin %q id %q", got[1].Origin, got[1].MessageID)
	}
	en2.Record(got[1])
	if n := len(en2.Entries()); n != 2 {
		t.Errorf("recording a read-back entry again gave %d entries", n)
	}
}

func approxEqual(a, b float64) bool {
	d := a - b
	return d < 1e-12 && d > -1e-12
}

func TestEngine_SnapshotAnchorsSummaryAndCache(t *testing.T) {
	f := newEngineFixture(t)
	now := f.clock.Now()
	en := f.engine(t, nil)
	// A completed block eight hours ago and an active one now.
	en.Record(Entry{Timestamp: now.Add(-8 * time.Hour), MessageID: "msg_old", RequestID: "req_old", Model: "claude-sonnet-5", Input: 500, AccountID: "a"})
	en.Record(Entry{Timestamp: now.Add(-30 * time.Minute), MessageID: "msg_new", RequestID: "req_new", Model: "claude-sonnet-5", Input: 10, AccountID: "a"})

	anchor := now.Add(-40 * time.Minute)
	snap := en.Snapshot("a", now, []time.Time{anchor})
	if snap.Active == nil || !snap.Active.Anchored || !snap.Active.Start.Equal(anchor.Truncate(time.Millisecond)) {
		t.Fatalf("active block = %+v", snap.Active)
	}
	if snap.Summary.MaxBlockTokens != 500 || len(snap.Summary.Anchors5h) != 1 {
		t.Fatalf("summary = %+v", snap.Summary)
	}

	// Cached for the TTL with the same anchors.
	en.Record(Entry{Timestamp: now.Add(-time.Minute), MessageID: "msg_more", RequestID: "req_more", Model: "claude-sonnet-5", Input: 1, AccountID: "a"})
	if again := en.Snapshot("a", now.Add(time.Second), []time.Time{anchor}); again != snap {
		t.Error("snapshot within the TTL was rebuilt")
	}
	later := en.Snapshot("a", now.Add(DefaultSnapshotTTL), []time.Time{anchor})
	if later == snap || later.Window5h.Entries != 2 {
		t.Errorf("snapshot after the TTL: entries %d", later.Window5h.Entries)
	}

	// The anchor and the max block survive a restart; the remembered anchor
	// still anchors a snapshot taken without one.
	if err := en.Close(); err != nil {
		t.Fatal(err)
	}
	en2 := f.engine(t, nil)
	en2.Refresh()
	s := en2.Summary("a")
	if s.MaxBlockTokens != 500 || len(s.Anchors5h) != 1 {
		t.Fatalf("reloaded summary = %+v", s)
	}
	if snap := en2.Snapshot("a", now, nil); snap.Active == nil || !snap.Active.Anchored {
		t.Errorf("remembered anchor not applied: %+v", snap.Active)
	}

	en2.UpdateSummary("a", func(s *Summary) { s.CalibratedCostUSD5h = 12.5 })
	if got := en2.Summary("a").CalibratedCostUSD5h; got != 12.5 {
		t.Errorf("calibrated = %v", got)
	}
}

func TestMergeAnchors(t *testing.T) {
	base := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	known := []time.Time{base}
	merged, changed := mergeAnchors(known, []time.Time{base.Add(20 * time.Second), base.Add(5 * time.Hour), base.Add(-time.Hour * 100)}, base.Add(-time.Hour))
	if !changed || len(merged) != 2 || !merged[0].Equal(base.Add(20*time.Second)) || !merged[1].Equal(base.Add(5*time.Hour)) {
		t.Fatalf("merged = %v changed %v", merged, changed)
	}
	if _, changed := mergeAnchors(merged, merged, base.Add(-time.Hour)); changed {
		t.Error("merging known anchors reported a change")
	}
}

func TestEngine_SessionDurationMustBeWholeHours(t *testing.T) {
	for _, d := range []time.Duration{90 * time.Minute, 30 * time.Minute, -time.Hour} {
		if _, err := NewEngine(EngineOptions{LedgerRoot: t.TempDir(), SessionDuration: d}); err == nil {
			t.Errorf("session duration %v accepted", d)
		}
	}
}

func TestEngine_Nil(t *testing.T) {
	var en *Engine
	en.Start(context.Background())
	en.Record(Entry{MessageID: "x"})
	en.Refresh()
	en.UpdateSummary("a", func(*Summary) {})
	if en.Snapshot("a", time.Now(), nil) != nil || en.Entries() != nil || en.Root() != "" {
		t.Error("nil engine returned data")
	}
	if en.Summary("a").MaxBlockTokens != 0 || en.Stats() != (EngineStats{}) {
		t.Error("nil engine returned stats")
	}
	if err := en.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestEngine_StartAndClose(t *testing.T) {
	f := newEngineFixture(t)
	writeFile(t, f.transcript("sess-1"), transcriptLine(f.clock.Now().Add(-time.Hour), "req_011CA", "msg_01A", "claude-sonnet-5", 1, 2))
	before := runtime.NumGoroutine()
	en, err := NewEngine(func() EngineOptions { o := f.options(); o.PollInterval = 10 * time.Millisecond; return o }())
	if err != nil {
		t.Fatal(err)
	}
	en.Start(context.Background())
	en.Start(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for len(en.Entries()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("poller never loaded the transcript")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := en.Close(); err != nil {
		t.Fatal(err)
	}
	if err := en.Close(); err != nil {
		t.Fatal(err)
	}
	en.Start(context.Background()) // after Close: no goroutine
	deadline = time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: %d before, %d after Close", before, runtime.NumGoroutine())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// touchLater moves a file's mtime forward, so a rewrite is visible even on
// file systems with coarse timestamps.
func touchLater(t *testing.T, path string, d time.Duration) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime().Add(d), info.ModTime().Add(d)); err != nil {
		t.Fatal(err)
	}
}

func TestEngine_TailDetectsRewrites(t *testing.T) {
	cases := []struct {
		name    string
		rewrite func(t *testing.T, path, content string)
		grow    bool
	}{
		{"replaced inode", func(t *testing.T, path, content string) {
			tmp := path + ".tmp"
			writeFile(t, tmp, content)
			if err := os.Rename(tmp, path); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"larger in-place rewrite", func(t *testing.T, path, content string) {
			writeFile(t, path, content)
			touchLater(t, path, 2*time.Second)
		}, true},
		{"same-size in-place rewrite", func(t *testing.T, path, content string) {
			writeFile(t, path, content)
			touchLater(t, path, 2*time.Second)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEngineFixture(t)
			ts := f.clock.Now().Add(-time.Hour)
			path := f.transcript("sess-1")
			line := func(i int) string {
				return transcriptLine(ts.Add(time.Duration(i)*time.Second), fmt.Sprintf("req_011C%d", i), fmt.Sprintf("msg_01%d", i), "claude-sonnet-5", 1, 1)
			}
			writeFile(t, path, line(1)+line(2))
			en := f.engine(t, nil)
			en.Refresh()
			if n := len(en.Entries()); n != 2 {
				t.Fatalf("initial entries = %d", n)
			}

			// The same byte count (or more) with other lines: an offset kept
			// from the old file would skip them.
			content := line(3) + line(4)
			if tc.grow {
				content += line(5)
			}
			if !tc.grow && len(content) != len(line(1)+line(2)) {
				t.Fatal("fixture lines differ in length")
			}
			tc.rewrite(t, path, content)
			en.Refresh()
			ids := messageIDs(en.Entries())
			want := 4
			if tc.grow {
				want = 5
			}
			if len(ids) != want || ids[2] != "msg_013" {
				t.Fatalf("after rewrite: %v", ids)
			}
		})
	}
}

func TestEngine_AppendKeepsOffset(t *testing.T) {
	f := newEngineFixture(t)
	ts := f.clock.Now().Add(-time.Hour)
	path := f.transcript("sess-1")
	// A line without a message ID is never deduplicated, so a reread from
	// the start would count it twice.
	noID := fmt.Sprintf(`{"timestamp":%q,"requestId":"req_011CX","message":{"model":"claude-sonnet-5","usage":{"input_tokens":1,"output_tokens":1}}}`+"\n", ts.UTC().Format("2006-01-02T15:04:05.000Z07:00"))
	writeFile(t, path, noID)
	en := f.engine(t, nil)
	en.Refresh()
	appendFile(t, path, transcriptLine(ts.Add(time.Second), "req_011C2", "msg_012", "claude-sonnet-5", 1, 1))
	touchLater(t, path, 2*time.Second)
	en.Refresh()
	if n := len(en.Entries()); n != 2 {
		t.Fatalf("entries after append = %d, want 2", n)
	}
}

func TestEngine_TailStopsOnCancel(t *testing.T) {
	f := newEngineFixture(t)
	ts := f.clock.Now().Add(-time.Hour)
	var b strings.Builder
	for i := range 3 * ctxCheckLines {
		b.WriteString(transcriptLine(ts, fmt.Sprintf("req_011C%d", i), fmt.Sprintf("msg_01%d", i), "claude-sonnet-5", 1, 1))
	}
	writeFile(t, f.transcript("sess-1"), b.String())
	en := f.engine(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	en.refresh(ctx)
	if n := len(en.Entries()); n >= 3*ctxCheckLines {
		t.Fatalf("cancelled refresh read everything (%d)", n)
	}
	// The next pass resumes where the cancelled one stopped.
	en.Refresh()
	if n := len(en.Entries()); n != 3*ctxCheckLines {
		t.Fatalf("entries after resume = %d", n)
	}
}

func TestEngine_DropsStaleLocalFiles(t *testing.T) {
	f := newEngineFixture(t)
	path := f.transcript("sess-1")
	writeFile(t, path, transcriptLine(f.clock.Now().Add(-time.Hour), "req_011CA", "msg_01A", "claude-sonnet-5", 1, 1))
	en := f.engine(t, nil)
	en.Refresh()
	if st := en.Stats(); st.Files != 1 {
		t.Fatalf("files = %d", st.Files)
	}
	old := f.clock.Now().Add(-DefaultEngineWindow - time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	en.Refresh()
	if st := en.Stats(); st.Files != 0 {
		t.Errorf("stale file still tracked: %d", st.Files)
	}
}

func TestEngine_UnattributedSummaryNotSaved(t *testing.T) {
	f := newEngineFixture(t)
	now := f.clock.Now()
	en := f.engine(t, nil)
	en.Record(Entry{Timestamp: now.Add(-8 * time.Hour), MessageID: "m1", RequestID: "r1", Model: "claude-sonnet-5", Input: 5})
	en.Record(Entry{Timestamp: now.Add(-8 * time.Hour), MessageID: "m2", RequestID: "r2", Model: "claude-sonnet-5", Input: 5, AccountID: "a"})
	en.Snapshot("", now, nil)
	en.Snapshot("a", now, nil)
	if err := en.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(f.ledgerRoot, SummaryFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"":`) || !strings.Contains(string(data), `"a":`) {
		t.Errorf("summary file = %s", data)
	}
}
