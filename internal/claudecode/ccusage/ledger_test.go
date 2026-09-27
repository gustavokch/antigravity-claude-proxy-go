package ccusage

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

func newTestLedger(t *testing.T, root string, opts LedgerOptions) *Ledger {
	t.Helper()
	if opts.Now == nil {
		opts.Now = fixedNow(time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC))
	}
	l, err := NewLedger(root, opts)
	if err != nil {
		t.Fatalf("NewLedger: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func sampleLedgerEntry() Entry {
	return Entry{
		Timestamp:     time.Date(2026, 9, 27, 10, 11, 12, 345*int(time.Millisecond), time.UTC),
		SessionID:     "sess-1",
		RequestID:     "req_011CX",
		MessageID:     "msg_01ABC",
		Model:         "claude-sonnet-4-20250514",
		Speed:         "fast",
		Input:         120,
		Output:        45,
		CacheCreate:   300,
		CacheCreate5m: 100,
		CacheCreate1h: 200,
		CacheRead:     5000,
		CostUSD:       ptr(0.0123456789),
		AccountID:     "acct-1",
		Origin:        OriginCacheBump,
	}
}

func readLines(t *testing.T, path string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var lines [][]byte
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		lines = append(lines, slices.Clone(sc.Bytes()))
	}
	return lines
}

func TestLedger_RoundTrip(t *testing.T) {
	root := t.TempDir()
	l := newTestLedger(t, root, LedgerOptions{})
	in := sampleLedgerEntry()
	if !l.Append(in) {
		t.Fatal("Append returned false")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	path := filepath.Join(root, "projects", "acct-1", "2026-09-27.jsonl")
	lines := readLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	line := lines[0]
	for _, want := range []string{`"usage":{`, `"timestamp":"2026-09-27T10:11:12.345Z"`, `"source":"cachebump"`, `"accountId":"acct-1"`, `"type":"assistant"`} {
		if !bytes.Contains(line, []byte(want)) {
			t.Errorf("line lacks %s: %s", want, line)
		}
	}
	for _, bad := range []string{"null", `"version"`} {
		if bytes.Contains(line, []byte(bad)) {
			t.Errorf("line contains %s: %s", bad, line)
		}
	}

	want := in
	want.DisplayModel = in.Model + "-fast"
	want.Source = SourceLedger
	want.Project = "acct-1"
	want.PathSessionID = "sess-1"
	want.ProjectPath = "proxy/acct-1"

	got := ParseLedgerLine(line)
	if len(got) != 1 {
		t.Fatalf("ParseLedgerLine returned %d entries", len(got))
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("ParseLedgerLine:\n got %+v\nwant %+v", got[0], want)
	}

	fromFile, err := ReadLedgerFile(path)
	if err != nil || len(fromFile) != 1 || !reflect.DeepEqual(fromFile[0], want) {
		t.Errorf("ReadLedgerFile = %+v, %v; want %+v", fromFile, err, want)
	}

	// ParseLine sees the same usage fields, without the ledger attribution.
	plain := ParseLine(line)
	if len(plain) != 1 {
		t.Fatalf("ParseLine returned %d entries", len(plain))
	}
	p := plain[0]
	if p.SessionID != in.SessionID || p.RequestID != in.RequestID || p.MessageID != in.MessageID ||
		p.Model != in.Model || p.Speed != in.Speed || !p.Timestamp.Equal(in.Timestamp) ||
		p.Input != in.Input || p.Output != in.Output || p.CacheCreate != in.CacheCreate ||
		p.CacheCreate5m != in.CacheCreate5m || p.CacheCreate1h != in.CacheCreate1h ||
		p.CacheRead != in.CacheRead || p.CostUSD == nil || *p.CostUSD != *in.CostUSD {
		t.Errorf("ParseLine = %+v, want fields of %+v", p, in)
	}
}

func TestMarshalLedgerLine_OmitsEmpty(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 2*3600))
	line, err := MarshalLedgerLine(Entry{Timestamp: ts, Input: 1, Output: -5, CacheCreate: 7, Speed: "turbo"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"assistant","timestamp":"2026-01-02T01:04:05.000Z","message":{"usage":{"input_tokens":1,"output_tokens":0,"cache_creation_input_tokens":7,"cache_read_input_tokens":0}},"source":"proxy"}` + "\n"
	if string(line) != want {
		t.Errorf("line =\n%s\nwant\n%s", line, want)
	}
	got := ParseLedgerLine(bytes.TrimSpace(line))
	if len(got) != 1 {
		t.Fatalf("ParseLedgerLine returned %d entries", len(got))
	}
	e := got[0]
	if e.CacheCreate5m != 7 || e.CacheCreate1h != 0 || e.Model != "" || e.Speed != "" || e.CostUSD != nil ||
		e.SessionID != "2026-01-02" || e.PathSessionID != "2026-01-02" || e.ProjectPath != "proxy" ||
		e.Project != unattributedDir || e.Origin != OriginProxy || e.AccountID != "" {
		t.Errorf("entry = %+v", e)
	}
}

func TestLedger_Layout(t *testing.T) {
	root := t.TempDir()
	l := newTestLedger(t, root, LedgerOptions{})
	base := sampleLedgerEntry()
	for i, spec := range []struct {
		acct string
		day  int
	}{{"acct-1", 25}, {"acct-1", 26}, {"acct-1", 26}, {"acct-2", 26}, {"", 27}} {
		e := base
		e.AccountID = spec.acct
		e.MessageID = fmt.Sprintf("msg_%d", i)
		e.Timestamp = time.Date(2026, 9, spec.day, 23, 59, 0, 0, time.UTC)
		l.Append(e)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "projects", "acct-1"))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("account dir mode = %v, %v; want 0700", info, err)
	}
	files := UsageFiles([]string{root})
	var rel []string
	for _, f := range files {
		r, _ := filepath.Rel(root, f)
		rel = append(rel, filepath.ToSlash(r))
		info, err := os.Stat(f)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, %v; want 0600", r, info, err)
		}
	}
	want := []string{
		"projects/_unattributed/2026-09-27.jsonl",
		"projects/acct-1/2026-09-25.jsonl",
		"projects/acct-1/2026-09-26.jsonl",
		"projects/acct-2/2026-09-26.jsonl",
	}
	if !slices.Equal(rel, want) {
		t.Errorf("files = %v, want %v", rel, want)
	}
	if n := len(readLines(t, filepath.Join(root, "projects", "acct-1", "2026-09-26.jsonl"))); n != 2 {
		t.Errorf("acct-1 2026-09-26 lines = %d, want 2", n)
	}
	// ccusage's path parsing would make the date the session; the ledger
	// reader groups by sessionId instead.
	es, err := ReadLedgerFile(filepath.Join(root, "projects", "_unattributed", "2026-09-27.jsonl"))
	if err != nil || len(es) != 1 || es[0].AccountID != "" || es[0].PathSessionID != "sess-1" {
		t.Errorf("unattributed = %+v, %v", es, err)
	}
	if s := l.Stats(); s.Written != 5 || s.Dropped != 0 || s.Errors != 0 {
		t.Errorf("Stats = %+v", s)
	}
}

func TestReadLedgerFile_AccountFromDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "projects", "acct-9")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"timestamp":"2026-09-27T01:00:00Z","requestId":"req_1","message":{"id":"msg_1","model":"claude-x","usage":{"input_tokens":1,"output_tokens":2}}}` + "\n"
	path := filepath.Join(dir, "2026-09-27.jsonl")
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	es, err := ReadLedgerFile(path)
	if err != nil || len(es) != 1 {
		t.Fatalf("ReadLedgerFile = %v, %v", es, err)
	}
	if e := es[0]; e.AccountID != "acct-9" || e.ProjectPath != "proxy/acct-9" || e.Project != "acct-9" || e.Source != SourceLedger {
		t.Errorf("entry = %+v", e)
	}
}

func TestLedger_DropOnOverflow(t *testing.T) {
	root := t.TempDir()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	l := newTestLedger(t, root, LedgerOptions{
		QueueSize: 2,
		writeHook: func(Entry) {
			once.Do(func() {
				started <- struct{}{}
				<-release
			})
		},
	})
	e := sampleLedgerEntry()
	if !l.Append(e) {
		t.Fatal("first Append failed")
	}
	<-started // the writer holds the first entry; the queue is empty
	for i := range 2 {
		if !l.Append(e) {
			t.Fatalf("Append %d into an empty queue failed", i)
		}
	}
	for i := range 3 {
		if l.Append(e) {
			t.Fatalf("Append %d into a full queue succeeded", i)
		}
	}
	close(release)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if s := l.Stats(); s.Written != 3 || s.Dropped != 3 || s.Errors != 0 {
		t.Errorf("Stats = %+v, want 3 written, 3 dropped", s)
	}
	if l.Append(e) {
		t.Error("Append after Close succeeded")
	}
	if s := l.Stats(); s.Dropped != 4 {
		t.Errorf("Dropped after Close = %d, want 4", s.Dropped)
	}
	if err := l.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestLedger_CloseDrains(t *testing.T) {
	root := t.TempDir()
	l := newTestLedger(t, root, LedgerOptions{
		QueueSize: 1000,
		writeHook: func(Entry) { time.Sleep(50 * time.Microsecond) },
	})
	const n = 500
	e := sampleLedgerEntry()
	for i := range n {
		e.MessageID = fmt.Sprintf("msg_%d", i)
		if !l.Append(e) {
			t.Fatalf("Append %d failed", i)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, filepath.Join(root, "projects", "acct-1", "2026-09-27.jsonl"))
	if len(lines) != n {
		t.Fatalf("lines = %d, want %d", len(lines), n)
	}
	for i, line := range lines {
		es := ParseLedgerLine(line)
		if len(es) != 1 || es[0].MessageID != fmt.Sprintf("msg_%d", i) {
			t.Fatalf("line %d = %s", i, line)
		}
	}
}

func TestLedger_Pruning(t *testing.T) {
	root := t.TempDir()
	write := func(rel string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Today is 2026-09-27; with 10 days of retention 2026-09-17 is kept.
	old := []string{"projects/a/2026-09-16.jsonl", "projects/b/2025-01-01.jsonl"}
	kept := []string{
		"projects/a/2026-09-17.jsonl",
		"projects/a/2026-09-27.jsonl",
		"projects/a/notes.jsonl",
		"projects/a/2026-09-16.json",
		"projects/a/2026-9-1.jsonl",
		"projects/a/sub/2020-01-01.jsonl",
		"projects/2020-01-01.jsonl",
		"other/2020-01-01.jsonl",
		"summary.json",
	}
	for _, f := range append(slices.Clone(old), kept...) {
		write(f)
	}
	l := newTestLedger(t, root, LedgerOptions{RetentionDays: 10})
	for _, f := range old {
		if _, err := os.Stat(filepath.Join(root, f)); !os.IsNotExist(err) {
			t.Errorf("%s not pruned (err %v)", f, err)
		}
	}
	for _, f := range kept {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("%s removed: %v", f, err)
		}
	}
	if s := l.Stats(); s.Errors != 0 {
		t.Errorf("Errors = %d", s.Errors)
	}
}

func TestLedgerAccountDir(t *testing.T) {
	for _, id := range []string{"acct-1", "user@example.com", "a.b_c+d", "ABC123"} {
		if got := LedgerAccountDir(id); got != id {
			t.Errorf("LedgerAccountDir(%q) = %q, want unchanged", id, got)
		}
	}
	unsafe := []string{"..", ".", "../etc", "a/b", `a\b`, ".hidden", "a b", "é", "_unattributed", strings.Repeat("x", 200), "a/../b"}
	seen := map[string]string{}
	for _, id := range unsafe {
		got := LedgerAccountDir(id)
		if got == id || got == "" || got == "." || got == ".." || strings.ContainsAny(got, `/\`) ||
			strings.HasPrefix(got, ".") || len(got) > 110 || got == unattributedDir {
			t.Errorf("LedgerAccountDir(%q) = %q, not a safe name", id, got)
		}
		if prev, dup := seen[got]; dup {
			t.Errorf("LedgerAccountDir(%q) and (%q) both = %q", id, prev, got)
		}
		seen[got] = id
	}
	if got := LedgerAccountDir(""); got != unattributedDir {
		t.Errorf("LedgerAccountDir(\"\") = %q", got)
	}

	// The original ID is kept in the line.
	root := t.TempDir()
	l := newTestLedger(t, root, LedgerOptions{})
	e := sampleLedgerEntry()
	e.AccountID = "../escape"
	l.Append(e)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	files := UsageFiles([]string{root})
	if len(files) != 1 || !strings.HasPrefix(files[0], filepath.Join(root, "projects")+string(filepath.Separator)) {
		t.Fatalf("files = %v", files)
	}
	es, err := ReadLedgerFile(files[0])
	if err != nil || len(es) != 1 || es[0].AccountID != "../escape" || es[0].ProjectPath != "proxy/../escape" {
		t.Errorf("entries = %+v, %v", es, err)
	}
}

func TestLedger_Concurrent(t *testing.T) {
	root := t.TempDir()
	l := newTestLedger(t, root, LedgerOptions{QueueSize: 64})
	const goroutines, per = 32, 200
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for g := range goroutines {
		wg.Go(func() {
			n := 0
			for i := range per {
				e := sampleLedgerEntry()
				e.AccountID = fmt.Sprintf("acct-%d", g%4)
				e.MessageID = fmt.Sprintf("msg_%d_%d", g, i)
				if l.Append(e) {
					n++
				}
			}
			mu.Lock()
			accepted += n
			mu.Unlock()
		})
	}
	// Close races the tail of the appends; nothing may be lost or panic.
	wg.Wait()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	s := l.Stats()
	if s.Written+s.Dropped != goroutines*per || int(s.Written) != accepted || s.Errors != 0 {
		t.Errorf("Stats = %+v, accepted %d", s, accepted)
	}
	total := 0
	for _, f := range UsageFiles([]string{root}) {
		es, err := ReadLedgerFile(f)
		if err != nil {
			t.Fatal(err)
		}
		total += len(es)
	}
	if total != accepted {
		t.Errorf("entries on disk = %d, want %d", total, accepted)
	}
}

func TestLedger_AppendDuringClose(t *testing.T) {
	root := t.TempDir()
	l := newTestLedger(t, root, LedgerOptions{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 500 {
				l.Append(sampleLedgerEntry())
			}
		})
	}
	time.Sleep(time.Millisecond)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	s := l.Stats()
	if s.Written+s.Dropped != 8*500 {
		t.Errorf("Stats = %+v", s)
	}
}

func TestLedgerAccountDir_LongTraversal(t *testing.T) {
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	for _, id := range []string{
		strings.Repeat("a", 96) + "/../../../x",
		strings.Repeat("a", 127) + "/",
		strings.Repeat(".", 200),
	} {
		dir := LedgerAccountDir(id)
		if strings.ContainsAny(dir, `/\`) || dir == ".." || strings.HasPrefix(dir, ".") {
			t.Errorf("LedgerAccountDir(%q) = %q", id, dir)
		}
		p := filepath.Join(projects, dir, "2026-09-27.jsonl")
		if rel, err := filepath.Rel(projects, p); err != nil || strings.HasPrefix(rel, "..") || filepath.Dir(rel) != dir {
			t.Errorf("path for %q escapes projects/: %s", id, p)
		}
	}
	l := newTestLedger(t, root, LedgerOptions{})
	e := sampleLedgerEntry()
	e.AccountID = strings.Repeat("a", 96) + "/../../../x"
	l.Append(e)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	var found []string
	filepath.WalkDir(filepath.Dir(root), func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
			found = append(found, path)
		}
		return nil
	})
	if len(found) != 1 || !strings.HasPrefix(found[0], projects+string(filepath.Separator)) {
		t.Errorf("ledger files = %v, want one under %s", found, projects)
	}
}

func TestLedger_TerminatesPartialLine(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "acct-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-09-27.jsonl")
	if err := os.WriteFile(path, []byte(`{"timestamp":"2026-09-27T01:00:00Z","mess`), 0o600); err != nil {
		t.Fatal(err)
	}
	l := newTestLedger(t, root, LedgerOptions{})
	l.Append(sampleLedgerEntry())
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, path)
	if len(lines) != 2 || len(ParseLedgerLine(lines[1])) != 1 {
		t.Fatalf("lines = %q", lines)
	}
}

func TestLedger_ReopensAfterWriteError(t *testing.T) {
	root := t.TempDir()
	var l *Ledger
	n := 0
	l = newTestLedger(t, root, LedgerOptions{writeHook: func(Entry) {
		n++
		if n == 2 {
			// Break the open handle under the writer.
			for _, f := range l.files {
				f.Close()
			}
		}
	}})
	for i := range 3 {
		e := sampleLedgerEntry()
		e.MessageID = fmt.Sprintf("msg_%d", i)
		l.Append(e)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if s := l.Stats(); s.Written != 2 || s.Errors != 1 {
		t.Errorf("Stats = %+v, want 2 written, 1 error", s)
	}
	lines := readLines(t, filepath.Join(root, "projects", "acct-1", "2026-09-27.jsonl"))
	if len(lines) != 2 || ParseLedgerLine(lines[1])[0].MessageID != "msg_2" {
		t.Errorf("lines = %q", lines)
	}
}

func TestMarshalLedgerLine_CacheSplit(t *testing.T) {
	ts := time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		total, c5, c1 int64
		want          string // cache_creation, or "" when left out
		want5, want1  int64
	}{
		{300, 100, 200, `{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200}`, 100, 200},
		{300, 0, 200, `{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200}`, 100, 200},
		{300, 250, 200, `{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200}`, 100, 200},
		{300, 300, 0, `{"ephemeral_5m_input_tokens":300,"ephemeral_1h_input_tokens":0}`, 300, 0},
		{300, 0, 400, "", 300, 0},
		{300, 0, 0, "", 300, 0},
		{0, 0, 0, "", 0, 0},
	} {
		line, err := MarshalLedgerLine(Entry{Timestamp: ts, CacheCreate: tc.total, CacheCreate5m: tc.c5, CacheCreate1h: tc.c1})
		if err != nil {
			t.Fatal(err)
		}
		has := bytes.Contains(line, []byte(`"cache_creation":`+tc.want))
		if tc.want == "" {
			has = !bytes.Contains(line, []byte(`"cache_creation":`))
		}
		if !has {
			t.Errorf("%+v: line %s, want cache_creation %q", tc, line, tc.want)
		}
		e := ParseLedgerLine(bytes.TrimSpace(line))[0]
		if e.CacheCreate != tc.total || e.CacheCreate5m != tc.want5 || e.CacheCreate1h != tc.want1 {
			t.Errorf("%+v: read back %d = %d + %d", tc, e.CacheCreate, e.CacheCreate5m, e.CacheCreate1h)
		}
	}
}

func TestMarshalLedgerLine_NegativeCost(t *testing.T) {
	line, err := MarshalLedgerLine(Entry{Timestamp: time.Now(), CostUSD: ptr(-1)})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(line, []byte("costUSD")) {
		t.Errorf("negative cost written: %s", line)
	}
}

func TestLedger_FixesPermissions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "acct-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "2026-09-27.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{root, filepath.Join(root, "projects"), dir} {
		os.Chmod(p, 0o755)
	}
	os.Chmod(path, 0o644)
	l := newTestLedger(t, root, LedgerOptions{})
	l.Append(sampleLedgerEntry())
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{root: 0o700, filepath.Join(root, "projects"): 0o700, dir: 0o700, path: 0o600} {
		if info, err := os.Stat(p); err != nil || info.Mode().Perm() != want {
			t.Errorf("%s mode = %v, %v; want %v", p, info, err, want)
		}
	}
}

func TestReadLedgerFile_HashedDirIsNotAnAccount(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "projects", LedgerAccountDir("a/b"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := `{"timestamp":"2026-09-27T01:00:00Z","message":{"usage":{"input_tokens":1,"output_tokens":2}}}` + "\n"
	path := filepath.Join(dir, "2026-09-27.jsonl")
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	es, err := ReadLedgerFile(path)
	if err != nil || len(es) != 1 || es[0].AccountID != "" {
		t.Errorf("entries = %+v, %v; want one unattributed entry", es, err)
	}
}

func TestLedger_KeepsPreviousDayOpen(t *testing.T) {
	root := t.TempDir()
	var l *Ledger
	var open []string
	l = newTestLedger(t, root, LedgerOptions{writeHook: func(Entry) {
		var keys []string
		for k := range l.files {
			keys = append(keys, k.date)
		}
		slices.Sort(keys)
		open = append(open, strings.Join(keys, ","))
	}})
	for _, day := range []int{26, 27, 26, 28, 25, 28} {
		e := sampleLedgerEntry()
		e.Timestamp = time.Date(2026, 9, day, 12, 0, 0, 0, time.UTC)
		l.Append(e)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	// Open files seen before each write.
	want := []string{
		"",
		"2026-09-26",
		"2026-09-26,2026-09-27",
		"2026-09-26,2026-09-27",
		"2026-09-27,2026-09-28", // 26 closed when 28 arrived
		"2026-09-27,2026-09-28", // late 25 written and closed
	}
	if !slices.Equal(open, want) {
		t.Errorf("open files = %q, want %q", open, want)
	}
	if n := len(readLines(t, filepath.Join(root, "projects", "acct-1", "2026-09-26.jsonl"))); n != 2 {
		t.Errorf("2026-09-26 lines = %d, want 2", n)
	}
}
