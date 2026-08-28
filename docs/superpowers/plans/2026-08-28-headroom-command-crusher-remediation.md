# Headroom CommandCrusher Stage Remediation Plan

**Goal:** Remediate review findings on PR #40 (CommandCrusher stage): fix Go test subtest/parallel runner noise filtering, fix Go and Cargo signature detection edge cases, handle CRLF line endings in test filters, unify JS test checkmark sets, and clean remaining git status hints.

**Architecture:** Pure, deterministic Go line filtering and signature detection in `internal/headroom/`.

**Tech Stack:** Go stdlib (`strings`, `regexp`).

**Spec Reference:** PR #40 Review (`https://github.com/gustavokch/antigravity-claude-proxy-go/pull/40#issuecomment-5458130306`).

---

### Task 1: Fix Go Test Subtest / Parallel Verb Filtering & Signature Detection
- **Target Files:**
  - Modify: `internal/headroom/crusher_gorust.go`
  - Modify: `internal/headroom/command_crusher.go`
  - Test: `internal/headroom/command_crusher_test.go`
- **Consumes / Produces:**
  - Consumes: Go test stdout (including subtests with indentation, parallel tests with PAUSE/CONT, single line package results).
  - Produces: Cleaned Go test output retaining failures, errors, panics, package status.
- **Step 1: Write failing test:**
  Add `TestGoTestFilter_SubtestsAndParallel` and `TestDetectSignature_GoTestSingleLine` in `command_crusher_test.go`.
- **Step 2: Run test to confirm failure:**
  `go test ./internal/headroom/ -run "TestGoTestFilter_SubtestsAndParallel|TestDetectSignature_GoTestSingleLine"`
- **Step 3: Minimal implementation:**
  In `crusher_gorust.go`, check trimmed line for `=== RUN`, `=== PAUSE`, `=== CONT`, `--- PASS:`.
  In `command_crusher.go`, add `strings.HasPrefix(head, "ok  \t") || strings.HasPrefix(head, "FAIL\t")` in `detectSignature`.
- **Step 4: Run test to confirm pass:**
  `go test ./internal/headroom/ -v`
- **Step 5: Git commit command:**
  `git commit -m "fix(headroom): handle go test subtests parallel markers and single-line detection"`

---

### Task 2: Fix Cargo Check Signature Detection & CRLF / Verb Robustness
- **Target Files:**
  - Modify: `internal/headroom/command_crusher.go`
  - Modify: `internal/headroom/crusher_gorust.go`
  - Test: `internal/headroom/command_crusher_test.go`
- **Consumes / Produces:**
  - Consumes: Cargo build/check/clippy/test output across various whitespace alignments and CRLF endings.
  - Produces: Filtered Cargo diagnostics and test results.
- **Step 1: Write failing test:**
  Add `TestDetectSignature_CargoCheck` and `TestCargoTestFilter_CRLF` in `command_crusher_test.go`.
- **Step 2: Run test to confirm failure:**
  `go test ./internal/headroom/ -run "TestDetectSignature_CargoCheck|TestCargoTestFilter_CRLF"`
- **Step 3: Minimal implementation:**
  In `command_crusher.go`, add `   Checking ` to `sigCargoBuild` signature detection.
  In `crusher_gorust.go`, trim `\r` and whitespace when checking Cargo test and build verb lines.
- **Step 4: Run test to confirm pass:**
  `go test ./internal/headroom/ -v`
- **Step 5: Git commit command:**
  `git commit -m "fix(headroom): support cargo check signature and handle crlf in cargo tests"`

---

### Task 3: Fix CRLF Handling in Pytest and Unittest Filters
- **Target Files:**
  - Modify: `internal/headroom/crusher_python.go`
  - Test: `internal/headroom/command_crusher_test.go`
- **Consumes / Produces:**
  - Consumes: Python pytest and unittest stdout with `\r\n` line endings.
  - Produces: Filtered pytest and unittest output without failing regexes on CRLF.
- **Step 1: Write failing test:**
  Add `TestPytestFilter_CRLF` and `TestUnittestFilter_CRLF` in `command_crusher_test.go`.
- **Step 2: Run test to confirm failure:**
  `go test ./internal/headroom/ -run "TestPytestFilter_CRLF|TestUnittestFilter_CRLF"`
- **Step 3: Minimal implementation:**
  In `crusher_python.go`, trim trailing `\r` and evaluate regexes against trimmed line strings.
- **Step 4: Run test to confirm pass:**
  `go test ./internal/headroom/ -v`
- **Step 5: Git commit command:**
  `git commit -m "fix(headroom): handle crlf line endings in python test filters"`

---

### Task 4: Unify JS Checkmarks and Clean Git Status Untracked Hints
- **Target Files:**
  - Modify: `internal/headroom/crusher_javascript.go`
  - Modify: `internal/headroom/crusher_git.go`
  - Test: `internal/headroom/command_crusher_test.go`
- **Consumes / Produces:**
  - Consumes: Jest/Mocha/Vitest outputs with checkmark variants (`✓`, `✔`, `√`) and git status with untracked hint footer.
  - Produces: Cleaned JS test outputs and git status outputs.
- **Step 1: Write failing test:**
  Add `TestJestFilter_HeavyCheckmark`, `TestMochaFilter_SqrtCheckmark`, and `TestGitStatusFilter_UntrackedHint` in `command_crusher_test.go`.
- **Step 2: Run test to confirm failure:**
  `go test ./internal/headroom/ -run "TestJestFilter_HeavyCheckmark|TestMochaFilter_SqrtCheckmark|TestGitStatusFilter_UntrackedHint"`
- **Step 3: Minimal implementation:**
  In `crusher_javascript.go`, support `✓`, `✔`, `√` in both `crushJest` and `crushMocha`.
  In `crusher_git.go`, filter `nothing added to commit but untracked files present`.
- **Step 4: Run test to confirm pass:**
  `go test ./internal/headroom/ -v`
- **Step 5: Git commit command:**
  `git commit -m "fix(headroom): unify js checkmark sets and strip untracked git status hints"`
