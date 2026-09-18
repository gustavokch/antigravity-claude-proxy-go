# Throttle Calibration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Derive the proxy's pacing and cooldown settings from the 429 journal it already writes, instead of from static heuristics or a destructive upstream benchmark.

**Architecture:** `internal/accounts` gains a per-account meter that tracks in-flight and prior-minute request counts, and the existing `Forensics429Recorder` gains three fields plus a recovery record so the journal carries concurrency, rate, and recovery-window evidence. A new read-only package `internal/calibrate` parses that journal and derives config values behind confidence guards, and `cmd/calibrate` prints a suggested JSON fragment. Nothing writes `config.json`.

**Tech Stack:** Go 1.27rc2, standard library only. Tests use `testing` and `net/http/httptest`. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-18-throttle-calibration-design.md`

## Global Constraints

- Go 1.27rc2 (`go.mod`). Standard library only — do not add dependencies.
- Journal records carry response data only: no request headers, no tokens, no cookies, no prompt text. Every field added in this plan is a counter or a timestamp. This is R2 of `docs/superpowers/plans/2026-09-17-cloudcode-429-throttle-dimension-spec.md`.
- Recorded bodies stay capped at `maxForensicsBodyLen` = 4096 bytes.
- The calibrator never writes `config.json`. It prints.
- The active probe is capped at 10 upstream requests and runs only under an explicit flag.
- Naming follows the package's existing style: full words, no abbreviations (`account`, not `acct`; `request`, not `req`). Receiver names are spelled out (`func (dispatcher *Dispatcher)`, `func (meter *throttleMeter)`).
- Guards never guess. A failed guard omits the key from the fragment and prints `insufficient data`.

## File Structure

| File | Responsibility |
|---|---|
| `internal/accounts/throttlemeter.go` *(create)* | Per-account in-flight gauge and 60s send counter. Pure, no I/O. |
| `internal/accounts/throttlemeter_test.go` *(create)* | Meter behaviour with an injected clock. |
| `internal/accounts/forensics.go` *(modify)* | Three new `Forensics429Entry` fields and the outcome constants. |
| `internal/accounts/dispatcher.go` *(modify)* | Own the meter, populate the new fields, write the recovery record. |
| `internal/calibrate/journal.go` *(create)* | Read and parse the JSONL, tolerating pre-migration lines. |
| `internal/calibrate/derive.go` *(create)* | Formulas, guards, report text, config fragment. |
| `internal/calibrate/probe.go` *(create)* | The daily-endpoint probe and its body parser. |
| `cmd/calibrate/main.go` *(create)* | Flags, wiring, output. |

**Deviation from the spec's file list, noted deliberately:** §8 put the probe in `cmd/calibrate/main.go`. This plan puts it in `internal/calibrate/probe.go` so its body parser and its 401 re-resolve path can be tested against `httptest` — logic in `main` cannot be. No behaviour changes.

---

### Task 1: Per-account throttle meter

The meter answers two questions the journal cannot currently answer: how many requests were in flight when a 429 landed, and how many that account had sent in the previous minute. It also remembers that an account was rejected, so the next success can be journalled as a recovery.

**Files:**
- Create: `internal/accounts/throttlemeter.go`
- Test: `internal/accounts/throttlemeter_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `newThrottleMeter(now func() time.Time) *throttleMeter`; `(*throttleMeter).Begin(account string) (inFlight int, priorMinute int)`; `(*throttleMeter).End(account string)`; `(*throttleMeter).Observe(account string) (inFlight int, priorMinute int)`; `(*throttleMeter).MarkRejected(account string)`; `(*throttleMeter).TakeRecovery(account string) bool`. All unexported to the package.

- [ ] **Step 1: Write the failing test**

Create `internal/accounts/throttlemeter_test.go`:

```go
package accounts

import (
	"sync"
	"testing"
	"time"
)

// fakeClock returns a clock function whose value the test advances by hand, so
// the sixty-second window can be crossed without sleeping.
func fakeClock(start time.Time) (func() time.Time, func(time.Duration)) {
	var mu sync.Mutex
	current := start
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
	advance := func(delta time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		current = current.Add(delta)
	}
	return now, advance
}

func TestBeginCountsInFlightAndPriorMinute(t *testing.T) {
	now, _ := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)

	inFlight, priorMinute := meter.Begin("a@example.com")
	if inFlight != 1 || priorMinute != 1 {
		t.Fatalf("first Begin: got inFlight=%d priorMinute=%d, want 1 and 1", inFlight, priorMinute)
	}

	inFlight, priorMinute = meter.Begin("a@example.com")
	if inFlight != 2 || priorMinute != 2 {
		t.Fatalf("second Begin: got inFlight=%d priorMinute=%d, want 2 and 2", inFlight, priorMinute)
	}
}

func TestEndDecrementsInFlightButNotPriorMinute(t *testing.T) {
	now, _ := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)

	meter.Begin("a@example.com")
	meter.Begin("a@example.com")
	meter.End("a@example.com")

	inFlight, priorMinute := meter.Observe("a@example.com")
	if inFlight != 1 {
		t.Fatalf("inFlight after End: got %d, want 1", inFlight)
	}
	if priorMinute != 2 {
		t.Fatalf("priorMinute after End: got %d, want 2 — a completed request was still sent", priorMinute)
	}
}

func TestEndDoesNotGoNegative(t *testing.T) {
	now, _ := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)

	meter.End("a@example.com")

	inFlight, _ := meter.Observe("a@example.com")
	if inFlight != 0 {
		t.Fatalf("inFlight after unmatched End: got %d, want 0", inFlight)
	}
}

func TestPriorMinuteDropsSendsOlderThanTheWindow(t *testing.T) {
	now, advance := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)

	meter.Begin("a@example.com")
	meter.End("a@example.com")
	advance(61 * time.Second)

	_, priorMinute := meter.Begin("a@example.com")
	if priorMinute != 1 {
		t.Fatalf("priorMinute after the window passed: got %d, want 1", priorMinute)
	}
}

func TestPriorMinuteKeepsSendsInsideTheWindow(t *testing.T) {
	now, advance := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)

	meter.Begin("a@example.com")
	meter.End("a@example.com")
	advance(59 * time.Second)

	_, priorMinute := meter.Begin("a@example.com")
	if priorMinute != 2 {
		t.Fatalf("priorMinute inside the window: got %d, want 2", priorMinute)
	}
}

func TestAccountsAreTrackedIndependently(t *testing.T) {
	now, _ := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)

	meter.Begin("a@example.com")
	meter.Begin("a@example.com")
	meter.Begin("b@example.com")

	inFlight, _ := meter.Observe("b@example.com")
	if inFlight != 1 {
		t.Fatalf("second account inFlight: got %d, want 1", inFlight)
	}
}

func TestTakeRecoveryOnlyFiresOncePerRejection(t *testing.T) {
	now, _ := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)

	if meter.TakeRecovery("a@example.com") {
		t.Fatal("TakeRecovery before any rejection: got true, want false")
	}

	meter.MarkRejected("a@example.com")
	if !meter.TakeRecovery("a@example.com") {
		t.Fatal("first TakeRecovery after a rejection: got false, want true")
	}
	if meter.TakeRecovery("a@example.com") {
		t.Fatal("second TakeRecovery after one rejection: got true, want false")
	}
}

func TestRecoveryIsPerAccount(t *testing.T) {
	now, _ := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)

	meter.MarkRejected("a@example.com")
	if meter.TakeRecovery("b@example.com") {
		t.Fatal("TakeRecovery for an unrejected account: got true, want false")
	}
}

func TestNilClockDefaultsToTimeNow(t *testing.T) {
	meter := newThrottleMeter(nil)
	if _, priorMinute := meter.Begin("a@example.com"); priorMinute != 1 {
		t.Fatalf("Begin with a nil clock: got priorMinute=%d, want 1", priorMinute)
	}
}

func TestMeterIsSafeUnderConcurrentUse(t *testing.T) {
	now, _ := fakeClock(time.Unix(1_700_000_000, 0))
	meter := newThrottleMeter(now)

	var group sync.WaitGroup
	for range 50 {
		group.Add(1)
		go func() {
			defer group.Done()
			meter.Begin("a@example.com")
			meter.Observe("a@example.com")
			meter.End("a@example.com")
		}()
	}
	group.Wait()

	inFlight, priorMinute := meter.Observe("a@example.com")
	if inFlight != 0 {
		t.Fatalf("inFlight after balanced concurrent use: got %d, want 0", inFlight)
	}
	if priorMinute != 50 {
		t.Fatalf("priorMinute after 50 sends: got %d, want 50", priorMinute)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/accounts/ -run 'Throttle|Meter|Begin|End|PriorMinute|Recovery|Accounts' -v`

Expected: FAIL — `undefined: newThrottleMeter`.

- [ ] **Step 3: Write the implementation**

Create `internal/accounts/throttlemeter.go`:

```go
package accounts

import (
	"sync"
	"time"
)

// rateWindow is the lookback behind PriorMinuteRequests. The calibrator derives
// a per-minute ceiling, so the counter has to cover exactly a minute.
const rateWindow = time.Minute

// throttleMeter tracks, per account, how many requests are in flight and how
// many were sent inside rateWindow. The 429 journal records both numbers so a
// calibrator can recover concurrency and rate ceilings from production traffic
// instead of from a benchmark that has to throttle a real account to learn
// anything.
//
// It also remembers that an account was rejected, so the next success on that
// account can be journalled as a recovery. The gap between the two is the only
// measurement of how long the throttle actually held.
type throttleMeter struct {
	mu       sync.Mutex
	now      func() time.Time
	inFlight map[string]int
	sends    map[string][]time.Time
	rejected map[string]bool
}

func newThrottleMeter(now func() time.Time) *throttleMeter {
	if now == nil {
		now = time.Now
	}
	return &throttleMeter{
		now:      now,
		inFlight: make(map[string]int),
		sends:    make(map[string][]time.Time),
		rejected: make(map[string]bool),
	}
}

// Begin records a request leaving for account. It returns the in-flight count
// including this request and the number of sends inside rateWindow including
// this one, which are the two numbers the journal needs at rejection time.
func (meter *throttleMeter) Begin(account string) (int, int) {
	meter.mu.Lock()
	defer meter.mu.Unlock()
	now := meter.now()
	meter.inFlight[account]++
	sends := append(trimSendsBefore(meter.sends[account], now.Add(-rateWindow)), now)
	meter.sends[account] = sends
	return meter.inFlight[account], len(sends)
}

// End records a request returning for account. It leaves the send history
// alone: a completed request was still sent, and still counts against a rate.
func (meter *throttleMeter) End(account string) {
	meter.mu.Lock()
	defer meter.mu.Unlock()
	if meter.inFlight[account] > 0 {
		meter.inFlight[account]--
	}
}

// Observe reports the current counts without recording a send. Rejection sites
// that do not hold the values Begin returned read them from here.
func (meter *throttleMeter) Observe(account string) (int, int) {
	meter.mu.Lock()
	defer meter.mu.Unlock()
	sends := trimSendsBefore(meter.sends[account], meter.now().Add(-rateWindow))
	meter.sends[account] = sends
	return meter.inFlight[account], len(sends)
}

// MarkRejected notes that account was throttled, arming the next success on it
// to be journalled as a recovery.
func (meter *throttleMeter) MarkRejected(account string) {
	meter.mu.Lock()
	defer meter.mu.Unlock()
	meter.rejected[account] = true
}

// TakeRecovery reports whether this success is the first one after a rejection,
// and disarms so the successes behind it are not journalled too.
func (meter *throttleMeter) TakeRecovery(account string) bool {
	meter.mu.Lock()
	defer meter.mu.Unlock()
	if !meter.rejected[account] {
		return false
	}
	delete(meter.rejected, account)
	return true
}

// trimSendsBefore drops timestamps at or before cutoff, keeping the history
// bounded by the request rate rather than by process uptime.
func trimSendsBefore(stamps []time.Time, cutoff time.Time) []time.Time {
	index := 0
	for index < len(stamps) && !stamps[index].After(cutoff) {
		index++
	}
	return stamps[index:]
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/accounts/ -run 'Throttle|Meter|Begin|End|PriorMinute|Recovery|Accounts|NilClock' -v`

Expected: PASS, all eleven tests.

- [ ] **Step 5: Run with the race detector**

Run: `go test ./internal/accounts/ -run TestMeterIsSafeUnderConcurrentUse -race`

Expected: PASS with no race reported.

- [ ] **Step 6: Commit**

```bash
git add internal/accounts/throttlemeter.go internal/accounts/throttlemeter_test.go
git commit -m "feat(accounts): track per-account in-flight and prior-minute counts

The 429 journal records which account was rejected but not how hard it was
being driven at the time, so neither a concurrency ceiling nor a rate ceiling
can be recovered from it. The meter supplies both, and remembers a rejection so
the next success can be journalled as the end of the throttle window."
```

---

### Task 2: Journal the new evidence

Add the three fields to the record, populate them at the rejection sites, and write a recovery record at the first success after a rejection. Without the recovery record the window length is unmeasurable.

**Files:**
- Modify: `internal/accounts/forensics.go:21-32` (the `Forensics429Entry` struct)
- Modify: `internal/accounts/dispatcher.go:82` (struct field), `:143` (constructor), `:374-382` (hot path), `:423` and `:636` (rejection sites), `:726-749` (`record429`)
- Test: `internal/accounts/forensics_test.go`

**Interfaces:**
- Consumes: `newThrottleMeter`, `Begin`, `End`, `Observe`, `MarkRejected`, `TakeRecovery` from Task 1.
- Produces: `Forensics429Entry` fields `InFlight int` (`json:"inFlight,omitempty"`), `PriorMinuteRequests int` (`json:"priorMinuteRequests,omitempty"`), `Outcome string` (`json:"outcome,omitempty"`); constants `OutcomeReject = "reject"` and `OutcomeRecover = "recover"`. Task 3 parses exactly these JSON names.

- [ ] **Step 1: Write the failing test**

Append to `internal/accounts/forensics_test.go` (create the file with a `package accounts` header if it does not exist):

```go
func TestRecordCarriesTheCalibrationFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream-429.jsonl")
	recorder := NewForensics429Recorder(path)

	recorder.Record(Forensics429Entry{
		Timestamp:           time.Unix(1_700_000_000, 0).UTC(),
		Account:             "a@example.com",
		Status:              429,
		Outcome:             OutcomeReject,
		InFlight:            4,
		PriorMinuteRequests: 37,
	})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	var entry Forensics429Entry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("unmarshal journal line: %v", err)
	}
	if entry.InFlight != 4 {
		t.Fatalf("InFlight: got %d, want 4", entry.InFlight)
	}
	if entry.PriorMinuteRequests != 37 {
		t.Fatalf("PriorMinuteRequests: got %d, want 37", entry.PriorMinuteRequests)
	}
	if entry.Outcome != OutcomeReject {
		t.Fatalf("Outcome: got %q, want %q", entry.Outcome, OutcomeReject)
	}
}

func TestRecoveryRecordCarriesNoBody(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream-429.jsonl")
	recorder := NewForensics429Recorder(path)

	recorder.Record(Forensics429Entry{
		Timestamp: time.Unix(1_700_000_060, 0).UTC(),
		Account:   "a@example.com",
		Status:    200,
		Outcome:   OutcomeRecover,
	})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if strings.Contains(string(data), `"body"`) {
		t.Fatalf("recovery record carried a body field: %s", data)
	}
	if strings.Contains(string(data), `"headers"`) {
		t.Fatalf("recovery record carried a headers field: %s", data)
	}
}
```

Imports this file needs: `encoding/json`, `os`, `path/filepath`, `strings`, `testing`, `time`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/accounts/ -run 'TestRecordCarriesTheCalibrationFields|TestRecoveryRecordCarriesNoBody' -v`

Expected: FAIL — `unknown field Outcome in struct literal` and `undefined: OutcomeReject`.

- [ ] **Step 3: Add the fields and constants**

In `internal/accounts/forensics.go`, replace the `Forensics429Entry` struct with:

```go
// Outcome values for Forensics429Entry. A reject record is one upstream 429; a
// recover record is the first success on an account that had been rejected.
// The gap between a matching pair is the measured throttle window.
const (
	OutcomeReject  = "reject"
	OutcomeRecover = "recover"
)

// Forensics429Entry is one recorded upstream throttle event. Response data
// only: no request headers, no tokens, no cookies, no prompt text (R2 of the
// cloudcode-429-throttle-dimension spec). InFlight and PriorMinuteRequests are
// counters, not content.
type Forensics429Entry struct {
	Timestamp           time.Time         `json:"timestamp"`
	Account             string            `json:"account,omitempty"`
	Project             string            `json:"project,omitempty"`
	Model               string            `json:"model,omitempty"`
	Endpoint            string            `json:"endpoint,omitempty"`
	Status              int               `json:"status"`
	Outcome             string            `json:"outcome,omitempty"`
	InFlight            int               `json:"inFlight,omitempty"`
	PriorMinuteRequests int               `json:"priorMinuteRequests,omitempty"`
	Headers             map[string]string `json:"headers,omitempty"`
	Body                string            `json:"body,omitempty"`
	AppliedWait         string            `json:"appliedWait,omitempty"`
	Failures            int               `json:"failures,omitempty"`
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/accounts/ -run 'TestRecordCarriesTheCalibrationFields|TestRecoveryRecordCarriesNoBody' -v`

Expected: PASS.

- [ ] **Step 5: Give the dispatcher a meter**

In `internal/accounts/dispatcher.go`, add the field next to `forensics429` (line 82):

```go
	forensics429             *Forensics429Recorder
	meter                    *throttleMeter
```

And initialise it in the constructor next to the `forensics429` assignment (line 143):

```go
		forensics429:             options.Forensics429,
		meter:                    newThrottleMeter(nil),
```

The meter takes `nil` rather than `dispatcher.now` because `dispatcher.now` is not yet assigned at this point in the literal. Both resolve to `time.Now` in production; tests inject a clock through `newThrottleMeter` directly.

- [ ] **Step 6: Populate the counts on the hot path**

In `internal/accounts/dispatcher.go`, replace lines 374-382:

```go
			response, requestErr := client.StreamGenerateContent(ctx, payload, options, func(event cloudcode.SSEEvent) error {
				eventCount++
				return consume(event)
			})
			if requestErr == nil {
				dispatcher.manager.MarkSuccess(account, model)
				cloudcode.SetExecutionMetadata(ctx, account.Email, project)
				return response, nil
			}
```

with:

```go
			inFlight, priorMinute := dispatcher.meter.Begin(account.Email)
			response, requestErr := client.StreamGenerateContent(ctx, payload, options, func(event cloudcode.SSEEvent) error {
				eventCount++
				return consume(event)
			})
			dispatcher.meter.End(account.Email)
			if requestErr == nil {
				if dispatcher.meter.TakeRecovery(account.Email) {
					dispatcher.recordRecovery(account, project, model, inFlight, priorMinute)
				}
				dispatcher.manager.MarkSuccess(account, model)
				cloudcode.SetExecutionMetadata(ctx, account.Email, project)
				return response, nil
			}
```

`End` runs before the rejection sites read the meter, but `inFlight` and `priorMinute` are already captured in locals, so the rejection record still reports the counts as they stood while the request was outstanding.

- [ ] **Step 7: Widen record429 and add recordRecovery**

In `internal/accounts/dispatcher.go`, replace `record429` (lines 724-749) with:

```go
// record429 persists one upstream 429 verbatim for later throttle-dimension
// analysis, together with how hard the account was being driven when it was
// rejected. No-op when forensics is disabled.
func (dispatcher *Dispatcher) record429(account *Account, project, model string, upstreamError *cloudcode.HTTPError, wait time.Duration, failures, inFlight, priorMinute int) {
	dispatcher.mu.RLock()
	recorder := dispatcher.forensics429
	dispatcher.mu.RUnlock()
	email := ""
	if account != nil {
		email = account.Email
	}
	dispatcher.meter.MarkRejected(email)
	if recorder == nil || !recorder.Enabled() {
		return
	}
	recorder.Record(Forensics429Entry{
		Timestamp:           dispatcher.now(),
		Account:             email,
		Project:             project,
		Model:               model,
		Endpoint:            upstreamError.Endpoint,
		Status:              upstreamError.StatusCode,
		Outcome:             OutcomeReject,
		InFlight:            inFlight,
		PriorMinuteRequests: priorMinute,
		Headers:             forensicsHeaders(upstreamError.Header),
		Body:                upstreamError.Body,
		AppliedWait:         wait.Round(time.Second).String(),
		Failures:            failures,
	})
}

// recordRecovery persists the first success on an account that had been
// throttled. The gap from its matching reject record is the only direct
// measurement of how long the throttle held.
func (dispatcher *Dispatcher) recordRecovery(account *Account, project, model string, inFlight, priorMinute int) {
	dispatcher.mu.RLock()
	recorder := dispatcher.forensics429
	dispatcher.mu.RUnlock()
	if recorder == nil || !recorder.Enabled() {
		return
	}
	email := ""
	if account != nil {
		email = account.Email
	}
	recorder.Record(Forensics429Entry{
		Timestamp:           dispatcher.now(),
		Account:             email,
		Project:             project,
		Model:               model,
		Status:              http.StatusOK,
		Outcome:             OutcomeRecover,
		InFlight:            inFlight,
		PriorMinuteRequests: priorMinute,
	})
}
```

`MarkRejected` sits above the `recorder == nil` guard on purpose: the meter has to arm even when journalling is switched off, so enabling the journal mid-process never produces an orphaned recovery record.

- [ ] **Step 8: Update the two call sites**

At `internal/accounts/dispatcher.go:423`, pass the loop locals:

```go
				dispatcher.record429(account, project, model, upstreamError, wait, failures, inFlight, priorMinute)
```

At `internal/accounts/dispatcher.go:636`, which is a handler without those locals, read them from the meter:

```go
	case http.StatusTooManyRequests:
		wait := Decorrelate(SmartBackoff(ClassifyError(body, upstreamError.StatusCode), ParseResetTime(upstreamError.Header, body, dispatcher.now()), dispatcher.manager.FailureCount(account)), dispatcher.random)
		email := ""
		if account != nil {
			email = account.Email
		}
		inFlight, priorMinute := dispatcher.meter.Observe(email)
		dispatcher.record429(account, "", model, upstreamError, wait, dispatcher.manager.FailureCount(account), inFlight, priorMinute)
		dispatcher.manager.MarkRateLimited(account, model, wait)
		return true
```

- [ ] **Step 9: Verify the package builds and the existing suite still passes**

Run: `go build ./... && go test ./internal/accounts/ -count=1`

Expected: build clean, all tests PASS. If `net/http` is not already imported in `dispatcher.go`, add it — `recordRecovery` uses `http.StatusOK`.

- [ ] **Step 10: Commit**

```bash
git add internal/accounts/forensics.go internal/accounts/forensics_test.go internal/accounts/dispatcher.go
git commit -m "feat(accounts): journal in-flight, rate, and recovery evidence

The journal recorded every rejection but nothing about how hard the account was
being driven, and no successes at all, so the three numbers a calibrator needs
were all missing. Rejections now carry the in-flight and prior-minute counts,
and the first success after a rejection is journalled as a recovery."
```

---

### Task 3: Read the journal

A tolerant reader. The existing journal already holds pre-migration lines with none of the new fields, and an append-only file written by a long-lived process can end in a partial line.

**Files:**
- Create: `internal/calibrate/journal.go`
- Test: `internal/calibrate/journal_test.go`

**Interfaces:**
- Consumes: the JSON field names from Task 2.
- Produces: `type Entry struct` with exported fields `Timestamp time.Time`, `Account string`, `Model string`, `Endpoint string`, `Status int`, `Outcome string`, `InFlight int`, `PriorMinuteRequests int`, `Failures int`; `type Journal struct { Entries []Entry; Skipped int }`; `func ReadJournal(path string) (Journal, error)`; `func DefaultJournalPath() string`.

- [ ] **Step 1: Write the failing test**

Create `internal/calibrate/journal_test.go`:

```go
package calibrate

import (
	"os"
	"path/filepath"
	"testing"
)

// writeJournal puts lines in a temp file and returns its path.
func writeJournal(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "upstream-429.jsonl")
	content := ""
	for _, line := range lines {
		content += line + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write journal: %v", err)
	}
	return path
}

func TestReadJournalParsesTheCalibrationFields(t *testing.T) {
	path := writeJournal(t,
		`{"timestamp":"2026-09-17T21:40:00Z","account":"a@example.com","status":429,"outcome":"reject","inFlight":4,"priorMinuteRequests":37}`,
	)

	journal, err := ReadJournal(path)
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}
	if len(journal.Entries) != 1 {
		t.Fatalf("entries: got %d, want 1", len(journal.Entries))
	}
	entry := journal.Entries[0]
	if entry.Account != "a@example.com" {
		t.Fatalf("Account: got %q, want %q", entry.Account, "a@example.com")
	}
	if entry.InFlight != 4 {
		t.Fatalf("InFlight: got %d, want 4", entry.InFlight)
	}
	if entry.PriorMinuteRequests != 37 {
		t.Fatalf("PriorMinuteRequests: got %d, want 37", entry.PriorMinuteRequests)
	}
	if entry.Outcome != "reject" {
		t.Fatalf("Outcome: got %q, want %q", entry.Outcome, "reject")
	}
}

func TestReadJournalTreatsPreMigrationLinesAsRejects(t *testing.T) {
	path := writeJournal(t,
		`{"timestamp":"2026-09-17T21:40:00Z","account":"a@example.com","status":429,"body":"Resource has been exhausted"}`,
	)

	journal, err := ReadJournal(path)
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}
	entry := journal.Entries[0]
	if entry.Outcome != "reject" {
		t.Fatalf("Outcome for a line written before the field existed: got %q, want %q", entry.Outcome, "reject")
	}
	if entry.InFlight != 0 {
		t.Fatalf("InFlight for a pre-migration line: got %d, want 0", entry.InFlight)
	}
}

func TestReadJournalSkipsUnparseableLinesAndCountsThem(t *testing.T) {
	path := writeJournal(t,
		`{"timestamp":"2026-09-17T21:40:00Z","account":"a@example.com","status":429,"outcome":"reject"}`,
		`{"timestamp":"2026-09-17T21:41:00Z","acc`,
		``,
		`{"timestamp":"2026-09-17T21:42:00Z","account":"a@example.com","status":200,"outcome":"recover"}`,
	)

	journal, err := ReadJournal(path)
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}
	if len(journal.Entries) != 2 {
		t.Fatalf("entries: got %d, want 2", len(journal.Entries))
	}
	if journal.Skipped != 1 {
		t.Fatalf("Skipped: got %d, want 1 — the blank line is not a skip", journal.Skipped)
	}
}

func TestReadJournalReturnsEmptyForAMissingFile(t *testing.T) {
	journal, err := ReadJournal(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil {
		t.Fatalf("ReadJournal on a missing file: got error %v, want nil", err)
	}
	if len(journal.Entries) != 0 {
		t.Fatalf("entries: got %d, want 0", len(journal.Entries))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/calibrate/ -v`

Expected: FAIL — the package does not exist.

- [ ] **Step 3: Write the implementation**

Create `internal/calibrate/journal.go`:

```go
// Package calibrate derives the proxy's pacing and cooldown settings from the
// 429 journal the dispatcher writes in production. It reads; it never writes
// configuration.
package calibrate

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"antigravity-go-proxy/internal/config"
)

// Outcome values, mirroring internal/accounts. They are duplicated rather than
// imported so the calibrator can read a journal written by any build.
const (
	OutcomeReject  = "reject"
	OutcomeRecover = "recover"
)

// Entry is one journal line. It carries the calibration fields and drops the
// forensic ones — bodies and headers answer the dimension question, not the
// pacing question.
type Entry struct {
	Timestamp           time.Time `json:"timestamp"`
	Account             string    `json:"account"`
	Model               string    `json:"model"`
	Endpoint            string    `json:"endpoint"`
	Status              int       `json:"status"`
	Outcome             string    `json:"outcome"`
	InFlight            int       `json:"inFlight"`
	PriorMinuteRequests int       `json:"priorMinuteRequests"`
	Failures            int       `json:"failures"`
}

// Journal is a parsed journal file. Skipped counts lines that did not parse, so
// a report can say how much of the file it could not read.
type Journal struct {
	Entries []Entry
	Skipped int
}

// maxJournalLine bounds one line. Bodies are capped at 4 KB upstream, so a
// larger line is corruption rather than a record.
const maxJournalLine = 64 * 1024

// DefaultJournalPath is where the dispatcher writes when forensics is enabled.
func DefaultJournalPath() string {
	return filepath.Join(config.GetConfigDir(), "forensics", "upstream-429.jsonl")
}

// ReadJournal parses path. A missing file is an empty journal, not an error:
// the operator may simply not have hit a 429 yet. Unparseable lines are skipped
// and counted — an append-only file written by a live process can end mid-line.
func ReadJournal(path string) (Journal, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Journal{}, nil
		}
		return Journal{}, err
	}
	defer file.Close()

	var journal Journal
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), maxJournalLine)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			journal.Skipped++
			continue
		}
		if entry.Outcome == "" {
			// Written before the field existed. Every line in that era was a
			// rejection, because successes were not journalled at all.
			entry.Outcome = OutcomeReject
		}
		journal.Entries = append(journal.Entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return journal, err
	}
	return journal, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/calibrate/ -v`

Expected: PASS, all four tests.

- [ ] **Step 5: Commit**

```bash
git add internal/calibrate/journal.go internal/calibrate/journal_test.go
git commit -m "feat(calibrate): read the 429 journal

Tolerant by design: the file on disk already holds lines written before the
calibration fields existed, and an append-only file written by a live process
can end mid-line. Both cases are data, not failures."
```

---

### Task 4: Derive the settings

The formulas from spec §5, each behind a guard. A guard that fails omits the key rather than emitting a number the sample cannot support.

**Files:**
- Create: `internal/calibrate/derive.go`
- Test: `internal/calibrate/derive_test.go`

**Interfaces:**
- Consumes: `Entry`, `Journal`, `OutcomeReject`, `OutcomeRecover` from Task 3.
- Produces: `type IntResult struct { Value int; OK bool; N int; Need int }`; `type DurationResult struct { Value time.Duration; OK bool; N int; Need int }`; `type Result struct { ConcurrencySafe IntResult; RPMSafe IntResult; Recover DurationResult; RecoverDaily time.Duration; Skipped int }`; `func Derive(journal Journal) Result`; `func (Result) Fragment() map[string]any`; `func (Result) Report() string`. Task 5 sets `Result.RecoverDaily`; Task 6 calls `Derive`, `Fragment`, and `Report`.

- [ ] **Step 1: Write the failing test**

Create `internal/calibrate/derive_test.go`:

```go
package calibrate

import (
	"strings"
	"testing"
	"time"
)

var journalStart = time.Date(2026, 9, 17, 21, 0, 0, 0, time.UTC)

// rejects builds n reject entries on one account, each with the given counts.
func rejects(account string, n, inFlight, priorMinute int) []Entry {
	entries := make([]Entry, 0, n)
	for index := range n {
		entries = append(entries, Entry{
			Timestamp:           journalStart.Add(time.Duration(index) * time.Hour),
			Account:             account,
			Status:              429,
			Outcome:             OutcomeReject,
			InFlight:            inFlight,
			PriorMinuteRequests: priorMinute,
		})
	}
	return entries
}

// rejectRecoverPair builds one reject and the recovery that followed it after
// gap, with failures set so the pair counts as a retried gap.
func rejectRecoverPair(account string, at time.Time, gap time.Duration) []Entry {
	return []Entry{
		{Timestamp: at, Account: account, Status: 429, Outcome: OutcomeReject, InFlight: 4, PriorMinuteRequests: 40, Failures: 1},
		{Timestamp: at.Add(gap), Account: account, Status: 200, Outcome: OutcomeRecover},
	}
}

func TestConcurrencySafeIsOneBelowTheLowestRejectingInFlight(t *testing.T) {
	entries := append(rejects("a@example.com", 3, 6, 40), rejects("a@example.com", 2, 4, 40)...)

	result := Derive(Journal{Entries: entries})

	if !result.ConcurrencySafe.OK {
		t.Fatalf("guard: got not OK with n=%d", result.ConcurrencySafe.N)
	}
	if result.ConcurrencySafe.Value != 3 {
		t.Fatalf("ConcurrencySafe: got %d, want 3 — one below the lowest rejecting in-flight of 4", result.ConcurrencySafe.Value)
	}
}

func TestConcurrencySafeNeedsFiveRejects(t *testing.T) {
	result := Derive(Journal{Entries: rejects("a@example.com", 4, 4, 40)})

	if result.ConcurrencySafe.OK {
		t.Fatal("guard: got OK with only four reject records")
	}
	if result.ConcurrencySafe.N != 4 || result.ConcurrencySafe.Need != 5 {
		t.Fatalf("guard counts: got n=%d need=%d, want 4 and 5", result.ConcurrencySafe.N, result.ConcurrencySafe.Need)
	}
}

func TestConcurrencySafeIgnoresRecordsWithoutTheField(t *testing.T) {
	entries := append(rejects("a@example.com", 5, 0, 40), rejects("a@example.com", 5, 4, 40)...)

	result := Derive(Journal{Entries: entries})

	if result.ConcurrencySafe.Value != 3 {
		t.Fatalf("ConcurrencySafe: got %d, want 3 — zero means the field was absent, not that one request rejected", result.ConcurrencySafe.Value)
	}
	if result.ConcurrencySafe.N != 5 {
		t.Fatalf("guard n: got %d, want 5 — only records carrying the field count", result.ConcurrencySafe.N)
	}
}

func TestConcurrencySafeFloorsAtOne(t *testing.T) {
	result := Derive(Journal{Entries: rejects("a@example.com", 5, 1, 40)})

	if result.ConcurrencySafe.Value != 1 {
		t.Fatalf("ConcurrencySafe: got %d, want 1 — never advise zero concurrency", result.ConcurrencySafe.Value)
	}
}

func TestRPMSafeIsOneBelowTheLowestRejectingRate(t *testing.T) {
	entries := append(rejects("a@example.com", 3, 4, 60), rejects("a@example.com", 2, 4, 37)...)

	result := Derive(Journal{Entries: entries})

	if !result.RPMSafe.OK {
		t.Fatalf("guard: got not OK with n=%d", result.RPMSafe.N)
	}
	if result.RPMSafe.Value != 36 {
		t.Fatalf("RPMSafe: got %d, want 36", result.RPMSafe.Value)
	}
}

func TestRecoverIsTheMedianRetriedGap(t *testing.T) {
	var entries []Entry
	entries = append(entries, rejectRecoverPair("a@example.com", journalStart, 10*time.Minute)...)
	entries = append(entries, rejectRecoverPair("a@example.com", journalStart.Add(2*time.Hour), 20*time.Minute)...)
	entries = append(entries, rejectRecoverPair("a@example.com", journalStart.Add(4*time.Hour), 30*time.Minute)...)

	result := Derive(Journal{Entries: entries})

	if !result.Recover.OK {
		t.Fatalf("guard: got not OK with n=%d", result.Recover.N)
	}
	if result.Recover.Value != 20*time.Minute {
		t.Fatalf("Recover: got %s, want 20m", result.Recover.Value)
	}
}

func TestRecoverNeedsThreePairs(t *testing.T) {
	var entries []Entry
	entries = append(entries, rejectRecoverPair("a@example.com", journalStart, 10*time.Minute)...)
	entries = append(entries, rejectRecoverPair("a@example.com", journalStart.Add(2*time.Hour), 20*time.Minute)...)

	result := Derive(Journal{Entries: entries})

	if result.Recover.OK {
		t.Fatal("guard: got OK with only two pairs")
	}
	if result.Recover.Need != 3 {
		t.Fatalf("guard need: got %d, want 3", result.Recover.Need)
	}
}

func TestRecoverSkipsGapsWithNoRetryAttempted(t *testing.T) {
	var entries []Entry
	for index, gap := range []time.Duration{10 * time.Minute, 20 * time.Minute, 30 * time.Minute} {
		pair := rejectRecoverPair("a@example.com", journalStart.Add(time.Duration(index)*2*time.Hour), gap)
		pair[0].Failures = 0
		entries = append(entries, pair...)
	}

	result := Derive(Journal{Entries: entries})

	if result.Recover.OK {
		t.Fatal("guard: got OK from gaps where no retry was attempted — those measure operator idleness, not the throttle")
	}
}

func TestRecoverPairsPerAccount(t *testing.T) {
	entries := []Entry{
		{Timestamp: journalStart, Account: "a@example.com", Status: 429, Outcome: OutcomeReject, Failures: 1},
		{Timestamp: journalStart.Add(1 * time.Minute), Account: "b@example.com", Status: 200, Outcome: OutcomeRecover},
	}

	result := Derive(Journal{Entries: entries})

	if result.Recover.N != 0 {
		t.Fatalf("pairs: got %d, want 0 — a recovery on another account is not this account's window", result.Recover.N)
	}
}

func TestFragmentOmitsKeysWhoseGuardFailed(t *testing.T) {
	result := Derive(Journal{Entries: rejects("a@example.com", 4, 4, 40)})

	fragment := result.Fragment()

	if _, present := fragment["requestDelayMs"]; present {
		t.Fatal("fragment carried requestDelayMs despite a failed guard")
	}
	if _, present := fragment["capacityBackoffTiersMs"]; present {
		t.Fatal("fragment carried capacityBackoffTiersMs despite a failed guard")
	}
}

func TestFragmentDerivesEveryKeyFromAFullSample(t *testing.T) {
	entries := rejects("a@example.com", 5, 4, 37)
	for index, gap := range []time.Duration{20 * time.Minute, 20 * time.Minute, 20 * time.Minute} {
		entries = append(entries, rejectRecoverPair("a@example.com", journalStart.Add(time.Duration(index+10)*time.Hour), gap)...)
	}

	fragment := Derive(Journal{Entries: entries}).Fragment()

	if fragment["requestDelayMs"] != 1667 {
		t.Fatalf("requestDelayMs: got %v, want 1667 — ceil(60000/36)", fragment["requestDelayMs"])
	}
	if fragment["sharedThrottleWindowMs"] != 1_200_000 {
		t.Fatalf("sharedThrottleWindowMs: got %v, want 1200000", fragment["sharedThrottleWindowMs"])
	}
	tiers, ok := fragment["capacityBackoffTiersMs"].([]int)
	if !ok {
		t.Fatalf("capacityBackoffTiersMs: got %T, want []int", fragment["capacityBackoffTiersMs"])
	}
	want := []int{10_000, 1_200_000, 1_800_000}
	if len(tiers) != len(want) {
		t.Fatalf("tiers: got %v, want %v", tiers, want)
	}
	for index := range want {
		if tiers[index] != want[index] {
			t.Fatalf("tiers: got %v, want %v — the top tier is capped at 1800s", tiers, want)
		}
	}
	bucket, ok := fragment["accountSelection.tokenBucket"].(map[string]any)
	if !ok {
		t.Fatalf("accountSelection.tokenBucket: got %T, want map", fragment["accountSelection.tokenBucket"])
	}
	if bucket["tokensPerMinute"] != 36 {
		t.Fatalf("tokensPerMinute: got %v, want 36", bucket["tokensPerMinute"])
	}
	if bucket["maxTokens"] != 9 {
		t.Fatalf("maxTokens: got %v, want 9 — min(20, 3*3)", bucket["maxTokens"])
	}
}

func TestRequestDelayNeverDropsBelowTheFloor(t *testing.T) {
	entries := rejects("a@example.com", 5, 4, 1000)

	fragment := Derive(Journal{Entries: entries}).Fragment()

	if fragment["requestDelayMs"] != 200 {
		t.Fatalf("requestDelayMs: got %v, want the 200ms floor", fragment["requestDelayMs"])
	}
}

func TestMaxTokensIsCappedAtTwenty(t *testing.T) {
	entries := rejects("a@example.com", 5, 40, 37)

	fragment := Derive(Journal{Entries: entries}).Fragment()

	bucket := fragment["accountSelection.tokenBucket"].(map[string]any)
	if bucket["maxTokens"] != 20 {
		t.Fatalf("maxTokens: got %v, want the cap of 20", bucket["maxTokens"])
	}
}

func TestReportNamesTheMissingSampleSize(t *testing.T) {
	report := Derive(Journal{Entries: rejects("a@example.com", 4, 4, 40)}).Report()

	if !strings.Contains(report, "insufficient data (n=4, need 5)") {
		t.Fatalf("report did not state the shortfall:\n%s", report)
	}
}

func TestReportFlagsDisagreementWithTheDailyProbe(t *testing.T) {
	var entries []Entry
	for index := range 3 {
		entries = append(entries, rejectRecoverPair("a@example.com", journalStart.Add(time.Duration(index)*2*time.Hour), 20*time.Minute)...)
	}
	result := Derive(Journal{Entries: entries})
	result.RecoverDaily = 60 * time.Minute

	report := result.Report()

	if !strings.Contains(report, "disagree") {
		t.Fatalf("report did not flag a 200%% disagreement with the daily probe:\n%s", report)
	}
}

func TestRecoverFallsBackToTheDailyProbeWhenTheGuardFails(t *testing.T) {
	result := Derive(Journal{})
	result.RecoverDaily = 17 * time.Minute

	fragment := result.Fragment()

	if fragment["sharedThrottleWindowMs"] != 1_020_000 {
		t.Fatalf("sharedThrottleWindowMs: got %v, want 1020000 from the daily probe fallback", fragment["sharedThrottleWindowMs"])
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/calibrate/ -run Derive -v`

Expected: FAIL — `undefined: Derive`.

- [ ] **Step 3: Write the implementation**

Create `internal/calibrate/derive.go`:

```go
package calibrate

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Sample sizes below which a value is reported as insufficient rather than
// emitted. A ceiling read off one or two rejections is an anecdote.
const (
	needRejects = 5
	needPairs   = 3
)

// Bounds on the emitted configuration.
const (
	minRequestDelayMs = 200
	maxBucketTokens   = 20
	maxBackoffTierMs  = 1_800_000
	firstBackoffTier  = 10_000
)

// dailyDisagreementRatio is how far the journal's window and the daily probe's
// may diverge before the report says they describe different buckets.
const dailyDisagreementRatio = 0.25

// IntResult is a derived count and the evidence behind it. OK false means the
// guard failed and Value must not be used.
type IntResult struct {
	Value int
	OK    bool
	N     int
	Need  int
}

// DurationResult is a derived duration and the evidence behind it.
type DurationResult struct {
	Value time.Duration
	OK    bool
	N     int
	Need  int
}

// Result is everything derived from one journal. RecoverDaily is filled by the
// active probe when it runs, and is zero otherwise.
type Result struct {
	ConcurrencySafe IntResult
	RPMSafe         IntResult
	Recover         DurationResult
	RecoverDaily    time.Duration
	Skipped         int
}

// Derive computes the safe ceilings from a journal. Every output carries its
// own guard; nothing is inferred from an empty sample.
func Derive(journal Journal) Result {
	result := Result{Skipped: journal.Skipped}

	var inFlights, rates []int
	for _, entry := range journal.Entries {
		if entry.Outcome != OutcomeReject {
			continue
		}
		// A zero means the field was absent on a pre-migration line, not that
		// the request rejected with nothing in flight — the rejected request
		// itself was always in flight.
		if entry.InFlight > 0 {
			inFlights = append(inFlights, entry.InFlight)
		}
		if entry.PriorMinuteRequests > 0 {
			rates = append(rates, entry.PriorMinuteRequests)
		}
	}

	result.ConcurrencySafe = oneBelowMinimum(inFlights)
	result.RPMSafe = oneBelowMinimum(rates)
	result.Recover = medianRetriedGap(journal.Entries)
	return result
}

// oneBelowMinimum takes the lowest observed rejecting value and steps one below
// it, which is the highest value never seen to reject.
func oneBelowMinimum(values []int) IntResult {
	outcome := IntResult{N: len(values), Need: needRejects}
	if len(values) < needRejects {
		return outcome
	}
	lowest := values[0]
	for _, value := range values[1:] {
		if value < lowest {
			lowest = value
		}
	}
	outcome.Value = max(1, lowest-1)
	outcome.OK = true
	return outcome
}

// medianRetriedGap pairs each reject with the next recovery on the same
// account and takes the median gap. Pairs where no retry was attempted are
// dropped: that gap measures how long the account was left alone, not how long
// the throttle held.
func medianRetriedGap(entries []Entry) DurationResult {
	ordered := make([]Entry, len(entries))
	copy(ordered, entries)
	sort.SliceStable(ordered, func(first, second int) bool {
		return ordered[first].Timestamp.Before(ordered[second].Timestamp)
	})

	type pending struct {
		at      time.Time
		retried bool
	}
	open := make(map[string]pending)
	var gaps []time.Duration

	for _, entry := range ordered {
		switch entry.Outcome {
		case OutcomeReject:
			open[entry.Account] = pending{at: entry.Timestamp, retried: entry.Failures > 0}
		case OutcomeRecover:
			start, ok := open[entry.Account]
			if !ok {
				continue
			}
			delete(open, entry.Account)
			if !start.retried {
				continue
			}
			if gap := entry.Timestamp.Sub(start.at); gap > 0 {
				gaps = append(gaps, gap)
			}
		}
	}

	outcome := DurationResult{N: len(gaps), Need: needPairs}
	if len(gaps) < needPairs {
		return outcome
	}
	sort.Slice(gaps, func(first, second int) bool { return gaps[first] < gaps[second] })
	outcome.Value = gaps[len(gaps)/2]
	outcome.OK = true
	return outcome
}

// window returns the recovery window to emit: the journal's measurement when
// its guard passed, otherwise the daily probe's value, otherwise zero.
func (result Result) window() time.Duration {
	if result.Recover.OK {
		return result.Recover.Value
	}
	return result.RecoverDaily
}

// Fragment renders the derived settings as config keys. A key whose guard
// failed is absent: an omitted key leaves the operator's current value alone,
// which is the safe outcome for a thin sample.
func (result Result) Fragment() map[string]any {
	fragment := map[string]any{}

	if result.RPMSafe.OK {
		fragment["requestDelayMs"] = max(minRequestDelayMs, int(math.Ceil(60000/float64(result.RPMSafe.Value))))
	}
	if result.RPMSafe.OK || result.ConcurrencySafe.OK {
		bucket := map[string]any{}
		if result.RPMSafe.OK {
			bucket["tokensPerMinute"] = result.RPMSafe.Value
		}
		if result.ConcurrencySafe.OK {
			bucket["maxTokens"] = min(maxBucketTokens, result.ConcurrencySafe.Value*3)
		}
		fragment["accountSelection.tokenBucket"] = bucket
	}
	if window := result.window(); window > 0 {
		windowMs := int(window / time.Millisecond)
		fragment["sharedThrottleWindowMs"] = windowMs
		fragment["capacityBackoffTiersMs"] = []int{
			firstBackoffTier,
			min(maxBackoffTierMs, windowMs),
			min(maxBackoffTierMs, windowMs*2),
		}
	}
	return fragment
}

// Report is the human-readable summary. It states what was derived, what was
// not, and how far short the evidence fell.
func (result Result) Report() string {
	var builder strings.Builder

	builder.WriteString("throttle calibration\n")
	if result.Skipped > 0 {
		fmt.Fprintf(&builder, "  %d journal line(s) did not parse and were skipped\n", result.Skipped)
	}

	writeInt := func(label string, value IntResult) {
		if value.OK {
			fmt.Fprintf(&builder, "  %-18s %d  (n=%d)\n", label, value.Value, value.N)
			return
		}
		fmt.Fprintf(&builder, "  %-18s insufficient data (n=%d, need %d)\n", label, value.N, value.Need)
	}
	writeInt("concurrency safe", result.ConcurrencySafe)
	writeInt("requests/min safe", result.RPMSafe)

	if result.Recover.OK {
		fmt.Fprintf(&builder, "  %-18s %s  (n=%d, median)\n", "recovery window", result.Recover.Value.Round(time.Second), result.Recover.N)
	} else {
		fmt.Fprintf(&builder, "  %-18s insufficient data (n=%d, need %d)\n", "recovery window", result.Recover.N, result.Recover.Need)
	}

	if result.RecoverDaily > 0 {
		fmt.Fprintf(&builder, "  %-18s %s  (daily endpoint)\n", "recovery window", result.RecoverDaily.Round(time.Second))
		if result.Recover.OK && disagree(result.Recover.Value, result.RecoverDaily) {
			builder.WriteString("  the journal and the daily endpoint disagree by more than 25%: they may describe different buckets\n")
		}
		if !result.Recover.OK {
			builder.WriteString("  using the daily endpoint value; it is a fallback, not a measurement of the bucket cloudcode-pa enforces\n")
		}
	}
	return builder.String()
}

// disagree reports whether two window measurements differ by more than
// dailyDisagreementRatio of the larger one.
func disagree(first, second time.Duration) bool {
	larger := math.Max(float64(first), float64(second))
	if larger == 0 {
		return false
	}
	return math.Abs(float64(first)-float64(second))/larger > dailyDisagreementRatio
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/calibrate/ -v`

Expected: PASS, all tests from Tasks 3 and 4.

- [ ] **Step 5: Commit**

```bash
git add internal/calibrate/derive.go internal/calibrate/derive_test.go
git commit -m "feat(calibrate): derive pacing settings behind confidence guards

Each ceiling is one step below the lowest value ever observed to reject, and
the recovery window is the median gap over pairs where a retry was actually
attempted. A guard that fails omits its key rather than emitting a number the
sample cannot support."
```

---

### Task 5: The daily-endpoint probe

The fallback for a journal with too few recovery pairs. Per spec §6.1, `daily-cloudcode-pa.googleapis.com` is the only endpoint observed to return `RetryInfo.retryDelay` and `quotaResetTimeStamp`; `cloudcode-pa.googleapis.com` returns a bare body with no quota, rate, or retry headers.

**Files:**
- Create: `internal/calibrate/probe.go`
- Test: `internal/calibrate/probe_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `func ParseRetryDelay(body string) (time.Duration, bool)`; `func GuardBody(body string) error`; `var ErrAccountProtected error`; `const MaxProbeRequests = 10`. Task 6 uses `MaxProbeRequests` directly — it is exported so there is exactly one cap in the tree.

`ParseRetryDelay` is written fresh rather than reusing `accounts.ParseResetTime`. That function returns `500 * time.Millisecond` when it finds nothing (`internal/accounts/retry.go:183-191`), which is indistinguishable from a real short delay. The probe needs a found/not-found answer.

- [ ] **Step 1: Write the failing test**

Create `internal/calibrate/probe_test.go`:

```go
package calibrate

import (
	"errors"
	"testing"
	"time"
)

// dailyBody is the verbatim daily-endpoint rejection recorded in
// docs/superpowers/plans/2026-09-17-cloudcode-429-throttle-dimension-spec.md §6.1.
const dailyBody = `{ "error": { "code": 429, "message": "Individual quota reached. Please upgrade your subscription to increase your limits. Resets in 17m31s.", "status": "RESOURCE_EXHAUSTED", "details": [ { "@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "QUOTA_EXHAUSTED", "domain": "cloudcode-pa.googleapis.com", "metadata": { "uiMessage": "true", "model": "gemini-3.8-flash-high", "quotaResetDelay": "17m31.337247485s", "quotaResetTimeStamp": "2026-09-18T00:58:20Z" } }, { "@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "1051.337247485s" } ] } }`

func TestParseRetryDelayReadsTheRecordedDailyBody(t *testing.T) {
	delay, ok := ParseRetryDelay(dailyBody)

	if !ok {
		t.Fatal("ParseRetryDelay: got not found on the recorded daily body")
	}
	if delay.Round(time.Second) != 1051*time.Second {
		t.Fatalf("delay: got %s, want 1051s", delay)
	}
}

func TestParseRetryDelayFallsBackToQuotaResetDelay(t *testing.T) {
	body := `{"error":{"details":[{"metadata":{"quotaResetDelay":"17m31.337247485s"}}]}}`

	delay, ok := ParseRetryDelay(body)

	if !ok {
		t.Fatal("ParseRetryDelay: got not found with only quotaResetDelay present")
	}
	if delay.Round(time.Second) != 17*time.Minute+31*time.Second {
		t.Fatalf("delay: got %s, want 17m31s", delay)
	}
}

func TestParseRetryDelayReportsNotFoundOnTheBareBody(t *testing.T) {
	body := `{ "error": { "code": 429, "message": "Resource has been exhausted (e.g. check quota).", "status": "RESOURCE_EXHAUSTED" } }`

	if _, ok := ParseRetryDelay(body); ok {
		t.Fatal("ParseRetryDelay: got found on the bare cloudcode-pa body, which carries no delay")
	}
}

func TestParseRetryDelayReportsNotFoundOnGarbage(t *testing.T) {
	if _, ok := ParseRetryDelay("not json at all"); ok {
		t.Fatal("ParseRetryDelay: got found on non-JSON input")
	}
}

func TestGuardBodyStopsOnValidationRequired(t *testing.T) {
	err := GuardBody(`{"error":{"status":"PERMISSION_DENIED","message":"VALIDATION_REQUIRED"}}`)

	if !errors.Is(err, ErrAccountProtected) {
		t.Fatalf("GuardBody: got %v, want ErrAccountProtected", err)
	}
}

func TestGuardBodyStopsOnADisabledAccount(t *testing.T) {
	err := GuardBody(`{"error":{"message":"This account has been disabled"}}`)

	if !errors.Is(err, ErrAccountProtected) {
		t.Fatalf("GuardBody: got %v, want ErrAccountProtected", err)
	}
}

func TestGuardBodyStopsOnATermsViolation(t *testing.T) {
	err := GuardBody(`{"error":{"message":"Gemini disabled for violation of terms of service"}}`)

	if !errors.Is(err, ErrAccountProtected) {
		t.Fatalf("GuardBody: got %v, want ErrAccountProtected", err)
	}
}

func TestGuardBodyStopsOnAPermanentAuthError(t *testing.T) {
	for _, body := range []string{`{"error":"invalid_grant"}`, `{"error":"unauthorized_client"}`} {
		if err := GuardBody(body); !errors.Is(err, ErrAccountProtected) {
			t.Fatalf("GuardBody(%s): got %v, want ErrAccountProtected", body, err)
		}
	}
}

func TestGuardBodyStopsOnAValidationURL(t *testing.T) {
	err := GuardBody(`{"error":{"message":"see validation_url https://accounts.google.com/signin/continue?x=1"}}`)

	if !errors.Is(err, ErrAccountProtected) {
		t.Fatalf("GuardBody: got %v, want ErrAccountProtected", err)
	}
}

func TestGuardBodyPassesAnOrdinaryThrottle(t *testing.T) {
	if err := GuardBody(dailyBody); err != nil {
		t.Fatalf("GuardBody on an ordinary quota rejection: got %v, want nil", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/calibrate/ -run 'ParseRetryDelay|GuardBody' -v`

Expected: FAIL — `undefined: ParseRetryDelay`.

- [ ] **Step 3: Write the implementation**

Create `internal/calibrate/probe.go`:

```go
package calibrate

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

// MaxProbeRequests caps the active probe. Its only job is to draw one
// rejection carrying a delay; more requests buy no more information and cost
// the operator real capacity.
const MaxProbeRequests = 10

// ErrAccountProtected stops the probe on anything that looks like account
// jeopardy rather than an ordinary throttle. The caller exits 3.
var ErrAccountProtected = errors.New("account protection triggered")

var (
	// retryDelayPattern matches google.rpc.RetryInfo's retryDelay, which the
	// daily endpoint returns as a bare seconds string.
	retryDelayPattern = regexp.MustCompile(`"retryDelay"\s*:\s*"(\d+(?:\.\d+)?)s"`)
	// quotaResetDelayPattern matches the Go-style duration in ErrorInfo
	// metadata, e.g. "17m31.337247485s".
	quotaResetDelayPattern = regexp.MustCompile(`"quotaResetDelay"\s*:\s*"([^"]+)"`)
)

// ParseRetryDelay extracts the upstream's own statement of how long the caller
// must wait. It reports found/not-found rather than a sentinel: a bare
// cloudcode-pa rejection carries no delay at all, and treating that as a short
// delay would emit a backoff ladder far below the measured window.
func ParseRetryDelay(body string) (time.Duration, bool) {
	if match := retryDelayPattern.FindStringSubmatch(body); match != nil {
		if delay, err := time.ParseDuration(match[1] + "s"); err == nil && delay > 0 {
			return delay, true
		}
	}
	if match := quotaResetDelayPattern.FindStringSubmatch(body); match != nil {
		if delay, err := time.ParseDuration(match[1]); err == nil && delay > 0 {
			return delay, true
		}
	}
	return 0, false
}

// protectionMarkers are the substrings that mean stop probing. Account
// jeopardy is never worth another request.
var protectionMarkers = []string{
	"validation_required",
	"validation_url",
	"accounts.google.com/signin/continue",
	"has been disabled",
	"violation of terms of service",
	"invalid_grant",
	"unauthorized_client",
}

// GuardBody returns ErrAccountProtected when a response body signals account
// jeopardy rather than an ordinary throttle.
func GuardBody(body string) error {
	lower := strings.ToLower(body)
	for _, marker := range protectionMarkers {
		if strings.Contains(lower, marker) {
			return ErrAccountProtected
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/calibrate/ -v`

Expected: PASS, all tests from Tasks 3, 4, and 5.

- [ ] **Step 5: Commit**

```bash
git add internal/calibrate/probe.go internal/calibrate/probe_test.go
git commit -m "feat(calibrate): parse the daily endpoint's stated retry delay

accounts.ParseResetTime cannot serve here: it returns 500ms when it finds
nothing, which is indistinguishable from a real short delay, and the probe has
to know whether the upstream stated a delay at all."
```

---

### Task 6: The CLI

Wire the parts together. Read the journal, optionally run the probe, print the report and the fragment. Never write `config.json`.

**Files:**
- Create: `cmd/calibrate/main.go`
- Test: `internal/calibrate/fragment_test.go`

**Interfaces:**
- Consumes: `ReadJournal`, `DefaultJournalPath`, `Derive`, `Result.Fragment`, `Result.Report`, `ParseRetryDelay`, `GuardBody`, `ErrAccountProtected`, `MaxProbeRequests`.
- Produces: the `calibrate` binary.

- [ ] **Step 1: Write the failing test**

This is spec §10 item 5: prove every emitted key matches a real `config.Config` struct tag. Create `internal/calibrate/fragment_test.go`:

```go
package calibrate

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"antigravity-go-proxy/internal/config"
)

func TestFragmentKeysMatchTheConfigStruct(t *testing.T) {
	entries := rejects("a@example.com", 5, 4, 37)
	for index := range 3 {
		entries = append(entries, rejectRecoverPair("a@example.com", journalStart.Add(time.Duration(index+10)*time.Hour), 20*time.Minute)...)
	}
	fragment := Derive(Journal{Entries: entries}).Fragment()

	// The bucket is nested under accountSelection in the real config, so lift
	// it into the shape config.Config actually declares before decoding.
	nested := map[string]any{}
	for key, value := range fragment {
		if key == "accountSelection.tokenBucket" {
			nested["accountSelection"] = map[string]any{"tokenBucket": value}
			continue
		}
		nested[key] = value
	}

	encoded, err := json.Marshal(nested)
	if err != nil {
		t.Fatalf("marshal fragment: %v", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var parsed config.Config
	if err := decoder.Decode(&parsed); err != nil {
		t.Fatalf("fragment does not match config.Config: %v\nfragment: %s", err, encoded)
	}

	if parsed.RequestDelayMs != 1667 {
		t.Fatalf("RequestDelayMs after the round trip: got %d, want 1667", parsed.RequestDelayMs)
	}
	if parsed.SharedThrottleWindowMs != 1_200_000 {
		t.Fatalf("SharedThrottleWindowMs after the round trip: got %d, want 1200000", parsed.SharedThrottleWindowMs)
	}
	if len(parsed.CapacityBackoffTiersMs) != 3 {
		t.Fatalf("CapacityBackoffTiersMs after the round trip: got %v, want three tiers", parsed.CapacityBackoffTiersMs)
	}
}

// AccountSelectionConfig.TokenBucket is a map[string]any
// (internal/config/config.go:97), so DisallowUnknownFields cannot police its
// keys. Check them against the defaults the proxy ships instead.
func TestBucketKeysExistInTheDefaultConfig(t *testing.T) {
	entries := rejects("a@example.com", 5, 4, 37)
	fragment := Derive(Journal{Entries: entries}).Fragment()

	bucket, ok := fragment["accountSelection.tokenBucket"].(map[string]any)
	if !ok {
		t.Fatalf("accountSelection.tokenBucket: got %T, want map", fragment["accountSelection.tokenBucket"])
	}
	if len(bucket) == 0 {
		t.Fatal("fragment emitted an empty token bucket")
	}
	defaults := config.DefaultConfig().AccountSelection.TokenBucket
	for key := range bucket {
		if _, present := defaults[key]; !present {
			t.Fatalf("fragment emits tokenBucket key %q, which the default config does not define", key)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails or reveals a key mismatch**

Run: `go test ./internal/calibrate/ -run TestFragmentKeysMatchTheConfigStruct -v`

Expected: this test either fails to compile (the file is new but the helpers exist from Task 4, so it should compile) or fails with a decode error naming a key that does not exist on `config.Config`. If it names one, fix the key in `Fragment` to match the struct tag in `internal/config/config.go` — the tags to match are `requestDelayMs` (line 117), `sharedThrottleWindowMs` (line 122), `capacityBackoffTiersMs` (line 127), and `maxTokens` (line 150). Do not change `config.Config` to fit the fragment.

- [ ] **Step 3: Write the CLI**

Create `cmd/calibrate/main.go`:

```go
// Command calibrate derives the proxy's pacing and cooldown settings from the
// 429 journal the dispatcher writes in production, and prints them. It never
// writes config.json: a thin or skewed sample must not silently repace the
// proxy.
//
// -probe-daily draws one rejection from daily-cloudcode-pa.googleapis.com,
// which is the only endpoint observed to state its own retry delay. It is a
// fallback for a journal with too few recovery pairs, and is capped at ten
// requests.
//
// Not part of the proxy. See
// docs/superpowers/specs/2026-09-18-throttle-calibration-design.md
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/calibrate"
	"antigravity-go-proxy/internal/cloudcode"
)

const exitAccountProtected = 3

func main() {
	journalPath := flag.String("journal", calibrate.DefaultJournalPath(), "path to the 429 journal")
	probeDaily := flag.Bool("probe-daily", false, "draw one rejection from the daily endpoint to read its stated retry delay")
	dryRun := flag.Bool("dry-run", false, "read and derive without making any network call (implies -probe-daily=false)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	journal, err := calibrate.ReadJournal(*journalPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read journal:", err)
		os.Exit(1)
	}
	fmt.Printf("journal: %s (%d record(s))\n\n", *journalPath, len(journal.Entries))

	result := calibrate.Derive(journal)

	if *probeDaily && !*dryRun {
		delay, err := probeDailyEndpoint(ctx)
		switch {
		case errors.Is(err, calibrate.ErrAccountProtected):
			fmt.Fprintln(os.Stderr, "probe stopped: the upstream response signals account jeopardy, not an ordinary throttle")
			os.Exit(exitAccountProtected)
		case err != nil:
			fmt.Fprintln(os.Stderr, "probe:", err)
		default:
			result.RecoverDaily = delay
		}
	}

	fmt.Print(result.Report())

	fragment := result.Fragment()
	if len(fragment) == 0 {
		fmt.Println("\nno setting could be derived from this journal — nothing to suggest")
		return
	}
	encoded, err := json.MarshalIndent(fragment, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "render fragment:", err)
		os.Exit(1)
	}
	fmt.Printf("\nsuggested config.json values (apply by hand):\n%s\n", encoded)
}

// probeDailyEndpoint sends minimal requests to the daily endpoint until it
// rejects with a stated delay, and returns that delay. It stops at
// MaxProbeRequests, and re-resolves credentials once on a 401 — a long run
// outlives a token, and treating that as a permanent auth failure would abort
// a healthy probe.
func probeDailyEndpoint(ctx context.Context) (time.Duration, error) {
	path, err := accounts.DefaultConfigPath()
	if err != nil {
		return 0, err
	}
	file, err := accounts.Load(path)
	if err != nil {
		return 0, err
	}
	if len(file.Accounts) == 0 {
		return 0, errors.New("no accounts in the pool")
	}
	account := file.Accounts[0]
	resolver := accounts.NewCredentialResolver(auth.Manager{}, nil)

	resolve := func() (*cloudcode.Client, error) {
		resolveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		credentials, err := resolver.Resolve(resolveCtx, account)
		if err != nil {
			return nil, err
		}
		return cloudcode.New(cloudcode.Options{AccessToken: credentials.AccessToken, Timeout: 60 * time.Second}), nil
	}

	client, err := resolve()
	if err != nil {
		return 0, err
	}

	payload := map[string]any{
		"project": account.ProjectID,
		"model":   "gemini-3.8-flash-high",
		"request": map[string]any{
			"contents": []any{map[string]any{
				"role":  "user",
				"parts": []any{map[string]any{"text": "Say OK"}},
			}},
		},
	}

	reauthorized := false
	for attempt := range calibrate.MaxProbeRequests {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		_, requestErr := client.DoSSE(ctx, []string{cloudcode.DailyEndpoint}, cloudcode.PathStreamGenerate,
			payload, cloudcode.RequestOptions{}, func(cloudcode.SSEEvent) error { return nil })
		if requestErr == nil {
			fmt.Printf("  probe %d/%d: accepted\n", attempt+1, calibrate.MaxProbeRequests)
			continue
		}
		var upstreamError *cloudcode.HTTPError
		if !errors.As(requestErr, &upstreamError) {
			return 0, requestErr
		}
		if err := calibrate.GuardBody(upstreamError.Body); err != nil {
			return 0, err
		}
		if upstreamError.StatusCode == 401 && !reauthorized {
			reauthorized = true
			fmt.Println("  probe: 401, re-resolving credentials once")
			client, err = resolve()
			if err != nil {
				return 0, fmt.Errorf("re-resolve after 401: %w", err)
			}
			continue
		}
		if delay, ok := calibrate.ParseRetryDelay(upstreamError.Body); ok {
			fmt.Printf("  probe %d/%d: rejected, stated delay %s\n", attempt+1, calibrate.MaxProbeRequests, delay.Round(time.Second))
			return delay, nil
		}
		fmt.Printf("  probe %d/%d: rejected with status %d and no stated delay\n", attempt+1, calibrate.MaxProbeRequests, upstreamError.StatusCode)
	}
	return 0, fmt.Errorf("no rejection stated a delay within %d requests", calibrate.MaxProbeRequests)
}
```

- [ ] **Step 4: Verify it builds and the whole suite passes**

Run: `go build ./... && go vet ./internal/calibrate/ ./cmd/calibrate/ && go test ./internal/calibrate/ ./internal/accounts/ -count=1`

Expected: build clean, vet clean, all tests PASS.

- [ ] **Step 5: Run the dry run against the real journal**

Run: `go run ./cmd/calibrate -dry-run`

Expected: prints the journal path and record count, then the report. Because the journal on disk predates Task 2, every guard should report `insufficient data` and the tool should print `no setting could be derived from this journal`. Confirm no network call is attempted.

- [ ] **Step 6: Commit**

```bash
git add cmd/calibrate/main.go internal/calibrate/fragment_test.go
git commit -m "feat(calibrate): add the calibrate binary

Reads the journal, derives what the evidence supports, prints a suggestion.
-probe-daily is the fallback for a journal with too few recovery pairs and is
capped at ten requests. Nothing writes config.json."
```

---

## Self-Review

**Spec coverage.**

| Spec section | Task |
|---|---|
| §4 `InFlight`, `PriorMinuteRequests`, `Outcome` | Task 2 Step 3 |
| §4 in-flight gauge, 60s ring counter | Task 1 |
| §4 recovery record at first success | Task 2 Steps 6-7 |
| §5 `C_safe`, `RPM_safe` and their guards | Task 4 (`oneBelowMinimum`) |
| §5 `T_recover`, median, retried-gaps-only | Task 4 (`medianRetriedGap`) |
| §5 `requestDelayMs`, `tokensPerMinute`, `maxTokens` | Task 4 (`Fragment`) |
| §5 backoff tiers capped at 1800s | Task 4 (`maxBackoffTierMs`) |
| §5 `sharedThrottleWindowMs`, no isolated branch | Task 4 (`Fragment`) |
| §6 daily-endpoint delay parse | Task 5 (`ParseRetryDelay`) |
| §6 protection kill-switch | Task 5 (`GuardBody`) |
| §6 10-request cap | Task 5, Task 6 |
| §6 401 re-resolve once | Task 6 (`probeDailyEndpoint`) |
| §6 signal handling | Task 6 (`signal.NotifyContext`) |
| §6 `T_recover_daily` fallback and 25% disagreement flag | Task 4 (`window`, `Report`, `disagree`) |
| §7 print only, never write | Task 6 |
| §10.1 guard boundary tests | Tasks 4 and 5 |
| §10.2 pre-migration journal | Task 3 (`TestReadJournalTreatsPreMigrationLinesAsRejects`) |
| §10.3 package test run | Tasks 2, 4, 6 |
| §10.4 `-dry-run` with no network call | Task 6 Step 5 |
| §10.5 `DisallowUnknownFields` decode | Task 6 Step 1 |

No gaps.

**Type consistency.** `Forensics429Entry.InFlight` / `PriorMinuteRequests` / `Outcome` (Task 2) parse into `calibrate.Entry` fields of the same names via the same JSON tags (Task 3). `Derive` returns `Result` with `ConcurrencySafe`, `RPMSafe`, `Recover`, `RecoverDaily` (Task 4), and Task 6 sets `RecoverDaily` and calls `Report` and `Fragment` on it. `MaxProbeRequests` is exported from `internal/calibrate` and referenced directly by `cmd/calibrate`, so the ten-request cap exists once in the tree.

**Validation limit, stated rather than papered over.** `AccountSelectionConfig.TokenBucket` is a `map[string]any` (`internal/config/config.go:97`), so the `DisallowUnknownFields` decode in Task 6 Step 1 proves only the top-level keys. `TestBucketKeysExistInTheDefaultConfig` covers the bucket keys by comparing them against `config.DefaultConfig()`, whose bucket is built at `internal/config/config.go:293`.

**Known risk, carried deliberately.** Task 6 Step 5 will report insufficient data on every guard, because the journal on disk was written before Task 2. That is the correct behaviour, not a defect: real calibration waits for production traffic to accumulate under the new recorder. The `-probe-daily` fallback exists for exactly that interval.
