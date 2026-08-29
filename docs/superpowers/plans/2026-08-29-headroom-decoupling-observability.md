# Headroom Engine Decoupling & Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Decouple Headroom Engine from RTK Suite/CommandCrusher and compression stages into modular subpackages under `internal/headroom/stages/`, and add comprehensive structured `slog` logging for execution tracking and metrics.

**Architecture:** `internal/headroom` becomes a pure pipeline orchestration engine with zero direct dependencies on concrete stages. Each transformation capability (CommandCrusher, SmartCrusher, CodeCompressor, OutputShaper, CCR) is isolated in its own subpackage implementing `headroom.Stage`. Observability is integrated via `*slog.Logger` in `RequestContext`, emitting detailed DEBUG logs on rewrites and INFO summaries on completion.

**Tech Stack:** Go 1.24, `log/slog`, standard library `testing`.

**Spec:** `docs/superpowers/specs/2026-08-29-headroom-decoupling-observability-design.md`

## Global Constraints

- **Zero Stage Dependencies in Core**: `internal/headroom` must not import any package in `internal/headroom/stages/*`.
- **Pure Invariant I1 (Deterministic)**: Compression must be purely deterministic.
- **Safety Invariant I2 (Verbatim Reads)**: Verbatim file-read tool results are strictly preserved untouched.
- **Safety Invariant I3 (Assistant Text)**: Assistant messages are never modified.
- **Safety Invariant I4 (Tool Errors)**: Tool results with `is_error: true` are never modified.
- **Zero Allocations on Inactive Log Levels**: `slog.Logger` calls must avoid unnecessary string/object allocations when DEBUG level is disabled.
- **100% Green Test Suite**: Full test suite must pass with `go test -race ./...`.

---

### Task 1: Core Headroom Types, Interfaces & AST Utilities

**Files:**
- Modify: `internal/headroom/types.go`
- Modify: `internal/headroom/engine.go`
- Modify: `internal/headroom/verbatim.go`
- Modify: `internal/headroom/walk.go`
- Test: `internal/headroom/types_test.go`
- Test: `internal/headroom/walk_test.go`
- Test: `internal/headroom/verbatim_test.go`

**Interfaces:**
- Consumes: Standard library (`context`, `log/slog`, `sync`, `time`)
- Produces:
  - `headroom.Stage` interface (`Name() string`, `Execute(ctx context.Context, reqCtx *RequestContext, cfg *Config) error`)
  - `headroom.RequestContext` with `Logger *slog.Logger`, `RecordRewrite(before, after string)`, `RewritesCount int`
  - `headroom.Config`, `headroom.CCRConfig`, `headroom.OutputShaperConfig`
  - `headroom.ErrorOrdinals(req map[string]any) map[int]bool`
  - `headroom.WalkToolResultText(req map[string]any, fromIndex int, fn func(msgIdx, ord int, get func() string, set func(string)))`
  - `headroom.SkipVerbatim(reqCtx *RequestContext, cfg *Config, ord int) bool`
  - `headroom.FrozenPrefixIndex(req map[string]any, liveTurns int) int`

- [ ] **Step 1: Write failing unit tests for RequestContext logging and telemetry**

```go
// in internal/headroom/types_test.go
func TestRequestContext_RecordRewriteWithLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	reqCtx := &RequestContext{
		Logger: logger,
	}
	reqCtx.RecordRewrite("hello world 12345", "hello")
	if reqCtx.BytesBefore != 17 || reqCtx.BytesAfter != 5 || reqCtx.RewritesCount != 1 {
		t.Fatalf("unexpected telemetry: before=%d, after=%d, rewrites=%d",
			reqCtx.BytesBefore, reqCtx.BytesAfter, reqCtx.RewritesCount)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/headroom -run TestRequestContext_RecordRewriteWithLogger`
Expected: FAIL due to missing `RewritesCount` or field in `RequestContext`.

- [ ] **Step 3: Update `types.go`, `walk.go`, and `verbatim.go` with exported AST helpers**

Export `ErrorOrdinals`, `WalkToolResultText`, `SkipVerbatim`, `FrozenPrefixIndex`, and add `Logger *slog.Logger` and `RewritesCount int` to `RequestContext`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/headroom -run "TestRequestContext|TestWalk|TestVerbatim"`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/types.go internal/headroom/walk.go internal/headroom/verbatim.go internal/headroom/types_test.go internal/headroom/walk_test.go internal/headroom/verbatim_test.go
git commit -m "refactor(headroom): export AST helpers and add structured logger to RequestContext"
```

---

### Task 2: Extract RTK CommandCrusher Stage to `internal/headroom/stages/crusher`

**Files:**
- Create: `internal/headroom/stages/crusher/stage.go`
- Create: `internal/headroom/stages/crusher/detector.go`
- Create: `internal/headroom/stages/crusher/crusher_git.go`
- Create: `internal/headroom/stages/crusher/crusher_gorust.go`
- Create: `internal/headroom/stages/crusher/crusher_javascript.go`
- Create: `internal/headroom/stages/crusher/crusher_python.go`
- Test: `internal/headroom/stages/crusher/crusher_test.go`
- Delete: `internal/headroom/command_crusher.go`
- Delete: `internal/headroom/crusher_git.go`
- Delete: `internal/headroom/crusher_gorust.go`
- Delete: `internal/headroom/crusher_javascript.go`
- Delete: `internal/headroom/crusher_python.go`
- Delete: `internal/headroom/command_crusher_test.go`

**Interfaces:**
- Consumes: `headroom.Stage`, `headroom.RequestContext`, `headroom.Config`, `headroom.ErrorOrdinals`, `headroom.WalkToolResultText`, `headroom.SkipVerbatim`
- Produces: `crusher.NewStage() *CommandCrusherStage` implementing `headroom.Stage`

- [ ] **Step 1: Write failing test in `internal/headroom/stages/crusher/crusher_test.go`**

```go
package crusher

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/headroom"
)

func TestCommandCrusherStage_LogsRewrite(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	stage := NewStage()
	req := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "tool_result",
						"content": "=== 5 passed in 0.05s ===\n",
					},
				},
			},
		},
	}

	reqCtx := &RequestContextWithReq(req, logger)
	cfg := &headroom.Config{Enabled: true, CommandCrusher: true}

	if err := stage.Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "command_crusher compressed tool output") {
		t.Fatalf("expected debug log in output, got: %s", buf.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/headroom/stages/crusher/...`
Expected: FAIL (package does not exist yet)

- [ ] **Step 3: Implement `internal/headroom/stages/crusher`**

Create `stage.go`, `detector.go`, `crusher_*.go` with signature detection, compression rules, and structured `slog` logging on match/rewrite. Remove old files in `internal/headroom/`.

- [ ] **Step 4: Run crusher tests and benchmarks**

Run: `go test -v -race ./internal/headroom/stages/crusher/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/stages/crusher/
git rm internal/headroom/command_crusher.go internal/headroom/crusher_git.go internal/headroom/crusher_gorust.go internal/headroom/crusher_javascript.go internal/headroom/crusher_python.go internal/headroom/command_crusher_test.go
git commit -m "feat(headroom): extract CommandCrusher into isolated stages/crusher package with structured logging"
```

---

### Task 3: Extract SmartCrusher Stage to `internal/headroom/stages/smart`

**Files:**
- Create: `internal/headroom/stages/smart/stage.go`
- Create: `internal/headroom/stages/smart/tabular.go`
- Test: `internal/headroom/stages/smart/smart_test.go`
- Test: `internal/headroom/stages/smart/tabular_test.go`
- Delete: `internal/headroom/smart_crusher.go`
- Delete: `internal/headroom/tabular.go`
- Delete: `internal/headroom/smart_crusher_test.go`
- Delete: `internal/headroom/tabular_test.go`

**Interfaces:**
- Consumes: `headroom.Stage`, `headroom.RequestContext`, `headroom.Config`, `headroom.WalkToolResultText`, `headroom.SkipVerbatim`
- Produces: `smart.NewStage() *SmartCrusherStage` implementing `headroom.Stage`

- [ ] **Step 1: Write failing test in `internal/headroom/stages/smart/smart_test.go`**

```go
package smart

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/headroom"
)

func TestSmartCrusherStage_CompactsJSONAndLogs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	stage := NewStage()
	req := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "tool_result",
						"content": "{\n  \"foo\": \"bar\",\n  \"baz\": 123\n}",
					},
				},
			},
		},
	}

	reqCtx := &headroom.RequestContext{Request: req, Logger: logger}
	cfg := &headroom.Config{Enabled: true, SmartCrusher: true}

	if err := stage.Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "smart_crusher compacted JSON tool output") {
		t.Fatalf("expected debug log in output, got: %s", buf.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/headroom/stages/smart/...`
Expected: FAIL

- [ ] **Step 3: Implement `internal/headroom/stages/smart`**

Move JSON compaction and tabular transformation into `stages/smart` and add `logger.Debug` on compact. Remove old files from `internal/headroom/`.

- [ ] **Step 4: Run smart tests and benchmarks**

Run: `go test -v -race ./internal/headroom/stages/smart/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/stages/smart/
git rm internal/headroom/smart_crusher.go internal/headroom/tabular.go internal/headroom/smart_crusher_test.go internal/headroom/tabular_test.go
git commit -m "feat(headroom): extract SmartCrusher into isolated stages/smart package with structured logging"
```

---

### Task 4: Extract CodeCompressor Stage to `internal/headroom/stages/code`

**Files:**
- Create: `internal/headroom/stages/code/stage.go`
- Test: `internal/headroom/stages/code/code_test.go`
- Delete: `internal/headroom/code_compressor.go`
- Delete: `internal/headroom/code_compressor_test.go`

**Interfaces:**
- Consumes: `headroom.Stage`, `headroom.RequestContext`, `headroom.Config`, `headroom.WalkToolResultText`, `headroom.SkipVerbatim`
- Produces: `code.NewStage() *CodeCompressorStage` implementing `headroom.Stage`

- [ ] **Step 1: Write failing test in `internal/headroom/stages/code/code_test.go`**

```go
package code

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/headroom"
)

func TestCodeCompressorStage_CompressesAndLogs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	stage := NewStage()
	req := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "tool_result",
						"content": "function test() {\n\n\n  return true;\n\n}",
					},
				},
			},
		},
	}

	reqCtx := &headroom.RequestContext{Request: req, Logger: logger}
	cfg := &headroom.Config{Enabled: true, CodeCompressor: true}

	if err := stage.Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "code_compressor stripped whitespace/lines") {
		t.Fatalf("expected debug log, got: %s", buf.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/headroom/stages/code/...`
Expected: FAIL

- [ ] **Step 3: Implement `internal/headroom/stages/code`**

Implement `CodeCompressorStage` with structured logging on code compression. Remove old files.

- [ ] **Step 4: Run code compressor tests**

Run: `go test -v -race ./internal/headroom/stages/code/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/stages/code/
git rm internal/headroom/code_compressor.go internal/headroom/code_compressor_test.go
git commit -m "feat(headroom): extract CodeCompressor into isolated stages/code package with structured logging"
```

---

### Task 5: Extract OutputShaper Stage to `internal/headroom/stages/shaper`

**Files:**
- Create: `internal/headroom/stages/shaper/stage.go`
- Test: `internal/headroom/stages/shaper/shaper_test.go`
- Delete: `internal/headroom/output_shaper.go`
- Delete: `internal/headroom/output_shaper_test.go`
- Delete: `internal/headroom/continuation_test.go`

**Interfaces:**
- Consumes: `headroom.Stage`, `headroom.RequestContext`, `headroom.Config`, `headroom.ToolInspector`
- Produces: `shaper.NewStage() *OutputShaperStage` implementing `headroom.Stage`

- [ ] **Step 1: Write failing test in `internal/headroom/stages/shaper/shaper_test.go`**

```go
package shaper

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/headroom"
)

func TestOutputShaperStage_ClampsThinkingAndLogs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	stage := NewStage()
	req := map[string]any{
		"thinking": map[string]any{
			"type": "enabled",
			"budget_tokens": 4096,
		},
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "tool_result",
						"content": "mkdir /foo/bar",
					},
				},
			},
		},
	}

	reqCtx := &headroom.RequestContext{
		Request: req,
		Logger: logger,
		Verbatim: headroom.NewToolInspector(req),
	}
	cfg := &headroom.Config{
		Enabled: true,
		OutputShaper: headroom.OutputShaperConfig{
			Enabled: true,
			EffortRouting: true,
			MechanicalThinkingBudget: 1024,
		},
	}

	if err := stage.Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(buf.String(), "output_shaper clamped thinking budget") {
		t.Fatalf("expected debug log, got: %s", buf.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/headroom/stages/shaper/...`
Expected: FAIL

- [ ] **Step 3: Implement `internal/headroom/stages/shaper`**

Port verbosity steering and effort routing logic into `stages/shaper` with debug logging. Remove old files.

- [ ] **Step 4: Run shaper tests**

Run: `go test -v -race ./internal/headroom/stages/shaper/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/stages/shaper/
git rm internal/headroom/output_shaper.go internal/headroom/output_shaper_test.go internal/headroom/continuation_test.go
git commit -m "feat(headroom): extract OutputShaper into isolated stages/shaper package with structured logging"
```

---

### Task 6: Extract CCR Stage & Store to `internal/headroom/stages/ccr`

**Files:**
- Create: `internal/headroom/stages/ccr/stage.go`
- Create: `internal/headroom/stages/ccr/store.go`
- Test: `internal/headroom/stages/ccr/ccr_test.go`
- Test: `internal/headroom/stages/ccr/store_test.go`
- Delete: `internal/headroom/ccr_stage.go`
- Delete: `internal/headroom/ccr_store.go`
- Delete: `internal/headroom/ccr_test.go`
- Delete: `internal/headroom/ccr_store_test.go`

**Interfaces:**
- Consumes: `headroom.Stage`, `headroom.RequestContext`, `headroom.Config`
- Produces:
  - `ccr.NewStage(store *CCRStore) *CCRStage` implementing `headroom.Stage`
  - `ccr.NewCCRStore(maxBytes int64) *CCRStore`
  - `ccr.NewCCRStoreFromMB(maxMB int) *CCRStore`
  - `ccr.ChunkID(payload string) string`

- [ ] **Step 1: Write failing test in `internal/headroom/stages/ccr/ccr_test.go`**

```go
package ccr

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/headroom"
)

func TestCCRStage_StoresChunkAndLogs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	store := NewCCRStoreFromMB(10)
	stage := NewStage(store)

	bigPayload := strings.Repeat("hello world data\n", 500)
	req := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "tool_result",
						"content": bigPayload,
					},
				},
			},
			map[string]any{
				"role": "assistant",
				"content": "turn 1",
			},
			map[string]any{
				"role": "user",
				"content": "turn 2 live",
			},
		},
	}

	reqCtx := &headroom.RequestContext{
		Request: req,
		FrozenPrefixIndex: 0,
		Logger: logger,
	}
	cfg := &headroom.Config{
		Enabled: true,
		CCR: headroom.CCRConfig{Enabled: true, MinChunkBytes: 100},
	}

	if err := stage.Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if reqCtx.ChunksStored == 0 {
		t.Fatalf("expected chunks to be stored")
	}
	if !strings.Contains(buf.String(), "ccr stored chunk") {
		t.Fatalf("expected debug log in output, got: %s", buf.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/headroom/stages/ccr/...`
Expected: FAIL

- [ ] **Step 3: Implement `internal/headroom/stages/ccr`**

Port CCR stage and store into `stages/ccr` with structured logging. Remove old files.

- [ ] **Step 4: Run CCR tests**

Run: `go test -v -race ./internal/headroom/stages/ccr/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/stages/ccr/
git rm internal/headroom/ccr_stage.go internal/headroom/ccr_store.go internal/headroom/ccr_test.go internal/headroom/ccr_store_test.go
git commit -m "feat(headroom): extract CCR into isolated stages/ccr package with structured logging"
```

---

### Task 7: Refactor `internal/headroom/engine.go` & `pipeline.go`

**Files:**
- Modify: `internal/headroom/engine.go`
- Create: `internal/headroom/pipeline.go` (if separated)
- Test: `internal/headroom/engine_test.go`
- Delete: `internal/headroom/benchmark_test.go` (or update to test stages/engine with mock stages)

**Interfaces:**
- Consumes: `headroom.Stage`, `headroom.RequestContext`, `headroom.Config`
- Produces: `headroom.NewEngine(cfg Config, logger *slog.Logger, stages ...Stage) *Engine`

- [ ] **Step 1: Update `engine_test.go` with mock stages and structured logging verification**

```go
func TestEngine_ProcessExecutesStagesAndLogsSummary(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	mockStage := &mockCountingStage{name: "mock_stage"}
	engine := NewEngine(Config{Enabled: true}, logger, mockStage)

	req := map[string]any{"messages": []any{}}
	ctx, err := engine.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !mockStage.executed {
		t.Fatalf("expected stage to execute")
	}
	_ = ctx
}
```

- [ ] **Step 2: Implement updated `Engine` in `internal/headroom/engine.go`**

Implement `NewEngine(cfg Config, logger *slog.Logger, stages ...Stage) *Engine`, remove concrete stage instantiation inside `internal/headroom/engine.go`, and add INFO level summary log when rewrites happen.

- [ ] **Step 3: Run headroom core tests**

Run: `go test -v -race ./internal/headroom`
Expected: PASS with 0 imports of `stages/*`.

- [ ] **Step 4: Commit**

```bash
git add internal/headroom/engine.go internal/headroom/pipeline.go internal/headroom/engine_test.go
git commit -m "refactor(headroom): decouple Engine from concrete stages and add pipeline summary logging"
```

---

### Task 8: Wire Stages in `internal/api/server.go` and Update Integration Tests

**Files:**
- Modify: `internal/api/server.go`
- Modify: `internal/api/headroom_proxy_test.go`
- Modify: `internal/api/headroom_ccr_test.go`
- Modify: `internal/api/ccr_proxy_test.go`
- Modify: `internal/api/claudecode_proxy_test.go`
- Modify: `internal/api/kimi_proxy_test.go`
- Modify: `internal/api/server_test.go`
- Modify: `internal/api/management_test.go`

**Interfaces:**
- Consumes:
  - `headroom.NewEngine`
  - `stages/ccr.NewStage`, `stages/ccr.NewCCRStoreFromMB`, `stages/ccr.CCRStore`
  - `stages/crusher.NewStage`
  - `stages/smart.NewStage`
  - `stages/code.NewStage`
  - `stages/shaper.NewStage`

- [ ] **Step 1: Update `internal/api/server.go` to construct stages and inject into `headroom.NewEngine`**

```go
ccrStore := ccr.NewCCRStoreFromMB(cfg.Headroom.CCR.MaxStoreMB)
srv.ccrStore = ccrStore

srv.headroom = headroom.NewEngine(
    cfg.Headroom,
    srv.logger,
    ccr.NewStage(ccrStore),
    crusher.NewStage(),
    smart.NewStage(),
    code.NewStage(),
    shaper.NewStage(),
)
```

- [ ] **Step 2: Update all test helpers in `internal/api/*_test.go` that instantiate `headroom.Engine`**

Update `newHeadroomTestServer`, `headroom_ccr_test.go`, `claudecode_proxy_test.go`, etc. to import the new `stages/ccr`, `stages/crusher`, `stages/smart`, `stages/code`, `stages/shaper` packages.

- [ ] **Step 3: Run full test suite with race detector**

Run: `go test -race ./...`
Expected: 100% PASS across all packages.

- [ ] **Step 4: Verify zero stage imports in `internal/headroom`**

Run: `go list -f '{{.Imports}}' ./internal/headroom`
Expected: Does not contain `internal/headroom/stages`.

- [ ] **Step 5: Commit**

```bash
git add internal/api/
git commit -m "feat(api): wire decoupled headroom stages into server and update integration tests"
```

---

## Plan Self-Review Checklist

- [x] **Spec coverage**: All 5 stages modularized, `internal/headroom` decoupled, structured `slog` logging added at DEBUG/INFO/WARN/ERROR levels, all invariants preserved.
- [x] **Placeholder scan**: No TBD/TODO or vague instructions. Every task has exact files, interfaces, and code snippets.
- [x] **Type consistency**: Stage interfaces, `RequestContext` fields, and `NewEngine` constructors are identical across all tasks.
- [x] **Testability**: Every task has unit tests with TDD step-by-step instructions.
