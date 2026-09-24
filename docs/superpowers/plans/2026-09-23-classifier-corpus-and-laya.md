# Classifier Corpus Capture & Laya Backend Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist every Claude Code classifier request together with the verdict actually returned for it as local JSONL, then add a `laya` backend wire format so those requests can be answered by a local `laya-serve` sidecar.

**Architecture:** A new leaf package `internal/classifier/corpus` owns parsing, response capture and JSONL writing; `internal/api/server.go` installs a transparent `ResponseTap` around the response writer when capture is enabled and the request is a classifier request, and a single deferred call writes exactly one row whose `source` field names which code path answered. Phase 2 adds `BackendFormatLaya` plus a `laya` adapter behind the existing `backendFormatAdapter` seam, which requires threading the detected `classifier.Kind` into the adapter functions.

**Tech Stack:** Go 1.x (module `antigravity-go-proxy`), stdlib `testing` (no assertion library), Alpine.js + vanilla JS webui, Python 3 for the export script.

**Spec:** `docs/superpowers/specs/2026-09-23-classifier-corpus-and-laya-design.md`

## Global Constraints

- Severity is an integer **0–100**; **50 is the allow/block boundary**, below 50 allows.
- Laya-derived severity is clamped to `layaMaxSeverity`, default **49**. A Laya verdict must never block.
- Corpus directories are created `0700`, files `0600`.
- Capture and every failure inside it is logged and swallowed. It must never fail, delay, or alter a user request.
- `source` is written on every row. Valid values: `upstream`, `stub`, `rule`, `laya`. A fine-tune consumes `upstream` rows only.
- Captured response bytes are capped at **64 KiB** (`64 << 10`).
- Default state cap sent to Laya is **1200 characters**, truncated from the **left**.
- Tests use stdlib `testing` with `t.Errorf` / `t.Fatalf`. No testify, no assertion helpers.
- Existing tests in `internal/api/classifier_*_test.go` must keep passing; the only permitted edit to them is the mechanical `classifierCall` signature change in Task 9.

---

## File Structure

**Phase 1 — capture**

| File | Responsibility |
|---|---|
| `internal/classifier/corpus/verdict.go` | `Verdict` type, `ParseVerdict` — pure text parsing |
| `internal/classifier/corpus/request.go` | `Request` type, `ParseRequest`, `ExtractAction` — pulls action/context/hashes out of a classifier request body |
| `internal/classifier/corpus/tap.go` | `ResponseTap` — transparent `http.ResponseWriter` wrapper, extracts verdict text from JSON or SSE |
| `internal/classifier/corpus/recorder.go` | `Source`, `Entry`, `EntryInput`, `BuildEntry`, `Options`, `Recorder` — JSONL writing, rotation, prune, redaction |
| `internal/config/config.go` | `ClassifierCaptureConfig` + `Resolved()` defaults |
| `internal/api/management.go` | Capture config validation on save |
| `internal/api/server.go` | Recorder field, tap installation, `captureSource` threading |
| `internal/api/classifier_rules.go` | Sets `captureSource` for stub/reroute paths |
| `scripts/corpus_to_laya.py` | JSONL → Laya fine-tune dataset |

**Phase 2 — laya**

| File | Responsibility |
|---|---|
| `internal/config/config.go` | `BackendFormatLaya`, `TargetBackend` laya override fields |
| `internal/api/classifier_rules.go` | `classifierCall` struct, laya adapter, timeout default, registration |
| `internal/api/management.go` | Laya field validation |
| `internal/webui/public/js/components/classifier-config.js` | Capture section state, `laya` format option, advanced overrides |
| `internal/webui/public/views/settings.html` | Markup for both |
| `internal/webui/public/js/translations/en.js`, `pt.js` | Strings |

**Test files**

`internal/classifier/corpus/verdict_test.go`, `request_test.go`, `tap_test.go`, `recorder_test.go`; `internal/api/classifier_capture_test.go`; `internal/api/classifier_laya_test.go`; `scripts/test_corpus_to_laya.py`.

---

# Phase 1 — Corpus capture

## Task 1: Verdict parsing

**Files:**
- Create: `internal/classifier/corpus/verdict.go`
- Test: `internal/classifier/corpus/verdict_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Verdict struct { Raw string; Severity int; Category string; Thinking string }` and `func ParseVerdict(text string) Verdict`. `Severity` is `-1` when absent or unparseable. `Category` and `Thinking` are `""` when absent, trimmed of surrounding whitespace when present.

- [ ] **Step 1: Write the failing test**

Create `internal/classifier/corpus/verdict_test.go`:

```go
package corpus

import "testing"

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		severity int
		category string
		thinking string
	}{
		{
			name:     "stage 1 bare severity",
			text:     "<severity>0</severity>",
			severity: 0,
		},
		{
			name:     "stage 2 thinking then severity",
			text:     "<thinking>Routine cleanup.</thinking><severity>8</severity>",
			severity: 8,
			thinking: "Routine cleanup.",
		},
		{
			name:     "stage 2 blocking verdict carries a category",
			text:     "<thinking>Deletes history.</thinking><severity>80</severity><category>Destructive Git</category>",
			severity: 80,
			thinking: "Deletes history.",
			category: "Destructive Git",
		},
		{
			name:     "multiline thinking",
			text:     "<thinking>line one\nline two</thinking><severity>12</severity>",
			severity: 12,
			thinking: "line one\nline two",
		},
		{
			name:     "no severity tag",
			text:     "<block>true</block>",
			severity: -1,
		},
		{
			name:     "severity is not a number",
			text:     "<severity>high</severity>",
			severity: -1,
		},
		{
			name:     "empty input",
			text:     "",
			severity: -1,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			verdict := ParseVerdict(testCase.text)
			if verdict.Raw != testCase.text {
				t.Errorf("Raw = %q, want %q", verdict.Raw, testCase.text)
			}
			if verdict.Severity != testCase.severity {
				t.Errorf("Severity = %d, want %d", verdict.Severity, testCase.severity)
			}
			if verdict.Category != testCase.category {
				t.Errorf("Category = %q, want %q", verdict.Category, testCase.category)
			}
			if verdict.Thinking != testCase.thinking {
				t.Errorf("Thinking = %q, want %q", verdict.Thinking, testCase.thinking)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/classifier/corpus/ -run TestParseVerdict -v`
Expected: build failure — the package does not exist yet.

- [ ] **Step 3: Write the implementation**

Create `internal/classifier/corpus/verdict.go`:

```go
// Package corpus captures Claude Code classifier requests together with the
// verdict that was actually returned for them, as append-only JSONL on the
// operator's own machine. The rows are the labelled data a local decision
// model is fine-tuned on; see
// docs/superpowers/specs/2026-09-23-classifier-corpus-and-laya-design.md.
package corpus

import (
	"regexp"
	"strconv"
	"strings"
)

// Verdict is a parsed classifier answer. Severity is -1 when the response
// carried no parseable <severity> tag: an unreadable answer is itself a
// finding and must be recorded rather than dropped.
type Verdict struct {
	Raw      string `json:"verdict_raw"`
	Severity int    `json:"severity"`
	Category string `json:"category"`
	Thinking string `json:"thinking"`
}

// (?s) lets . match newlines: stage 2 <thinking> blocks are multi-line.
var (
	severityPattern = regexp.MustCompile(`(?s)<severity>\s*(-?\d+)\s*</severity>`)
	categoryPattern = regexp.MustCompile(`(?s)<category>(.*?)</category>`)
	thinkingPattern = regexp.MustCompile(`(?s)<thinking>(.*?)</thinking>`)
)

// ParseVerdict extracts the typed fields from a verdict string. It never
// fails: an unrecognized shape yields Severity -1 with Raw preserved.
func ParseVerdict(text string) Verdict {
	verdict := Verdict{Raw: text, Severity: -1}
	if match := severityPattern.FindStringSubmatch(text); match != nil {
		if value, err := strconv.Atoi(match[1]); err == nil {
			verdict.Severity = value
		}
	}
	if match := categoryPattern.FindStringSubmatch(text); match != nil {
		verdict.Category = strings.TrimSpace(match[1])
	}
	if match := thinkingPattern.FindStringSubmatch(text); match != nil {
		verdict.Thinking = strings.TrimSpace(match[1])
	}
	return verdict
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/classifier/corpus/ -run TestParseVerdict -v`
Expected: PASS, all seven subtests.

- [ ] **Step 5: Commit**

```bash
git add internal/classifier/corpus/verdict.go internal/classifier/corpus/verdict_test.go
git commit -m "feat(corpus): parse classifier verdict text into typed fields"
```

---

## Task 2: Request parsing

**Files:**
- Create: `internal/classifier/corpus/request.go`
- Test: `internal/classifier/corpus/request_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type Request struct { Action string; Context []string; SystemSHA256 string; FooterSHA256 string }`
  - `func ParseRequest(rawBody []byte, contextEntries int) (Request, error)`
  - `func ExtractAction(rawBody []byte, contextEntries int) (string, []string, error)` — thin wrapper, used by the Phase 2 Laya adapter (Task 11).
  - `Action` is the last transcript entry verbatim. `Context` holds up to `contextEntries` entries immediately preceding it, **oldest first**. `SystemSHA256` hashes the first system block containing the monitor prompt prefix; `FooterSHA256` hashes the footer block (the block after `</transcript>`). Both are lowercase hex, no `sha256:` prefix.
  - An error is returned when the body does not parse, has no message, has no `</transcript>` block, or has no entry before it.

- [ ] **Step 1: Write the failing test**

Create `internal/classifier/corpus/request_test.go`:

```go
package corpus

import (
	"strings"
	"testing"
)

// stage2Body mirrors the block layout captured in
// docs/classifier-fallback-notes.md: a literal <transcript> block, one block
// per turn as single-line JSON, a literal </transcript> block, then the footer.
const stage2Body = `{
  "model": "claude-opus-4-5",
  "system": [
    {"type": "text", "text": "x-anthropic-billing-header: cc_version=1"},
    {"type": "text", "text": "You are a security monitor for autonomous AI coding agents.\nRules follow."}
  ],
  "messages": [{"role": "user", "content": [
    {"type": "text", "text": "<transcript>"},
    {"type": "text", "text": "{\"user\":\"clean the build dir\"}"},
    {"type": "text", "text": "{\"Bash\":\"ls build/\"}"},
    {"type": "text", "text": "{\"Bash\":\"rm -rf build/\"}"},
    {"type": "text", "text": "</transcript>"},
    {"type": "text", "text": "Use <thinking> first, then respond with <severity>N</severity>, plus <category>"}
  ]}]
}`

func TestParseRequestExtractsActionAndContext(t *testing.T) {
	parsed, err := ParseRequest([]byte(stage2Body), 2)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if parsed.Action != `{"Bash":"rm -rf build/"}` {
		t.Errorf("Action = %q, want the last transcript entry", parsed.Action)
	}
	want := []string{`{"user":"clean the build dir"}`, `{"Bash":"ls build/"}`}
	if len(parsed.Context) != len(want) {
		t.Fatalf("Context has %d entries, want %d: %v", len(parsed.Context), len(want), parsed.Context)
	}
	for i := range want {
		if parsed.Context[i] != want[i] {
			t.Errorf("Context[%d] = %q, want %q (oldest first)", i, parsed.Context[i], want[i])
		}
	}
}

func TestParseRequestContextEntriesZeroYieldsEmptyContext(t *testing.T) {
	parsed, err := ParseRequest([]byte(stage2Body), 0)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if len(parsed.Context) != 0 {
		t.Errorf("Context = %v, want empty", parsed.Context)
	}
}

func TestParseRequestContextEntriesBeyondAvailable(t *testing.T) {
	parsed, err := ParseRequest([]byte(stage2Body), 20)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	// Only two entries precede the action; <transcript> itself is not one.
	if len(parsed.Context) != 2 {
		t.Errorf("Context has %d entries, want 2: %v", len(parsed.Context), parsed.Context)
	}
}

func TestParseRequestHashesAreStableAndDistinct(t *testing.T) {
	first, err := ParseRequest([]byte(stage2Body), 2)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	second, err := ParseRequest([]byte(stage2Body), 2)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if first.SystemSHA256 != second.SystemSHA256 {
		t.Error("SystemSHA256 is not stable across two parses of the same body")
	}
	if len(first.SystemSHA256) != 64 {
		t.Errorf("SystemSHA256 = %q, want 64 lowercase hex characters", first.SystemSHA256)
	}
	if len(first.FooterSHA256) != 64 {
		t.Errorf("FooterSHA256 = %q, want 64 lowercase hex characters", first.FooterSHA256)
	}
	if first.SystemSHA256 == first.FooterSHA256 {
		t.Error("SystemSHA256 and FooterSHA256 hash the same block")
	}
}

func TestParseRequestErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "not json", body: `not json at all`},
		{name: "no messages", body: `{"system":[],"messages":[]}`},
		{
			name: "content is a plain string",
			body: `{"messages":[{"role":"user","content":"<transcript></transcript>"}]}`,
		},
		{
			name: "no closing transcript block",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"<transcript>"}]}]}`,
		},
		{
			name: "transcript is empty",
			body: `{"messages":[{"role":"user","content":[
				{"type":"text","text":"<transcript>"},
				{"type":"text","text":"</transcript>"},
				{"type":"text","text":"footer"}]}]}`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := ParseRequest([]byte(testCase.body), 2); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

func TestExtractActionDelegatesToParseRequest(t *testing.T) {
	action, context, err := ExtractAction([]byte(stage2Body), 1)
	if err != nil {
		t.Fatalf("ExtractAction: %v", err)
	}
	if action != `{"Bash":"rm -rf build/"}` {
		t.Errorf("action = %q", action)
	}
	if len(context) != 1 || !strings.Contains(context[0], "ls build/") {
		t.Errorf("context = %v, want the single entry before the action", context)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/classifier/corpus/ -run TestParseRequest -v`
Expected: FAIL — `undefined: ParseRequest`.

- [ ] **Step 3: Write the implementation**

Create `internal/classifier/corpus/request.go`:

```go
package corpus

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// monitorPromptPrefix is the opening of the shared monitor system block,
// duplicated from internal/classifier so this leaf package stays importable
// from the parent without a cycle. Captured verbatim in
// docs/classifier-fallback-notes.md.
const monitorPromptPrefix = "You are a security monitor for autonomous AI coding agents."

// transcriptClose is the literal block that terminates the transcript. The
// entry immediately before it is the action being graded.
const transcriptClose = "</transcript>"

// ErrNoTranscript reports a body whose block layout does not match any
// captured classifier request. Callers fall back rather than guess.
var ErrNoTranscript = errors.New("corpus: request carries no readable transcript")

// Request is everything the corpus keeps from a classifier request body.
type Request struct {
	Action       string
	Context      []string
	SystemSHA256 string
	FooterSHA256 string
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type systemBlock struct {
	Text string `json:"text"`
}

type messageEnvelope struct {
	Content json.RawMessage `json:"content"`
}

type requestEnvelope struct {
	System   []systemBlock     `json:"system"`
	Messages []messageEnvelope `json:"messages"`
}

// ParseRequest reads the action, its preceding context, and the two block
// hashes out of a classifier request body. The system prompt runs to roughly
// 125KB, so it is hashed rather than stored: that is enough to notice
// upstream changing the prompt without paying 125KB per row.
func ParseRequest(rawBody []byte, contextEntries int) (Request, error) {
	var envelope requestEnvelope
	if err := json.Unmarshal(rawBody, &envelope); err != nil {
		return Request{}, err
	}
	if len(envelope.Messages) == 0 {
		return Request{}, ErrNoTranscript
	}

	var blocks []contentBlock
	if err := json.Unmarshal(envelope.Messages[len(envelope.Messages)-1].Content, &blocks); err != nil {
		return Request{}, ErrNoTranscript
	}

	closeIndex := -1
	for i := len(blocks) - 1; i >= 0; i-- {
		if strings.TrimSpace(blocks[i].Text) == transcriptClose {
			closeIndex = i
			break
		}
	}
	// closeIndex must leave room for at least one entry after the opening
	// <transcript> block; index 0 or 1 means the transcript carried nothing.
	if closeIndex < 2 {
		return Request{}, ErrNoTranscript
	}

	actionIndex := closeIndex - 1
	parsed := Request{Action: blocks[actionIndex].Text}

	if contextEntries > 0 {
		start := actionIndex - contextEntries
		// Index 0 is the literal <transcript> block, never a turn.
		if start < 1 {
			start = 1
		}
		for i := start; i < actionIndex; i++ {
			parsed.Context = append(parsed.Context, blocks[i].Text)
		}
	}

	for _, block := range envelope.System {
		if strings.Contains(block.Text, monitorPromptPrefix) {
			parsed.SystemSHA256 = hashText(block.Text)
			break
		}
	}
	if closeIndex+1 < len(blocks) {
		parsed.FooterSHA256 = hashText(blocks[closeIndex+1].Text)
	}

	return parsed, nil
}

// ExtractAction returns just the action and its context. The Laya adapter
// needs no hashes, so it calls this rather than reaching into Request.
func ExtractAction(rawBody []byte, contextEntries int) (string, []string, error) {
	parsed, err := ParseRequest(rawBody, contextEntries)
	if err != nil {
		return "", nil, err
	}
	return parsed.Action, parsed.Context, nil
}

func hashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/classifier/corpus/ -v`
Expected: PASS — Task 1's `TestParseVerdict` plus all six `ParseRequest`/`ExtractAction` tests.

- [ ] **Step 5: Commit**

```bash
git add internal/classifier/corpus/request.go internal/classifier/corpus/request_test.go
git commit -m "feat(corpus): extract graded action, context and block hashes"
```

---

## Task 3: Response tap

**Files:**
- Create: `internal/classifier/corpus/tap.go`
- Test: `internal/classifier/corpus/tap_test.go`

**Interfaces:**
- Consumes: `ParseVerdict` from Task 1 (not called here, but `VerdictText` feeds it in Task 4).
- Produces:
  - `func NewResponseTap(inner http.ResponseWriter) *ResponseTap`
  - `*ResponseTap` implements `http.ResponseWriter` and `http.Flusher`.
  - `func (t *ResponseTap) Status() int` — 200 when `WriteHeader` was never called.
  - `func (t *ResponseTap) Truncated() bool`
  - `func (t *ResponseTap) ElapsedMs() int64` — milliseconds since `NewResponseTap`.
  - `func (t *ResponseTap) VerdictText() string` — text from an Anthropic JSON envelope, else concatenated SSE `content_block_delta` text, else the captured bytes as a string.

**Why `http.Flusher` is mandatory:** `writeClassifierResponse` (`internal/api/classifier_rules.go:231`) type-asserts the writer to `http.Flusher` and returns an error when the assertion fails. A tap without `Flush` breaks every streamed classifier response.

- [ ] **Step 1: Write the failing test**

Create `internal/classifier/corpus/tap_test.go`:

```go
package corpus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponseTapIsTransparent(t *testing.T) {
	recorder := httptest.NewRecorder()
	tap := NewResponseTap(recorder)

	tap.Header().Set("Content-Type", "application/json")
	tap.WriteHeader(http.StatusOK)
	payload := `{"content":[{"type":"text","text":"<severity>0</severity>"}]}`
	if _, err := tap.Write([]byte(payload)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if recorder.Body.String() != payload {
		t.Errorf("inner writer got %q, want the bytes written verbatim", recorder.Body.String())
	}
	if recorder.Header().Get("Content-Type") != "application/json" {
		t.Error("headers did not reach the inner writer")
	}
	if tap.Status() != http.StatusOK {
		t.Errorf("Status = %d, want 200", tap.Status())
	}
}

func TestResponseTapDefaultsStatusTo200(t *testing.T) {
	tap := NewResponseTap(httptest.NewRecorder())
	if _, err := tap.Write([]byte("x")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if tap.Status() != http.StatusOK {
		t.Errorf("Status = %d, want 200 when WriteHeader was never called", tap.Status())
	}
}

func TestResponseTapImplementsFlusher(t *testing.T) {
	var writer http.ResponseWriter = NewResponseTap(httptest.NewRecorder())
	if _, ok := writer.(http.Flusher); !ok {
		t.Fatal("ResponseTap must implement http.Flusher; writeClassifierResponse asserts it")
	}
	// Flushing a writer that cannot flush must not panic.
	writer.(http.Flusher).Flush()
}

func TestResponseTapVerdictFromJSONEnvelope(t *testing.T) {
	tap := NewResponseTap(httptest.NewRecorder())
	body := `{"id":"msg_1","type":"message","content":[
		{"type":"thinking","thinking":"ignored"},
		{"type":"text","text":"<thinking>Routine.</thinking>"},
		{"type":"text","text":"<severity>4</severity>"}
	]}`
	if _, err := tap.Write([]byte(body)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	want := "<thinking>Routine.</thinking><severity>4</severity>"
	if got := tap.VerdictText(); got != want {
		t.Errorf("VerdictText = %q, want %q", got, want)
	}
}

func TestResponseTapVerdictFromSSE(t *testing.T) {
	tap := NewResponseTap(httptest.NewRecorder())
	frames := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"<severity>"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"7</severity>"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	if _, err := tap.Write([]byte(frames)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := tap.VerdictText(); got != "<severity>7</severity>" {
		t.Errorf("VerdictText = %q, want the concatenated deltas", got)
	}
}

func TestResponseTapVerdictFallsBackToRawBytes(t *testing.T) {
	tap := NewResponseTap(httptest.NewRecorder())
	if _, err := tap.Write([]byte("<block>true</block>")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := tap.VerdictText(); got != "<block>true</block>" {
		t.Errorf("VerdictText = %q, want the raw bytes when neither shape parses", got)
	}
}

func TestResponseTapCapsCapturedBytes(t *testing.T) {
	recorder := httptest.NewRecorder()
	tap := NewResponseTap(recorder)
	oversized := strings.Repeat("a", maxCapturedResponse+4096)
	written, err := tap.Write([]byte(oversized))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if written != len(oversized) {
		t.Errorf("Write returned %d, want %d: the client must receive every byte", written, len(oversized))
	}
	if recorder.Body.Len() != len(oversized) {
		t.Errorf("inner writer got %d bytes, want %d", recorder.Body.Len(), len(oversized))
	}
	if !tap.Truncated() {
		t.Error("Truncated = false, want true")
	}
	if len(tap.VerdictText()) > maxCapturedResponse {
		t.Errorf("captured %d bytes, want at most %d", len(tap.VerdictText()), maxCapturedResponse)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/classifier/corpus/ -run TestResponseTap -v`
Expected: FAIL — `undefined: NewResponseTap`.

- [ ] **Step 3: Write the implementation**

Create `internal/classifier/corpus/tap.go`:

```go
package corpus

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// maxCapturedResponse bounds what the tap keeps. Classifier responses are
// small by construction (max_tokens of 64, 8192 and 2112 across the three
// variants), so this is a safety bound, not a routine path.
const maxCapturedResponse = 64 << 10

// ResponseTap wraps an http.ResponseWriter and copies what is written into a
// bounded buffer. Every write reaches the inner writer first, so the client
// sees byte-identical output whether or not capture is on.
type ResponseTap struct {
	inner     http.ResponseWriter
	buffer    bytes.Buffer
	status    int
	truncated bool
	start     time.Time
}

func NewResponseTap(inner http.ResponseWriter) *ResponseTap {
	return &ResponseTap{inner: inner, start: time.Now()}
}

func (tap *ResponseTap) Header() http.Header {
	return tap.inner.Header()
}

func (tap *ResponseTap) WriteHeader(status int) {
	if tap.status == 0 {
		tap.status = status
	}
	tap.inner.WriteHeader(status)
}

func (tap *ResponseTap) Write(payload []byte) (int, error) {
	written, err := tap.inner.Write(payload)
	if tap.status == 0 {
		tap.status = http.StatusOK
	}
	room := maxCapturedResponse - tap.buffer.Len()
	if room <= 0 {
		tap.truncated = true
		return written, err
	}
	if len(payload) > room {
		tap.buffer.Write(payload[:room])
		tap.truncated = true
		return written, err
	}
	tap.buffer.Write(payload)
	return written, err
}

// Flush delegates to the inner writer. writeClassifierResponse asserts
// http.Flusher before emitting SSE, so this method is load-bearing.
func (tap *ResponseTap) Flush() {
	if flusher, ok := tap.inner.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (tap *ResponseTap) Status() int {
	if tap.status == 0 {
		return http.StatusOK
	}
	return tap.status
}

func (tap *ResponseTap) Truncated() bool {
	return tap.truncated
}

func (tap *ResponseTap) ElapsedMs() int64 {
	return time.Since(tap.start).Milliseconds()
}

type anthropicEnvelope struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type sseEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Text string `json:"text"`
	} `json:"delta"`
}

// VerdictText reconstructs the answer text from whichever shape the response
// took. An unrecognized shape returns the captured bytes verbatim: an
// unreadable answer is a finding worth keeping, not a row worth dropping.
func (tap *ResponseTap) VerdictText() string {
	captured := tap.buffer.Bytes()
	if len(captured) == 0 {
		return ""
	}

	var envelope anthropicEnvelope
	if err := json.Unmarshal(captured, &envelope); err == nil && len(envelope.Content) > 0 {
		var builder strings.Builder
		for _, block := range envelope.Content {
			if block.Type == "text" {
				builder.WriteString(block.Text)
			}
		}
		if builder.Len() > 0 {
			return builder.String()
		}
	}

	var builder strings.Builder
	for _, line := range strings.Split(string(captured), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event sseEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		if event.Type == "content_block_delta" {
			builder.WriteString(event.Delta.Text)
		}
	}
	if builder.Len() > 0 {
		return builder.String()
	}

	return string(captured)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/classifier/corpus/ -v`
Expected: PASS — Tasks 1 through 3.

- [ ] **Step 5: Commit**

```bash
git add internal/classifier/corpus/tap.go internal/classifier/corpus/tap_test.go
git commit -m "feat(corpus): transparent response tap that reads JSON and SSE verdicts"
```

---

## Task 4: Recorder and row building

**Files:**
- Create: `internal/classifier/corpus/recorder.go`
- Test: `internal/classifier/corpus/recorder_test.go`

**Interfaces:**
- Consumes: `ParseVerdict` (Task 1), `ParseRequest` (Task 2), `*ResponseTap` (Task 3).
- Produces:
  - `type Source string` with `SourceUpstream`, `SourceStub`, `SourceRule`, `SourceLaya`.
  - `type Entry struct` — the JSONL row, field tags exactly as in the spec.
  - `type EntryInput struct { RawBody []byte; Kind string; Model string; Source Source; Tap *ResponseTap; ContextEntries int }`
  - `func BuildEntry(input EntryInput) Entry`
  - `type Options struct { MaxFiles int; MaxFileBytes int64; RedactPaths bool }`
  - `func New(dir string, options Options) *Recorder`
  - `func (r *Recorder) Enabled() bool` — false for a nil recorder or an empty dir.
  - `func (r *Recorder) Record(entry Entry)`

`Kind` is a `string` rather than `classifier.Kind` so this package stays a leaf: `internal/classifier` must never import `internal/classifier/corpus`. Callers pass `kind.String()`.

- [ ] **Step 1: Write the failing test**

Create `internal/classifier/corpus/recorder_test.go`:

```go
package corpus

import (
	"encoding/json"
	"fmt"
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

func TestRecorderRedactsHomeDirectory(t *testing.T) {
	dir := t.TempDir()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this platform")
	}

	recorder := New(dir, Options{MaxFiles: 8, MaxFileBytes: 1 << 20, RedactPaths: true})
	recorder.Record(Entry{
		Version: 1,
		Action:  `{"Bash":"cat ` + home + `/.ssh/config"}`,
		Context: []string{`{"user":"look in ` + home + `/notes"}`},
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
```

Every test above is complete as written; nothing needs editing before the run.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/classifier/corpus/ -run 'TestRecorder|TestBuildEntry' -v`
Expected: FAIL — `undefined: BuildEntry`, `undefined: New`.

- [ ] **Step 3: Write the implementation**

Create `internal/classifier/corpus/recorder.go`:

```go
package corpus

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Source names the code path that produced a row. A fine-tune consumes
// SourceUpstream rows only: training on SourceLaya rows would teach the local
// model its own answers and entrench its errors.
type Source string

const (
	SourceUpstream Source = "upstream"
	SourceStub     Source = "stub"
	SourceRule     Source = "rule"
	SourceLaya     Source = "laya"
)

// Entry is one JSONL row. Schema version 1; see the design spec.
type Entry struct {
	Version      int      `json:"v"`
	Timestamp    string   `json:"ts"`
	Kind         string   `json:"kind"`
	Model        string   `json:"model"`
	Action       string   `json:"action"`
	Context      []string `json:"context"`
	SystemSHA256 string   `json:"system_sha256"`
	FooterSHA256 string   `json:"footer_sha256"`
	VerdictRaw   string   `json:"verdict_raw"`
	Severity     int      `json:"severity"`
	Category     string   `json:"category"`
	Thinking     string   `json:"thinking"`
	Status       int      `json:"status"`
	LatencyMs    int64    `json:"latency_ms"`
	Truncated    bool     `json:"truncated"`
	Source       Source   `json:"source"`
}

// EntryInput is everything BuildEntry needs from the request path.
type EntryInput struct {
	RawBody        []byte
	Kind           string
	Model          string
	Source         Source
	Tap            *ResponseTap
	ContextEntries int
}

// BuildEntry assembles a row. It never fails: a body whose transcript cannot
// be read still yields a row with empty Action, because the fact that such a
// request reached the classifier is itself worth keeping.
func BuildEntry(input EntryInput) Entry {
	entry := Entry{
		Version:   1,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Kind:      input.Kind,
		Model:     input.Model,
		Context:   []string{},
		Severity:  -1,
		Source:    input.Source,
	}

	if parsed, err := ParseRequest(input.RawBody, input.ContextEntries); err == nil {
		entry.Action = parsed.Action
		if parsed.Context != nil {
			entry.Context = parsed.Context
		}
		entry.SystemSHA256 = parsed.SystemSHA256
		entry.FooterSHA256 = parsed.FooterSHA256
	}

	if input.Tap != nil {
		verdict := ParseVerdict(input.Tap.VerdictText())
		entry.VerdictRaw = verdict.Raw
		entry.Severity = verdict.Severity
		entry.Category = verdict.Category
		entry.Thinking = verdict.Thinking
		entry.Status = input.Tap.Status()
		entry.LatencyMs = input.Tap.ElapsedMs()
		entry.Truncated = input.Tap.Truncated()
	}

	return entry
}

// Options tunes file handling and redaction.
type Options struct {
	MaxFiles     int
	MaxFileBytes int64
	RedactPaths  bool
}

// Recorder appends rows as JSONL, one file per UTC date. It mirrors
// internal/accounts/forensics.go: an empty dir disables it, files are opened
// per record so external rotation and manual deletion stay safe, and every
// error is logged and swallowed.
type Recorder struct {
	dir      string
	options  Options
	home     string
	mu       sync.Mutex
	lastDate string
	dropped  int
	lastWarn time.Time
}

func New(dir string, options Options) *Recorder {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return &Recorder{dir: dir, options: options, home: home}
}

func (recorder *Recorder) Enabled() bool {
	return recorder != nil && recorder.dir != ""
}

// Dropped reports how many rows the size cap rejected.
func (recorder *Recorder) Dropped() int {
	if recorder == nil {
		return 0
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return recorder.dropped
}

func (recorder *Recorder) Record(entry Entry) {
	if !recorder.Enabled() {
		return
	}
	if recorder.options.RedactPaths && recorder.home != "" {
		entry.Action = strings.ReplaceAll(entry.Action, recorder.home, "~")
		for i, item := range entry.Context {
			entry.Context[i] = strings.ReplaceAll(item, recorder.home, "~")
		}
	}

	line, err := json.Marshal(entry)
	if err != nil {
		slog.Warn("corpus: marshal row", "error", err)
		return
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()

	date := time.Now().UTC().Format("2006-01-02")
	path := filepath.Join(recorder.dir, "classifier-"+date+".jsonl")

	if err := os.MkdirAll(recorder.dir, 0o700); err != nil {
		slog.Warn("corpus: create directory", "error", err)
		return
	}
	if date != recorder.lastDate {
		recorder.lastDate = date
		recorder.prune()
	}

	if info, err := os.Stat(path); err == nil && info.Size() >= recorder.options.MaxFileBytes {
		recorder.dropped++
		if time.Since(recorder.lastWarn) > time.Hour {
			recorder.lastWarn = time.Now()
			slog.Warn("corpus: file at its size cap, dropping rows",
				"file", path, "dropped", recorder.dropped)
		}
		return
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Warn("corpus: open row file", "error", err)
		return
	}
	defer file.Close()
	if _, err := file.Write(append(line, '\n')); err != nil {
		slog.Warn("corpus: write row", "error", err)
	}
}

// prune deletes all but the newest MaxFiles day files. Callers hold the mutex.
func (recorder *Recorder) prune() {
	if recorder.options.MaxFiles <= 0 {
		return
	}
	matches, err := filepath.Glob(filepath.Join(recorder.dir, "classifier-*.jsonl"))
	if err != nil || len(matches) <= recorder.options.MaxFiles {
		return
	}
	// ISO dates sort lexicographically in chronological order.
	sort.Strings(matches)
	for _, path := range matches[:len(matches)-recorder.options.MaxFiles] {
		if err := os.Remove(path); err != nil {
			slog.Warn("corpus: prune old file", "file", path, "error", err)
		}
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/classifier/corpus/ -v`
Expected: PASS — every test in the package.

- [ ] **Step 5: Commit**

```bash
git add internal/classifier/corpus/recorder.go internal/classifier/corpus/recorder_test.go
git commit -m "feat(corpus): JSONL recorder with rotation, size cap and path redaction"
```

---

## Task 5: Capture configuration

**Files:**
- Modify: `internal/config/config.go` — add `ClassifierCaptureConfig`, add `Capture` to `ClassifierConfig` (struct at `internal/config/config.go:259`)
- Modify: `internal/api/management.go:1150` region — validation, after the `classifierReq.Variants` loop
- Test: `internal/config/config_test.go` (append)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type ClassifierCaptureConfig struct { Enabled bool; Dir string; ContextEntries int; MaxFiles int; MaxFileBytes int64; RedactPaths *bool }`
  - `func (c ClassifierCaptureConfig) Resolved() ClassifierCaptureConfig` — fills defaults for zero values.
  - `func (c ClassifierCaptureConfig) RedactPathsEnabled() bool` — `true` when `RedactPaths` is nil.
  - `ClassifierConfig.Capture ClassifierCaptureConfig`

`RedactPaths` is a `*bool` because its default is `true` and a plain `bool` cannot distinguish "absent" from "explicitly false". `ClassifierVariantConfig.CompactTranscript` already uses this pattern in the same file.

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go`:

```go
func TestClassifierCaptureResolvedFillsDefaults(t *testing.T) {
	resolved := ClassifierCaptureConfig{Enabled: true}.Resolved()

	if resolved.Dir == "" {
		t.Error("Dir is empty, want the default corpus directory")
	}
	if !strings.HasSuffix(resolved.Dir, filepath.Join("corpus")) {
		t.Errorf("Dir = %q, want it to end in corpus", resolved.Dir)
	}
	if resolved.ContextEntries != 2 {
		t.Errorf("ContextEntries = %d, want 2", resolved.ContextEntries)
	}
	if resolved.MaxFiles != 8 {
		t.Errorf("MaxFiles = %d, want 8", resolved.MaxFiles)
	}
	if resolved.MaxFileBytes != 67108864 {
		t.Errorf("MaxFileBytes = %d, want 67108864", resolved.MaxFileBytes)
	}
}

func TestClassifierCaptureResolvedKeepsExplicitValues(t *testing.T) {
	resolved := ClassifierCaptureConfig{
		Enabled:        true,
		Dir:            "/tmp/custom",
		ContextEntries: 5,
		MaxFiles:       3,
		MaxFileBytes:   1048576,
	}.Resolved()

	if resolved.Dir != "/tmp/custom" {
		t.Errorf("Dir = %q", resolved.Dir)
	}
	if resolved.ContextEntries != 5 {
		t.Errorf("ContextEntries = %d, want 5", resolved.ContextEntries)
	}
	if resolved.MaxFiles != 3 {
		t.Errorf("MaxFiles = %d, want 3", resolved.MaxFiles)
	}
	if resolved.MaxFileBytes != 1048576 {
		t.Errorf("MaxFileBytes = %d", resolved.MaxFileBytes)
	}
}

func TestClassifierCaptureContextEntriesZeroIsHonored(t *testing.T) {
	// 0 is a meaningful value (action only), so Resolved must not replace it
	// with the default. It is distinguished by ContextEntries being set to -1
	// when unset is intended; see Resolved's contract.
	resolved := ClassifierCaptureConfig{Enabled: true, ContextEntries: -1}.Resolved()
	if resolved.ContextEntries != 0 {
		t.Errorf("ContextEntries = %d, want 0 when explicitly disabled with -1", resolved.ContextEntries)
	}
}

func TestClassifierCaptureRedactPathsDefaultsToTrue(t *testing.T) {
	if !(ClassifierCaptureConfig{}).RedactPathsEnabled() {
		t.Error("RedactPathsEnabled = false when unset, want true")
	}
	off := false
	if (ClassifierCaptureConfig{RedactPaths: &off}).RedactPathsEnabled() {
		t.Error("RedactPathsEnabled = true when explicitly false")
	}
}

func TestClassifierConfigDecodesCapture(t *testing.T) {
	raw := `{"enabled":true,"capture":{"enabled":true,"dir":"/tmp/c","contextEntries":4,"maxFiles":2,"maxFileBytes":2048,"redactPaths":false}}`
	var decoded ClassifierConfig
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !decoded.Capture.Enabled {
		t.Error("Capture.Enabled = false")
	}
	if decoded.Capture.Dir != "/tmp/c" {
		t.Errorf("Capture.Dir = %q", decoded.Capture.Dir)
	}
	if decoded.Capture.ContextEntries != 4 {
		t.Errorf("Capture.ContextEntries = %d", decoded.Capture.ContextEntries)
	}
	if decoded.Capture.RedactPathsEnabled() {
		t.Error("RedactPathsEnabled = true, want false")
	}
}
```

Make sure `encoding/json`, `path/filepath` and `strings` are in the file's import block.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/config/ -run TestClassifierCapture -v`
Expected: FAIL — `undefined: ClassifierCaptureConfig`.

- [ ] **Step 3: Write the implementation**

In `internal/config/config.go`, add after the `ClassifierConfig` struct (currently ending at line 271):

```go
// ClassifierCaptureConfig controls persistence of classifier requests and the
// verdicts returned for them. Off by default, like
// Upstream429ForensicsEnabled: this writes the operator's own shell commands
// to disk and must be opted into.
type ClassifierCaptureConfig struct {
	Enabled bool   `json:"enabled"`
	Dir     string `json:"dir,omitempty"`
	// ContextEntries is how many transcript entries before the graded action
	// are kept. 0 means the action only; pass -1 to mean "explicitly zero"
	// when the default would otherwise apply.
	ContextEntries int   `json:"contextEntries,omitempty"`
	MaxFiles       int   `json:"maxFiles,omitempty"`
	MaxFileBytes   int64 `json:"maxFileBytes,omitempty"`
	// RedactPaths is a pointer because its default is true: a plain bool
	// cannot tell an absent field from an explicit false.
	RedactPaths *bool `json:"redactPaths,omitempty"`
}

// Capture defaults. Named rather than inlined so the WebUI, the validator and
// Resolved cannot drift apart.
const (
	DefaultCaptureContextEntries = 2
	DefaultCaptureMaxFiles       = 8
	DefaultCaptureMaxFileBytes   = int64(64 << 20)
)

// Resolved returns the config with zero values replaced by defaults. A
// ContextEntries of -1 resolves to 0, which is how an operator asks for the
// action with no surrounding context.
func (capture ClassifierCaptureConfig) Resolved() ClassifierCaptureConfig {
	if capture.Dir == "" {
		capture.Dir = filepath.Join(GetConfigDir(), "corpus")
	}
	switch {
	case capture.ContextEntries < 0:
		capture.ContextEntries = 0
	case capture.ContextEntries == 0:
		capture.ContextEntries = DefaultCaptureContextEntries
	}
	if capture.MaxFiles <= 0 {
		capture.MaxFiles = DefaultCaptureMaxFiles
	}
	if capture.MaxFileBytes <= 0 {
		capture.MaxFileBytes = DefaultCaptureMaxFileBytes
	}
	return capture
}

// RedactPathsEnabled reports whether the home directory is replaced with ~ in
// captured commands. Unset means enabled.
func (capture ClassifierCaptureConfig) RedactPathsEnabled() bool {
	return capture.RedactPaths == nil || *capture.RedactPaths
}
```

Add the field to `ClassifierConfig`:

```go
	Backends          map[string]TargetBackend           `json:"backends,omitempty"`
	Capture           ClassifierCaptureConfig            `json:"capture,omitempty"`
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ -v -run TestClassifier`
Expected: PASS — the new capture tests plus the existing `TestClassifierConfigDecodesRulesAndBackends`.

- [ ] **Step 5: Add validation in management.go**

In `internal/api/management.go`, immediately after the `for variantKey, variant := range classifierReq.Variants` loop closes (around line 1150), insert:

```go
		capture := classifierReq.Capture
		if capture.ContextEntries < -1 || capture.ContextEntries > 20 {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "classifier capture contextEntries must be between -1 and 20"})
			return
		}
		if capture.MaxFiles < 0 || capture.MaxFiles > 365 {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "classifier capture maxFiles must be between 0 and 365"})
			return
		}
		if capture.MaxFileBytes < 0 || capture.MaxFileBytes > int64(4)<<30 {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "classifier capture maxFileBytes must be between 0 and 4294967296"})
			return
		}
		if capture.Dir != "" && !filepath.IsAbs(capture.Dir) {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "classifier capture dir must be an absolute path"})
			return
		}
```

Ensure `path/filepath` is imported in `management.go`.

- [ ] **Step 6: Run the API package build and tests**

Run: `go build ./... && go test ./internal/api/ -run TestClassifier -v`
Expected: build succeeds, existing classifier tests pass.

- [ ] **Step 7: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go internal/api/management.go
git commit -m "feat(config): classifier capture settings with defaults and validation"
```

---

## Task 6: Wire capture into the request path

**Files:**
- Modify: `internal/api/server.go` — `Server` struct near line 119, `applyClassifierConfig` in `internal/api/classifier_rules.go:29`, the classifier block at `internal/api/server.go:711-745`
- Modify: `internal/api/classifier_rules.go` — `classifierRequest` gains `captureSource *corpus.Source`
- Test: `internal/api/classifier_capture_test.go` (create)

**Interfaces:**
- Consumes: `corpus.New`, `corpus.Options`, `corpus.NewResponseTap`, `corpus.BuildEntry`, `corpus.EntryInput`, `corpus.Source*` (Task 4); `config.ClassifierCaptureConfig.Resolved` (Task 5).
- Produces: `Server.classifierCorpus *corpus.Recorder`, and the invariant that **exactly one row is written per captured classifier request**, whatever answered it.

**The single-row invariant.** The tap is installed once, before rule matching, and one `defer` writes the row. Whichever path answers assigns `captureSource`; the deferred closure reads it at return time. Recording inside each branch instead would double-write whenever a branch fell through.

- [ ] **Step 1: Verify applyClassifierConfig runs at startup**

Run: `grep -rn "applyClassifierConfig" internal/ --include=\*.go | grep -v _test`
Expected: a call in `management.go` (config save) **and** one on the startup path. If startup does not call it, add the call where `classifierAudit` is built (`internal/api/server.go:170`) before continuing — the recorder is built in `applyClassifierConfig`, so it would otherwise stay nil until the first settings save.

- [ ] **Step 2: Write the failing test**

Create `internal/api/classifier_capture_test.go`:

```go
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/classifier"
	"antigravity-go-proxy/internal/classifier/corpus"
	"antigravity-go-proxy/internal/config"
)

// captureBody is a stage 1 classifier request with a full transcript block
// layout, so ParseRequest can read an action out of it.
const captureBody = `{
  "model": "claude-sonnet-5",
  "max_tokens": 64,
  "system": [
    {"type": "text", "text": "x-anthropic-billing-header: cc_version=1"},
    {"type": "text", "text": "You are a security monitor for autonomous AI coding agents."}
  ],
  "messages": [{"role": "user", "content": [
    {"type": "text", "text": "<transcript>"},
    {"type": "text", "text": "{\"user\":\"tidy up\"}"},
    {"type": "text", "text": "{\"Bash\":\"rm -rf build/\"}"},
    {"type": "text", "text": "</transcript>"},
    {"type": "text", "text": "Stage 1 does NOT apply user intent or ALLOW exceptions — stage 2 will handle those.\nRespond with <severity>N</severity> ONLY. Grade HARM ONLY — do NOT reduce for user intent. No other text."}
  ]}]
}`

func readCaptureRows(t *testing.T, dir string) []corpus.Entry {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "classifier-*.jsonl"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var rows []corpus.Entry
	for _, path := range matches {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
			if line == "" {
				continue
			}
			var entry corpus.Entry
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatalf("unmarshal %q: %v", line, err)
			}
			rows = append(rows, entry)
		}
	}
	return rows
}

func TestClassifierCaptureRecordsStubbedVerdict(t *testing.T) {
	dir := t.TempDir()
	srv := &Server{classifierAudit: classifier.NewRecorder(10)}
	srv.applyClassifierConfig(config.ClassifierConfig{
		Enabled: true,
		Capture: config.ClassifierCaptureConfig{Enabled: true, Dir: dir},
		Rules: []config.Rule{{
			ID:      "stage1-stub",
			Name:    "Stage 1",
			Enabled: true,
			Conditions: config.RuleConditions{
				FooterPatterns: []config.MatchPattern{
					{Type: config.PatternSubstring, Pattern: "Grade HARM ONLY"},
				},
			},
			Action:          config.RuleActionStub,
			VerdictTemplate: "<severity>0</severity>",
		}},
	})

	if !srv.classifierCorpus.Enabled() {
		t.Fatal("recorder is disabled after applyClassifierConfig with capture on")
	}

	rule, backend, matched := srv.classifierMatcher.Match([]byte(captureBody))
	if !matched {
		t.Fatal("expected the rule to match")
	}

	recorder := httptest.NewRecorder()
	source := corpus.SourceUpstream
	tap := corpus.NewResponseTap(recorder)
	var writer http.ResponseWriter = tap

	responded, _ := srv.applyClassifierRule(writer, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(captureBody)), classifierRequest{
		rule:          rule,
		backend:       backend,
		rawBody:       []byte(captureBody),
		model:         "claude-sonnet-5",
		captureSource: &source,
	})
	if !responded {
		t.Fatal("expected the stub rule to respond")
	}
	if source != corpus.SourceStub {
		t.Errorf("captureSource = %q, want stub", source)
	}

	srv.classifierCorpus.Record(corpus.BuildEntry(corpus.EntryInput{
		RawBody:        []byte(captureBody),
		Kind:           classifier.KindStage1Severity.String(),
		Model:          "claude-sonnet-5",
		Source:         source,
		Tap:            tap,
		ContextEntries: 2,
	}))

	rows := readCaptureRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want exactly 1", len(rows))
	}
	row := rows[0]
	if row.Source != corpus.SourceStub {
		t.Errorf("Source = %q, want stub", row.Source)
	}
	if row.Severity != 0 {
		t.Errorf("Severity = %d, want 0", row.Severity)
	}
	if row.Action != `{"Bash":"rm -rf build/"}` {
		t.Errorf("Action = %q", row.Action)
	}
	if row.Kind != "stage1-severity" {
		t.Errorf("Kind = %q", row.Kind)
	}
}

func TestClassifierCaptureDisabledCreatesNoRecorder(t *testing.T) {
	srv := &Server{classifierAudit: classifier.NewRecorder(10)}
	srv.applyClassifierConfig(config.ClassifierConfig{Enabled: true})
	if srv.classifierCorpus.Enabled() {
		t.Error("recorder is enabled when capture config is off")
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/api/ -run TestClassifierCapture -v`
Expected: FAIL — `srv.classifierCorpus undefined`, `classifierRequest has no field captureSource`.

- [ ] **Step 4: Add the recorder field and build it in applyClassifierConfig**

In `internal/api/server.go`, next to `classifierAudit *classifier.Recorder` (line 119):

```go
	classifierCorpus   *corpus.Recorder
```

In `internal/api/classifier_rules.go`, add `"antigravity-go-proxy/internal/classifier/corpus"` to the imports and rebuild the recorder at the top of `applyClassifierConfig`:

```go
func (server *Server) applyClassifierConfig(cfg config.ClassifierConfig) {
	// The recorder is rebuilt on every config application so a settings save
	// takes effect without a restart.
	if cfg.Capture.Enabled {
		resolved := cfg.Capture.Resolved()
		server.classifierCorpus = corpus.New(resolved.Dir, corpus.Options{
			MaxFiles:     resolved.MaxFiles,
			MaxFileBytes: resolved.MaxFileBytes,
			RedactPaths:  resolved.RedactPathsEnabled(),
		})
	} else {
		server.classifierCorpus = nil
	}

	if server.classifierMatcher == nil {
```

(The rest of `applyClassifierConfig` is unchanged.)

- [ ] **Step 5: Add captureSource to classifierRequest and set it**

In `internal/api/classifier_rules.go`, extend the struct at line 53:

```go
// classifierRequest bundles the inputs needed to evaluate and apply a rule.
type classifierRequest struct {
	rule            *config.Rule
	backend         *config.TargetBackend
	rawBody         []byte
	model           string
	streamRequested bool
	// captureSource is the corpus source for this request. A rule that
	// answers assigns through it so the single deferred recorder in messages
	// writes one row with the right provenance.
	captureSource *corpus.Source
}
```

Add a helper next to `applyClassifierRule`:

```go
// setCaptureSource records which path answered, when capture is on.
func (req classifierRequest) setCaptureSource(source corpus.Source) {
	if req.captureSource != nil {
		*req.captureSource = source
	}
}
```

Then in `applyClassifierRule`, set it on each answering branch:

- In `case config.RuleActionStub:`, immediately before `record(classifier.EventStatusStubbed, "")`, add `req.setCaptureSource(corpus.SourceStub)`.
- In `case config.RuleActionReroute:`, immediately before `record(classifier.EventStatusRerouted, "")`, add:

```go
		if req.backend != nil && req.backend.Format == config.BackendFormatLaya {
			req.setCaptureSource(corpus.SourceLaya)
		} else {
			req.setCaptureSource(corpus.SourceRule)
		}
```

`config.BackendFormatLaya` does not exist until Task 10. Until then, use `req.setCaptureSource(corpus.SourceRule)` alone; Task 12 Step 5 replaces it with the branch above.

- [ ] **Step 6: Install the tap in server.go**

In `internal/api/server.go`, insert immediately after `streamRequested, _ := anthropicRequest["stream"].(bool)` (line 720) and before the rule-matching block at line 726:

```go
	// Capture installs one tap and one deferred writer, before any branch can
	// answer. Recording inside each branch instead would double-write when a
	// branch falls through to the next.
	captureSource := corpus.SourceUpstream
	var captureRef *corpus.Source
	if server.classifierCorpus.Enabled() {
		if kind, detected := classifier.Detect(rawBody); detected {
			tap := corpus.NewResponseTap(writer)
			writer = tap
			captureRef = &captureSource
			contextEntries := cfg.Classifier.Capture.Resolved().ContextEntries
			defer func() {
				server.classifierCorpus.Record(corpus.BuildEntry(corpus.EntryInput{
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

Then pass `captureSource: captureRef` in the `classifierRequest` literal at line 731:

```go
				classifierRequest{
					rule:            rule,
					backend:         backend,
					rawBody:         rawBody,
					model:           model,
					streamRequested: streamRequested,
					captureSource:   captureRef,
				},
```

Finally, in the built-in `always_stub` branch further down (the block beginning at line 783, `if effectiveAction == config.ActionAlwaysStub`), set `captureSource = corpus.SourceStub` immediately before the stub is written.

Add `"antigravity-go-proxy/internal/classifier/corpus"` to the `server.go` imports.

- [ ] **Step 7: Confirm the writer reassignment reaches every later write**

Run: `grep -n "writer\b" internal/api/server.go | sed -n '1,80p'`
Expected: every response write after line 720 goes through the local `writer` variable. If any later code captured the original writer into another variable **before** line 720, move the capture block above that assignment so the tap still wraps it.

- [ ] **Step 8: Run the tests**

Run: `go test ./internal/api/ -run TestClassifier -v`
Expected: PASS — the two new capture tests plus every existing classifier test.

- [ ] **Step 9: Run the full suite**

Run: `go build ./... && go test ./...`
Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add internal/api/server.go internal/api/classifier_rules.go internal/api/classifier_capture_test.go
git commit -m "feat(api): capture classifier requests and their verdicts as JSONL"
```

---

## Task 7: Export script

**Files:**
- Create: `scripts/corpus_to_laya.py`
- Test: `scripts/test_corpus_to_laya.py`

**Interfaces:**
- Consumes: the JSONL row schema from Task 4.
- Produces: `load_rows(path)`, `bucket_for(severity)`, `to_example(row)`, `convert(rows)` — importable from the test.

**Bucket boundaries** (severity is 0–100, 50 blocks):

| Bucket | Severity range |
|---|---|
| `A` | 0–9 |
| `B` | 10–24 |
| `C` | 25–49 |
| `D` | 50–100 |

- [ ] **Step 1: Write the failing test**

Create `scripts/test_corpus_to_laya.py`:

```python
"""Tests for corpus_to_laya. Run: python3 -m pytest scripts/test_corpus_to_laya.py"""
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))

from corpus_to_laya import bucket_for, convert, load_rows, to_example


def test_bucket_boundaries():
    assert bucket_for(0) == "A"
    assert bucket_for(9) == "A"
    assert bucket_for(10) == "B"
    assert bucket_for(24) == "B"
    assert bucket_for(25) == "C"
    assert bucket_for(49) == "C"
    assert bucket_for(50) == "D"
    assert bucket_for(100) == "D"


def test_to_example_shape():
    row = {
        "v": 1,
        "action": '{"Bash":"rm -rf build/"}',
        "severity": 8,
        "source": "upstream",
    }
    example = to_example(row)
    assert example["state"] == {"action": '{"Bash":"rm -rf build/"}'}
    assert example["answers"]["risk"] == "A"
    assert example["questions"]["risk"]["type"] == "choice"
    assert set(example["questions"]["risk"]["criteria"]) == {"A", "B", "C", "D"}


def test_convert_keeps_only_upstream_rows():
    rows = [
        {"action": "a", "severity": 1, "source": "upstream"},
        {"action": "b", "severity": 2, "source": "laya"},
        {"action": "c", "severity": 3, "source": "stub"},
        {"action": "d", "severity": 4, "source": "rule"},
    ]
    examples, stats = convert(rows)
    assert len(examples) == 1
    assert examples[0]["state"]["action"] == "a"
    assert stats["skipped_source"] == 3


def test_convert_skips_unlabelled_rows():
    rows = [
        {"action": "a", "severity": -1, "source": "upstream"},
        {"action": "b", "severity": 5, "source": "upstream"},
    ]
    examples, stats = convert(rows)
    assert len(examples) == 1
    assert stats["skipped_unlabelled"] == 1


def test_convert_skips_rows_without_an_action():
    rows = [{"action": "", "severity": 5, "source": "upstream"}]
    examples, stats = convert(rows)
    assert examples == []
    assert stats["skipped_no_action"] == 1


def test_convert_counts_distinct_system_hashes():
    rows = [
        {"action": "a", "severity": 1, "source": "upstream", "system_sha256": "aa"},
        {"action": "b", "severity": 2, "source": "upstream", "system_sha256": "bb"},
    ]
    _, stats = convert(rows)
    assert stats["system_hashes"] == 2


def test_load_rows_skips_blank_and_broken_lines(tmp_path):
    path = tmp_path / "classifier-2026-09-23.jsonl"
    path.write_text(
        json.dumps({"action": "a", "severity": 1, "source": "upstream"})
        + "\n\nnot json\n"
        + json.dumps({"action": "b", "severity": 2, "source": "upstream"})
        + "\n"
    )
    rows = load_rows(path)
    assert len(rows) == 2
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `python3 -m pytest scripts/test_corpus_to_laya.py -q`
Expected: FAIL — `ModuleNotFoundError: No module named 'corpus_to_laya'`.

- [ ] **Step 3: Write the implementation**

Create `scripts/corpus_to_laya.py`:

```python
#!/usr/bin/env python3
"""Convert a classifier corpus into a Laya fine-tune dataset.

Reads the JSONL written by the proxy's classifier capture and emits one
example per labelled row, as a `choice` question over four risk buckets.

Only rows with source == "upstream" carry a teacher label. Rows produced by
the local model itself (source == "laya") are excluded on purpose: training on
them would teach the model its own answers and entrench its errors.

Usage:
    python3 scripts/corpus_to_laya.py ~/.config/antigravity-proxy/corpus/*.jsonl -o train.jsonl
"""
import argparse
import json
import sys
from pathlib import Path

# Severity is an integer 0-100 with 50 as the allow/block boundary; see
# docs/classifier-fallback-notes.md. Bucket edges live here, not in Go, so
# they can be revised without recollecting the corpus.
BUCKETS = (
    ("A", 0, 9),
    ("B", 10, 24),
    ("C", 25, 49),
    ("D", 50, 100),
)

CRITERIA = {
    "A": "read-only inspection: listing, reading, searching, status, diff",
    "B": "local write: edits files, installs dependencies, runs build or tests",
    "C": "outbound or shared effect: network, push, publish, deploy, send",
    "D": "destructive or credential-touching: deletes data, rewrites history, reads secrets",
}

INSTRUCTIONS = "Classify the risk of this shell action taken by a coding agent."

TRAINING_SOURCE = "upstream"


def bucket_for(severity):
    """Return the bucket label for a 0-100 severity."""
    for label, low, high in BUCKETS:
        if low <= severity <= high:
            return label
    return BUCKETS[-1][0] if severity > 100 else BUCKETS[0][0]


def to_example(row):
    """Return one Laya training example for a labelled row."""
    return {
        "state": {"action": row["action"]},
        "questions": {
            "risk": {
                "type": "choice",
                "instructions": INSTRUCTIONS,
                "criteria": dict(CRITERIA),
            }
        },
        "answers": {"risk": bucket_for(row["severity"])},
    }


def convert(rows):
    """Return (examples, stats). Filtering is explicit and counted."""
    stats = {
        "total": len(rows),
        "skipped_source": 0,
        "skipped_unlabelled": 0,
        "skipped_no_action": 0,
        "system_hashes": 0,
    }
    hashes = set()
    examples = []
    for row in rows:
        if row.get("source") != TRAINING_SOURCE:
            stats["skipped_source"] += 1
            continue
        if row.get("severity", -1) < 0:
            stats["skipped_unlabelled"] += 1
            continue
        if not row.get("action"):
            stats["skipped_no_action"] += 1
            continue
        if row.get("system_sha256"):
            hashes.add(row["system_sha256"])
        examples.append(to_example(row))
    stats["system_hashes"] = len(hashes)
    return examples, stats


def load_rows(path):
    """Read one JSONL file, skipping blank and unparseable lines."""
    rows = []
    with open(path, "r", encoding="utf-8") as handle:
        for line in handle:
            line = line.strip()
            if not line:
                continue
            try:
                rows.append(json.loads(line))
            except ValueError:
                continue
    return rows


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("inputs", nargs="+", type=Path, help="corpus JSONL files")
    parser.add_argument("-o", "--output", type=Path, required=True, help="dataset JSONL to write")
    args = parser.parse_args(argv)

    rows = []
    for path in args.inputs:
        rows.extend(load_rows(path))

    examples, stats = convert(rows)

    with open(args.output, "w", encoding="utf-8") as handle:
        for example in examples:
            handle.write(json.dumps(example, ensure_ascii=False) + "\n")

    print(f"read {stats['total']} rows, wrote {len(examples)} examples to {args.output}")
    print(
        f"skipped: {stats['skipped_source']} non-upstream, "
        f"{stats['skipped_unlabelled']} unlabelled, "
        f"{stats['skipped_no_action']} without an action"
    )
    if stats["system_hashes"] > 1:
        print(
            f"WARNING: {stats['system_hashes']} distinct monitor prompts in this corpus. "
            "The teacher changed mid-collection; these rows are not one dataset.",
            file=sys.stderr,
        )
    if not examples:
        print(
            "WARNING: no labelled rows. Capture only yields labels while classifier "
            "requests reach upstream — check that rules were set to passthrough.",
            file=sys.stderr,
        )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `python3 -m pytest scripts/test_corpus_to_laya.py -q`
Expected: PASS — 7 tests.

- [ ] **Step 5: Commit**

```bash
git add scripts/corpus_to_laya.py scripts/test_corpus_to_laya.py
git commit -m "feat(scripts): convert a classifier corpus into a laya fine-tune set"
```

---

## Task 8: Capture settings in the WebUI

**Files:**
- Modify: `internal/webui/public/js/components/classifier-config.js` — `config` defaults object near line 10
- Modify: `internal/webui/public/views/settings.html` — classifier settings section
- Modify: `internal/webui/public/js/translations/en.js` near line 781, `pt.js` at the matching keys

**Interfaces:**
- Consumes: the `capture` config shape from Task 5.
- Produces: no JS API other than the component's own `config.capture` state.

- [ ] **Step 1: Add the capture defaults to the component**

In `internal/webui/public/js/components/classifier-config.js`, inside the `config` object literal, after `backends: {}`:

```js
        backends: {},
        capture: {
            enabled: false,
            dir: '',
            contextEntries: 2,
            maxFiles: 8,
            maxFileBytes: 67108864,
            redactPaths: true
        }
```

Add a guard beside `ensureVariants` so a config saved before this change still renders:

```js
    ensureCapture() {
        if (!this.config.capture) {
            this.config.capture = {
                enabled: false,
                dir: '',
                contextEntries: 2,
                maxFiles: 8,
                maxFileBytes: 67108864,
                redactPaths: true
            };
        }
    },
```

Call `this.ensureCapture()` wherever `this.ensureVariants()` is already called after a config load.

- [ ] **Step 2: Find the classifier section in the markup**

Run: `grep -n "classifierSettingsTitle\|classifierAction\b" internal/webui/public/views/settings.html`
Expected: the anchor for the classifier settings block. Insert the markup below after the action-mode control and before the rules editor.

- [ ] **Step 3: Add the markup**

```html
<div class="setting-group" x-show="config.capture">
    <h4 x-text="$store.i18n.t('classifierCaptureTitle')"></h4>
    <p class="setting-hint" x-text="$store.i18n.t('classifierCaptureDesc')"></p>
    <p class="setting-hint setting-hint-warning" x-text="$store.i18n.t('classifierCaptureCost')"></p>
    <label class="toggle">
        <input type="checkbox" x-model="config.capture.enabled">
        <span x-text="$store.i18n.t('classifierCaptureEnabled')"></span>
    </label>
    <label>
        <span x-text="$store.i18n.t('classifierCaptureDir')"></span>
        <input type="text" x-model="config.capture.dir"
               :placeholder="$store.i18n.t('classifierCaptureDirPlaceholder')">
    </label>
    <label>
        <span x-text="$store.i18n.t('classifierCaptureContextEntries')"></span>
        <input type="number" min="-1" max="20" x-model.number="config.capture.contextEntries">
    </label>
    <label>
        <span x-text="$store.i18n.t('classifierCaptureMaxFiles')"></span>
        <input type="number" min="1" max="365" x-model.number="config.capture.maxFiles">
    </label>
    <label class="toggle">
        <input type="checkbox" x-model="config.capture.redactPaths">
        <span x-text="$store.i18n.t('classifierCaptureRedact')"></span>
    </label>
</div>
```

- [ ] **Step 4: Add the strings**

In `internal/webui/public/js/translations/en.js`, beside the existing `classifier*` keys near line 781:

```js
    classifierCaptureTitle: "Corpus Capture",
    classifierCaptureDesc: "Write each classifier request and the verdict returned for it to local JSONL, as training data for a local decision model.",
    classifierCaptureCost: "Verdict labels are only collected while classifier requests reach upstream. During a collection window, rules must be set to passthrough, which consumes upstream quota.",
    classifierCaptureEnabled: "Enable Corpus Capture",
    classifierCaptureDir: "Corpus Directory",
    classifierCaptureDirPlaceholder: "<config dir>/corpus",
    classifierCaptureContextEntries: "Context Entries Kept (0 for the action only)",
    classifierCaptureMaxFiles: "Day Files Retained",
    classifierCaptureRedact: "Replace Home Directory With ~",
```

In `pt.js`, at the matching position:

```js
    classifierCaptureTitle: "Captura de Corpus",
    classifierCaptureDesc: "Grava cada requisição do classificador e o veredito retornado em JSONL local, como dados de treino para um modelo de decisão local.",
    classifierCaptureCost: "Os rótulos de veredito só são coletados enquanto as requisições do classificador chegam ao upstream. Durante uma janela de coleta, as regras precisam estar em passthrough, o que consome cota upstream.",
    classifierCaptureEnabled: "Ativar Captura de Corpus",
    classifierCaptureDir: "Diretório do Corpus",
    classifierCaptureDirPlaceholder: "<diretório de config>/corpus",
    classifierCaptureContextEntries: "Entradas de Contexto Mantidas (0 para apenas a ação)",
    classifierCaptureMaxFiles: "Arquivos Diários Retidos",
    classifierCaptureRedact: "Substituir o Diretório Home por ~",
```

- [ ] **Step 5: Verify the key sets match**

Run: `grep -c "classifierCapture" internal/webui/public/js/translations/en.js internal/webui/public/js/translations/pt.js`
Expected: the same count, 9, in both files.

- [ ] **Step 6: Verify in the browser**

Run the proxy, open the settings page, go to the classifier tab. Toggle capture on, save, and confirm the settings round-trip after a reload.

- [ ] **Step 7: Commit**

```bash
git add internal/webui/public/js/components/classifier-config.js internal/webui/public/views/settings.html internal/webui/public/js/translations/en.js internal/webui/public/js/translations/pt.js
git commit -m "feat(webui): corpus capture settings in the classifier tab"
```

---

# Phase 2 — Laya backend

## Task 9: Thread the variant into the adapters

**Files:**
- Modify: `internal/api/classifier_rules.go:131-219` — `backendFormatAdapter`, both existing adapters, `callClassifierBackend`
- Modify: `internal/api/classifier_rules.go:53` — `classifierRequest` gains `kind`
- Modify: `internal/api/classifier_rules.go:113` — the reroute branch passes the call struct

**Interfaces:**
- Consumes: `classifier.Detect` (`internal/classifier/classifier.go:90`).
- Produces:

```go
type classifierCall struct {
	rawBody []byte
	model   string
	kind    classifier.Kind
	backend *config.TargetBackend
}

type backendFormatAdapter struct {
	preparePayload func(call classifierCall) ([]byte, error)
	setHeaders     func(req *http.Request, apiKey string)
	parseResponse  func(respBody []byte, call classifierCall) ([]byte, error)
}

func (server *Server) callClassifierBackend(ctx context.Context, call classifierCall) ([]byte, error)
```

**Why:** Stage 1 must answer with a bare `<severity>N</severity>` and Stage 2 with `<thinking>…</thinking><severity>N</severity>`. `parseResponse` currently sees neither the variant nor the request body, so the Laya adapter cannot pick a shape. This is a pure refactor: the `anthropic` and `openai` adapters ignore `kind` and their behavior is unchanged.

- [ ] **Step 1: Run the existing tests to record the baseline**

Run: `go test ./internal/api/ -run TestApplyClassifierRule -v`
Expected: PASS. Note the test names; they must still pass unchanged at the end of this task.

- [ ] **Step 2: Change the struct and both adapters**

Replace `internal/api/classifier_rules.go:131-173` with:

```go
// classifierCall is one backend invocation. It carries the detected variant
// because Stage 1 and Stage 2 require different answer shapes, and
// parseResponse would otherwise have no way to tell them apart.
type classifierCall struct {
	rawBody []byte
	model   string
	kind    classifier.Kind
	backend *config.TargetBackend
}

type backendFormatAdapter struct {
	preparePayload func(call classifierCall) ([]byte, error)
	setHeaders     func(req *http.Request, apiKey string)
	parseResponse  func(respBody []byte, call classifierCall) ([]byte, error)
}

var openAIFormatAdapter = backendFormatAdapter{
	preparePayload: func(call classifierCall) ([]byte, error) {
		return classifier.TranslateAnthropicToOpenAI(call.rawBody, call.backend.Model, call.backend.MaxTokens)
	},
	setHeaders: func(req *http.Request, apiKey string) {
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	},
	parseResponse: func(respBody []byte, call classifierCall) ([]byte, error) {
		return classifier.TranslateOpenAIToAnthropic(respBody, call.model)
	},
}

var anthropicFormatAdapter = backendFormatAdapter{
	preparePayload: func(call classifierCall) ([]byte, error) {
		return call.rawBody, nil
	},
	setHeaders: func(req *http.Request, apiKey string) {
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("x-api-key", apiKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	},
	parseResponse: func(respBody []byte, call classifierCall) ([]byte, error) {
		return respBody, nil
	},
}

func getBackendFormatAdapter(format config.BackendFormat) backendFormatAdapter {
	if format == config.BackendFormatOpenAI {
		return openAIFormatAdapter
	}
	return anthropicFormatAdapter
}
```

- [ ] **Step 3: Change callClassifierBackend**

Replace the signature and the three call-site lines in `internal/api/classifier_rules.go:178-219`:

```go
func (server *Server) callClassifierBackend(
	ctx context.Context,
	call classifierCall,
) ([]byte, error) {
	timeout := time.Duration(call.backend.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultClassifierBackendTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	adapter := getBackendFormatAdapter(call.backend.Format)

	payload, err := adapter.preparePayload(call)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, call.backend.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	adapter.setHeaders(req, call.backend.APIKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxClassifierBackendResponse))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("classifier backend returned %d", resp.StatusCode)
	}

	return adapter.parseResponse(body, call)
}
```

- [ ] **Step 4: Add kind to classifierRequest and build the call**

Add the field to `classifierRequest` (`internal/api/classifier_rules.go:53`):

```go
	// kind is the detected variant. Rules match on their own patterns, so it
	// is resolved here rather than taken from the rule.
	kind classifier.Kind
```

In `applyClassifierRule`, at the top of `case config.RuleActionReroute:` after the `req.backend == nil` guard, resolve it and build the call:

```go
		kind := req.kind
		if kind == classifier.KindNone {
			kind, _ = classifier.Detect(req.rawBody)
		}
		message, err := server.callClassifierBackend(request.Context(), classifierCall{
			rawBody: req.rawBody,
			model:   req.model,
			kind:    kind,
			backend: req.backend,
		})
```

(The following `if err != nil` block is unchanged.)

- [ ] **Step 5: Build and run the existing tests**

Run: `go build ./... && go test ./internal/api/ -run TestClassifier -v`
Expected: PASS, with no edits to any existing test file. If a test fails to compile, it called `callClassifierBackend` directly — update only its call to the new struct form.

- [ ] **Step 6: Commit**

```bash
git add internal/api/classifier_rules.go
git commit -m "refactor(api): pass a classifierCall struct to backend format adapters"
```

---

## Task 10: Laya wire format and backend overrides

**Files:**
- Modify: `internal/config/config.go:238-257` — `BackendFormatLaya`, `TargetBackend` fields
- Modify: `internal/api/management.go` — validation, after the capture validation from Task 5
- Test: `internal/config/config_test.go` (append)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `BackendFormatLaya BackendFormat = "laya"`
  - On `TargetBackend`: `LayaQuestionName string`, `LayaInstructions string`, `LayaCriteria map[string]string`, `LayaSeverityMap map[string]int`, `LayaMaxSeverity int`, `LayaStateChars int`
  - `func (b TargetBackend) LayaSettings() LayaSettings` — resolves defaults
  - `type LayaSettings struct { QuestionName string; Instructions string; Criteria map[string]string; SeverityMap map[string]int; MaxSeverity int; StateChars int }`

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go`:

```go
func TestLayaSettingsDefaults(t *testing.T) {
	settings := TargetBackend{Format: BackendFormatLaya}.LayaSettings()

	if settings.QuestionName != "risk" {
		t.Errorf("QuestionName = %q, want risk", settings.QuestionName)
	}
	if settings.MaxSeverity != 49 {
		t.Errorf("MaxSeverity = %d, want 49", settings.MaxSeverity)
	}
	if settings.StateChars != 1200 {
		t.Errorf("StateChars = %d, want 1200", settings.StateChars)
	}
	if len(settings.Criteria) != 4 {
		t.Errorf("Criteria has %d entries, want 4", len(settings.Criteria))
	}
	for _, label := range []string{"A", "B", "C", "D"} {
		if _, exists := settings.Criteria[label]; !exists {
			t.Errorf("Criteria is missing %q", label)
		}
		if _, exists := settings.SeverityMap[label]; !exists {
			t.Errorf("SeverityMap is missing %q", label)
		}
	}
	for label, severity := range settings.SeverityMap {
		if severity >= 50 {
			t.Errorf("default severity for %q is %d; a laya verdict must never reach the block boundary", label, severity)
		}
	}
	if settings.Instructions == "" {
		t.Error("Instructions is empty")
	}
}

func TestLayaSettingsHonorsOverrides(t *testing.T) {
	backend := TargetBackend{
		Format:           BackendFormatLaya,
		LayaQuestionName: "danger",
		LayaInstructions: "custom",
		LayaCriteria:     map[string]string{"X": "one", "Y": "two"},
		LayaSeverityMap:  map[string]int{"X": 1, "Y": 2},
		LayaMaxSeverity:  10,
		LayaStateChars:   400,
	}
	settings := backend.LayaSettings()

	if settings.QuestionName != "danger" {
		t.Errorf("QuestionName = %q", settings.QuestionName)
	}
	if settings.Instructions != "custom" {
		t.Errorf("Instructions = %q", settings.Instructions)
	}
	if len(settings.Criteria) != 2 {
		t.Errorf("Criteria has %d entries, want 2", len(settings.Criteria))
	}
	if settings.SeverityMap["Y"] != 2 {
		t.Errorf("SeverityMap[Y] = %d, want 2", settings.SeverityMap["Y"])
	}
	if settings.MaxSeverity != 10 {
		t.Errorf("MaxSeverity = %d, want 10", settings.MaxSeverity)
	}
	if settings.StateChars != 400 {
		t.Errorf("StateChars = %d, want 400", settings.StateChars)
	}
}

func TestBackendFormatLayaDecodes(t *testing.T) {
	raw := `{"name":"laya","url":"http://127.0.0.1:8000/v1/systemone","format":"laya","model":"english"}`
	var backend TargetBackend
	if err := json.Unmarshal([]byte(raw), &backend); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if backend.Format != BackendFormatLaya {
		t.Errorf("Format = %q, want laya", backend.Format)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/config/ -run 'TestLaya|TestBackendFormatLaya' -v`
Expected: FAIL — `undefined: BackendFormatLaya`.

- [ ] **Step 3: Write the implementation**

In `internal/config/config.go`, extend the format constants at line 242:

```go
	BackendFormatAnthropic BackendFormat = "anthropic"
	BackendFormatOpenAI    BackendFormat = "openai"
	// BackendFormatLaya speaks laya-serve's POST /v1/systemone typed-decision
	// protocol, which is not a chat API: the adapter builds a question from
	// the graded action and maps the chosen label back to a severity.
	BackendFormatLaya BackendFormat = "laya"
```

Extend `TargetBackend` (line 249):

```go
type TargetBackend struct {
	Name      string        `json:"name"`
	URL       string        `json:"url"`
	Format    BackendFormat `json:"format"`
	APIKey    string        `json:"apiKey,omitempty"`
	Model     string        `json:"model"`
	MaxTokens int           `json:"maxTokens,omitempty"`
	TimeoutMs int           `json:"timeoutMs,omitempty"`

	// Laya overrides. Each empty or zero value falls back to the default in
	// LayaSettings. They are ignored unless Format is BackendFormatLaya.
	LayaQuestionName string            `json:"layaQuestionName,omitempty"`
	LayaInstructions string            `json:"layaInstructions,omitempty"`
	LayaCriteria     map[string]string `json:"layaCriteria,omitempty"`
	LayaSeverityMap  map[string]int    `json:"layaSeverityMap,omitempty"`
	LayaMaxSeverity  int               `json:"layaMaxSeverity,omitempty"`
	LayaStateChars   int               `json:"layaStateChars,omitempty"`
}

// LayaSettings is a Laya backend's resolved question and mapping.
type LayaSettings struct {
	QuestionName string
	Instructions string
	Criteria     map[string]string
	SeverityMap  map[string]int
	MaxSeverity  int
	StateChars   int
}

// Laya defaults. Severity is 0-100 with 50 as the allow/block boundary, so
// every default sits well below it: the local model is a plausible-verdict
// source, not a gate. Its base checkpoints score near chance zero-shot.
const (
	DefaultLayaQuestionName = "risk"
	DefaultLayaMaxSeverity  = 49
	DefaultLayaStateChars   = 1200
	DefaultLayaInstructions = "Classify the risk of this shell action taken by a coding agent."
)

// defaultLayaCriteria uses opaque A-D keys on purpose: laya renders choice
// keys verbatim and its checkpoints can follow a semantic key instead of the
// option description.
var defaultLayaCriteria = map[string]string{
	"A": "read-only inspection: listing, reading, searching, status, diff",
	"B": "local write: edits files, installs dependencies, runs build or tests",
	"C": "outbound or shared effect: network, push, publish, deploy, send",
	"D": "destructive or credential-touching: deletes data, rewrites history, reads secrets",
}

var defaultLayaSeverityMap = map[string]int{"A": 0, "B": 5, "C": 15, "D": 35}

// LayaSettings resolves the backend's overrides against the defaults.
func (backend TargetBackend) LayaSettings() LayaSettings {
	settings := LayaSettings{
		QuestionName: backend.LayaQuestionName,
		Instructions: backend.LayaInstructions,
		Criteria:     backend.LayaCriteria,
		SeverityMap:  backend.LayaSeverityMap,
		MaxSeverity:  backend.LayaMaxSeverity,
		StateChars:   backend.LayaStateChars,
	}
	if settings.QuestionName == "" {
		settings.QuestionName = DefaultLayaQuestionName
	}
	if settings.Instructions == "" {
		settings.Instructions = DefaultLayaInstructions
	}
	if len(settings.Criteria) == 0 {
		settings.Criteria = defaultLayaCriteria
	}
	if len(settings.SeverityMap) == 0 {
		settings.SeverityMap = defaultLayaSeverityMap
	}
	if settings.MaxSeverity <= 0 {
		settings.MaxSeverity = DefaultLayaMaxSeverity
	}
	if settings.StateChars <= 0 {
		settings.StateChars = DefaultLayaStateChars
	}
	return settings
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ -v -run 'TestLaya|TestBackendFormat|TestClassifier'`
Expected: PASS.

- [ ] **Step 5: Add validation in management.go**

After the capture validation added in Task 5, insert:

```go
		for backendKey, backend := range classifierReq.Backends {
			if backend.Format != config.BackendFormatLaya {
				continue
			}
			if backend.LayaMaxSeverity < 0 || backend.LayaMaxSeverity > 100 {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("backend %q: layaMaxSeverity must be between 0 and 100", backendKey)})
				return
			}
			if backend.LayaStateChars != 0 && (backend.LayaStateChars < 200 || backend.LayaStateChars > 8000) {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("backend %q: layaStateChars must be between 200 and 8000", backendKey)})
				return
			}
			if len(backend.LayaCriteria) == 0 && len(backend.LayaSeverityMap) == 0 {
				continue
			}
			if len(backend.LayaCriteria) < 2 {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("backend %q: layaCriteria needs at least 2 options", backendKey)})
				return
			}
			if len(backend.LayaCriteria) != len(backend.LayaSeverityMap) {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("backend %q: layaCriteria and layaSeverityMap must have the same keys", backendKey)})
				return
			}
			for label, severity := range backend.LayaSeverityMap {
				if _, exists := backend.LayaCriteria[label]; !exists {
					writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("backend %q: layaSeverityMap has label %q with no matching criteria entry", backendKey, label)})
					return
				}
				if severity < 0 || severity > 100 {
					writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("backend %q: layaSeverityMap[%q] must be between 0 and 100", backendKey, label)})
					return
				}
			}
		}
```

- [ ] **Step 6: Build and test**

Run: `go build ./... && go test ./internal/api/ ./internal/config/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go internal/api/management.go
git commit -m "feat(config): laya backend format with question and severity overrides"
```

---

## Task 11: Laya request payload

**Files:**
- Create: `internal/api/classifier_laya.go`
- Test: `internal/api/classifier_laya_test.go`

**Interfaces:**
- Consumes: `corpus.ExtractAction` (Task 2), `config.TargetBackend.LayaSettings` (Task 10), `classifierCall` (Task 9).
- Produces: `func buildLayaPayload(call classifierCall) ([]byte, error)`, and the unexported types `layaRequest`, `layaQuestion`.

- [ ] **Step 1: Write the failing test**

Create `internal/api/classifier_laya_test.go`:

```go
package api

import (
	"encoding/json"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/classifier"
	"antigravity-go-proxy/internal/config"
)

// layaBody is a stage 1 classifier request with a complete transcript layout.
const layaBody = `{
  "model": "claude-sonnet-5",
  "system": [
    {"type": "text", "text": "You are a security monitor for autonomous AI coding agents."}
  ],
  "messages": [{"role": "user", "content": [
    {"type": "text", "text": "<transcript>"},
    {"type": "text", "text": "{\"user\":\"tidy up\"}"},
    {"type": "text", "text": "{\"Bash\":\"rm -rf build/\"}"},
    {"type": "text", "text": "</transcript>"},
    {"type": "text", "text": "Grade HARM ONLY — do NOT reduce for user intent"}
  ]}]
}`

func layaBackend() *config.TargetBackend {
	return &config.TargetBackend{
		Name:   "laya",
		URL:    "http://127.0.0.1:8000/v1/systemone",
		Format: config.BackendFormatLaya,
		Model:  "english",
	}
}

func TestBuildLayaPayloadShape(t *testing.T) {
	payload, err := buildLayaPayload(classifierCall{
		rawBody: []byte(layaBody),
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: layaBackend(),
	})
	if err != nil {
		t.Fatalf("buildLayaPayload: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}

	if decoded["model"] != "english" {
		t.Errorf("model = %v, want english", decoded["model"])
	}

	state, ok := decoded["state"].(map[string]any)
	if !ok {
		t.Fatalf("state is not an object: %v", decoded["state"])
	}
	action, _ := state["action"].(string)
	if action != `{"Bash":"rm -rf build/"}` {
		t.Errorf("state.action = %q, want the graded action", action)
	}

	questions, ok := decoded["questions"].(map[string]any)
	if !ok {
		t.Fatalf("questions is not an object: %v", decoded["questions"])
	}
	risk, ok := questions["risk"].(map[string]any)
	if !ok {
		t.Fatalf("questions.risk is missing: %v", questions)
	}
	if risk["type"] != "choice" {
		t.Errorf("question type = %v, want choice", risk["type"])
	}
	criteria, ok := risk["criteria"].(map[string]any)
	if !ok || len(criteria) != 4 {
		t.Errorf("criteria = %v, want 4 entries", risk["criteria"])
	}
	for _, label := range []string{"A", "B", "C", "D"} {
		if _, exists := criteria[label]; !exists {
			t.Errorf("criteria is missing %q", label)
		}
	}
}

func TestBuildLayaPayloadTruncatesFromTheLeft(t *testing.T) {
	backend := layaBackend()
	backend.LayaStateChars = 200

	long := strings.Repeat("x", 500)
	body := strings.Replace(layaBody, `{\"Bash\":\"rm -rf build/\"}`, long+`END`, 1)

	payload, err := buildLayaPayload(classifierCall{
		rawBody: []byte(body),
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: backend,
	})
	if err != nil {
		t.Fatalf("buildLayaPayload: %v", err)
	}

	var decoded struct {
		State struct {
			Action string `json:"action"`
		} `json:"state"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.State.Action) != 200 {
		t.Errorf("action is %d characters, want 200", len(decoded.State.Action))
	}
	if !strings.HasSuffix(decoded.State.Action, "END") {
		t.Errorf("action = %q, want the tail kept: the graded action sits at the end", decoded.State.Action)
	}
}

func TestBuildLayaPayloadHonorsCustomCriteria(t *testing.T) {
	backend := layaBackend()
	backend.LayaQuestionName = "danger"
	backend.LayaCriteria = map[string]string{"X": "safe", "Y": "unsafe"}
	backend.LayaSeverityMap = map[string]int{"X": 0, "Y": 20}

	payload, err := buildLayaPayload(classifierCall{
		rawBody: []byte(layaBody),
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: backend,
	})
	if err != nil {
		t.Fatalf("buildLayaPayload: %v", err)
	}

	var decoded struct {
		Questions map[string]struct {
			Criteria map[string]string `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	question, exists := decoded.Questions["danger"]
	if !exists {
		t.Fatalf("questions has no danger entry: %v", decoded.Questions)
	}
	if len(question.Criteria) != 2 {
		t.Errorf("criteria = %v, want the 2 custom options", question.Criteria)
	}
}

func TestBuildLayaPayloadRejectsAnUnreadableTranscript(t *testing.T) {
	if _, err := buildLayaPayload(classifierCall{
		rawBody: []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"no transcript"}]}]}`),
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: layaBackend(),
	}); err == nil {
		t.Error("expected an error so the caller falls back to built-in handling")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/api/ -run TestBuildLayaPayload -v`
Expected: FAIL — `undefined: buildLayaPayload`.

- [ ] **Step 3: Write the implementation**

Create `internal/api/classifier_laya.go`:

```go
package api

import (
	"encoding/json"

	"antigravity-go-proxy/internal/classifier/corpus"
	"antigravity-go-proxy/internal/config"
)

// layaQuestion is one typed question in laya-serve's /v1/systemone protocol.
type layaQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type layaRequest struct {
	Model     string                  `json:"model,omitempty"`
	State     map[string]string       `json:"state"`
	Questions map[string]layaQuestion `json:"questions"`
}

// buildLayaPayload turns a classifier request into a laya typed-decision
// request. The classifier body runs to roughly 125KB while laya's English
// checkpoint holds about 320 tokens of state, so only the graded action is
// sent; an unreadable transcript is an error, never a guess.
func buildLayaPayload(call classifierCall) ([]byte, error) {
	settings := call.backend.LayaSettings()

	action, _, err := corpus.ExtractAction(call.rawBody, 0)
	if err != nil {
		return nil, err
	}
	// Truncate from the left: the graded action sits at the tail, so the head
	// is what can be lost without losing the thing being judged.
	if runes := []rune(action); len(runes) > settings.StateChars {
		action = string(runes[len(runes)-settings.StateChars:])
	}

	payload := layaRequest{
		Model: call.backend.Model,
		State: map[string]string{"action": action},
		Questions: map[string]layaQuestion{
			settings.QuestionName: {
				Type:         "choice",
				Instructions: settings.Instructions,
				Criteria:     settings.Criteria,
			},
		},
	}
	return json.Marshal(payload)
}

// layaSettingsFor is a nil-safe accessor used by the adapter functions.
func layaSettingsFor(backend *config.TargetBackend) config.LayaSettings {
	if backend == nil {
		return config.TargetBackend{}.LayaSettings()
	}
	return backend.LayaSettings()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/api/ -run TestBuildLayaPayload -v`
Expected: PASS — four tests.

- [ ] **Step 5: Commit**

```bash
git add internal/api/classifier_laya.go internal/api/classifier_laya_test.go
git commit -m "feat(api): build laya typed-decision payloads from classifier requests"
```

---

## Task 12: Laya response mapping and registration

**Files:**
- Modify: `internal/api/classifier_laya.go`
- Modify: `internal/api/classifier_rules.go` — register the adapter, Laya timeout default, `SourceLaya`
- Test: `internal/api/classifier_laya_test.go` (append)

**Interfaces:**
- Consumes: `buildLayaPayload` (Task 11), `classifier.StubWithText` and `classifier.ErrUnsupportedKind` (`internal/classifier/classifier.go:186`, `:52`), `corpus.SourceLaya` (Task 4).
- Produces: `func parseLayaResponse(respBody []byte, call classifierCall) ([]byte, error)`, `var layaFormatAdapter backendFormatAdapter`, `const defaultLayaBackendTimeout`.

- [ ] **Step 1: Write the failing test**

Append to `internal/api/classifier_laya_test.go`:

```go
func layaAnswer(label string) []byte {
	return []byte(`{"answers":{"risk":{"choice":"` + label + `","confidence":0.91}},
		"usage":{"input_tokens":40,"output_tokens":0}}`)
}

func verdictTextFrom(t *testing.T, message []byte) string {
	t.Helper()
	var envelope struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(message, &envelope); err != nil {
		t.Fatalf("response is not an Anthropic message: %v", err)
	}
	var text string
	for _, block := range envelope.Content {
		if block.Type == "text" {
			text += block.Text
		}
	}
	return text
}

func TestParseLayaResponseStage1(t *testing.T) {
	cases := map[string]string{
		"A": "<severity>0</severity>",
		"B": "<severity>5</severity>",
		"C": "<severity>15</severity>",
		"D": "<severity>35</severity>",
	}
	for label, want := range cases {
		t.Run(label, func(t *testing.T) {
			message, err := parseLayaResponse(layaAnswer(label), classifierCall{
				model:   "claude-sonnet-5",
				kind:    classifier.KindStage1Severity,
				backend: layaBackend(),
			})
			if err != nil {
				t.Fatalf("parseLayaResponse: %v", err)
			}
			if got := verdictTextFrom(t, message); got != want {
				t.Errorf("verdict = %q, want %q", got, want)
			}
		})
	}
}

func TestParseLayaResponseStage2HasThinkingAndNoCategory(t *testing.T) {
	message, err := parseLayaResponse(layaAnswer("B"), classifierCall{
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage2Severity,
		backend: layaBackend(),
	})
	if err != nil {
		t.Fatalf("parseLayaResponse: %v", err)
	}
	verdict := verdictTextFrom(t, message)
	if !strings.HasPrefix(verdict, "<thinking>") {
		t.Errorf("verdict = %q, want it to open with <thinking>", verdict)
	}
	if !strings.HasSuffix(verdict, "<severity>5</severity>") {
		t.Errorf("verdict = %q, want it to end with the severity tag", verdict)
	}
	if strings.Contains(verdict, "<category>") {
		t.Errorf("verdict = %q, want no category tag: allow verdicts omit it", verdict)
	}
}

func TestParseLayaResponseNeverReachesTheBlockBoundary(t *testing.T) {
	backend := layaBackend()
	// An operator map that would block, with the default clamp still in force.
	backend.LayaSeverityMap = map[string]int{"A": 0, "B": 5, "C": 15, "D": 95}
	backend.LayaCriteria = map[string]string{"A": "a", "B": "b", "C": "c", "D": "d"}

	message, err := parseLayaResponse(layaAnswer("D"), classifierCall{
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: backend,
	})
	if err != nil {
		t.Fatalf("parseLayaResponse: %v", err)
	}
	if got := verdictTextFrom(t, message); got != "<severity>49</severity>" {
		t.Errorf("verdict = %q, want the clamp to hold it at 49", got)
	}
}

func TestParseLayaResponseHonorsAnExplicitMaxSeverity(t *testing.T) {
	backend := layaBackend()
	backend.LayaMaxSeverity = 10

	message, err := parseLayaResponse(layaAnswer("D"), classifierCall{
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: backend,
	})
	if err != nil {
		t.Fatalf("parseLayaResponse: %v", err)
	}
	if got := verdictTextFrom(t, message); got != "<severity>10</severity>" {
		t.Errorf("verdict = %q, want 10", got)
	}
}

func TestParseLayaResponseErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		kind classifier.Kind
	}{
		{name: "unknown label", body: `{"answers":{"risk":{"choice":"Z"}}}`, kind: classifier.KindStage1Severity},
		{name: "missing question", body: `{"answers":{"other":{"choice":"A"}}}`, kind: classifier.KindStage1Severity},
		{name: "no answers", body: `{"usage":{}}`, kind: classifier.KindStage1Severity},
		{name: "not json", body: `<html>502</html>`, kind: classifier.KindStage1Severity},
		{name: "block prefilter", body: `{"answers":{"risk":{"choice":"A"}}}`, kind: classifier.KindBlockPrefilter},
		{name: "not a classifier request", body: `{"answers":{"risk":{"choice":"A"}}}`, kind: classifier.KindNone},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := parseLayaResponse([]byte(testCase.body), classifierCall{
				model:   "claude-sonnet-5",
				kind:    testCase.kind,
				backend: layaBackend(),
			}); err == nil {
				t.Error("expected an error so the caller falls back to built-in handling")
			}
		})
	}
}

func TestLayaAdapterIsRegistered(t *testing.T) {
	adapter := getBackendFormatAdapter(config.BackendFormatLaya)
	payload, err := adapter.preparePayload(classifierCall{
		rawBody: []byte(layaBody),
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: layaBackend(),
	})
	if err != nil {
		t.Fatalf("preparePayload: %v", err)
	}
	if !strings.Contains(string(payload), `"questions"`) {
		t.Errorf("registered adapter did not build a laya payload: %s", payload)
	}
}

func TestLayaAdapterSetsBearerHeaderOnlyWithAKey(t *testing.T) {
	adapter := getBackendFormatAdapter(config.BackendFormatLaya)

	withKey := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8000/v1/systemone", nil)
	adapter.setHeaders(withKey, "secret")
	if got := withKey.Header.Get("Authorization"); got != "Bearer secret" {
		t.Errorf("Authorization = %q, want Bearer secret", got)
	}

	withoutKey := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8000/v1/systemone", nil)
	adapter.setHeaders(withoutKey, "")
	if got := withoutKey.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want it absent", got)
	}
	if got := withoutKey.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
}
```

Add `"net/http"` and `"net/http/httptest"` to this file's import block.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/api/ -run 'TestParseLayaResponse|TestLayaAdapter' -v`
Expected: FAIL — `undefined: parseLayaResponse`.

- [ ] **Step 3: Write the implementation**

Append to `internal/api/classifier_laya.go` (and add `"fmt"` and the `classifier` import):

```go
// layaAnswer is one typed answer in a /v1/systemone response.
type layaAnswer struct {
	Choice     string  `json:"choice"`
	Confidence float64 `json:"confidence"`
}

type layaResponse struct {
	Answers map[string]layaAnswer `json:"answers"`
}

// layaThinkingPhrases give stage 2 a one-line rationale per label. Stage 2
// requires a <thinking> block; allow verdicts carry no <category> tag.
var layaThinkingPhrases = map[string]string{
	"A": "Read-only inspection; no policy match.",
	"B": "Local write action; no policy match.",
	"C": "Outbound effect; no policy match.",
	"D": "Destructive shape; no policy match.",
}

const layaFallbackThinking = "Routine action, no policy match."

// parseLayaResponse maps a laya label to a severity and renders the verdict
// shape the detected variant requires. Severity is clamped below the 50
// allow/block boundary by MaxSeverity, so a laya answer cannot block.
func parseLayaResponse(respBody []byte, call classifierCall) ([]byte, error) {
	settings := layaSettingsFor(call.backend)

	var decoded layaResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return nil, fmt.Errorf("laya: response is not JSON: %w", err)
	}
	answer, exists := decoded.Answers[settings.QuestionName]
	if !exists {
		return nil, fmt.Errorf("laya: response carries no answer for question %q", settings.QuestionName)
	}
	severity, known := settings.SeverityMap[answer.Choice]
	if !known {
		return nil, fmt.Errorf("laya: answer label %q is not in the severity map", answer.Choice)
	}
	if severity > settings.MaxSeverity {
		severity = settings.MaxSeverity
	}
	if severity < 0 {
		severity = 0
	}

	var verdict string
	switch call.kind {
	case classifier.KindStage1Severity:
		verdict = fmt.Sprintf("<severity>%d</severity>", severity)
	case classifier.KindStage2Severity:
		thinking, ok := layaThinkingPhrases[answer.Choice]
		if !ok {
			thinking = layaFallbackThinking
		}
		verdict = fmt.Sprintf("<thinking>%s</thinking><severity>%d</severity>", thinking, severity)
	default:
		// KindBlockPrefilter's response format was never captured, so it must
		// not be guessed; KindNone is not a classifier request at all.
		return nil, classifier.ErrUnsupportedKind
	}

	return classifier.StubWithText(call.model, verdict)
}

var layaFormatAdapter = backendFormatAdapter{
	preparePayload: buildLayaPayload,
	setHeaders: func(req *http.Request, apiKey string) {
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	},
	parseResponse: parseLayaResponse,
}
```

Add `"net/http"` to this file's imports.

- [ ] **Step 4: Register the adapter and the timeout**

In `internal/api/classifier_rules.go`, extend `getBackendFormatAdapter`:

```go
func getBackendFormatAdapter(format config.BackendFormat) backendFormatAdapter {
	switch format {
	case config.BackendFormatOpenAI:
		return openAIFormatAdapter
	case config.BackendFormatLaya:
		return layaFormatAdapter
	default:
		return anthropicFormatAdapter
	}
}
```

Add the timeout constant next to `defaultClassifierBackendTimeout` (line 20):

```go
// defaultLayaBackendTimeout is shorter than the general backend timeout: laya
// answers in well under a second even on CPU, and this call sits in front of
// the user's permission prompt.
const defaultLayaBackendTimeout = 5 * time.Second
```

And use it in `callClassifierBackend`:

```go
	timeout := time.Duration(call.backend.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultClassifierBackendTimeout
		if call.backend.Format == config.BackendFormatLaya {
			timeout = defaultLayaBackendTimeout
		}
	}
```

- [ ] **Step 5: Mark laya rows in the corpus**

In `applyClassifierRule`'s reroute branch, replace the `req.setCaptureSource(corpus.SourceRule)` line added in Task 6 with:

```go
		if req.backend != nil && req.backend.Format == config.BackendFormatLaya {
			req.setCaptureSource(corpus.SourceLaya)
		} else {
			req.setCaptureSource(corpus.SourceRule)
		}
```

Then add this test to `internal/api/classifier_capture_test.go`, so the `laya` provenance that keeps the fine-tune honest is actually asserted:

```go
func TestClassifierCaptureMarksLayaRows(t *testing.T) {
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"risk":{"choice":"A","confidence":0.9}}}`))
	}))
	defer backendServer.Close()

	srv := &Server{classifierAudit: classifier.NewRecorder(10)}
	srv.applyClassifierConfig(config.ClassifierConfig{Enabled: true})

	source := corpus.SourceUpstream
	backend := config.TargetBackend{
		Name:   "laya",
		URL:    backendServer.URL + "/v1/systemone",
		Format: config.BackendFormatLaya,
		Model:  "english",
	}
	rule := config.Rule{ID: "r", Name: "r", Action: config.RuleActionReroute, TargetBackend: "laya"}

	responded, _ := srv.applyClassifierRule(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(captureBody)),
		classifierRequest{
			rule:          &rule,
			backend:       &backend,
			rawBody:       []byte(captureBody),
			model:         "claude-sonnet-5",
			kind:          classifier.KindStage1Severity,
			captureSource: &source,
		},
	)
	if !responded {
		t.Fatal("expected the laya reroute to answer")
	}
	if source != corpus.SourceLaya {
		t.Errorf("captureSource = %q, want laya: a row marked otherwise would feed the model its own answers", source)
	}
}
```

Add `"net/http/httptest"` and `"net/http"` to that file's imports if they are not already present.

- [ ] **Step 6: Add the end-to-end test**

Append to `internal/api/classifier_laya_test.go`:

```go
func TestLayaRerouteEndToEnd(t *testing.T) {
	var receivedPath string
	var receivedBody []byte
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(layaAnswer("C"))
	}))
	defer backendServer.Close()

	srv := &Server{classifierAudit: classifier.NewRecorder(10)}
	srv.applyClassifierConfig(config.ClassifierConfig{
		Enabled: true,
		Rules: []config.Rule{{
			ID:      "stage1-laya",
			Name:    "Stage 1 to laya",
			Enabled: true,
			Conditions: config.RuleConditions{
				FooterPatterns: []config.MatchPattern{
					{Type: config.PatternSubstring, Pattern: "Grade HARM ONLY"},
				},
			},
			Action:        config.RuleActionReroute,
			TargetBackend: "laya",
		}},
		Backends: map[string]config.TargetBackend{
			"laya": {
				Name:   "laya",
				URL:    backendServer.URL + "/v1/systemone",
				Format: config.BackendFormatLaya,
				Model:  "english",
			},
		},
	})

	rule, backend, matched := srv.classifierMatcher.Match([]byte(layaBody))
	if !matched {
		t.Fatal("expected the rule to match")
	}

	recorder := httptest.NewRecorder()
	responded, _ := srv.applyClassifierRule(
		recorder,
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(layaBody)),
		classifierRequest{
			rule:    rule,
			backend: backend,
			rawBody: []byte(layaBody),
			model:   "claude-sonnet-5",
			kind:    classifier.KindStage1Severity,
		},
	)
	if !responded {
		t.Fatal("expected the reroute to answer")
	}
	if receivedPath != "/v1/systemone" {
		t.Errorf("laya was called at %q, want /v1/systemone", receivedPath)
	}
	if !strings.Contains(string(receivedBody), `"questions"`) {
		t.Errorf("laya received %s, want a typed-decision request", receivedBody)
	}
	if got := verdictTextFrom(t, recorder.Body.Bytes()); got != "<severity>15</severity>" {
		t.Errorf("client received %q, want the mapped severity for label C", got)
	}
}
```

Add `"io"` to the import block.

- [ ] **Step 7: Run every classifier test**

Run: `go test ./internal/api/ -run 'TestLaya|TestParseLaya|TestBuildLaya|TestClassifier|TestApplyClassifierRule' -v`
Expected: PASS.

- [ ] **Step 8: Run the full suite**

Run: `go build ./... && go test ./...`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/api/classifier_laya.go internal/api/classifier_laya_test.go internal/api/classifier_rules.go
git commit -m "feat(api): answer classifier requests from a local laya backend"
```

---

## Task 13: Laya backend in the WebUI

**Files:**
- Modify: `internal/webui/public/js/components/classifier-config.js`
- Modify: `internal/webui/public/views/settings.html`
- Modify: `internal/webui/public/js/translations/en.js`, `pt.js`

- [ ] **Step 1: Find the backend format selector**

Run: `grep -n "backendFormat\|format" internal/webui/public/js/components/classifier-config.js internal/webui/public/views/settings.html | grep -i "openai\|anthropic"`
Expected: the `<select>` bound to a backend's `format`, listing `anthropic` and `openai`.

- [ ] **Step 2: Add the laya option**

In `settings.html`, inside that `<select>`:

```html
<option value="laya" x-text="$store.i18n.t('classifierBackendFormatLaya')"></option>
```

- [ ] **Step 3: Add the advanced overrides block**

Immediately after the format selector, inside the same backend editor:

```html
<template x-if="backend.format === 'laya'">
    <div class="setting-subgroup">
        <p class="setting-hint" x-text="$store.i18n.t('classifierLayaHint')"></p>
        <label>
            <span x-text="$store.i18n.t('classifierLayaMaxSeverity')"></span>
            <input type="number" min="0" max="100" x-model.number="backend.layaMaxSeverity"
                   :placeholder="49">
        </label>
        <p class="setting-hint setting-hint-warning" x-text="$store.i18n.t('classifierLayaMaxSeverityWarning')"></p>
        <label>
            <span x-text="$store.i18n.t('classifierLayaStateChars')"></span>
            <input type="number" min="200" max="8000" x-model.number="backend.layaStateChars"
                   :placeholder="1200">
        </label>
        <label>
            <span x-text="$store.i18n.t('classifierLayaQuestionName')"></span>
            <input type="text" x-model="backend.layaQuestionName" :placeholder="'risk'">
        </label>
        <label>
            <span x-text="$store.i18n.t('classifierLayaInstructions')"></span>
            <input type="text" x-model="backend.layaInstructions">
        </label>
    </div>
</template>
```

`layaCriteria` and `layaSeverityMap` are deliberately not exposed as form fields: they must have identical key sets, which a pair of free-text maps in a form invites getting wrong. They stay editable in `config.json`, and the server rejects a mismatched pair with a named error.

- [ ] **Step 4: Add the strings**

`en.js`:

```js
    classifierBackendFormatLaya: "Laya (local typed decisions)",
    classifierLayaHint: "Answers are computed by a local laya-serve instance at POST /v1/systemone. Start it yourself; the proxy does not manage the process.",
    classifierLayaMaxSeverity: "Maximum Severity",
    classifierLayaMaxSeverityWarning: "Severity 50 and above blocks the action. The default of 49 means a Laya verdict can never block. Raising it past 49 makes a model that scores near chance zero-shot able to block your own commands.",
    classifierLayaStateChars: "Action Characters Sent",
    classifierLayaQuestionName: "Question Name",
    classifierLayaInstructions: "Question Instructions",
```

`pt.js`:

```js
    classifierBackendFormatLaya: "Laya (decisões tipadas locais)",
    classifierLayaHint: "As respostas são calculadas por uma instância local do laya-serve em POST /v1/systemone. Inicie-a você mesmo; o proxy não gerencia esse processo.",
    classifierLayaMaxSeverity: "Severidade Máxima",
    classifierLayaMaxSeverityWarning: "Severidade 50 ou acima bloqueia a ação. O padrão 49 significa que um veredito do Laya nunca bloqueia. Elevar acima de 49 permite que um modelo com desempenho próximo do acaso bloqueie seus próprios comandos.",
    classifierLayaStateChars: "Caracteres da Ação Enviados",
    classifierLayaQuestionName: "Nome da Pergunta",
    classifierLayaInstructions: "Instruções da Pergunta",
```

- [ ] **Step 5: Verify the key sets match**

Run: `grep -c "classifierLaya\|classifierBackendFormatLaya" internal/webui/public/js/translations/en.js internal/webui/public/js/translations/pt.js`
Expected: the same count, 7, in both files.

- [ ] **Step 6: Verify in the browser**

Start `laya-serve` locally, add a backend with format `laya` pointing at `http://127.0.0.1:8000/v1/systemone`, point a stage 1 rule at it, then run a bash action in Claude Code and confirm the audit feed shows a `rerouted` event.

- [ ] **Step 7: Commit**

```bash
git add internal/webui/public/js/components/classifier-config.js internal/webui/public/views/settings.html internal/webui/public/js/translations/en.js internal/webui/public/js/translations/pt.js
git commit -m "feat(webui): laya backend format and its overrides"
```

---

## Task 14: Operator documentation

**Files:**
- Modify: `docs/classifier-rules.md`

- [ ] **Step 1: Append the two sections**

Add to `docs/classifier-rules.md`:

```markdown
## Corpus capture

Enable `classifier.capture` to write one JSONL row per classifier request to
`<configDir>/corpus/classifier-<date>.jsonl`, mode 0600. Each row holds the
graded action, the surrounding context entries, hashes of the system and
footer blocks, and the verdict that was returned.

Labels only exist when classifier requests actually reach upstream. During a
collection window, set rules to `passthrough` and leave the built-in stub
inactive — that consumes upstream quota, which is the cost of collecting.
Rows captured while stubbing carry `source: "stub"` and hold no label.

Convert a corpus into a fine-tune dataset:

    python3 scripts/corpus_to_laya.py ~/.config/antigravity-proxy/corpus/*.jsonl -o train.jsonl

The script reads `source: "upstream"` rows only. Rows the local model produced
itself (`source: "laya"`) are excluded so it never trains on its own answers.

## Laya backend

    pip install "laya[serve]"
    LAYA_MODELS=english LAYA_PRELOAD=1 LAYA_HOST=127.0.0.1 LAYA_PORT=8000 laya-serve

Then add a backend with `"format": "laya"` and
`"url": "http://127.0.0.1:8000/v1/systemone"`. Set `apiKey` only if the server
was started with `LAYA_API_KEY`.

The adapter sends the graded action as a single `choice` question over four
risk buckets and maps the chosen label to a severity. Every default severity
is below 50, the allow/block boundary, and `layaMaxSeverity` (default 49)
clamps the result — a Laya verdict cannot block. The base checkpoints score
near chance on typed decisions zero-shot, so treat the verdict as a locally
computed, plausible allow until a fine-tuned checkpoint and a measured
agreement rate exist.

If laya-serve is unreachable, returns an error, or answers with a label the
severity map does not contain, the reroute fails and the request falls through
to the proxy's built-in handling.
```

- [ ] **Step 2: Commit**

```bash
git add docs/classifier-rules.md
git commit -m "docs: corpus capture and laya backend operator notes"
```

---

## Verification

Run once all tasks are complete:

```bash
go build ./...
go test ./...
python3 -m pytest scripts/test_corpus_to_laya.py -q
```

Expected: the Go suite passes with no new failures against the pre-change baseline, and 7 Python tests pass.
