# Classifier Corpus Follow-ups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the items the final review of PR #93 parked: an unsynchronized recorder swap (M1), a disk write on the response's critical path (M2), redaction without path boundaries (M3), missing Laya field validation (M5), a misleading exporter hint, a test that leaks global config, and spec sections that trail the code.

**Architecture:** The recorder pointer on `Server` becomes an `atomic.Pointer`, loaded once per request. The handler's deferred capture freezes the tap timing and hands the row to `Recorder.RecordAsync`, which builds and writes it on a goroutine, with at most 64 writes in flight. Redaction moves to a boundary-aware `redactHome` function. `handleConfigSave` gains the two Laya checks spec §4.6 already names. The exporter hint and the spec text are brought in line with the code.

**Tech Stack:** Go 1.27 stdlib (`sync/atomic`, `testing`), Python 3 + pytest for the exporter, Markdown docs.

**Spec:** `docs/superpowers/specs/2026-09-23-classifier-corpus-and-laya-design.md` (§3.5 file handling, §3.6 redaction, §3.7 configuration, §3.8 export tooling, §4.6 Laya configuration, §5 failure behavior). These items come from the final whole-branch review of PR #93 (gustavokch/antigravity-claude-proxy-go).

**User decisions (binding):**
- All commits go on the existing branch `feat/classifier-corpus-laya`, one commit per task, pushed to update PR #93.
- M2: one goroutine per row, capped at 64 writes in flight. A row past the cap is dropped and counted. `Wait()` exists for tests. Rows still in flight at process exit are lost.
- M3: redact the proxy process's home only. Fix the path boundary, turn redaction off for a home of `/`, and document whose home it is. No generic `/home/*` or `/Users/*` patterns, and no configurable prefix list.

## Global Constraints

- Capture and every failure inside it is logged and swallowed. It must never fail, delay, or alter a user request. With capture off, response bytes and status are byte-identical, and no goroutine is started.
- Corpus directories are created `0700`, files `0600`.
- `source` is written on every row. Valid values: `upstream`, `stub`, `rule`, `laya`, `gateway`. A fine-tune consumes `upstream` rows only.
- One row per request: one `ResponseTap` and one deferred capture call; the source is assigned through the `captureSource` pointer before the row is handed off.
- Severity is an integer 0–100; 50 is the allow/block boundary. Laya-derived severity stays clamped to `layaMaxSeverity` (default 49).
- Tests use stdlib `testing` with `t.Errorf` / `t.Fatalf`. No testify, no assertion helpers.
- Existing test functions in files that predate the branch (`internal/api/classifier_fallback_test.go`, and the pre-existing functions in `internal/api/classifier_config_test.go`) are not edited. New test functions may be added.
- Out of scope: the `maxFiles` default of 8 (I4), the A–D criteria wording (I6).
- Commit only the files the task names. The working tree has unrelated untracked files (`.gemini/`, `.gocache/`, `.ignore`, `.mcp.json`, `GEMINI.md`, `opencode.json`, `pr.sh`, `scripts/__pycache__/`, `tools/quotaprobe/`); never add them.
- Comments, docs and commit messages are in plain English prose.

---

## File Structure

| File | Change | Task |
|---|---|---|
| `internal/api/server.go` | `classifierCorpus` becomes `atomic.Pointer[corpus.Recorder]`; the capture block loads it once; the defer calls `RecordAsync` | 1, 2 |
| `internal/api/classifier_rules.go` | `applyClassifierConfig` stores through the atomic pointer | 1 |
| `internal/api/classifier_capture_test.go` | `.Load()` at the three field uses; fallback-stub test restores config; `readCaptureRows` waits | 1, 2 |
| `internal/api/classifier_capture_e2e_test.go` | config-swap race test; `readCaptureRows` call sites | 1, 2 |
| `internal/classifier/corpus/tap.go` | `Finish()` freezes elapsed time | 2 |
| `internal/classifier/corpus/tap_test.go` | `Finish` test | 2 |
| `internal/classifier/corpus/recorder.go` | `RecordAsync`, `Wait`, in-flight cap, atomic drop counter; `usableHome` and `redactHome` wired in | 2, 3 |
| `internal/classifier/corpus/recorder_test.go` | async tests; root-home test | 2, 3 |
| `internal/classifier/corpus/redact.go` | new: `usableHome`, `redactHome` and byte helpers | 3 |
| `internal/classifier/corpus/redact_test.go` | new: table tests | 3 |
| `internal/api/management.go` | `layaQuestionName` pattern and blank `layaInstructions` checks | 4 |
| `internal/api/classifier_config_test.go` | three new test functions | 4 |
| `scripts/corpus_to_laya.py` | no-rows hint names the kind filter | 5 |
| `scripts/test_corpus_to_laya.py` | two new tests | 5 |
| `docs/classifier-rules.md` | async write, redaction scope, no-rows hint | 2, 3, 5 |
| `docs/superpowers/specs/2026-09-23-classifier-corpus-and-laya-design.md` | §3.5, §3.6, §3.7, §3.8, §5 | 6 |

---

## Task 1: Swap the recorder atomically (M1)

**Files:**
- Modify: `internal/api/server.go:121` (field), `internal/api/server.go:724-745` (capture block)
- Modify: `internal/api/classifier_rules.go:36-48` (`applyClassifierConfig`)
- Modify: `internal/api/classifier_capture_test.go:82,110,141` and `:205-212` (fallback-stub test)
- Test: `internal/api/classifier_capture_e2e_test.go` (new test function)

**Interfaces:**
- Consumes: `corpus.New`, `(*corpus.Recorder).Enabled` (nil-safe), `(*corpus.Recorder).Record`; test helpers `newAccountBackedTestServer`, `verdictBackend`, `postClassifierMessages`, `classifierShapedBody`, `classifierTestModel`, `classifierStage1Footer`.
- Produces: `Server.classifierCorpus` of type `atomic.Pointer[corpus.Recorder]`. Every reader calls `server.classifierCorpus.Load()`, and `Load()` can return nil (capture off). Task 2 relies on this.

Today `applyClassifierConfig` (run by a settings save) writes `server.classifierCorpus` with no synchronization, while `messages` reads it once for `Enabled()` and again inside the deferred write. Under `-race` that is a data race; in practice a swap mid-request can send a row to a recorder that never saw the request start.

- [ ] **Step 1: Write the failing race test**

Add `"sync"` to the imports of `internal/api/classifier_capture_e2e_test.go`, then append:

```go
// TestClassifierCaptureConfigSwapIsRaceFree pins that a settings save can
// swap the recorder while classifier requests are in flight. It proves
// nothing without -race: run it with go test -race.
func TestClassifierCaptureConfigSwapIsRaceFree(t *testing.T) {
	t.Setenv("ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK", "")
	orig := config.Get()
	t.Cleanup(func() { config.SetForTest(orig) })
	dir := t.TempDir()
	server, _ := newAccountBackedTestServer(t)
	server.backend = &verdictBackend{verdict: "<severity>12</severity>"}

	cfg := config.Get()
	cfg.Classifier.Enabled = false
	cfg.Classifier.Capture = config.ClassifierCaptureConfig{Enabled: true, Dir: dir}
	config.SetForTest(cfg)
	server.applyClassifierConfig(cfg.Classifier)

	body := classifierShapedBody(t, classifierTestModel, classifierStage1Footer)
	const rounds = 50
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			swapped := cfg.Classifier
			swapped.Capture.Enabled = i%2 == 0
			server.applyClassifierConfig(swapped)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			postClassifierMessages(t, server, body)
		}
	}()
	wg.Wait()
}
```

- [ ] **Step 2: Run it under the race detector and confirm it fails**

Run: `go test -race -count=1 -run TestClassifierCaptureConfigSwapIsRaceFree ./internal/api/`
Expected: FAIL, with `WARNING: DATA RACE` naming `applyClassifierConfig` and `messages`.

- [ ] **Step 3: Make the field atomic**

In `internal/api/server.go`, change the field (line 121; `sync/atomic` is already imported):

```go
	classifierCorpus   atomic.Pointer[corpus.Recorder]
```

In `internal/api/classifier_rules.go`, replace the recorder block at the top of `applyClassifierConfig`:

```go
	// The recorder is rebuilt on every config application so a settings save
	// takes effect without a restart. It is swapped atomically because
	// requests read it concurrently.
	if cfg.Capture.Enabled {
		resolved := cfg.Capture.Resolved()
		server.classifierCorpus.Store(corpus.New(resolved.Dir, corpus.Options{
			MaxFiles:     resolved.MaxFiles,
			MaxFileBytes: resolved.MaxFileBytes,
			RedactPaths:  resolved.RedactPathsEnabled(),
		}))
	} else {
		server.classifierCorpus.Store(nil)
	}
```

In `internal/api/server.go`, replace the capture block (currently `if server.classifierCorpus.Enabled() {` through the closing brace of the deferred write) with:

```go
	captureSource := corpus.SourceUpstream
	var captureRef *corpus.Source
	// The recorder is loaded once: a settings save can swap it mid-request,
	// and the row belongs to the recorder that saw the request start.
	if recorder := server.classifierCorpus.Load(); recorder.Enabled() {
		if kind, detected := classifier.Detect(rawBody); detected {
			tap := corpus.NewResponseTap(writer)
			writer = tap
			captureRef = &captureSource
			contextEntries := cfg.Classifier.Capture.Resolved().ContextEntries
			defer func() {
				recorder.Record(corpus.BuildEntry(corpus.EntryInput{
					RawBody:        rawBody,
					Kind:           kind.String(),
					Model:          model,
					Source:         captureSource,
					Tap:            tap,
					ContextEntries: contextEntries,
				}))
			}()
		}
	}
```

Keep the existing three-line comment above `captureSource` ("Capture installs one tap and one deferred writer…") as it is.

- [ ] **Step 4: Update the direct field uses in tests**

In `internal/api/classifier_capture_test.go`:
- line 82: `if !srv.classifierCorpus.Load().Enabled() {`
- line 110: `srv.classifierCorpus.Load().Record(corpus.BuildEntry(corpus.EntryInput{`
- line 141: `if srv.classifierCorpus.Load().Enabled() {`

Run `grep -rn "classifierCorpus" internal/` afterwards: every remaining use must go through `.Load()` or `.Store(`.

- [ ] **Step 5: Stop the fallback-stub test from leaking config**

`TestClassifierCaptureFallbackStubWritesOneStubRow` (in `internal/api/classifier_capture_test.go`) calls `config.SetForTest` with capture on and never restores the config, so later tests in the package start with capture enabled. Insert these two lines right after its `t.Setenv(...)` line and before `dir := t.TempDir()`:

```go
	orig := config.Get()
	t.Cleanup(func() { config.SetForTest(orig) })
```

- [ ] **Step 6: Run the race test and the package**

Run: `go test -race -count=1 -run 'TestClassifierCapture|TestLaya' ./internal/api/`
Expected: PASS, no `DATA RACE`.
Run: `go vet ./internal/api/ && go test -count=1 ./internal/api/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/api/server.go internal/api/classifier_rules.go internal/api/classifier_capture_test.go internal/api/classifier_capture_e2e_test.go
git commit -m "fix(capture): swap the corpus recorder atomically and load it once per request"
```

---

## Task 2: Write the row off the response's critical path (M2)

**Files:**
- Modify: `internal/classifier/corpus/tap.go` (struct, `ElapsedMs`, new `Finish`)
- Modify: `internal/classifier/corpus/recorder.go` (imports, struct, `New`, `Dropped`, size-cap drop, new `RecordAsync`, `Wait`, `noteDrop`)
- Modify: `internal/api/server.go` (the deferred capture call from Task 1)
- Modify: `internal/api/classifier_capture_test.go` (`readCaptureRows` and its call sites)
- Modify: `internal/api/classifier_capture_e2e_test.go` (call sites; race test waits)
- Modify: `docs/classifier-rules.md`
- Test: `internal/classifier/corpus/tap_test.go`, `internal/classifier/corpus/recorder_test.go`

**Interfaces:**
- Consumes: `Server.classifierCorpus` as `atomic.Pointer[corpus.Recorder]` (Task 1).
- Produces:
  - `func (tap *ResponseTap) Finish()`: freezes `ElapsedMs`; idempotent.
  - `func (recorder *Recorder) RecordAsync(input EntryInput)`: nil-safe; never blocks.
  - `func (recorder *Recorder) Wait()`: nil-safe; blocks until every `RecordAsync` row is written.
  - `const maxInFlightRecords = 64` (package `corpus`).
  - `Dropped()` keeps its `int` return type and now also counts rows dropped at the in-flight cap.
  - Test helper `readCaptureRows(t *testing.T, server *Server, dir string) []corpus.Entry`.

A small response stays in net/http's buffer until the handler returns, so today's deferred `BuildEntry` + `Record` (JSON parse, `MkdirAll`, `Glob`, `Stat`, `Open`, `Write`, recorder mutex) sits in front of the permission prompt. That breaks spec §5 ("never delays").

- [ ] **Step 1: Write the failing tap test**

Add `"time"` to the imports of `internal/classifier/corpus/tap_test.go`, then append:

```go
// TestResponseTapFinishFreezesElapsed pins that latency stops at the
// response. The row is built later, on another goroutine, and must not count
// that wait.
func TestResponseTapFinishFreezesElapsed(t *testing.T) {
	tap := NewResponseTap(httptest.NewRecorder())
	time.Sleep(20 * time.Millisecond)
	tap.Finish()
	frozen := tap.ElapsedMs()
	if frozen < 20 {
		t.Errorf("ElapsedMs = %d, want at least the 20ms before Finish", frozen)
	}
	time.Sleep(30 * time.Millisecond)
	if got := tap.ElapsedMs(); got != frozen {
		t.Errorf("ElapsedMs moved from %d to %d after Finish", frozen, got)
	}
	tap.Finish()
	if got := tap.ElapsedMs(); got != frozen {
		t.Errorf("a second Finish moved ElapsedMs from %d to %d", frozen, got)
	}
}
```

- [ ] **Step 2: Write the failing recorder tests**

Append to `internal/classifier/corpus/recorder_test.go`:

```go
func TestRecordAsyncWritesTheRow(t *testing.T) {
	dir := t.TempDir()
	recorder := New(dir, Options{MaxFiles: 8, MaxFileBytes: 1 << 20})
	tap := tapWithBody(t, `{"content":[{"type":"text","text":"<severity>7</severity>"}]}`)
	tap.Finish()

	recorder.RecordAsync(EntryInput{
		RawBody:        []byte(stage2Body),
		Kind:           "stage1-severity",
		Model:          "claude-sonnet-5",
		Source:         SourceUpstream,
		Tap:            tap,
		ContextEntries: 2,
	})
	recorder.Wait()

	rows := readRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Severity != 7 {
		t.Errorf("Severity = %d, want 7", rows[0].Severity)
	}
	if rows[0].Action != `{"Bash":"rm -rf build/"}` {
		t.Errorf("Action = %q", rows[0].Action)
	}
}

// TestRecordAsyncReturnsWhileAWriteIsStalled pins the reason RecordAsync
// exists: the request handler must not wait on the disk.
func TestRecordAsyncReturnsWhileAWriteIsStalled(t *testing.T) {
	dir := t.TempDir()
	recorder := New(dir, Options{MaxFiles: 8, MaxFileBytes: 1 << 20})
	recorder.mu.Lock() // stands in for a write stuck on the disk

	returned := make(chan struct{})
	go func() {
		recorder.RecordAsync(EntryInput{Kind: "stage1-severity", Source: SourceUpstream})
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		recorder.mu.Unlock()
		t.Fatal("RecordAsync blocked on a stalled write")
	}

	recorder.mu.Unlock()
	recorder.Wait()
	if rows := readRows(t, dir); len(rows) != 1 {
		t.Errorf("got %d rows after the stall cleared, want 1", len(rows))
	}
}

func TestRecordAsyncDropsRowsPastTheInFlightCap(t *testing.T) {
	dir := t.TempDir()
	recorder := New(dir, Options{MaxFiles: 8, MaxFileBytes: 1 << 20})
	recorder.mu.Lock() // every started write parks on the mutex

	for i := 0; i < maxInFlightRecords+5; i++ {
		recorder.RecordAsync(EntryInput{Kind: "stage1-severity", Source: SourceUpstream})
	}
	if got := recorder.Dropped(); got != 5 {
		t.Errorf("Dropped = %d, want the 5 rows past the cap of %d", got, maxInFlightRecords)
	}

	recorder.mu.Unlock()
	recorder.Wait()
	if rows := readRows(t, dir); len(rows) != maxInFlightRecords {
		t.Errorf("got %d rows, want %d", len(rows), maxInFlightRecords)
	}
}

func TestRecordAsyncOnNilRecorderIsSafe(t *testing.T) {
	var recorder *Recorder
	recorder.RecordAsync(EntryInput{}) // must not panic
	recorder.Wait()
}
```

- [ ] **Step 3: Run the corpus tests and confirm they fail**

Run: `go test -count=1 ./internal/classifier/corpus/`
Expected: FAIL to compile: `tap.Finish undefined`, `recorder.RecordAsync undefined`, `undefined: maxInFlightRecords`.

- [ ] **Step 4: Add `Finish` to the tap**

In `internal/classifier/corpus/tap.go`, add an `end` field to `ResponseTap` after `start`:

```go
	start     time.Time
	end       time.Time
```

Replace `ElapsedMs` and add `Finish` directly above it:

```go
// Finish freezes the elapsed time. The row is built after the handler
// returns, on another goroutine, so latency must stop at the response rather
// than at the write. A second call changes nothing.
func (tap *ResponseTap) Finish() {
	if tap.end.IsZero() {
		tap.end = time.Now()
	}
}

func (tap *ResponseTap) ElapsedMs() int64 {
	end := tap.end
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(tap.start).Milliseconds()
}
```

- [ ] **Step 5: Add `RecordAsync`, `Wait` and the atomic drop counter**

In `internal/classifier/corpus/recorder.go`:

Add `"sync/atomic"` to the imports.

Add this constant directly above the `Recorder` type:

```go
// maxInFlightRecords bounds the row writes running at once. Classifier
// requests arrive about one per tool call, so reaching the bound means the
// disk has stalled; rows past it are dropped and counted rather than queued
// without limit.
const maxInFlightRecords = 64
```

Replace the `Recorder` struct, `New` and `Dropped` with:

```go
type Recorder struct {
	dir      string
	options  Options
	home     string
	mu       sync.Mutex
	lastDate string
	// dropped and lastWarn are atomic so RecordAsync can count a drop without
	// taking mu, which a stalled write may hold.
	dropped  atomic.Int64
	lastWarn atomic.Int64 // Unix nanoseconds of the last drop warning
	inFlight chan struct{}
	pending  sync.WaitGroup
}

func New(dir string, options Options) *Recorder {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return &Recorder{
		dir:      dir,
		options:  options,
		home:     home,
		inFlight: make(chan struct{}, maxInFlightRecords),
	}
}

// Dropped reports how many rows were dropped, at the size cap or at the
// in-flight cap.
func (recorder *Recorder) Dropped() int {
	if recorder == nil {
		return 0
	}
	return int(recorder.dropped.Load())
}

// RecordAsync builds and writes the row on its own goroutine, so the request
// handler returns without waiting on the disk: a small response stays in
// net/http's buffer until the handler returns, and the permission prompt
// waits on that response. Call Finish on input.Tap first, and do not write to
// the tap afterwards.
func (recorder *Recorder) RecordAsync(input EntryInput) {
	if !recorder.Enabled() {
		return
	}
	select {
	case recorder.inFlight <- struct{}{}:
	default:
		recorder.noteDrop("corpus: too many row writes in flight, dropping rows")
		return
	}
	recorder.pending.Add(1)
	go func() {
		defer recorder.pending.Done()
		defer func() { <-recorder.inFlight }()
		// A panic here would take the whole proxy down, not one request: the
		// net/http recovery that guarded the old in-handler write does not
		// reach this goroutine.
		defer func() {
			if value := recover(); value != nil {
				slog.Warn("corpus: building a row panicked", "panic", value)
			}
		}()
		recorder.Record(BuildEntry(input))
	}()
}

// Wait blocks until every row started by RecordAsync is written. The proxy
// never calls it; tests call it before they read rows back.
func (recorder *Recorder) Wait() {
	if recorder == nil {
		return
	}
	recorder.pending.Wait()
}

// noteDrop counts a dropped row and logs at most one warning per hour.
func (recorder *Recorder) noteDrop(message string, attrs ...any) {
	count := recorder.dropped.Add(1)
	now := time.Now().UnixNano()
	last := recorder.lastWarn.Load()
	if now-last > int64(time.Hour) && recorder.lastWarn.CompareAndSwap(last, now) {
		slog.Warn(message, append(attrs, "dropped", count)...)
	}
}
```

In `Record`, replace the size-cap block with:

```go
	// A MaxFileBytes of zero or less means no limit, matching MaxFiles.
	if recorder.options.MaxFileBytes > 0 {
		if info, err := os.Stat(path); err == nil && info.Size() >= recorder.options.MaxFileBytes {
			recorder.noteDrop("corpus: file at its size cap, dropping rows", "file", path)
			return
		}
	}
```

- [ ] **Step 6: Run the corpus tests**

Run: `go test -race -count=1 ./internal/classifier/corpus/`
Expected: PASS, including the existing `TestRecorderDropsRowsPastTheSizeCap`.

- [ ] **Step 7: Hand the row off in the handler**

In `internal/api/server.go`, replace the deferred function inside the capture block (from Task 1) with:

```go
			defer func() {
				// The row is built and written off this goroutine. A small
				// response stays in net/http's buffer until the handler
				// returns, so a synchronous write would delay the permission
				// prompt.
				tap.Finish()
				recorder.RecordAsync(corpus.EntryInput{
					RawBody:        rawBody,
					Kind:           kind.String(),
					Model:          model,
					Source:         captureSource,
					Tap:            tap,
					ContextEntries: contextEntries,
				})
			}()
```

- [ ] **Step 8: Make the API tests wait for the writer**

In `internal/api/classifier_capture_test.go`, change `readCaptureRows` to take the server and wait first:

```go
// readCaptureRows waits for the recorder's pending writes, then reads every
// row in dir. Rows are written after the handler returns, so reading without
// waiting races the writer.
func readCaptureRows(t *testing.T, server *Server, dir string) []corpus.Entry {
	t.Helper()
	server.classifierCorpus.Load().Wait()
	matches, err := filepath.Glob(filepath.Join(dir, "classifier-*.jsonl"))
```

(The rest of the body is unchanged.) Update every call site. The compiler lists them; they are `readCaptureRows(t, dir)` in `TestClassifierCaptureRecordsStubbedVerdict` (server variable `srv`), in `TestClassifierCaptureFallbackStubWritesOneStubRow` (`server`), and in the four end-to-end tests in `classifier_capture_e2e_test.go` (`server`). Each becomes `readCaptureRows(t, srv, dir)` or `readCaptureRows(t, server, dir)`.

In `TestClassifierCaptureConfigSwapIsRaceFree` (added in Task 1), track every recorder so the test can wait for all of them. A swap can leave an older recorder with a write still running, and it would otherwise write into `dir` while `t.TempDir` removes it. Replace the section from `body := ...` to the end of the function with:

```go
	body := classifierShapedBody(t, classifierTestModel, classifierStage1Footer)
	const rounds = 50
	// Only the swapping goroutine stores, so Load right after each apply
	// returns the recorder that apply created.
	created := []*corpus.Recorder{server.classifierCorpus.Load()}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			swapped := cfg.Classifier
			swapped.Capture.Enabled = i%2 == 0
			server.applyClassifierConfig(swapped)
			created = append(created, server.classifierCorpus.Load())
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			postClassifierMessages(t, server, body)
		}
	}()
	wg.Wait()
	for _, recorder := range created {
		recorder.Wait()
	}
}
```

- [ ] **Step 9: Document the asynchronous write**

In `docs/classifier-rules.md`, insert this paragraph directly after the paragraph that begins "Capture keeps at most `maxFiles` day files":

```markdown
Each row is written on a background goroutine after the response has gone out, so capture never delays the permission prompt. At most 64 writes run at once; if the disk stalls, rows past that are dropped and counted, and a warning is logged at most once per hour. A row that is still being written when the proxy exits is lost.
```

- [ ] **Step 10: Run the affected packages under the race detector**

Run: `go test -race -count=1 ./internal/classifier/corpus/ ./internal/api/`
Expected: PASS, no `DATA RACE`.
Run: `go vet ./internal/...`
Expected: no output.

- [ ] **Step 11: Commit**

```bash
git add internal/classifier/corpus/tap.go internal/classifier/corpus/tap_test.go internal/classifier/corpus/recorder.go internal/classifier/corpus/recorder_test.go internal/api/server.go internal/api/classifier_capture_test.go internal/api/classifier_capture_e2e_test.go docs/classifier-rules.md
git commit -m "fix(capture): write corpus rows off the response's critical path"
```

---

## Task 3: Redact the home directory on path boundaries (M3)

**Files:**
- Create: `internal/classifier/corpus/redact.go`
- Create: `internal/classifier/corpus/redact_test.go`
- Modify: `internal/classifier/corpus/recorder.go` (`New`, the redaction block in `Record`)
- Modify: `docs/classifier-rules.md`
- Test: `internal/classifier/corpus/recorder_test.go` (one new function)

**Interfaces:**
- Consumes: `Recorder.home`, `Options.RedactPaths`.
- Produces: `func usableHome(home string) string` and `func redactHome(text, home string) string` (unexported, package `corpus`).

Today `Record` runs `strings.ReplaceAll(text, home, "~")`. A home of `/` turns every separator into `~`. A home of `/home/a` also rewrites `/home/abc` and `/mnt/home/a`.

- [ ] **Step 1: Write the failing unit tests**

Create `internal/classifier/corpus/redact_test.go`:

```go
package corpus

import "testing"

func TestRedactHome(t *testing.T) {
	const home = "/home/a"
	cases := []struct {
		name string
		text string
		want string
	}{
		{"path under home", `{"Bash":"cat /home/a/.ssh/config"}`, `{"Bash":"cat ~/.ssh/config"}`},
		{"home alone", `{"Bash":"cd /home/a"}`, `{"Bash":"cd ~"}`},
		{"whole text", "/home/a", "~"},
		{"longer sibling", "cat /home/abc/x", "cat /home/abc/x"},
		{"dotted sibling", "cat /home/a.bak/x", "cat /home/a.bak/x"},
		{"non-ASCII sibling", "cat /home/aé/x", "cat /home/aé/x"},
		{"nested under another root", "cat /mnt/home/a/x", "cat /mnt/home/a/x"},
		{"adjacent occurrences", "/home/a/x /home/a/y", "~/x ~/y"},
		{"path list", "PATH=/home/a/bin:/home/a/go/bin", "PATH=~/bin:~/go/bin"},
		{"after a JSON escape", `echo hi\n/home/a/bin`, `echo hi\n~/bin`},
		{"no occurrence", "ls /tmp", "ls /tmp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactHome(tc.text, home); got != tc.want {
				t.Errorf("redactHome(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
	if got := redactHome("cat /home/a/x", ""); got != "cat /home/a/x" {
		t.Errorf("an empty home rewrote the text: %q", got)
	}
}

func TestUsableHome(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"/":          "",
		"/home/a":    "/home/a",
		"/home/a/":   "/home/a",
		"/Users/a//": "/Users/a",
	}
	for home, want := range cases {
		if got := usableHome(home); got != want {
			t.Errorf("usableHome(%q) = %q, want %q", home, got, want)
		}
	}
}
```

Append to `internal/classifier/corpus/recorder_test.go`:

```go
// TestRecorderSkipsRedactionForRootHome pins that a home of "/" (a service
// running as root with HOME set to /) leaves rows intact rather than turning
// every path separator into "~".
func TestRecorderSkipsRedactionForRootHome(t *testing.T) {
	t.Setenv("HOME", "/")
	dir := t.TempDir()
	recorder := New(dir, Options{MaxFiles: 8, MaxFileBytes: 1 << 20, RedactPaths: true})
	recorder.Record(Entry{Version: 1, Action: `{"Bash":"/usr/bin/env ls"}`})

	rows := readRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if rows[0].Action != `{"Bash":"/usr/bin/env ls"}` {
		t.Errorf("Action = %q, want it untouched for a root home", rows[0].Action)
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test -count=1 -run 'TestRedactHome|TestUsableHome|TestRecorderSkipsRedactionForRootHome' ./internal/classifier/corpus/`
Expected: FAIL to compile: `undefined: redactHome`, `undefined: usableHome`.

- [ ] **Step 3: Implement the redaction helpers**

Create `internal/classifier/corpus/redact.go`:

```go
package corpus

import (
	"path/filepath"
	"strings"
)

// usableHome returns home in the form redaction matches, or "" when
// redacting it would corrupt rows: a home of "/" would turn every path
// separator into "~".
func usableHome(home string) string {
	if home == "" {
		return ""
	}
	home = filepath.Clean(home)
	if home == "/" || home == "." {
		return ""
	}
	return home
}

// redactHome replaces each occurrence of home that stands as a whole path
// prefix with "~". An occurrence counts only when the byte before it cannot
// be part of a path and the byte after it ends the home component, so
// /home/a matches in "cat /home/a/x" and "cd /home/a" but not inside
// /home/abc or /mnt/home/a.
func redactHome(text, home string) string {
	if home == "" || !strings.Contains(text, home) {
		return text
	}
	var builder strings.Builder
	builder.Grow(len(text))
	copied := 0
	for from := 0; from < len(text); {
		index := strings.Index(text[from:], home)
		if index < 0 {
			break
		}
		start := from + index
		end := start + len(home)
		if startsPath(text, start) && endsHome(text, end) {
			builder.WriteString(text[copied:start])
			builder.WriteByte('~')
			copied = end
			from = end
			continue
		}
		from = start + 1
	}
	builder.WriteString(text[copied:])
	return builder.String()
}

// startsPath reports whether a path can begin at start. Rows hold JSON text,
// so a JSON escape such as \n or \t also ends the previous token even though
// its letter is a path byte.
func startsPath(text string, start int) bool {
	if start == 0 || !isPathByte(text[start-1]) {
		return true
	}
	return start >= 2 && text[start-2] == '\\'
}

// endsHome reports whether the home component ends at end.
func endsHome(text string, end int) bool {
	return end == len(text) || text[end] == '/' || !isPathByte(text[end])
}

// isPathByte reports whether b can sit inside a path name. Bytes of
// multi-byte UTF-8 characters count, so a home never matches as the prefix of
// a longer non-ASCII directory name.
func isPathByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '.' || b == '_' || b == '-' || b == '/' || b >= 0x80:
		return true
	}
	return false
}
```

- [ ] **Step 4: Wire it into the recorder**

In `internal/classifier/corpus/recorder.go`:

In `New`, store the normalized home: `home: usableHome(home),`.

In `Record`, replace the redaction block with:

```go
	if recorder.options.RedactPaths && recorder.home != "" {
		entry.Action = redactHome(entry.Action, recorder.home)
		for i, item := range entry.Context {
			entry.Context[i] = redactHome(item, recorder.home)
		}
		// The verdict and its rationale quote the command under judgement, so
		// they carry absolute paths as often as the action does.
		entry.VerdictRaw = redactHome(entry.VerdictRaw, recorder.home)
		entry.Thinking = redactHome(entry.Thinking, recorder.home)
	}
```

If the compiler then reports `"strings" imported and not used` in `recorder.go`, remove that import.

- [ ] **Step 5: Run the corpus tests**

Run: `go test -race -count=1 ./internal/classifier/corpus/`
Expected: PASS, including the existing `TestRecorderRedactsHomeDirectory` and `TestRecorderRedactionOffLeavesPathsIntact`.

- [ ] **Step 6: Document whose home is redacted**

In `docs/classifier-rules.md`, replace this sentence:

```markdown
`redactPaths` is on by default: it replaces the home directory with `~` in the action, context, raw verdict and thinking fields. Set it to `false` to keep absolute paths.
```

with:

```markdown
`redactPaths` is on by default: it replaces the home directory with `~` in the action, context, raw verdict and thinking fields, wherever the home directory stands as a whole path prefix, so `/home/a` is not rewritten inside `/home/abc` or `/mnt/home/a`. The home directory is the proxy's own (`$HOME` of the proxy process), not Claude Code's. When the proxy runs as a different user, the Claude Code user's paths are kept as they are; the shipped `antigravity-go-proxy.service` runs as `root`, for example. A home directory of `/` turns redaction off, rather than rewriting every path separator. Set `redactPaths` to `false` to keep absolute paths.
```

- [ ] **Step 7: Commit**

```bash
git add internal/classifier/corpus/redact.go internal/classifier/corpus/redact_test.go internal/classifier/corpus/recorder.go internal/classifier/corpus/recorder_test.go docs/classifier-rules.md
git commit -m "fix(capture): redact the home directory only on path boundaries"
```

---

## Task 4: Validate the Laya question name and instructions (M5)

**Files:**
- Modify: `internal/api/management.go` (package-level variable above `handleConfigSave` at `:1105`; checks in the Laya loop at `:1172-1183`)
- Test: `internal/api/classifier_config_test.go` (three new test functions)

**Interfaces:**
- Consumes: `config.TargetBackend.LayaQuestionName`, `config.TargetBackend.LayaInstructions`, test helpers `newTestServerWithManager`, `postConfigRules`.
- Produces: `var layaQuestionNamePattern` (package `api`).

Spec §4.6 requires `layaQuestionName` to match `^[A-Za-z0-9_]{1,32}$` and `layaInstructions` to be non-empty when set. Neither check exists. An empty string means "use the Go default", so the checks apply only to a value that is present. For instructions, "non-empty" means not blank after trimming whitespace.

- [ ] **Step 1: Write the failing tests**

Append to `internal/api/classifier_config_test.go` (`encoding/json` and `strings` are already imported):

```go
func TestConfigSaveRejectsInvalidLayaQuestionName(t *testing.T) {
	for _, name := range []string{"has space", "risk-level", strings.Repeat("q", 33), "rísk"} {
		t.Run(name, func(t *testing.T) {
			srv, _, _ := newTestServerWithManager(t)
			quoted, err := json.Marshal(name)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			blob := `{"backends":{"local":{"name":"Local Laya","url":"http://127.0.0.1:8000/v1/systemone","format":"laya","model":"english","layaQuestionName":` + string(quoted) + `}}}`

			recorder := postConfigRules(t, srv, blob)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "layaQuestionName") {
				t.Errorf("error should name layaQuestionName, got %s", recorder.Body.String())
			}
			if _, exists := config.Get().Classifier.Backends["local"]; exists {
				t.Error("rejected backend was saved anyway")
			}
		})
	}
}

func TestConfigSaveAcceptsValidLayaQuestionName(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	blob := `{"backends":{"local":{"name":"Local Laya","url":"http://127.0.0.1:8000/v1/systemone","format":"laya","model":"english","layaQuestionName":"risk_2"}}}`

	recorder := postConfigRules(t, srv, blob)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", recorder.Code, recorder.Body.String())
	}
	if got := config.Get().Classifier.Backends["local"].LayaQuestionName; got != "risk_2" {
		t.Errorf("saved LayaQuestionName = %q, want risk_2", got)
	}
}

func TestConfigSaveRejectsBlankLayaInstructions(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	blob := `{"backends":{"local":{"name":"Local Laya","url":"http://127.0.0.1:8000/v1/systemone","format":"laya","model":"english","layaInstructions":"   \n"}}}`

	recorder := postConfigRules(t, srv, blob)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "layaInstructions") {
		t.Errorf("error should name layaInstructions, got %s", recorder.Body.String())
	}
	if _, exists := config.Get().Classifier.Backends["local"]; exists {
		t.Error("rejected backend was saved anyway")
	}
}
```

- [ ] **Step 2: Run them and confirm the rejections fail**

Run: `go test -count=1 -run 'TestConfigSave.*Laya' ./internal/api/`
Expected: `TestConfigSaveRejectsInvalidLayaQuestionName` and `TestConfigSaveRejectsBlankLayaInstructions` FAIL with `status = 200, want 400`. `TestConfigSaveAcceptsValidLayaQuestionName` and the existing Laya tests PASS.

- [ ] **Step 3: Add the checks**

In `internal/api/management.go`, add directly above `func (server *Server) handleConfigSave`:

```go
// layaQuestionNamePattern is spec §4.6's rule for layaQuestionName. The name
// keys both the question sent to Laya and the answer read back, so it stays a
// short identifier.
var layaQuestionNamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)
```

In the Laya validation loop, insert directly after the `layaStateChars` check and before `if len(backend.LayaCriteria) == 0 && len(backend.LayaSeverityMap) == 0 {`:

```go
			if backend.LayaQuestionName != "" && !layaQuestionNamePattern.MatchString(backend.LayaQuestionName) {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("backend %q: layaQuestionName must be 1 to 32 letters, digits or underscores", backendKey)})
				return
			}
			if backend.LayaInstructions != "" && strings.TrimSpace(backend.LayaInstructions) == "" {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("backend %q: layaInstructions must not be blank", backendKey)})
				return
			}
```

The position matters: the criteria check that follows it `continue`s when no criteria are set, which would skip anything placed after it.

- [ ] **Step 4: Run the tests**

Run: `go test -count=1 -run 'TestConfigSave' ./internal/api/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/management.go internal/api/classifier_config_test.go
git commit -m "fix(config): validate layaQuestionName and layaInstructions on save"
```

---

## Task 5: Name the kind filter in the exporter's no-rows warning

**Files:**
- Modify: `scripts/corpus_to_laya.py:192-197` (the `if not examples:` block in `main`)
- Modify: `docs/classifier-rules.md`
- Test: `scripts/test_corpus_to_laya.py` (two new tests)

**Interfaces:**
- Consumes: `convert(rows, kinds)` → `(examples, stats)`, where `stats["skipped_kind"]` counts upstream rows of a kind outside `kinds` (the source filter runs first).
- Produces: no new names.

When the kind filter removes every row (for example a corpus of Stage 2 upstream rows only), the warning still tells the operator to check that rules were set to passthrough. The kind filter is the real cause.

- [ ] **Step 1: Write the failing tests**

Append to `scripts/test_corpus_to_laya.py`:

```python
def test_main_no_rows_hint_names_the_kind_filter(tmp_path, capsys):
    corpus = tmp_path / "classifier-2026-09-23.jsonl"
    _write_corpus(corpus, [
        {"action": "a", "severity": 12, "source": "upstream", "kind": "stage2-severity"},
    ])
    assert main([str(corpus), "-o", str(tmp_path / "train.jsonl")]) == 0
    err = capsys.readouterr().err
    assert "no labelled rows" in err
    assert "--kind" in err
    assert "passthrough" not in err


def test_main_no_rows_hint_names_passthrough_when_nothing_went_upstream(tmp_path, capsys):
    corpus = tmp_path / "classifier-2026-09-23.jsonl"
    _write_corpus(corpus, [
        {"action": "a", "severity": 0, "source": "stub", "kind": STAGE1},
    ])
    assert main([str(corpus), "-o", str(tmp_path / "train.jsonl")]) == 0
    err = capsys.readouterr().err
    assert "no labelled rows" in err
    assert "passthrough" in err
    assert "--kind" not in err
```

- [ ] **Step 2: Run them and confirm the first fails**

Run: `python3 -m pytest scripts/test_corpus_to_laya.py -q`
Expected: `test_main_no_rows_hint_names_the_kind_filter` FAILS (`assert "--kind" in err`); all others pass.

- [ ] **Step 3: Implement the hint**

In `scripts/corpus_to_laya.py`, replace the `if not examples:` block at the end of `main` with:

```python
    if not examples:
        if stats["skipped_kind"]:
            hint = (
                f"{stats['skipped_kind']} upstream rows were of another kind than "
                f"{', '.join(kinds)}; pass --kind to include them."
            )
        else:
            hint = (
                "Capture only yields labels while classifier requests reach upstream "
                "— check that rules were set to passthrough."
            )
        print(f"WARNING: no labelled rows. {hint}", file=sys.stderr)
```

- [ ] **Step 4: Run the tests**

Run: `python3 -m pytest scripts/test_corpus_to_laya.py -q`
Expected: 17 passed.

- [ ] **Step 5: Document the warning**

In `docs/classifier-rules.md`, append this sentence to the paragraph that begins "The script prints a warning to stderr when the kept rows were graded by more than one `model`":

```markdown
When no row survives, the warning names the cause: the kind filter, if it removed upstream rows, and otherwise the need for classifier requests to reach upstream.
```

- [ ] **Step 6: Commit**

```bash
git add scripts/corpus_to_laya.py scripts/test_corpus_to_laya.py docs/classifier-rules.md
git commit -m "fix(export): name the kind filter when no labelled rows survive"
```

---

## Task 6: Bring the spec in line with the code

**Files:**
- Modify: `docs/superpowers/specs/2026-09-23-classifier-corpus-and-laya-design.md` (§3.5 at `:205-215`, §3.6 at `:217-223`, §3.7 table at `:242-249`, §3.8 at `:258-271`, §5 at `:478-479`)

**Interfaces:**
- Consumes: the behavior from Tasks 2, 3 and 5, and the existing validation in `internal/api/management.go:1154-1170` and `ClassifierCaptureConfig.Resolved()` in `internal/config/config.go:379-396`.
- Produces: no code.

This task changes only the spec text. Before you edit each section, check every value against the code it describes.

- [ ] **Step 1: §3.5 Recorder file handling**

Replace the bullet that begins "On the first write of a new UTC date" with:

```markdown
- On the first write of a new UTC date, and on the first write after a restart or a config save,
  files matching `classifier-*.jsonl` in `dir` are listed and all but the newest `maxFiles` are
  deleted. Each deletion is logged at Warn, naming the file.
- Rows are built and written on a goroutine started when the handler returns
  (`Recorder.RecordAsync`), so the disk write never sits in front of the response. At most 64
  writes run at once; a row past that is dropped and counted with the size-cap drops. A row still
  in flight when the process exits is lost.
```

- [ ] **Step 2: §3.6 Redaction**

Replace the first sentence of §3.6 ("When `redactPaths` is true (the default), every occurrence … before the row is written.") with:

```markdown
When `redactPaths` is true (the default), the proxy process's home directory (`$HOME`) is replaced
with `~` in `action`, `context`, `verdict_raw` and `thinking` before the row is written, wherever it
stands as a whole path prefix: the byte before it is not a path character (or ends a JSON escape
such as `\n`), and the byte after it is `/`, the end of the text, or not a path character. So
`/home/a` is not rewritten inside `/home/abc` or `/mnt/home/a`. A home of `/` turns redaction off.
The home is the proxy's own: when the proxy runs as a different user than Claude Code (the shipped
`antigravity-go-proxy.service` runs as `root`), the Claude Code user's paths are not redacted.
```

Keep the rest of §3.6 ("This is the entire redaction contract. …") unchanged.

- [ ] **Step 3: §3.7 Configuration table**

Replace the `contextEntries`, `maxFiles` and `maxFileBytes` rows with:

```markdown
| `contextEntries` | `2` | −1 to 20. −1 keeps the action alone; 0 resolves to the default of 2. |
| `maxFiles` | `8` | 0–365. 0 resolves to 8. |
| `maxFileBytes` | `67108864` (64 MiB) | 0 – 4 GiB. 0 resolves to 64 MiB. |
```

- [ ] **Step 4: §3.8 Export tooling**

Replace the three bullets from "Reads `source == \"upstream\"` rows only" through "…the corpus mixes two teachers." with:

```markdown
- Reads `source == "upstream"` rows only, and asserts the filter loudly rather than silently
  dropping rows.
- Keeps `kind == "stage1-severity"` rows by default; `--kind` (repeatable) selects other kinds.
  Stage 1 grades harm only, while Stage 2 also applies user intent that the exported state does not
  carry, so mixing the stages can give one action two labels.
- Skips rows where `severity` is `-1`.
- Warns when more than one distinct `system_sha256` appears (the monitor prompt changed
  mid-collection), and when the kept rows were graded by more than one distinct `model` (more than
  one teacher).
- When no row survives, the warning names the kind filter if it removed upstream rows, and otherwise
  the passthrough requirement of §3.9.
```

Keep the remaining §3.8 bullets ("Emits a `choice` question …" and "Rationale for `choice` …") unchanged.

- [ ] **Step 5: §5 Failure behavior**

Replace the final paragraph of §5 ("Phase 1 failures — … Capture never fails, delays, or alters a user request.") with:

```markdown
Phase 1 failures — directory not writable, file at its size cap, too many writes in flight,
response unparseable — are logged and swallowed. Capture never fails, delays, or alters a user
request: the row is built and written after the handler returns, off its goroutine.
```

- [ ] **Step 6: Check the edits against the code**

Run: `grep -n "contextEntries\|maxFiles\|maxFileBytes" internal/api/management.go | sed -n 1,6p` and read `Resolved()` in `internal/config/config.go`. Confirm that every range and default in the new §3.7 rows matches. Confirm that `maxInFlightRecords` in `internal/classifier/corpus/recorder.go` is 64.

- [ ] **Step 7: Commit**

```bash
git add docs/superpowers/specs/2026-09-23-classifier-corpus-and-laya-design.md
git commit -m "docs(spec): bring capture sections in line with the shipped behavior"
```

---

## Verification (controller, after all tasks)

- [ ] `go build ./...` and `go vet ./internal/...`: no output.
- [ ] `go test -count=1 ./...`: every package ok, 0 FAIL.
- [ ] `go test -race -count=1 ./internal/classifier/corpus/ ./internal/api/`: ok, no `DATA RACE`.
- [ ] `python3 -m pytest scripts/test_corpus_to_laya.py -q`: 17 passed.
- [ ] `git push fork feat/classifier-corpus-laya` to update PR #93.
- [ ] Edit PR #93's description (`gh pr edit 93 --repo gustavokch/antigravity-claude-proxy-go --body-file <file>`). In "Known limitations", delete the "Minor, deferred" bullet and the spec-drift bullet. Add one line under "Read this before the diff": capture rows are written on a background goroutine after the response (at most 64 in flight), and rows in flight at exit are lost.
