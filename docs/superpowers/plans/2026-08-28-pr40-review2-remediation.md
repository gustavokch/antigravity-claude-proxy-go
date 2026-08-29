# PR #40 Second-Review Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the two confirmed defects and one confirmed inefficiency found in the second code review of PR #40 (CommandCrusher stage): Mocha signature detection missing the `√` checkmark, whitespace handling inconsistent between Cargo build detection and filtering, and the pytest progress gate routing `FAILED` lines through a regex that always rejects them.

**Architecture:** All changes are confined to pure, deterministic line-classification helpers in `internal/headroom/`. `detectSignature` picks a filter by scanning the first and last 4096 bytes of command output; the per-tool `crush*` functions then filter lines. The defects are all cases where detection is stricter than the filter it selects, so output that the filter could clean is passed through uncrushed. Each fix loosens detection to match its filter without loosening it enough to match ordinary source text.

**Tech Stack:** Go stdlib (`strings`, `regexp`). Tests are standard `go test` table-free unit tests in `internal/headroom/command_crusher_test.go`.

**Spec:** PR #40 second review comment — `https://github.com/gustavokch/antigravity-claude-proxy-go/pull/40#issuecomment-5458285424` (posted 2026-08-28T22:09:54Z). The first review comment was already remediated by `docs/superpowers/plans/2026-08-28-headroom-command-crusher-remediation.md`; this plan covers only findings that survived verification against branch `feat-rtk-suite` at commit `b2eb805`.

## Global Constraints

- Branch: `feat-rtk-suite` (PR #40). Base: `main`. Do not rebase or force-push.
- Go stdlib only. Do not add dependencies.
- `internal/headroom/` must stay allocation-light on the detection path: the existing benchmarks `BenchmarkCommandCrusher_Pytest100KB` and `BenchmarkCommandCrusher_Fallback100KB` must not regress beyond their current sub-200µs range.
- `TestDetectSignature_NoFalsePositiveOnSource` must keep passing after every task. Loosening detection must never make ordinary source code match a tool signature.
- Every task ends with `go build ./... && go test ./internal/headroom/` green before its commit.
- Commit messages follow the repository's Conventional Commits style with the `headroom` scope, e.g. `fix(headroom): ...`.

## Verification status of the source review

Confirmed by executing probes against `b2eb805` — these are the findings this plan fixes:

| Finding | Severity | Status |
|---|---|---|
| `detectSignature` Mocha case omits `√`; `√`-only output returns `sigNone` and passes uncrushed | bug | fixed by Task 1 |
| Cargo build detection requires exact leading indent; `crushCargoBuild` trims all leading spaces | risk | fixed by Task 2 |
| `isPytestProgress` gate admits `FAILED …` lines into a regex that always rejects them | risk (perf only) | fixed by Task 3 |

Deferred, with rationale — do not implement:

- `hasCommitLine` allocating a slice via `strings.Split` on up to 4096 bytes. Detection runs once per command output, not per line, and the benchmarks are already well inside budget. Replacing it with a manual index loop trades readable code for unmeasurable gain.
- Simplifying `pytestProgressRe` because its first alternative is unreachable for `.py` lines that end in `%]`. The first alternative is still reachable for `.py` lines that the fast path misses, and rewriting a regex with live coverage for cosmetic reasons risks a silent behavior change.

## File Structure

- `internal/headroom/command_crusher.go` — signature detection (`detectSignature`) and the shared line-filter helpers. Tasks 1 and 2 modify detection here; Task 2 also adds one small unexported helper next to `hasCommitLine`, which is the established home for per-signature detection predicates.
- `internal/headroom/crusher_python.go` — pytest and unittest filters. Task 3 modifies `isPytestProgress` only.
- `internal/headroom/command_crusher_test.go` — the single test file for the whole crusher stage. All three tasks add tests here, matching the existing one-function-per-scenario naming (`TestDetectSignature_*`, `Test<Tool>Filter_*`).

No new files. No file in this package is large enough to warrant a split.

---

### Task 1: Detect Mocha output that uses only the `√` checkmark

**Files:**
- Modify: `internal/headroom/command_crusher.go:169-170`
- Test: `internal/headroom/command_crusher_test.go`

**Interfaces:**
- Consumes: `detectSignature(text string) signature` and the `sigMocha` constant, both already defined in `command_crusher.go`. `isCheckmarkLine(trimmed string) bool` in `crusher_javascript.go` already recognizes all three checkmarks and needs no change.
- Produces: no new identifiers. Behavior change only: `detectSignature` returns `sigMocha` for Mocha output whose pass markers are all `√`.

Background: Mocha prints `✓` on most terminals, `✔` on some reporters, and `√` on Windows consoles that cannot render the first two. `crushMocha` strips all three through `isCheckmarkLine`, but `detectSignature` tests only `✓` and `✔`, so `√`-only output never reaches `crushMocha` and is emitted uncrushed.

- [ ] **Step 1: Write the failing test**

Add to `internal/headroom/command_crusher_test.go`, after `TestMochaFilter`:

```go
func TestDetectSignature_MochaSqrtCheckmark(t *testing.T) {
	// Windows consoles render Mocha's pass marker as √. Detection must match
	// the checkmark set crushMocha already strips.
	input := "  Calculator\n    √ adds\n    √ multiplies\n    1) divides by zero\n\n  2 passing (5ms)\n  1 failing\n"
	if sig := detectSignature(input); sig != sigMocha {
		t.Errorf("detectSignature = %v, want sigMocha", sig)
	}
	got, changed := CrushCommandOutput(input)
	if !changed {
		t.Fatal("expected sqrt-checkmark mocha output to be crushed")
	}
	if strings.Contains(got, "√") {
		t.Errorf("checkmarks survive:\n%q", got)
	}
	if !strings.Contains(got, "1) divides by zero") || !strings.Contains(got, "2 passing (5ms)") {
		t.Errorf("failure evidence lost:\n%q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/headroom/ -run TestDetectSignature_MochaSqrtCheckmark -v`
Expected: FAIL with `detectSignature = 0, want sigMocha` (0 is `sigNone`).

- [ ] **Step 3: Write minimal implementation**

In `internal/headroom/command_crusher.go`, replace the Mocha case:

```go
	case strings.Contains(tail, " passing") && (strings.Contains(head, "✓") || strings.Contains(head, "✔")):
		return sigMocha
```

with:

```go
	case strings.Contains(tail, " passing") &&
		(strings.Contains(head, "✓") || strings.Contains(head, "✔") || strings.Contains(head, "√")):
		return sigMocha
```

The `" passing"` tail requirement is retained; it is what keeps this case from matching arbitrary text containing a checkmark.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/headroom/ -v`
Expected: PASS, including `TestMochaFilter`, `TestJestFilter`, and `TestDetectSignature_NoFalsePositiveOnSource`.

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/command_crusher.go internal/headroom/command_crusher_test.go
git commit -m "fix(headroom): detect mocha output using the sqrt checkmark"
```

---

### Task 2: Make Cargo build detection tolerate indent variance

**Files:**
- Modify: `internal/headroom/command_crusher.go:177-179` (the `sigCargoBuild` case) and the helper block below `hasCommitLine`
- Test: `internal/headroom/command_crusher_test.go`

**Interfaces:**
- Consumes: `detectSignature(text string) signature`, the `sigCargoBuild` constant, and `signatureScanCap` (4096), all already in `command_crusher.go`.
- Produces: `hasCargoVerbLine(head string) bool` — unexported, defined in `command_crusher.go` next to `hasCommitLine`. Returns true when `head` contains a line that starts with at least one space and, after leading spaces are removed, begins with one of Cargo's status verbs.

Background: detection hard-codes Cargo's current column alignment (`"   Compiling "` with exactly three spaces, `"    Updating "` with four), while `crushCargoBuild` strips all leading spaces before comparing the bare verb. If Cargo ever changes its alignment, detection silently stops matching even though the filter would still work correctly. Aligning the two removes the coupling to a cosmetic detail of Cargo's output.

The leading-space requirement is deliberate and load-bearing: Cargo always indents these status lines, and requiring the indent is what prevents unindented prose or source code beginning with the word `Checking` or `Updating` from matching.

- [ ] **Step 1: Write the failing test**

Add to `internal/headroom/command_crusher_test.go`, after `TestDetectSignature_CargoCheck`:

```go
func TestDetectSignature_CargoIndentVariance(t *testing.T) {
	// crushCargoBuild trims all leading spaces before matching the verb, so
	// detection must not depend on Cargo's exact column alignment.
	for _, input := range []string{
		"  Compiling serde v1.0.0\n    Finished dev [unoptimized] target(s) in 3.1s\n",
		"     Checking serde v1.0.0\n    Finished dev [unoptimized] target(s) in 3.1s\n",
		" Updating crates.io index\n    Finished dev [unoptimized] target(s) in 3.1s\n",
	} {
		if sig := detectSignature(input); sig != sigCargoBuild {
			t.Errorf("detectSignature(%q) = %v, want sigCargoBuild", input, sig)
		}
	}
}

func TestDetectSignature_UnindentedCargoVerbIsNotCargo(t *testing.T) {
	// Prose and source text starting at column zero must not match. The
	// required leading indent is what keeps this case narrow.
	input := "Checking the inventory for missing parts\nUpdating the manifest\n"
	if sig := detectSignature(input); sig == sigCargoBuild {
		t.Error("unindented prose matched sigCargoBuild")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/headroom/ -run "TestDetectSignature_CargoIndentVariance|TestDetectSignature_UnindentedCargoVerbIsNotCargo" -v`
Expected: `TestDetectSignature_CargoIndentVariance` FAILs with `= 0, want sigCargoBuild` for all three inputs. `TestDetectSignature_UnindentedCargoVerbIsNotCargo` passes already; it is a regression guard for Step 3.

- [ ] **Step 3: Write minimal implementation**

In `internal/headroom/command_crusher.go`, replace the `sigCargoBuild` case:

```go
	case strings.HasPrefix(head, "   Compiling ") || strings.Contains(head, "\n   Compiling ") ||
		strings.HasPrefix(head, "   Checking ") || strings.Contains(head, "\n   Checking ") ||
		strings.HasPrefix(head, "    Updating ") || strings.Contains(head, "\n    Updating "):
		return sigCargoBuild
```

with:

```go
	case hasCargoVerbLine(head):
		return sigCargoBuild
```

Then add this helper immediately after `hasCommitLine` in the same file:

```go
// cargoVerbs are the Cargo status-line verbs that crushCargoBuild strips.
// Only the subset distinctive enough to identify Cargo output is listed;
// crushCargoBuild strips a wider set once this signature is chosen.
var cargoVerbs = []string{"Compiling ", "Checking ", "Updating "}

// hasCargoVerbLine reports whether head contains an indented Cargo status
// line. The leading space is required: Cargo always indents these lines, and
// demanding the indent keeps unindented prose from matching.
func hasCargoVerbLine(head string) bool {
	for _, line := range strings.Split(head, "\n") {
		if len(line) == 0 || (line[0] != ' ' && line[0] != '\t') {
			continue
		}
		trimmed := strings.TrimLeft(line, " \t")
		for _, verb := range cargoVerbs {
			if strings.HasPrefix(trimmed, verb) {
				return true
			}
		}
	}
	return false
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/headroom/ -v`
Expected: PASS, including `TestCargoBuildFilter`, `TestDetectSignature_CargoCheck`, and `TestDetectSignature_NoFalsePositiveOnSource`.

Then confirm no detection-path regression:

Run: `go test ./internal/headroom/ -bench . -benchtime 100x -run XXX`
Expected: `BenchmarkCommandCrusher_Pytest100KB` and `BenchmarkCommandCrusher_Fallback100KB` stay in their prior range (well under 200µs/op). `hasCargoVerbLine` is only reached when every earlier case misses, so the fallback benchmark is the one to watch.

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/command_crusher.go internal/headroom/command_crusher_test.go
git commit -m "fix(headroom): match cargo detection indent handling to its filter"
```

---

### Task 3: Stop routing pytest `FAILED` lines through the progress regex

**Files:**
- Modify: `internal/headroom/crusher_python.go:12-23` (`isPytestProgress`)
- Test: `internal/headroom/command_crusher_test.go`

**Interfaces:**
- Consumes: `isPytestProgress(line string) bool` and `pytestProgressRe`, both in `crusher_python.go`.
- Produces: no new identifiers. No behavior change — `isPytestProgress` already returns false for `FAILED …` lines because the regex rejects them. This task removes the wasted regex evaluation and makes the gate state its intent.

Background: the gate admits any line starting with `F` so that a bare progress run such as `FF..` reaches the regex. Pytest's short-summary lines (`FAILED test_calc.py::test_add - AssertionError`) also start with `F`, so every one of them runs the regex and is then rejected. Correctness is unaffected; this is purely wasted work on output that is often failure-heavy.

- [ ] **Step 1: Write the failing test**

Add to `internal/headroom/command_crusher_test.go`, after `TestPytestFilter_CRLF`:

```go
func TestPytestFilter_FailedSummaryLinesSurvive(t *testing.T) {
	// FAILED short-summary lines carry the failure signal and must never be
	// treated as progress, whatever the gate does.
	for _, line := range []string{
		"FAILED test_calc.py::test_add - AssertionError: 1 != 2",
		"FAILED test_calc.py::test_div",
		"ERROR test_calc.py::test_setup",
	} {
		if isPytestProgress(line) {
			t.Errorf("isPytestProgress(%q) = true, want false", line)
		}
	}
	// A bare failure-progress run is still progress and must be stripped.
	for _, line := range []string{"FF..", "F", "..F.. [ 50%]"} {
		if !isPytestProgress(line) {
			t.Errorf("isPytestProgress(%q) = false, want true", line)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it passes on the current code**

Run: `go test ./internal/headroom/ -run TestPytestFilter_FailedSummaryLinesSurvive -v`
Expected: PASS. This test is a behavior lock, not a red test — it pins the contract that Step 3 must preserve while changing the gate. Record that it passes before editing; if it fails here, stop and report, because the premise of this task is wrong.

- [ ] **Step 3: Write minimal implementation**

In `internal/headroom/crusher_python.go`, replace the gate:

```go
	if strings.HasSuffix(trimmed, "]") || strings.HasPrefix(trimmed, ".") || strings.HasPrefix(trimmed, "s") || strings.HasPrefix(trimmed, "F") || strings.HasPrefix(trimmed, "x") || strings.HasPrefix(trimmed, "X") {
		return pytestProgressRe.MatchString(trimmed)
	}
```

with:

```go
	// Short-summary lines start with a status word, not a progress glyph.
	// They can never be progress, so skip the regex entirely.
	if strings.HasPrefix(trimmed, "FAILED ") || strings.HasPrefix(trimmed, "ERROR ") {
		return false
	}
	if strings.HasSuffix(trimmed, "]") || strings.HasPrefix(trimmed, ".") || strings.HasPrefix(trimmed, "s") || strings.HasPrefix(trimmed, "F") || strings.HasPrefix(trimmed, "x") || strings.HasPrefix(trimmed, "X") {
		return pytestProgressRe.MatchString(trimmed)
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/headroom/ -v`
Expected: PASS, including `TestPytestFilter`, `TestPytestFilter_AllPass`, and `TestPytestFilter_CRLF`.

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/crusher_python.go internal/headroom/command_crusher_test.go
git commit -m "perf(headroom): skip progress regex for pytest status summary lines"
```

---

### Task 4: Close out the review

**Files:**
- Modify: none
- Test: whole-package gate

**Interfaces:**
- Consumes: the three commits from Tasks 1-3.
- Produces: a pushed branch and a reply on the review comment.

- [ ] **Step 1: Run the full gate from a clean tree**

```bash
git status --short
go build ./...
go test ./...
```

Expected: no unexpected modified files, build clean, all packages pass. Do not accept a self-reported pass from any delegated agent; run this yourself.

- [ ] **Step 2: Push the branch**

```bash
git push fork HEAD:feat-rtk-suite
```

- [ ] **Step 3: Reply on the review comment**

Post one comment on PR #40 stating, per finding: what was fixed and where, and for each deferred nit, the one-line rationale from the "Deferred" section above. State the fixes plainly; do not thank the reviewer.
