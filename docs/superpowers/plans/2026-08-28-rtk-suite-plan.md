# Headroom CommandCrusher Stage (RTK Suite) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a native Go `CommandCrusherStage` to the Headroom pipeline that compresses test-runner, linter, compiler, and git `tool_result` payloads by 40–80% with no subprocess calls.

**Architecture:** A signature-based pattern engine. The stage walks every `tool_result` text payload (reusing `walkToolResultText`), skips verbatim payloads and `is_error` payloads, detects the tool-output format from content signatures, and runs a pure, line-based filter function. Unrecognized payloads pass through unchanged. The stage registers between `CCRStage` and `SmartCrusherStage`.

**Tech Stack:** Go stdlib only (`strings`, `regexp` precompiled at package level). No new dependencies.

**Spec:** `docs/superpowers/specs/2026-08-28-headroom-command-crusher-design.md`

> **Plan-mode note:** this plan lives in the plan file during review. On execution start, copy it to `docs/superpowers/plans/2026-08-28-headroom-command-crusher-plan.md` and commit it there.

## Global Constraints

- Invariant I1: all transformations pure, deterministic, position-independent. No wall clock, no randomness, no request-global state leaking into output.
- Invariant I3: mutate only `tool_result` content text blocks. Never touch user prompts, assistant text, thinking blocks, tool_use inputs.
- Invariant I4: `skipVerbatim(reqCtx, cfg, ord)` guard before any mutation; verbatim payloads byte-for-byte.
- `is_error: true` tool_result payloads pass through unchanged (spec §4 step 2).
- `Filter(Filter(x)) == Filter(x)` — every filter idempotent.
- Fallback: no signature match → return input unchanged.
- Latency: <0.2ms per 100KB payload; single `strings.Builder` allocation per crushed payload; regexes precompiled at package level, none in per-line hot loops where a prefix/contains check suffices.
- Opt-in toggle `Config.CommandCrusher bool` (`json:"commandCrusher,omitempty"`), default off.
- Existing code patterns to reuse (do not reimplement):
  - `walkToolResultText(req, 0, fn)` — `internal/headroom/walk.go:13`
  - `skipVerbatim(reqCtx, cfg, ord)` — `internal/headroom/verbatim.go:169`
  - `reqCtx.RecordRewrite(before, after)` — `internal/headroom/types.go:81`
  - Ordinal accounting via `countTextPayloads` + `walkMessages` — pattern at `internal/headroom/verbatim.go:122-145`
  - `(string, bool)` return idiom from `CompactJSON` — `internal/headroom/smart_crusher.go:18`

## File Structure

- Modify: `internal/headroom/types.go` — add `CommandCrusher bool` field to `Config`.
- Modify: `internal/headroom/engine.go` — register `&CommandCrusherStage{}` after `NewCCRStage(store)`.
- Create: `internal/headroom/command_crusher.go` — stage, `errorOrdinals`, signature detection, `CrushCommandOutput` dispatcher, shared line helpers. (Spec names this one file for stage + parsers; parsers split into per-language files below to keep files focused — flag to reviewer if consolidation preferred.)
- Create: `internal/headroom/crusher_python.go` — pytest, unittest, ruff filters.
- Create: `internal/headroom/crusher_js.go` — jest/vitest, mocha, tsc, eslint filters.
- Create: `internal/headroom/crusher_gorust.go` — go test, golangci-lint, cargo test, cargo build/clippy filters.
- Create: `internal/headroom/crusher_git.go` — git status, git log filters.
- Create: `internal/headroom/command_crusher_test.go` — unit, invariant, idempotency, benchmark tests.
- Modify: `internal/headroom/engine_test.go` — pipeline-order and toggle tests.
- Out of scope (follow-up): WebUI toggle in `internal/webui/public/js/components/server-config.js` + `settings.html`. Config flows automatically via `internal/config/config.go:16` type alias, so JSON config works without UI.

## Spec deviations (recorded, reviewable)

1. **Ruff "group warnings by rule code"** → v1 implements dedupe of identical diagnostic lines only. Grouping reorders diagnostics and separates them from file context; dedupe captures most savings and stays trivially deterministic.
2. **Git log "strip author/date if repetitive"** → spec also says retain author. v1 drops `Date:` lines only; author lines always kept.
3. **Parser files split** per language group instead of one `command_crusher.go` (see File Structure).

---

### Task 1: Config toggle, stage skeleton, engine registration

**Files:**
- Modify: `internal/headroom/types.go` (Config struct, line 30-46)
- Create: `internal/headroom/command_crusher.go`
- Modify: `internal/headroom/engine.go:23-28`
- Test: `internal/headroom/engine_test.go`

**Interfaces:**
- Produces: `CommandCrusherStage` (implements `Stage`), `Config.CommandCrusher bool`, `CrushCommandOutput(text string) (string, bool)` (stub returns unchanged — later tasks fill in).

- [ ] **Step 1: Write failing integration test** in `engine_test.go`:

```go
func TestEngine_CommandCrusherRunsBeforeSmartCrusher(t *testing.T) {
	cfg := fullConfig()
	cfg.CommandCrusher = true
	engine := NewEngine(cfg)
	// A payload that is BOTH pytest output and invalid for JSON compaction:
	// crusher must strip the progress line, smart crusher must leave it alone.
	payload := "collected 2 items\n\ntest_a.py .. [100%]\n\n=== 2 passed in 0.01s ==="
	req := map[string]any{"messages": []any{toolResultMsg(payload)}}

	reqCtx, err := engine.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if strings.Contains(got, "[100%]") {
		t.Errorf("expected progress line stripped, got %q", got)
	}
	if !strings.Contains(got, "=== 2 passed in 0.01s ===") {
		t.Errorf("expected summary retained, got %q", got)
	}
	if reqCtx.BytesAfter >= reqCtx.BytesBefore {
		t.Errorf("expected savings, before=%d after=%d", reqCtx.BytesBefore, reqCtx.BytesAfter)
	}
}

func TestEngine_CommandCrusherDisabledByDefault(t *testing.T) {
	engine := NewEngine(fullConfig()) // fullConfig does not set CommandCrusher
	payload := "collected 2 items\n\ntest_a.py .. [100%]\n\n=== 2 passed in 0.01s ==="
	req := map[string]any{"messages": []any{toolResultMsg(payload)}}

	if _, err := engine.Process(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if got != payload {
		t.Errorf("disabled stage must not modify payload, got %q", got)
	}
}
```

- [ ] **Step 2: Run test, verify fail**

Run: `go test ./internal/headroom/ -run TestEngine_CommandCrusher -v`
Expected: FAIL — `unknown field 'CommandCrusher' in struct literal` / payload unchanged.

- [ ] **Step 3: Implement**

`types.go` — add to `Config`, first field after `Enabled`:

```go
	CommandCrusher bool `json:"commandCrusher,omitempty"`
```

`command_crusher.go` — stage + dispatcher stub:

```go
package headroom

import (
	"context"
	"strings"
)

type CommandCrusherStage struct{}

func (s *CommandCrusherStage) Name() string { return "command_crusher" }

// CrushCommandOutput detects a known tool-output signature and compresses it.
// Pure and deterministic (invariant I1); input returned unchanged when no
// signature matches or the filter finds nothing to strip.
func CrushCommandOutput(text string) (string, bool) {
	switch detectSignature(text) {
	case sigPytest:
		return crushPytest(text)
	case sigUnittest:
		return crushUnittest(text)
	case sigRuff:
		return crushRuff(text)
	case sigJest:
		return crushJest(text)
	case sigMocha:
		return crushMocha(text)
	case sigTSC:
		return crushTSC(text)
	case sigESLint:
		return crushESLint(text)
	case sigGoTest:
		return crushGoTest(text)
	case sigGolangci:
		return crushGolangci(text)
	case sigCargoTest:
		return crushCargoTest(text)
	case sigCargoBuild:
		return crushCargoBuild(text)
	case sigGitStatus:
		return crushGitStatus(text)
	case sigGitLog:
		return crushGitLog(text)
	}
	return text, false
}

func (s *CommandCrusherStage) Execute(ctx context.Context, reqCtx *RequestContext, cfg *Config) error {
	if !cfg.CommandCrusher {
		return nil
	}
	errOrds := errorOrdinals(reqCtx.Request)
	// from=0: history included. Position independence keeps the provider
	// prompt cache warm across turns (invariant I1).
	walkToolResultText(reqCtx.Request, 0, func(_, ord int, get func() string, set func(string)) {
		if errOrds[ord] {
			return // spec §4: is_error payloads pass through unchanged
		}
		if skipVerbatim(reqCtx, cfg, ord) {
			return
		}
		before := get()
		if after, changed := CrushCommandOutput(before); changed {
			set(after)
			reqCtx.RecordRewrite(before, after)
		}
	})
	return nil
}

// errorOrdinals returns the document-order ordinals of every text payload
// inside an is_error tool_result block, using the same ordinal accounting as
// walkToolResultText so the sets line up.
func errorOrdinals(req map[string]any) map[int]bool {
	var errs map[int]bool
	ord := 0
	walkMessages(req, func(msg map[string]any) {
		blocks, ok := msg["content"].([]any)
		if !ok {
			return
		}
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if !ok || block["type"] != "tool_result" {
				continue
			}
			n := countTextPayloads(block)
			if isErr, _ := block["is_error"].(bool); isErr {
				if errs == nil {
					errs = make(map[int]bool)
				}
				for j := ord; j < ord+n; j++ {
					errs[j] = true
				}
			}
			ord += n
		}
	})
	return errs
}

// signature identifies a tool-output format. Detection order in
// detectSignature is most-specific first.
type signature int

const (
	sigNone signature = iota
	sigGitStatus
	sigGitLog
	sigCargoTest
	sigGoTest
	sigGolangci
	sigPytest
	sigUnittest
	sigJest
	sigMocha
	sigTSC
	sigESLint
	sigRuff
	sigCargoBuild
)

// signatureScanCap bounds detection to the payload prefix; signatures are
// decided by header/footer markers visible early (or a capped full scan for
// footer markers — detectSignature may scan the whole text with Contains,
// which is SIMD-fast, but never with regex).
const signatureScanCap = 4096

func detectSignature(text string) signature {
	head := text
	if len(head) > signatureScanCap {
		head = head[:signatureScanCap]
	}
	switch {
	case strings.HasPrefix(head, "On branch ") || strings.Contains(head, "\nOn branch "):
		return sigGitStatus
	case hasCommitLine(head):
		return sigGitLog
	case strings.Contains(text, "test result:"):
		return sigCargoTest
	case strings.Contains(head, "=== RUN") || strings.Contains(head, "--- FAIL:") ||
		strings.Contains(head, "--- PASS:") || strings.Contains(text, "\nok  \t") || strings.Contains(text, "\nFAIL\t"):
		return sigGoTest
	case golangciLineRe.MatchString(head):
		return sigGolangci
	case strings.Contains(head, "collected ") && strings.Contains(head, " items") ||
		pytestFooterRe.MatchString(text) || strings.Contains(text, "short test summary info"):
		return sigPytest
	case strings.Contains(text, "FAILED (") || strings.Contains(text, "\nRan ") && strings.Contains(text, " tests"):
		return sigUnittest
	case strings.Contains(head, "PASS ") || strings.Contains(head, "FAIL ") || strings.Contains(text, "Tests: "):
		return sigJest
	case strings.Contains(text, " passing") && (strings.Contains(head, "✓") || strings.Contains(head, "✔")):
		return sigMocha
	case strings.Contains(head, "error TS"):
		return sigTSC
	case eslintLineRe.MatchString(head):
		return sigESLint
	case ruffLineRe.MatchString(head):
		return sigRuff
	case strings.HasPrefix(head, "   Compiling ") || strings.Contains(head, "\n   Compiling ") ||
		strings.HasPrefix(head, "    Updating ") || strings.Contains(head, "\n    Updating "):
		return sigCargoBuild
	}
	return sigNone
}

// hasCommitLine reports whether head contains a `commit <40-hex>` line.
func hasCommitLine(head string) bool {
	for _, line := range strings.Split(head, "\n") {
		if !strings.HasPrefix(line, "commit ") {
			continue
		}
		hash := strings.TrimSpace(strings.TrimPrefix(line, "commit "))
		if len(hash) == 40 && isLowerHex(hash) {
			return true
		}
	}
	return false
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// filterLines rebuilds text from the lines keep returns true for, preserving
// the original line endings. Returns (input, false) when nothing changed so
// callers can skip the mutation and byte accounting.
func filterLines(text string, keep func(line string) bool) (string, bool) {
	var b strings.Builder
	b.Grow(len(text))
	changed := false
	start := 0
	for i := 0; i <= len(text); i++ {
		if i < len(text) && text[i] != '\n' {
			continue
		}
		line := text[start:i]
		if keep(line) {
			b.WriteString(line)
			if i < len(text) {
				b.WriteByte('\n')
			}
		} else {
			changed = true
		}
		start = i + 1
	}
	if !changed {
		return text, false
	}
	return b.String(), true
}

// dedupeLines keeps the first occurrence of every line, preserving order.
// Idempotent by construction.
func dedupeLines(text string) (string, bool) {
	seen := make(map[string]struct{})
	return filterLines(text, func(line string) bool {
		if _, dup := seen[line]; dup && strings.TrimSpace(line) != "" {
			return false
		}
		seen[line] = struct{}{}
		return true
	})
}
```

Note: `golangciLineRe`, `pytestFooterRe`, `eslintLineRe`, `ruffLineRe` are defined in the per-language files (Tasks 3–5); for this task's compile, add temporary package-level stubs in `command_crusher.go` bottom... NO — cleaner: this task ships `detectSignature` + helpers + `errorOrdinals` + stage; stub filter functions returning `(text, false)` in the per-language files created as empty shells in this task. Each later task replaces stubs with real implementations via TDD.

Create `crusher_python.go`, `crusher_js.go`, `crusher_gorust.go`, `crusher_git.go` with stubs, e.g.:

```go
package headroom

func crushPytest(text string) (string, bool)   { return text, false }
func crushUnittest(text string) (string, bool) { return text, false }
func crushRuff(text string) (string, bool)     { return text, false }
```

and in `command_crusher.go` add the regexes (they belong with detection, so keep them here):

```go
import "regexp"

var (
	// pytestFooterRe matches the pytest summary banner: "=== 2 passed in 0.01s ===".
	pytestFooterRe = regexp.MustCompile(`=+ .* in [\d.]+s =+`)
	// eslintLineRe matches "  12:5  error  'x' is unused  no-unused-vars".
	eslintLineRe = regexp.MustCompile(`(?m)^\s*\d+:\d+\s+(error|warning)\s+\S.*\s[a-z@/][a-z0-9@/\-]*$`)
	// golangciLineRe matches "main.go:12:3: message (lintername)".
	golangciLineRe = regexp.MustCompile(`(?m)^[\w./\-]+\.go:\d+:\d+: .+ \([a-z][a-z0-9\-]*\)$`)
	// ruffLineRe matches "app.py:4:1: E402 module level import not at top".
	ruffLineRe = regexp.MustCompile(`(?m)^\S+\.py:\d+:\d+: [A-Z]+\d+ `)
)
```

`engine.go:23-28` — register between CCR and SmartCrusher:

```go
		pipeline: NewPipeline(
			NewCCRStage(store),
			&CommandCrusherStage{},
			&SmartCrusherStage{},
			&CodeCompressorStage{},
			&OutputShaperStage{},
		),
```

- [ ] **Step 4: Run tests, verify pass**

Run: `go test ./internal/headroom/ -run TestEngine_CommandCrusher -v` — stub filters make the first test FAIL (nothing stripped). Expected: `TestEngine_CommandCrusherDisabledByDefault` PASS, `...RunsBeforeSmartCrusher` FAIL until Task 3. Acceptable: mark first test with `t.Skip("pending pytest filter — Task 3")` in this task, remove skip in Task 3. Alternative: keep only the disabled test here. **Decision: ship only `TestEngine_CommandCrusherDisabledByDefault` in Task 1; move the ordering test to Task 3 with the pytest filter.**

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/types.go internal/headroom/engine.go internal/headroom/command_crusher.go internal/headroom/crusher_*.go internal/headroom/engine_test.go
git commit -m "feat(headroom): add CommandCrusher stage skeleton with signature detection"
```

---

### Task 2: Stage guards — is_error skip, verbatim skip, I3 isolation

**Files:**
- Test: `internal/headroom/command_crusher_test.go`

**Interfaces:**
- Consumes: `errorOrdinals` (Task 1), `skipVerbatim`, `CrushCommandOutput`.
- Produces: nothing new; locks stage guard behavior in tests before filters land.

- [ ] **Step 1: Write failing tests** in `command_crusher_test.go`:

```go
package headroom

import (
	"context"
	"strings"
	"testing"
)

func crusherConfig() Config {
	return Config{Enabled: true, CommandCrusher: true, PreserveVerbatimReads: true}
}

func TestCommandCrusher_IsErrorUntouched(t *testing.T) {
	payload := "collected 1 items\n\ntest_a.py F [100%]\n\n=== 1 failed in 0.01s ==="
	req := map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "is_error": true, "content": payload},
	}}}}

	reqCtx := &RequestContext{Request: req}
	if err := (&CommandCrusherStage{}).Execute(context.Background(), reqCtx, &Config{Enabled: true, CommandCrusher: true}); err != nil {
		t.Fatal(err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if got != payload {
		t.Errorf("is_error payload must pass through byte-for-byte, got %q", got)
	}
}

func TestCommandCrusher_VerbatimSkipped(t *testing.T) {
	// cat -n numbered source that happens to contain a go-test-looking line.
	payload := "     1\tpackage main\n     2\t// === RUN fake\n     3\tfunc main() {}\n"
	req := map[string]any{"messages": []any{toolResultMsg(payload)}}
	cfg := crusherConfig()
	reqCtx := &RequestContext{Request: req, Verbatim: NewToolInspector(req)}

	if err := (&CommandCrusherStage{}).Execute(context.Background(), reqCtx, &cfg); err != nil {
		t.Fatal(err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if got != payload {
		t.Errorf("verbatim payload corrupted, got %q", got)
	}
	if reqCtx.VerbatimSkipped != 1 {
		t.Errorf("expected VerbatimSkipped=1, got %d", reqCtx.VerbatimSkipped)
	}
}

func TestCommandCrusher_I3_AssistantTextUntouched(t *testing.T) {
	assistantText := "collected 1 items\n=== 1 passed in 0.01s ==="
	req := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": assistantText},
		}},
		toolResultMsg("collected 1 items\n\ntest_a.py . [100%]\n\n=== 1 passed in 0.01s ==="),
	}}
	cfg := crusherConfig()
	reqCtx := &RequestContext{Request: req}

	if err := (&CommandCrusherStage{}).Execute(context.Background(), reqCtx, &cfg); err != nil {
		t.Fatal(err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if got != assistantText {
		t.Errorf("assistant text mutated (I3 violation), got %q", got)
	}
}

func TestErrorOrdinals_MixedBlocks(t *testing.T) {
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "a", "is_error": true, "content": "err"},
			map[string]any{"type": "tool_result", "tool_use_id": "b", "content": []any{
				map[string]any{"type": "text", "text": "one"},
				map[string]any{"type": "text", "text": "two"},
			}},
		}},
	}}
	errs := errorOrdinals(req)
	if !errs[0] || errs[1] || errs[2] {
		t.Errorf("expected only ordinal 0 marked, got %v", errs)
	}
}
```

- [ ] **Step 2: Run, verify fail/pass mix**

Run: `go test ./internal/headroom/ -run 'TestCommandCrusher_|TestErrorOrdinals' -v`
Expected: all compile and PASS against Task 1 code except none — guards already implemented in Task 1 stage. If any fail, fix stage before proceeding. (This task is characterization-first: the guards were written in Task 1; tests here lock them. If strict RED required, write these tests in Task 1 Step 1 alongside. Either way, do not proceed to Task 3 until these pass.)

- [ ] **Step 3: Commit**

```bash
git add internal/headroom/command_crusher_test.go
git commit -m "test(headroom): lock CommandCrusher is_error, verbatim, and I3 guards"
```

---

### Task 3: Python filters (pytest, unittest, ruff)

**Files:**
- Modify: `internal/headroom/crusher_python.go`
- Test: `internal/headroom/command_crusher_test.go`

**Interfaces:**
- Consumes: `filterLines`, `dedupeLines` (Task 1).
- Produces: `crushPytest`, `crushUnittest`, `crushRuff` — all `(string) (string, bool)`, pure, idempotent.

- [ ] **Step 1: Write failing tests**:

```go
func TestPytestFilter(t *testing.T) {
	input := `collected 3 items

test_calc.py ..F [100%]

=================================== FAILURES ===================================
______________________________ test_add ______________________________

    def test_add():
>       assert add(1, 1) == 3
E       AssertionError: assert 2 == 3

test_calc.py:10: AssertionError
=========================== short test summary info ============================
FAILED test_calc.py::test_add - AssertionError: assert 2 == 3
=== 1 failed, 2 passed in 0.12s ===`
	got, changed := crushPytest(input)
	if !changed {
		t.Fatal("expected change")
	}
	for _, want := range []string{"AssertionError: assert 2 == 3", "FAILED test_calc.py::test_add", "=== 1 failed, 2 passed in 0.12s ==="} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
	if strings.Contains(got, "[100%]") {
		t.Errorf("progress line not stripped:\n%s", got)
	}
}

func TestPytestFilter_AllPass(t *testing.T) {
	input := "collected 50 items\n\ntest_a.py ............................................ [ 50%]\n\ntest_b.py  [100%]\n\n=== 50 passed in 1.40s ==="
	got, changed := crushPytest(input)
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Contains(got, "....") {
		t.Errorf("dot progress lines survive:\n%s", got)
	}
	if !strings.Contains(got, "=== 50 passed in 1.40s ===") {
		t.Errorf("summary lost:\n%s", got)
	}
}

func TestUnittestFilter(t *testing.T) {
	input := "...\n...\nF..\n======================================================================\nFAIL: test_add (test_calc.TestCalc)\n----------------------------------------------------------------------\nTraceback (most recent call last):\n  File \"test_calc.py\", line 10, in test_add\n    self.assertEqual(add(1, 1), 3)\nAssertionError: 2 != 3\n\n----------------------------------------------------------------------\nRan 9 tests in 0.002s\n\nFAILED (failures=1)\n"
	got, changed := crushUnittest(input)
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Contains(got, "...\n...") {
		t.Errorf("dot-only lines survive:\n%q", got)
	}
	if !strings.Contains(got, "F..") || !strings.Contains(got, "FAILED (failures=1)") || !strings.Contains(got, "Traceback") {
		t.Errorf("failure evidence lost:\n%q", got)
	}
}

func TestRuffFilter_Dedupes(t *testing.T) {
	input := "a.py:4:1: E402 module level import not at top\nb.py:9:1: E402 module level import not at top\na.py:4:1: E402 module level import not at top\nFound 3 errors."
	got, changed := crushRuff(input)
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Count(got, "a.py:4:1: E402") != 1 {
		t.Errorf("duplicate violation survives:\n%q", got)
	}
	if !strings.Contains(got, "b.py:9:1: E402") || !strings.Contains(got, "Found 3 errors.") {
		t.Errorf("unique lines lost:\n%q", got)
	}
}
```

Also unskip/add `TestEngine_CommandCrusherRunsBeforeSmartCrusher` (code in Task 1 Step 1, minus the skip).

- [ ] **Step 2: Run, verify fail**

Run: `go test ./internal/headroom/ -run 'TestPytest|TestUnittest|TestRuff' -v`
Expected: FAIL — stubs return unchanged.

- [ ] **Step 3: Implement** `crusher_python.go`:

```go
package headroom

import (
	"regexp"
	"strings"
)

// pytestProgressRe matches progress lines: "test_a.py ..F [100%]", bare
// dot runs "..........", and "....  [ 50%]".
var pytestProgressRe = regexp.MustCompile(`^\S*\.py\s+[.sFxXeE]+\s*\[\s*\d+%\]$|^[.sFxX]+\s*(\[\s*\d+%\])?$`)

// crushPytest strips progress/dot lines and PASSED short-summary lines.
// FAILURES/ERRORS sections, tracebacks, E-lines, and banner lines survive
// untouched.
func crushPytest(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		if pytestProgressRe.MatchString(line) {
			return false
		}
		if strings.HasPrefix(line, "PASSED ") {
			return false
		}
		return true
	})
}

// unittestDotRe matches a pure pass-progress line: dots only, no F/E.
var unittestDotRe = regexp.MustCompile(`^[.s]+$`)

// crushUnittest drops pure-dot progress lines. Lines containing F or E carry
// failure signal and stay, as do tracebacks and the FAILED/OK footer.
func crushUnittest(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		return !unittestDotRe.MatchString(line)
	})
}

// crushRuff dedupes identical violation lines (stable, first occurrence wins).
func crushRuff(text string) (string, bool) {
	return dedupeLines(text)
}
```

Wait — pytest progress lines containing `F` (e.g. `test_calc.py ..F [100%]`) carry signal: dropping loses the info that a test failed, but the FAILURES section retains full detail, so acceptable — spec says strip progress percentages. Keep as written; test `TestPytestFilter` asserts `AssertionError` retained.

- [ ] **Step 4: Run, verify pass**

Run: `go test ./internal/headroom/ -run 'TestPytest|TestUnittest|TestRuff|TestEngine_CommandCrusher' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/crusher_python.go internal/headroom/command_crusher_test.go internal/headroom/engine_test.go
git commit -m "feat(headroom): add pytest, unittest, and ruff crushers"
```

---

### Task 4: JS/TS filters (jest/vitest, mocha, tsc, eslint)

**Files:**
- Modify: `internal/headroom/crusher_js.go`
- Test: `internal/headroom/command_crusher_test.go`

**Interfaces:**
- Produces: `crushJest`, `crushMocha`, `crushTSC`, `crushESLint` — same `(string) (string, bool)` contract.

- [ ] **Step 1: Write failing tests**:

```go
func TestJestFilter(t *testing.T) {
	input := `PASS src/add.test.ts (12ms)
✓ adds numbers (2ms)
✓ subtracts numbers (1ms)
FAIL src/div.test.ts
✕ divides by zero (3ms)

● divides by zero

expect(received).toBe(expected)

Expected: Infinity
Received: NaN

Tests:       1 failed, 45 passed, 46 total
Snapshots:   0 total
Time:        1.234s`
	got, changed := crushJest(input)
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Contains(got, "✓") || strings.Contains(got, "PASS ") {
		t.Errorf("passing lines survive:\n%s", got)
	}
	for _, want := range []string{"✕ divides by zero", "FAIL src/div.test.ts", "Expected: Infinity", "Received: NaN", "Tests:       1 failed, 45 passed, 46 total"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

func TestMochaFilter(t *testing.T) {
	input := "  Calculator\n    ✓ adds\n    ✓ subtracts\n    1) divides by zero\n\n\n  2 passing (5ms)\n  1 failing\n\n  1) Calculator\n       divides by zero:\n     Error: boom\n      at Context.<anonymous> (test.js:10:5)\n"
	got, changed := crushMocha(input)
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Contains(got, "✓") {
		t.Errorf("checkmarks survive:\n%q", got)
	}
	if !strings.Contains(got, "2 passing (5ms)") || !strings.Contains(got, "Error: boom") {
		t.Errorf("failure evidence lost:\n%q", got)
	}
}

func TestTypeScriptCompilerFilter(t *testing.T) {
	input := "src/a.ts(12,5): error TS2322: Type 'string' is not assignable to type 'number'.\nsrc/a.ts(12,5): error TS2322: Type 'string' is not assignable to type 'number'.\nsrc/b.ts(3,1): error TS2304: Cannot find name 'foo'."
	got, changed := crushTSC(input)
	if !changed {
		t.Fatal("expected dedupe change")
	}
	if strings.Count(got, "error TS2322") != 1 || !strings.Contains(got, "error TS2304") {
		t.Errorf("bad tsc output:\n%q", got)
	}
}

func TestESLintFilter(t *testing.T) {
	input := "/app/src/a.ts\n  1:5  error  'x' is defined but never used  no-unused-vars\n  1:5  error  'x' is defined but never used  no-unused-vars\n  2:9  warning  Unexpected console statement  no-console\n\n✖ 3 problems (2 errors, 1 warning)"
	got, changed := crushESLint(input)
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Count(got, "no-unused-vars") != 1 {
		t.Errorf("duplicate eslint line survives:\n%q", got)
	}
	if !strings.Contains(got, "no-console") || !strings.Contains(got, "✖ 3 problems") {
		t.Errorf("unique lines lost:\n%q", got)
	}
}
```

- [ ] **Step 2: Run, verify fail**

Run: `go test ./internal/headroom/ -run 'TestJest|TestMocha|TestTypeScript|TestESLint' -v`
Expected: FAIL.

- [ ] **Step 3: Implement** `crusher_js.go`:

```go
package headroom

import "strings"

// crushJest strips passing suites ("PASS ") and green checkmark lines. It
// covers vitest, which emits the same ✓/✕ and Tests: summary shapes.
func crushJest(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "✓") || strings.HasPrefix(trimmed, "√") {
			return false
		}
		if strings.HasPrefix(trimmed, "PASS ") {
			return false
		}
		return true
	})
}

// crushMocha strips passing checkmark items; keeps the numbered failure
// items, error frames, and the passing/failing footer.
func crushMocha(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		trimmed := strings.TrimLeft(line, " \t")
		return !strings.HasPrefix(trimmed, "✓") && !strings.HasPrefix(trimmed, "✔")
	})
}

// crushTSC dedupes identical compiler diagnostics (repeating import errors).
func crushTSC(text string) (string, bool) {
	return dedupeLines(text)
}

// crushESLint dedupes identical rule violations; errors, unique warnings, and
// the ✖ summary survive.
func crushESLint(text string) (string, bool) {
	return dedupeLines(text)
}
```

- [ ] **Step 4: Run, verify pass**

Run: `go test ./internal/headroom/ -run 'TestJest|TestMocha|TestTypeScript|TestESLint' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/crusher_js.go internal/headroom/command_crusher_test.go
git commit -m "feat(headroom): add jest, mocha, tsc, and eslint crushers"
```

---

### Task 5: Go & Rust filters (go test, golangci-lint, cargo test, cargo build/clippy)

**Files:**
- Modify: `internal/headroom/crusher_gorust.go`
- Test: `internal/headroom/command_crusher_test.go`

**Interfaces:**
- Produces: `crushGoTest`, `crushGolangci`, `crushCargoTest`, `crushCargoBuild`.

- [ ] **Step 1: Write failing tests**:

```go
func TestGoTestFilter(t *testing.T) {
	input := "=== RUN   TestAdd\n--- PASS: TestAdd (0.00s)\n=== RUN   TestDiv\n--- FAIL: TestDiv (0.00s)\n    div_test.go:10: got NaN, want Inf\n=== RUN   TestMul/Sub\n    --- PASS: TestMul/Sub (0.00s)\nFAIL\nFAIL\texample.com/calc\t0.123s\nok  \texample.com/util\t0.05s"
	got, changed := crushGoTest(input)
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Contains(got, "=== RUN") || strings.Contains(got, "--- PASS:") {
		t.Errorf("pass noise survives:\n%s", got)
	}
	for _, want := range []string{"--- FAIL: TestDiv", "div_test.go:10: got NaN", "FAIL\texample.com/calc\t0.123s", "ok  \texample.com/util\t0.05s"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

func TestGoTestFilter_PanicSurvives(t *testing.T) {
	input := "=== RUN   TestBoom\n--- FAIL: TestBoom (0.00s)\npanic: runtime error: index out of range [3] with length 3\n\ngoroutine 6 [running]:\nexample.com/calc.Boom(...)\nFAIL\texample.com/calc\t0.01s"
	got, changed := crushGoTest(input)
	if !changed {
		t.Fatal("expected change")
	}
	if !strings.Contains(got, "panic: runtime error") || !strings.Contains(got, "goroutine 6") {
		t.Errorf("panic trace lost:\n%s", got)
	}
}

func TestGolangciFilter(t *testing.T) {
	input := "main.go:12:3: printf: fmt.Println arg list ends with redundant newline (govet)\nmain.go:12:3: printf: fmt.Println arg list ends with redundant newline (govet)\nutil.go:40:1: exported function Main should have comment (revive)"
	got, changed := crushGolangci(input)
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Count(got, "govet") != 1 || !strings.Contains(got, "revive") {
		t.Errorf("bad golangci output:\n%q", got)
	}
}

func TestCargoTestFilter(t *testing.T) {
	input := "   Compiling calc v0.1.0\n    Finished test [unoptimized + debuginfo] target(s) in 0.5s\n     Running unittests src/lib.rs\n\nrunning 3 tests\ntest tests::test_add ... ok\ntest tests::test_sub ... ok\ntest tests::test_div ... FAILED\n\nfailures:\n\n---- tests::test_div stdout ----\nthread 'tests::test_div' panicked at 'division by zero', src/lib.rs:10:5\n\nfailures:\n    tests::test_div\n\ntest result: FAILED. 1 failed; 2 passed; 0 ignored; finished in 0.00s"
	got, changed := crushCargoTest(input)
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Contains(got, "... ok") {
		t.Errorf("passing tests survive:\n%s", got)
	}
	for _, want := range []string{"test tests::test_div ... FAILED", "panicked at 'division by zero'", "test result: FAILED. 1 failed; 2 passed"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

func TestCargoBuildFilter(t *testing.T) {
	input := "   Compiling libc v0.2.1\n   Compiling serde v1.0.0\n    Updating crates.io index\nwarning: unused variable: `x`\n --> src/main.rs:2:9\n  |\n2 |     let x = 1;\n  |         ^\nerror[E0308]: mismatched types\n --> src/main.rs:4:5\n    Finished dev [unoptimized + debuginfo] target(s) in 1.2s"
	got, changed := crushCargoBuild(input)
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Contains(got, "Compiling") || strings.Contains(got, "Updating crates.io") {
		t.Errorf("build noise survives:\n%s", got)
	}
	for _, want := range []string{"warning: unused variable", "error[E0308]: mismatched types", "--> src/main.rs:2:9", "Finished dev"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}
```

- [ ] **Step 2: Run, verify fail**

Run: `go test ./internal/headroom/ -run 'TestGoTest|TestGolangci|TestCargo' -v`
Expected: FAIL.

- [ ] **Step 3: Implement** `crusher_gorust.go`:

```go
package headroom

import "strings"

// crushGoTest strips verbose-run scaffolding: "=== RUN" and "--- PASS:" /
// "    --- PASS:" lines. Failures, panics, and package summaries stay.
func crushGoTest(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		if strings.HasPrefix(line, "=== RUN") {
			return false
		}
		trimmed := strings.TrimLeft(line, " \t")
		return !strings.HasPrefix(trimmed, "--- PASS:")
	})
}

// crushGolangci dedupes identical diagnostics.
func crushGolangci(text string) (string, bool) {
	return dedupeLines(text)
}

// crushCargoTest strips "test <name> ... ok" lines. FAILED lines, panics, the
// failures: list, and the test result: footer stay.
func crushCargoTest(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		return !(strings.HasPrefix(line, "test ") && strings.HasSuffix(line, " ... ok"))
	})
}

// crushCargoBuild strips crate compilation noise; warning:/error: diagnostic
// blocks and the Finished line survive. Covers cargo clippy, which shares the
// Compiling/Checking/Updating line shapes.
func crushCargoBuild(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		for _, prefix := range []string{"   Compiling ", "   Downloaded ", "  Downloading ", "    Updating ", "   Checking ", "     Locking "} {
			if strings.HasPrefix(line, prefix) {
				return false
			}
		}
		return true
	})
}
```

- [ ] **Step 4: Run, verify pass**

Run: `go test ./internal/headroom/ -run 'TestGoTest|TestGolangci|TestCargo' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/crusher_gorust.go internal/headroom/command_crusher_test.go
git commit -m "feat(headroom): add go test, golangci-lint, and cargo crushers"
```

---

### Task 6: Git filters (git status, git log)

**Files:**
- Modify: `internal/headroom/crusher_git.go`
- Test: `internal/headroom/command_crusher_test.go`

**Interfaces:**
- Produces: `crushGitStatus`, `crushGitLog`.

- [ ] **Step 1: Write failing tests**:

```go
func TestGitStatusFilter(t *testing.T) {
	input := "On branch main\nYour branch is up to date with 'origin/main'.\n\nChanges not staged for commit:\n  (use \"git add <file>...\" to update what will be committed)\n  (use \"git restore <file>...\" to discard changes in working directory)\n\tmodified:   engine.go\n\nUntracked files:\n  (use \"git add <file>...\" to include in what will be committed)\n\tcommand_crusher.go\n\nno changes added to commit (use \"git add\" and/or \"git commit -a\")"
	got, changed := crushGitStatus(input)
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Contains(got, "(use \"") {
		t.Errorf("hint lines survive:\n%s", got)
	}
	for _, want := range []string{"On branch main", "Your branch is up to date", "modified:   engine.go", "command_crusher.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

func TestGitLogFilter(t *testing.T) {
	input := "commit 545eec4f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d\nAuthor: Gus <g@example.com>\nDate:   Thu Aug 28 10:00:00 2026 -0300\n\n    fix(headroom): safe formatInt\n\ncommit e04c77b0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6\nAuthor: Gus <g@example.com>\nDate:   Thu Aug 28 09:00:00 2026 -0300\n\n    fix(headroom): recreate HTTP client"
	got, changed := crushGitLog(input)
	if !changed {
		t.Fatal("expected change")
	}
	if strings.Contains(got, "Date:") {
		t.Errorf("date boilerplate survives:\n%s", got)
	}
	for _, want := range []string{"commit 545eec4", "Author: Gus <g@example.com>", "fix(headroom): safe formatInt"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}
```

- [ ] **Step 2: Run, verify fail**

Run: `go test ./internal/headroom/ -run 'TestGitStatus|TestGitLog' -v`
Expected: FAIL.

- [ ] **Step 3: Implement** `crusher_git.go`:

```go
package headroom

import "strings"

// crushGitStatus strips git's instructional "(use ..." hints; branch state,
// tracking info, and file lists survive.
func crushGitStatus(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "(use \"") {
			return false
		}
		// The trailing "no changes added to commit" line is also pure hint.
		if strings.HasPrefix(trimmed, "no changes added to commit") {
			return false
		}
		return true
	})
}

// crushGitLog strips Date: boilerplate. Commit hashes, authors, subjects, and
// bodies survive (spec: retain author).
func crushGitLog(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		return !strings.HasPrefix(line, "Date:")
	})
}
```

- [ ] **Step 4: Run, verify pass**

Run: `go test ./internal/headroom/ -run 'TestGitStatus|TestGitLog' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/crusher_git.go internal/headroom/command_crusher_test.go
git commit -m "feat(headroom): add git status and git log crushers"
```

---

### Task 7: Cross-cutting invariants — idempotency, fallback, detection

**Files:**
- Test: `internal/headroom/command_crusher_test.go`

**Interfaces:**
- Consumes: all filters + `CrushCommandOutput` + `detectSignature`.

- [ ] **Step 1: Write tests**:

```go
func TestCrushCommandOutput_Idempotent(t *testing.T) {
	samples := map[string]string{
		"pytest":     "collected 2 items\n\ntest_a.py .. [100%]\n\n=== 2 passed in 0.01s ===",
		"unittest":   "...\nF\nRan 4 tests in 0.01s\n\nFAILED (failures=1)\n",
		"ruff":       "a.py:1:1: E402 x\na.py:1:1: E402 x\n",
		"jest":       "✓ a (1ms)\n✕ b\nTests: 1 failed, 1 passed, 2 total",
		"mocha":      "  ✓ a\n  1 passing (1ms)\n",
		"tsc":        "a.ts(1,1): error TS2322: x\na.ts(1,1): error TS2322: x",
		"eslint":     "  1:1  error  x  no-undef\n  1:1  error  x  no-undef",
		"gotest":     "=== RUN   TestA\n--- PASS: TestA (0.00s)\nok  \tx/y\t0.1s",
		"golangci":   "a.go:1:1: x (govet)\na.go:1:1: x (govet)",
		"cargotest":  "running 1 test\ntest t::a ... ok\n\ntest result: ok. 1 passed",
		"cargobuild": "   Compiling x v1.0.0\n    Finished dev target(s) in 0.1s",
		"gitstatus":  "On branch main\n  (use \"git add <file>...\" to update what will be committed)\n\tmodified:   a.go",
		"gitlog":     "commit 545eec4f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d\nAuthor: A <a@b.c>\nDate:   Thu Aug 28 10:00:00 2026 -0300\n\n    subject",
	}
	for name, sample := range samples {
		once, changed := CrushCommandOutput(sample)
		if !changed {
			t.Errorf("%s: expected first pass to change output", name)
			continue
		}
		twice, changedAgain := CrushCommandOutput(once)
		if changedAgain || twice != once {
			t.Errorf("%s: not idempotent\nfirst:  %q\nsecond: %q", name, once, twice)
		}
	}
}

func TestCrushCommandOutput_FallbackUnchanged(t *testing.T) {
	for _, input := range []string{
		"",
		"hello world",
		"package main\n\nfunc main() {}\n",
		"{\"json\": true}",
		"     1\tline one\n     2\tline two\n     3\tline three\n",
	} {
		got, changed := CrushCommandOutput(input)
		if changed || got != input {
			t.Errorf("fallback mutated input %q -> %q", input, got)
		}
	}
}

func TestDetectSignature_NoFalsePositiveOnSource(t *testing.T) {
	// Go source mentioning test markers in comments/strings must not match
	// unless the shape is real go test output.
	src := "package main\n\n// === RUN is not a test log here\nfunc main() { println(\"ok  \tnot-a-package\") }\n"
	if sig := detectSignature(src); sig != sigGoTest && sig != sigNone {
		t.Errorf("unexpected signature %v", sig)
	}
}
```

Note on `TestDetectSignature_NoFalsePositiveOnSource`: `=== RUN` substring will trigger sigGoTest on this input; crushGoTest then strips that comment line. Acceptable per spec §4 — `ToolInspector`/`skipVerbatim` is the primary guard for file reads, and the stage is opt-in. The test pins detection, not safety; keep expectation loose as written, or tighten to `sigGoTest` explicitly. **Decision: assert `sig == sigGoTest` and add comment explaining the verbatim guard is the real defense.**

- [ ] **Step 2: Run, fix failures**

Run: `go test ./internal/headroom/ -run 'TestCrush|TestDetect' -v`
Expected: idempotency failures surface non-idempotent filters (e.g. dedupe leaves trailing blank-line shifts). Fix filters until green. Known risk spots: `dedupeLines` treats blank lines as always-keep (already handled via TrimSpace check); pytest regex must not match its own retained output — verify `=== 2 passed in 0.01s ===` does not match `pytestProgressRe`.

- [ ] **Step 3: Commit**

```bash
git add internal/headroom/command_crusher_test.go internal/headroom/crusher_*.go
git commit -m "test(headroom): add CommandCrusher idempotency and fallback tests"
```

---

### Task 8: Benchmarks and full gate

**Files:**
- Test: `internal/headroom/command_crusher_test.go` (append benchmarks)

- [ ] **Step 1: Write benchmarks**:

```go
func generatePytestOutput(tests int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("collected %d items\n\n", tests))
	for i := 0; i < tests/10; i++ {
		fmt.Fprintf(&sb, "test_mod_%d.py .......... [%3d%%]\n", i, (i+1)*100/(tests/10))
	}
	sb.WriteString("=== 200 passed in 12.40s ===\n")
	return sb.String()
}

func BenchmarkCommandCrusher_Pytest100KB(b *testing.B) {
	data := generatePytestOutput(10000) // ~100KB
	for len(data) < 100*1024 {
		data += data[:len(data)/2]
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, changed := CrushCommandOutput(data)
		if !changed || len(out) >= len(data) {
			b.Fatal("expected compression")
		}
	}
}

func BenchmarkCommandCrusher_Fallback100KB(b *testing.B) {
	data := strings.Repeat("just an ordinary log line with no signature\n", 2400) // ~100KB
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, changed := CrushCommandOutput(data); changed {
			b.Fatal("unexpected change")
		}
	}
}
```

- [ ] **Step 2: Run benchmarks, check budget**

Run: `go test ./internal/headroom/ -run '^$' -bench 'CommandCrusher' -benchmem -benchtime 100x`
Expected: <0.2ms per 100KB op (≈200µs). Fallback path must be detection-only (no Builder). If over budget: move full-text `strings.Contains(text, ...)` footer checks in `detectSignature` to capped head/tail scans.

- [ ] **Step 3: Full gate**

Run:
```bash
go build ./...
go vet ./...
go test ./...
```
Expected: all PASS. Pay attention to `internal/config`, `internal/api`, `internal/webui` tests that construct `headroom.Config` literals — new field is additive, so they should pass untouched; fix any positional (unkeyed) struct literals if they exist.

- [ ] **Step 4: Commit**

```bash
git add internal/headroom/command_crusher_test.go
git commit -m "test(headroom): add CommandCrusher benchmarks"
```

---

## Self-Review

**Spec coverage:**
- §2.1 pipeline order → Task 1 (engine.go registration). ✓
- §2.2 config toggle → Task 1 (types.go). ✓
- §3.1 pytest/unittest/ruff → Task 3. ✓
- §3.2 jest/vitest/mocha/tsc/eslint → Task 4. ✓ (jest filter covers vitest by shared ✓/✕/Tests: shapes — pinned in comment; add one vitest-shaped sample to TestCrushCommandOutput_Idempotent if reviewer asks.)
- §3.3 go test/golangci-lint/cargo test/cargo build+clippy → Task 5. ✓ (clippy covered via Checking/Locking prefixes in crushCargoBuild.)
- §3.4 git status/git log → Task 6. ✓ (git log scoped to Date: stripping — deviation 2.)
- §4 skipVerbatim, is_error, signature engine, fallback → Tasks 1–2. ✓
- §4 safety: error detail retention → per-filter tests assert failure evidence retained. ✓
- §5 unit tests → Tasks 3–7 (names match spec: TestPytestFilter, TestJestFilter, TestGoTestFilter, TestCargoTestFilter, TestGitStatusFilter, TestTypeScriptCompilerFilter, TestVerbatimSkip → named TestCommandCrusher_VerbatimSkipped, TestIdempotency → TestCrushCommandOutput_Idempotent). ✓
- §5 benchmarks <0.2ms/100KB → Task 8. ✓
- §5 E2E pipeline tests → Tasks 1 + 3 (engine_test.go). ✓

**Deviations from spec (must be acknowledged in PR):** ruff rule-grouping deferred to dedupe-only; git log keeps authors, drops dates only; parsers split across 4 per-language files.

**Type consistency:** all filters `(string) (string, bool)`; dispatcher `CrushCommandOutput(text string) (string, bool)`; stage name `"command_crusher"`; config field `CommandCrusher bool json:"commandCrusher,omitempty"`. Consistent across tasks.

## Verification (end-to-end)

1. `go build ./... && go vet ./...`
2. `go test ./internal/headroom/ -v` — all filter, guard, idempotency, fallback tests green.
3. `go test ./...` — repo-wide green (config/api/webui untouched by additive field).
4. `go test ./internal/headroom/ -run '^$' -bench CommandCrusher -benchmem` — <200µs/op at 100KB.
5. Manual smoke: run proxy with `"headroom": {"enabled": true, "commandCrusher": true}` in config.json, run a session that executes `pytest`/`go test`, confirm tool_result payload arrives crushed at the provider and `BytesBefore > BytesAfter` in telemetry.
