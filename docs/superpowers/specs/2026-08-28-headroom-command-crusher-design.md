# Design Specification: Headroom CommandCrusher Stage (RTK Suite)

**Date**: 2026-08-28  
**Status**: Approved (Draft for Planning)  
**Target Subsystem**: `internal/headroom`  

---

## 1. Overview & Objectives

**CommandCrusher** is a native Go pipeline stage for the Headroom Context Compression engine in `antigravity-claude-proxy-go`. It brings the token reduction capabilities of [RTK (Rust Token Killer)](https://github.com/rtk-ai/rtk) directly into the proxy middleware without external subprocess dependencies.

### Key Goals
1. **Reduce Context Consumption**: Compress verbose tool outputs (`tool_result`) by 40–80% for test runners, linters, compilers, and git commands across all connected clients (Claude Code, Cursor, RooCode, custom SDKs).
2. **Zero-Latency Overhead**: Execute in pure, memory-efficient Go (<0.2ms per tool_result block) avoiding subprocess `exec.Command` latency.
3. **Strict Invariant Adherence**:
   - **I1 (Cache Stability & Determinism)**: Transformations are pure and position-independent, preserving Anthropic/Gemini prompt caching.
   - **I2 (Unconditional Invariant)**: Injected schemas remain constant.
   - **I3 (Target Isolation)**: Only mutate `tool_result` content text blocks; never alter user prompts, assistant messages, or thinking blocks.
   - **I4 (Verbatim Read Preservation)**: Respect `skipVerbatim` and `PreserveVerbatimReads` so byte-exact file content for `Edit` calls is never corrupted.

---

## 2. Pipeline Integration

### 2.1 Pipeline Order
`CommandCrusherStage` sits between `CCRStage` and `SmartCrusherStage`:

```
Request Context
      │
      ▼
┌──────────────┐
│   CCRStage   │ (Demotes oversized historical chunks >2KB to chunk store)
└──────┬───────┘
       │
       ▼
┌──────────────────────┐
│ CommandCrusherStage  │ (RTK-style command output filtering for test/git/lint)
└──────┬───────────────┘
       │
       ▼
┌──────────────────────┐
│  SmartCrusherStage   │ (JSON compacting & Tabular conversions)
└──────┬───────────────┘
       │
       ▼
┌──────────────────────┐
│ CodeCompressorStage  │ (Whitespace trimming, blank line collapse, line repeats)
└──────┬───────────────┘
       │
       ▼
┌──────────────────────┐
│  OutputShaperStage   │ (Effort routing & verbosity steering)
└──────────────────────┘
```

### 2.2 Configuration Schema (`internal/headroom/types.go`)

```go
type Config struct {
    Enabled               bool               `json:"enabled"`
    CommandCrusher        bool               `json:"commandCrusher,omitempty"`
    SmartCrusher          bool               `json:"smartCrusher,omitempty"`
    TabularArrays         bool               `json:"tabularArrays,omitempty"`
    CodeCompressor        bool               `json:"codeCompressor,omitempty"`
    LiveTurns             int                `json:"liveTurns,omitempty"`
    PreserveVerbatimReads bool               `json:"preserveVerbatimReads"`
    CCR                   CCRConfig          `json:"ccr,omitempty"`
    OutputShaper          OutputShaperConfig `json:"outputShaper,omitempty"`
}
```

---

## 3. Supported Parsers & Compression Rules

### 3.1 Python Test & Lint Filters
* **`pytest`**:
  - Filter: Strip consecutive passing dots (`.`), `PASSED` lines, progress percentages (`[ 50%]`).
  - Retain: `FAILED`, `ERROR`, traceback frames, assertion comparisons (`E   AssertionError`), and final summary (`=== 2 failed, 120 passed in 1.4s ===`).
* **`unittest`**:
  - Filter: Collapse leading dot sequences (`....`).
  - Retain: `FAIL:`, `ERROR:`, traceback, failure footer (`FAILED (failures=1)`).
* **`ruff` / `flake8`**:
  - Filter: Deduplicate identical rule violations across files, group warnings by rule code.
  - Retain: All error messages with `file:line:col`.

### 3.2 TypeScript / JavaScript Test & Build Filters
* **`jest` / `vitest`**:
  - Filter: Strip passing suites and green checkmarks (`✓ path/to/test.ts (2ms)`).
  - Retain: Failing suites (`✕ path/to/test.ts`), failure titles, received/expected diffs, stack traces, and summary footer (`Tests: 1 failed, 45 passed, 46 total`).
* **`mocha` / `bun test` / `npm test`**:
  - Filter: Strip passing lines (`✔ test item`).
  - Retain: Failing items (`1) should do something`), error frames, summary.
* **`tsc` / Typecheck**:
  - Filter: Group compiler diagnostics by file path; deduplicate repeating import errors.
  - Retain: Exact `file.ts(line,col): error TSxxxx: message`.
* **`eslint`**:
  - Filter: Collapse passing file logs, deduplicate repeating warnings with count badges (`[+12 similar warnings]`).
  - Retain: Errors and unique warnings with line numbers.

### 3.3 Go & Rust Filters
* **`go test`**:
  - Filter: Strip `=== RUN` and `--- PASS:` lines from verbose test logs.
  - Retain: `--- FAIL:`, panic traces, failure output, and package summary lines (`FAIL\tpackage/path\t0.123s` or `ok\tpackage/path\t0.05s`).
* **`golangci-lint`**:
  - Filter: Group linter issues by linter name, collapse repetitive style warnings.
  - Retain: Error diagnostics and unique findings.
* **`cargo test`**:
  - Filter: Strip `test tests::test_name ... ok`.
  - Retain: `test tests::test_name ... FAILED`, panic messages, failure summary (`failures:\n    test_name\n\ntest result: FAILED. 1 failed; 42 passed`).
* **`cargo build` / `cargo clippy`**:
  - Filter: Strip repetitive `Compiling <crate>` build noise.
  - Retain: `warning:` and `error:` diagnostic blocks.

### 3.4 Git Operations Filters
* **`git status`**:
  - Filter: Remove boilerplate instructional hints (`(use "git add <file>..." to include in what will be committed)`, `(use "git restore <file>..." to discard changes)`).
  - Retain: Branch state (`On branch main`), tracking info (`Your branch is up to date`), staged/unstaged file lists (`modified:   file.go`).
* **`git log`**:
  - Filter: Strip redundant author/date boilerplate if repetitive in multi-commit logs.
  - Retain: Commit hash, author, subject, and body.

---

## 4. Parser Architecture & Safety Guarantees

```
┌─────────────────────────────────────────────────────────────┐
│                      CommandCrusher                         │
├─────────────────────────────────────────────────────────────┤
│ 1. Check ToolInspector: skipVerbatim? -> Return unchanged   │
│ 2. Check Payload Type: is_error: true? -> Return unchanged  │
│ 3. Match Output Signature (Pattern Engine):                 │
│    - Is Pytest/Unittest output? -> Run PythonTestFilter     │
│    - Is Jest/Vitest/Mocha output? -> Run JSTestFilter       │
│    - Is Go Test output? -> Run GoTestFilter                 │
│    - Is Cargo Test output? -> Run CargoTestFilter           │
│    - Is Git Status output? -> Run GitStatusFilter           │
│    - Is Compiler/Linter output? -> Run LinterFilter         │
│ 4. Fallback: If no signature matches -> Return unchanged    │
└─────────────────────────────────────────────────────────────┘
```

### Safety Constraints
1. **Never truncate error details**: Tracebacks, assertion diffs, line numbers, and error summaries must remain byte-exact.
2. **Deterministic & Idempotent**: Running `CommandCrusher` multiple times produces the exact same output (`Filter(Filter(x)) == Filter(x)`).
3. **No False Positives on Source Code**: Never match or corrupt regular code reads or diffs (guarded by `ToolInspector` and `skipVerbatim`).

---

## 5. Verification & Test Plan

1. **Unit Tests (`internal/headroom/command_crusher_test.go`)**:
   - `TestPytestFilter`: Verify passing dots are collapsed while traceback and failure footer survive.
   - `TestJestFilter`: Verify `✓` lines are removed and `✕` blocks survive.
   - `TestGoTestFilter`: Verify `=== RUN` and `--- PASS:` are stripped while `--- FAIL:` and panics survive.
   - `TestCargoTestFilter`: Verify `... ok` lines are removed and `... FAILED` blocks survive.
   - `TestGitStatusFilter`: Verify git advice hints are stripped and file states survive.
   - `TestTypeScriptCompilerFilter`: Verify `tsc` errors remain intact.
   - `TestVerbatimSkip`: Verify `cat -n` file reads and numbered source are left untouched.
   - `TestIdempotency`: Verify multiple passes produce identical output.
2. **Benchmark Tests**:
   - Verify execution time is <0.2ms per 100KB payload.
3. **End-to-End Pipeline Tests (`internal/headroom/engine_test.go`)**:
   - Verify `CommandCrusherStage` runs in sequence with `CCR`, `SmartCrusher`, `CodeCompressor`, and `OutputShaper`.
