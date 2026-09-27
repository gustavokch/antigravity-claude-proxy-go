package ccusage

import (
	"path/filepath"
	"testing"
	"time"
)

var dedupeTS = time.UnixMilli(1_775_000_000_000).UTC()

// fixtureEntry mirrors ccusage's loaded_usage_entry test helper.
func fixtureEntry(messageID, requestID string, sidechain bool, cacheRead, output int64) Entry {
	return Entry{
		Timestamp:   dedupeTS,
		SessionID:   "session-a",
		RequestID:   requestID,
		MessageID:   messageID,
		Model:       "claude-sonnet-4-20250514",
		Output:      output,
		CacheRead:   cacheRead,
		IsSidechain: sidechain,
		Source:      SourceLocal,
	}
}

func inSession(e Entry, session string) Entry {
	e.SessionID = session
	return e
}

func parseAll(t *testing.T, sessions map[string][]string, order ...string) []Entry {
	t.Helper()
	var out []Entry
	for _, s := range order {
		meta := FileMetaFor(filepath.FromSlash("/c/projects/project-a/" + s + "/chat.jsonl"))
		for _, line := range sessions[s] {
			for _, e := range ParseLine([]byte(line)) {
				meta.Apply(&e)
				out = append(out, e)
			}
		}
	}
	return out
}

func TestDedupe_RustCases(t *testing.T) {
	tests := []struct {
		name  string
		input []Entry
		check func(t *testing.T, got []Entry)
	}{
		{
			name: "gateway reuse of one message id across sessions is kept",
			input: parseAll(t, map[string][]string{
				"session-a": {`{"timestamp":"2026-05-22T02:34:40.000Z","message":{"id":"ocgo","model":"claude-sonnet-4-20250514","usage":{"input_tokens":100,"output_tokens":1}}}`},
				"session-b": {`{"timestamp":"2026-05-22T02:34:40.000Z","message":{"id":"ocgo","model":"claude-sonnet-4-20250514","usage":{"input_tokens":300,"output_tokens":1}}}`},
			}, "session-a", "session-b"),
			check: func(t *testing.T, got []Entry) {
				if len(got) != 2 || got[0].Input+got[1].Input != 400 {
					t.Errorf("got %d entries, want 2 summing 400 input", len(got))
				}
			},
		},
		{
			name: "requestless usage at distinct timestamps is kept",
			input: parseAll(t, map[string][]string{"session-a": {
				`{"timestamp":"2026-05-22T02:34:40.000Z","message":{"id":"ocgo","model":"claude-sonnet-4-20250514","usage":{"input_tokens":100,"output_tokens":25}}}`,
				`{"timestamp":"2026-05-22T02:34:41.000Z","message":{"id":"ocgo","model":"claude-sonnet-4-20250514","usage":{"input_tokens":100,"output_tokens":250,"speed":"standard"}}}`,
			}}, "session-a"),
			check: func(t *testing.T, got []Entry) {
				if len(got) != 2 || got[0].Output+got[1].Output != 275 {
					t.Errorf("got %+v, want 2 entries summing 275 output", got)
				}
			},
		},
		{
			name: "requestless rewrites at one timestamp collapse to the larger",
			input: parseAll(t, map[string][]string{"session-a": {
				`{"timestamp":"2026-05-22T02:34:40.000Z","message":{"id":"ocgo","model":"claude-sonnet-4-20250514","usage":{"input_tokens":100,"output_tokens":25}}}`,
				`{"timestamp":"2026-05-22T02:34:40.000Z","message":{"id":"ocgo","model":"claude-sonnet-4-20250514","usage":{"input_tokens":100,"output_tokens":250,"speed":"standard"}}}`,
			}}, "session-a"),
			check: func(t *testing.T, got []Entry) {
				if len(got) != 1 || got[0].Output != 250 {
					t.Errorf("got %+v, want one entry with 250 output", got)
				}
			},
		},
		{
			name: "copied transcripts with the same serialized session id",
			input: parseAll(t, map[string][]string{
				"session-a": {`{"timestamp":"2026-05-22T02:34:40.000Z","sessionId":"session-a","message":{"id":"ocgo","model":"claude-sonnet-4-20250514","usage":{"input_tokens":100,"output_tokens":1}}}`},
				"session-b": {`{"timestamp":"2026-05-22T02:34:40.000Z","sessionId":"session-a","message":{"id":"ocgo","model":"claude-sonnet-4-20250514","usage":{"input_tokens":200,"output_tokens":1}}}`},
			}, "session-a", "session-b"),
			check: func(t *testing.T, got []Entry) {
				if len(got) != 1 || got[0].Input != 200 {
					t.Errorf("got %+v, want one entry with 200 input", got)
				}
			},
		},
		{
			name: "copied transcripts with the same request id across sessions",
			input: parseAll(t, map[string][]string{
				"session-a": {`{"timestamp":"2026-05-22T02:34:40.000Z","sessionId":"session-a","requestId":"req-shared","message":{"id":"msg-shared","model":"claude-sonnet-4-20250514","usage":{"input_tokens":100,"output_tokens":1}}}`},
				"session-b": {`{"timestamp":"2026-05-22T02:34:40.000Z","sessionId":"session-b","requestId":"req-shared","message":{"id":"msg-shared","model":"claude-sonnet-4-20250514","usage":{"input_tokens":200,"output_tokens":1}}}`},
			}, "session-a", "session-b"),
			check: func(t *testing.T, got []Entry) {
				if len(got) != 1 || got[0].Input != 200 {
					t.Errorf("got %+v, want one entry with 200 input", got)
				}
			},
		},
		{
			name: "sidechain replay after an equal copy from another session",
			input: []Entry{
				inSession(fixtureEntry("msg-parent", "req-parent", false, 20, 10), "session-b"),
				fixtureEntry("msg-parent", "req-parent", false, 20, 10),
				fixtureEntry("msg-parent", "req-sidechain-replay", true, 50_000, 10),
			},
			check: func(t *testing.T, got []Entry) {
				if len(got) != 1 || got[0].RequestID != "req-parent" || got[0].CacheRead != 20 {
					t.Errorf("got %+v, want only the parent", got)
				}
			},
		},
		{
			name: "sidechain replay after cross-session survivor replacement",
			input: []Entry{
				inSession(fixtureEntry("msg-parent", "req-parent", false, 20, 10), "session-b"),
				fixtureEntry("msg-parent", "req-parent", false, 30, 10),
				inSession(fixtureEntry("msg-parent", "req-sidechain-replay", true, 50_000, 10), "session-b"),
			},
			check: func(t *testing.T, got []Entry) {
				if len(got) != 1 || got[0].RequestID != "req-parent" || got[0].SessionID != "session-a" || got[0].CacheRead != 30 {
					t.Errorf("got %+v, want the session-a parent with 30 cache read", got)
				}
			},
		},
		{
			name: "parent kept when a sidechain replays it with a new request id",
			input: []Entry{
				fixtureEntry("msg-parent", "req-parent", false, 20, 10),
				fixtureEntry("msg-parent", "req-sidechain-replay", true, 50_000, 10),
				fixtureEntry("msg-sidechain-answer", "req-sidechain-answer", true, 700, 30),
			},
			check: func(t *testing.T, got []Entry) {
				if len(got) != 2 || got[0].MessageID != "msg-parent" || got[0].CacheRead != 20 ||
					got[1].MessageID != "msg-sidechain-answer" || got[1].CacheRead != 700 {
					t.Errorf("got %+v, want parent then sidechain answer", got)
				}
			},
		},
		{
			name: "indexes refresh when a parent replaces a sidechain replay",
			input: []Entry{
				fixtureEntry("msg-parent", "req-sidechain-replay", true, 50_000, 10),
				fixtureEntry("msg-parent", "req-parent", false, 20, 10),
				fixtureEntry("msg-parent", "req-parent", false, 5, 5),
			},
			check: func(t *testing.T, got []Entry) {
				if len(got) != 1 || got[0].RequestID != "req-parent" || got[0].CacheRead != 20 {
					t.Errorf("got %+v, want the parent with 20 cache read", got)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.check(t, Dedupe(tt.input))
		})
	}
}

func TestDedupe_WinnerOrder(t *testing.T) {
	base := fixtureEntry("m", "r", false, 10, 10)
	withSpeed := base
	withSpeed.Speed = "standard"
	bigger := base
	bigger.Output = 20
	sidechainBigger := bigger
	sidechainBigger.IsSidechain = true
	ledger := base
	ledger.Source = SourceLedger
	ledger.AccountID = "acct"
	ledger.Output = 1

	tests := []struct {
		name       string
		first      Entry
		second     Entry
		wantOutput int64
		wantSpeed  string
		wantSource string
	}{
		{"more tokens wins", base, bigger, 20, "", SourceLocal},
		{"fewer tokens loses", bigger, base, 20, "", SourceLocal},
		{"speed breaks a tie", base, withSpeed, 10, "standard", SourceLocal},
		{"no speed does not replace", withSpeed, base, 10, "standard", SourceLocal},
		{"non-sidechain beats more tokens", base, sidechainBigger, 10, "", SourceLocal},
		{"ledger beats local with more tokens", bigger, ledger, 1, "", SourceLedger},
		{"local never replaces ledger", ledger, bigger, 1, "", SourceLedger},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Dedupe([]Entry{tt.first, tt.second})
			if len(got) != 1 {
				t.Fatalf("got %d entries, want 1", len(got))
			}
			if got[0].Output != tt.wantOutput || got[0].Speed != tt.wantSpeed || got[0].Source != tt.wantSource {
				t.Errorf("winner = output %d speed %q source %q; want %d %q %q",
					got[0].Output, got[0].Speed, got[0].Source, tt.wantOutput, tt.wantSpeed, tt.wantSource)
			}
		})
	}
}

func TestDeduper_Incremental(t *testing.T) {
	d := NewDeduper()
	small := fixtureEntry("m", "r", false, 0, 5)
	large := fixtureEntry("m", "r", false, 0, 50)

	if replaced, kept := d.Add(small); !kept || replaced != nil {
		t.Errorf("first Add = %v, %v; want nil, true", replaced, kept)
	}
	if replaced, kept := d.Add(large); !kept || replaced == nil || replaced.Output != 5 {
		t.Errorf("larger Add = %v, %v; want the small entry, true", replaced, kept)
	}
	if replaced, kept := d.Add(small); kept || replaced != nil {
		t.Errorf("smaller Add = %v, %v; want nil, false", replaced, kept)
	}
	noID := fixtureEntry("", "", false, 0, 1)
	d.Add(noID)
	d.Add(noID)
	if d.Len() != 3 {
		t.Errorf("Len = %d, want 3 (entries without a message id are always kept)", d.Len())
	}
	entries := d.Entries()
	entries[0].Output = -1
	if d.Entries()[0].Output != 50 {
		t.Errorf("Entries did not return a copy")
	}
}

func TestDedupe_FixtureCopies(t *testing.T) {
	files := UsageFiles([]string{filepath.Join("testdata", "claude")})
	var all []Entry
	for range 2 {
		for _, f := range files {
			entries, err := ReadUsageFile(f)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, entries...)
		}
	}
	if len(all) != 10 {
		t.Fatalf("read %d entries, want 10", len(all))
	}
	got := Dedupe(all)
	if len(got) != 5 {
		t.Errorf("Dedupe kept %d entries, want 5", len(got))
	}
	var total int64
	for _, e := range got {
		total += e.TotalTokens()
	}
	if total != 1240 {
		t.Errorf("total tokens = %d, want 1240", total)
	}
}
