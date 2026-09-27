package ccusage

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// ledgerLines renders entries as ledger lines.
func ledgerLines(t *testing.T, entries ...Entry) string {
	t.Helper()
	var out []byte
	for _, e := range entries {
		line, err := MarshalLedgerLine(e)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, line...)
	}
	return string(out)
}

func historyEntry(acct string, ts time.Time, n int) Entry {
	cost := float64(n)
	return Entry{
		Timestamp: ts, SessionID: "s", RequestID: fmt.Sprintf("req_h%d", n), MessageID: fmt.Sprintf("msg_h%d", n),
		Model: "claude-sonnet-5", Input: int64(n), CostUSD: &cost, AccountID: acct, Origin: OriginProxy,
	}
}

func TestEngine_ReportEntriesReadsLedgerHistory(t *testing.T) {
	f := newEngineFixture(t)
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	f.clock.Set(now)
	cutoff := now.Add(-DefaultEngineWindow)

	old := historyEntry("acct", now.AddDate(0, 0, -20), 1)
	before := historyEntry("acct", cutoff.Add(-time.Minute), 2)
	after := historyEntry("acct", cutoff.Add(time.Minute), 4)
	recent := historyEntry("", now.Add(-time.Hour), 8)
	day := func(ts time.Time) string { return ts.UTC().Format("2006-01-02") + ".jsonl" }
	projects := filepath.Join(f.ledgerRoot, "projects")
	// The old day repeats its line, which must dedupe.
	writeFile(t, filepath.Join(projects, "acct", day(old.Timestamp)), ledgerLines(t, old, old))
	writeFile(t, filepath.Join(projects, "acct", day(cutoff)), ledgerLines(t, before, after))
	writeFile(t, filepath.Join(projects, unattributedDir, day(now)), ledgerLines(t, recent))

	en := f.engine(t, nil)
	en.Refresh()

	sum := func(entries []Entry) (total float64) {
		for _, e := range entries {
			total += *e.CostUSD
		}
		return total
	}
	if got := sum(en.Entries()); got != 12 {
		t.Fatalf("window entries cost %v, want 12", got)
	}

	tests := []struct {
		since        string
		want         float64
		partialLocal bool
	}{
		{"", 15, true},
		{"20260720", 15, true},
		{"20260801", 14, true},
		{"20260806", 14, true},
		{"20260807", 12, false},
	}
	for _, tt := range tests {
		entries, windowStart, partialLocal := en.ReportEntries(tt.since, time.UTC)
		if got := sum(entries); got != tt.want {
			t.Errorf("since %q: cost %v, want %v", tt.since, got, tt.want)
		}
		if !windowStart.Equal(cutoff) || partialLocal != tt.partialLocal {
			t.Errorf("since %q: windowStart %v partialLocal %v", tt.since, windowStart, partialLocal)
		}
		for i := 1; i < len(entries); i++ {
			if entries[i].Timestamp.Before(entries[i-1].Timestamp) {
				t.Errorf("since %q: entries out of order", tt.since)
			}
		}
	}

	noLocal := f.engine(t, func(o *EngineOptions) { o.ScanLocalLogs = false })
	if _, _, partial := noLocal.ReportEntries("", time.UTC); partial {
		t.Error("partialLocal without local scanning")
	}
}

func TestEngine_ReloadCoalesces(t *testing.T) {
	f := newEngineFixture(t)
	en := f.engine(t, nil)
	first := en.Reload()
	second := en.Reload()
	if first != second {
		select {
		case <-first:
		default:
			t.Error("a second reload started while the first was running")
		}
	}
	<-second
	<-first

	var nilEngine *Engine
	select {
	case <-nilEngine.Reload():
	default:
		t.Error("nil engine reload channel is not closed")
	}

	// Close cancels and waits for a reload in flight.
	en.Reload()
	if err := en.Close(); err != nil {
		t.Fatal(err)
	}
}
