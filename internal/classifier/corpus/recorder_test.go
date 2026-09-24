package corpus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readRows(t *testing.T, dir string) []Entry {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "classifier-*.jsonl"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var rows []Entry
	for _, path := range matches {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
			if line == "" {
				continue
			}
			var entry Entry
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("unmarshal row %q: %v", line, err)
			}
			rows = append(rows, entry)
		}
	}
	return rows
}

func tapWithBody(t *testing.T, body string) *ResponseTap {
	t.Helper()
	tap := NewResponseTap(httptest.NewRecorder())
	if _, err := tap.Write([]byte(body)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return tap
}

func TestBuildEntryPopulatesEveryField(t *testing.T) {
	tap := tapWithBody(t, `{"content":[{"type":"text","text":"<thinking>Routine.</thinking><severity>8</severity>"}]}`)
	entry := BuildEntry(EntryInput{
		RawBody:        []byte(stage2Body),
		Kind:           "stage2-severity",
		Model:          "claude-opus-4-5",
		Source:         SourceUpstream,
		Tap:            tap,
		ContextEntries: 2,
	})

	if entry.Version != 1 {
		t.Errorf("Version = %d, want 1", entry.Version)
	}
	if entry.Kind != "stage2-severity" {
		t.Errorf("Kind = %q", entry.Kind)
	}
	if entry.Model != "claude-opus-4-5" {
		t.Errorf("Model = %q", entry.Model)
	}
	if entry.Action != `{"Bash":"rm -rf build/"}` {
		t.Errorf("Action = %q", entry.Action)
	}
	if len(entry.Context) != 2 {
		t.Errorf("Context has %d entries, want 2", len(entry.Context))
	}
	if entry.Severity != 8 {
		t.Errorf("Severity = %d, want 8", entry.Severity)
	}
	if entry.Thinking != "Routine." {
		t.Errorf("Thinking = %q", entry.Thinking)
	}
	if entry.Category != "" {
		t.Errorf("Category = %q, want empty on an allow verdict", entry.Category)
	}
	if entry.Status != 200 {
		t.Errorf("Status = %d, want 200", entry.Status)
	}
	if entry.Source != SourceUpstream {
		t.Errorf("Source = %q", entry.Source)
	}
	if entry.Truncated {
		t.Error("Truncated = true, want false")
	}
	if len(entry.SystemSHA256) != 64 {
		t.Errorf("SystemSHA256 = %q", entry.SystemSHA256)
	}
	if entry.Timestamp == "" {
		t.Error("Timestamp is empty")
	}
}

func TestBuildEntryUnparseableRequest(t *testing.T) {
	tap := tapWithBody(t, "<block>true</block>")
	entry := BuildEntry(EntryInput{
		RawBody: []byte(`{"messages":[]}`),
		Kind:    "block-prefilter",
		Model:   "gemini-3.8-flash-medium",
		Source:  SourceUpstream,
		Tap:     tap,
	})
	if entry.Action != "" {
		t.Errorf("Action = %q, want empty when the transcript is unreadable", entry.Action)
	}
	if entry.Severity != -1 {
		t.Errorf("Severity = %d, want -1", entry.Severity)
	}
	if entry.VerdictRaw != "<block>true</block>" {
		t.Errorf("VerdictRaw = %q, want the captured bytes preserved", entry.VerdictRaw)
	}
}

func TestRecorderDisabledWritesNothing(t *testing.T) {
	dir := t.TempDir()
	recorder := New("", Options{MaxFiles: 8, MaxFileBytes: 1 << 20})
	if recorder.Enabled() {
		t.Fatal("Enabled = true for an empty dir")
	}
	recorder.Record(Entry{Version: 1, Action: "x"})

	matches, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(matches) != 0 {
		t.Errorf("a disabled recorder created %v", matches)
	}
}

func TestRecorderNilIsSafe(t *testing.T) {
	var recorder *Recorder
	if recorder.Enabled() {
		t.Error("Enabled = true on a nil recorder")
	}
	recorder.Record(Entry{Version: 1}) // must not panic
}

func TestRecorderWritesOneLinePerRow(t *testing.T) {
	dir := t.TempDir()
	recorder := New(dir, Options{MaxFiles: 8, MaxFileBytes: 1 << 20})
	recorder.Record(Entry{Version: 1, Action: "first", Severity: 1, Source: SourceUpstream})
	recorder.Record(Entry{Version: 1, Action: "second", Severity: 2, Source: SourceStub})

	rows := readRows(t, dir)
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].Action != "first" || rows[1].Action != "second" {
		t.Errorf("rows out of order: %q then %q", rows[0].Action, rows[1].Action)
	}
	if rows[1].Source != SourceStub {
		t.Errorf("Source = %q, want stub", rows[1].Source)
	}
}

func TestRecorderCreatesFilesWithRestrictivePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "corpus")
	recorder := New(dir, Options{MaxFiles: 8, MaxFileBytes: 1 << 20})
	recorder.Record(Entry{Version: 1, Action: "x"})

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0o700 {
		t.Errorf("directory mode = %o, want 700", mode)
	}

	matches, _ := filepath.Glob(filepath.Join(dir, "classifier-*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("got %d files, want 1", len(matches))
	}
	fileInfo, err := os.Stat(matches[0])
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if mode := fileInfo.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o, want 600", mode)
	}
}

func TestRecorderDropsRowsPastTheSizeCap(t *testing.T) {
	dir := t.TempDir()
	recorder := New(dir, Options{MaxFiles: 8, MaxFileBytes: 200})
	for i := 0; i < 20; i++ {
		recorder.Record(Entry{Version: 1, Action: fmt.Sprintf("row-%02d-%s", i, strings.Repeat("x", 40))})
	}

	matches, _ := filepath.Glob(filepath.Join(dir, "classifier-*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("got %d files, want 1", len(matches))
	}
	info, err := os.Stat(matches[0])
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// The cap is checked before each write, so the file may overshoot by at
	// most one row; it must not grow without bound.
	if info.Size() > 200+300 {
		t.Errorf("file grew to %d bytes despite a 200 byte cap", info.Size())
	}
	if recorder.Dropped() == 0 {
		t.Error("Dropped = 0, want the skipped rows counted")
	}
}

func TestRecorderPrunesToMaxFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Names sort lexicographically, which for ISO dates is chronological.
	for _, day := range []string{"2026-09-01", "2026-09-02", "2026-09-03", "2026-09-04"} {
		path := filepath.Join(dir, "classifier-"+day+".jsonl")
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}

	recorder := New(dir, Options{MaxFiles: 2, MaxFileBytes: 1 << 20})
	recorder.Record(Entry{Version: 1, Action: "today"})

	matches, _ := filepath.Glob(filepath.Join(dir, "classifier-*.jsonl"))
	if len(matches) > 2 {
		t.Errorf("got %d files after prune, want at most 2: %v", len(matches), matches)
	}
	today := filepath.Join(dir, "classifier-"+time.Now().UTC().Format("2006-01-02")+".jsonl")
	if _, err := os.Stat(today); err != nil {
		t.Errorf("today's file was pruned: %v", err)
	}
}

// TestRecorderLogsEachPrunedFile pins that retention never deletes a day file
// in silence: those rows cost upstream quota to collect.
func TestRecorderLogsEachPrunedFile(t *testing.T) {
	var logBuffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	dir := t.TempDir()
	var seeded []string
	for _, day := range []string{"2026-09-01", "2026-09-02", "2026-09-03"} {
		path := filepath.Join(dir, "classifier-"+day+".jsonl")
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
		seeded = append(seeded, path)
	}

	// Two slots, one reserved for today's new file: the two oldest go.
	recorder := New(dir, Options{MaxFiles: 2, MaxFileBytes: 1 << 20})
	recorder.Record(Entry{Version: 1, Action: "today"})

	logged := logBuffer.String()
	for _, path := range seeded[:2] {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s still exists, want it pruned", path)
		}
		if !strings.Contains(logged, path) {
			t.Errorf("no log line names pruned file %s; log:\n%s", path, logged)
		}
	}
	if strings.Contains(logged, seeded[2]) {
		t.Errorf("log names the kept file %s; log:\n%s", seeded[2], logged)
	}
	if got := strings.Count(logged, "level=WARN"); got != 2 {
		t.Errorf("got %d warnings, want one per pruned file; log:\n%s", got, logged)
	}
}

func TestRecorderRedactsHomeDirectory(t *testing.T) {
	dir := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this platform")
	}

	recorder := New(dir, Options{MaxFiles: 8, MaxFileBytes: 1 << 20, RedactPaths: true})
	recorder.Record(Entry{
		Version:    1,
		Action:     `{"Bash":"cat ` + home + `/.ssh/config"}`,
		Context:    []string{`{"user":"look in ` + home + `/notes"}`},
		VerdictRaw: `<thinking>Reads ` + home + `/.ssh/config.</thinking><severity>3</severity>`,
		Thinking:   `Reads ` + home + `/.ssh/config.`,
	})

	rows := readRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if strings.Contains(rows[0].Action, home) {
		t.Errorf("Action still carries the home path: %q", rows[0].Action)
	}
	if !strings.Contains(rows[0].Action, "~/.ssh/config") {
		t.Errorf("Action = %q, want the home prefix replaced with ~", rows[0].Action)
	}
	if strings.Contains(rows[0].Context[0], home) {
		t.Errorf("Context still carries the home path: %q", rows[0].Context[0])
	}
	// The verdict and its rationale quote the command under judgement, so they
	// carry absolute paths just as often as the action does.
	if strings.Contains(rows[0].VerdictRaw, home) {
		t.Errorf("VerdictRaw still carries the home path: %q", rows[0].VerdictRaw)
	}
	if strings.Contains(rows[0].Thinking, home) {
		t.Errorf("Thinking still carries the home path: %q", rows[0].Thinking)
	}
}

func TestRecorderZeroMaxFileBytesMeansNoLimit(t *testing.T) {
	dir := t.TempDir()
	// An unset MaxFileBytes means no limit, matching MaxFiles <= 0.
	recorder := New(dir, Options{MaxFiles: 8})
	for i := 0; i < 20; i++ {
		recorder.Record(Entry{Version: 1, Action: fmt.Sprintf("row-%02d-%s", i, strings.Repeat("x", 40))})
	}

	rows := readRows(t, dir)
	if len(rows) != 20 {
		t.Errorf("got %d rows, want all 20 written when no cap is set", len(rows))
	}
	if recorder.Dropped() != 0 {
		t.Errorf("Dropped = %d, want 0 when no cap is set", recorder.Dropped())
	}
}

func TestRecorderRedactionOffLeavesPathsIntact(t *testing.T) {
	dir := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this platform")
	}

	recorder := New(dir, Options{MaxFiles: 8, MaxFileBytes: 1 << 20, RedactPaths: false})
	recorder.Record(Entry{Version: 1, Action: `{"Bash":"cat ` + home + `/x"}`})

	rows := readRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if !strings.Contains(rows[0].Action, home) {
		t.Errorf("Action = %q, want the path untouched when redaction is off", rows[0].Action)
	}
}
