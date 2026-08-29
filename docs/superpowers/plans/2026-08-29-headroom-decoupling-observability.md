# Headroom Engine Decoupling & Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Decouple the Headroom Engine from the RTK Suite/CommandCrusher and the other compression stages into modular subpackages under `internal/headroom/stages/`, and add structured `slog` observability to the pipeline and every stage.

**Architecture:** `internal/headroom` becomes a pure pipeline orchestrator with zero imports of concrete stages. Each transformation (CommandCrusher, SmartCrusher, CodeCompressor, OutputShaper, CCR) is isolated in its own subpackage implementing `headroom.Stage`. Observability flows through a request-scoped `*slog.Logger` on `RequestContext`, emitting guarded per-rewrite DEBUG records and one DEBUG summary per request, correlated by `request_id`.

**Tech Stack:** Go 1.27rc2 (`go.mod`), `log/slog`, standard library `testing`.

**Spec:** `docs/superpowers/specs/2026-08-29-headroom-decoupling-observability-design.md`

## Global Constraints

- **Green at every commit.** Each task ends with a compiling tree and a passing `go test -race ./...`. The task order below exists specifically to guarantee this: the engine is decoupled *before* any stage file moves, because `engine.go` constructs the concrete stages today and deleting them first breaks the build.
- **Move tests, never rewrite them.** `command_crusher_test.go` (~24 KB), `output_shaper_test.go` (~17 KB), `continuation_test.go`, `tabular_test.go`, and `ccr_test.go` carry the regression coverage that proves this refactor is a no-op. Each moves with its code; the only edits are the package clause, requalifying core helpers as `headroom.X`, and appending new log assertions.
- **Cross-stage tests need an external test package.** 14 existing tests in `internal/headroom` call `NewEngine` and assert on the *whole* pipeline (`engine_test.go`, `verbatim_test.go:16,46`, `ccr_test.go:183`, `continuation_test.go:351`, `benchmark_test.go:102,239`). Once the stages live in subpackages, a file declaring `package headroom` cannot import them — `stages/*` imports `headroom`, so that is an import cycle. These tests move to `package headroom_test` (external test package, same directory, no cycle), introduced in Task 2. They use only exported API today, so the move costs nothing but the package clause and a `headroom.` prefix.
- **Zero stage imports in core**: `internal/headroom` must not import any `internal/headroom/stages/*` package.
- **I1 (deterministic)**: compression stays pure and position-independent.
- **I2 (verbatim reads)**: verbatim file-read tool results stay byte-for-byte.
- **I3 (assistant text)**: assistant messages are never modified.
- **I4 (tool errors)**: `tool_result` blocks with `is_error: true` are never modified.
- **I5 (behavioral no-op)**: no compressed output changes anywhere in the series.
- **I6 (no cost when DEBUG is off)**: per-block logging sits behind a `Logger.Enabled(ctx, slog.LevelDebug)` guard resolved once per `Execute`.

---

### Task 1: Core Exports, Request-Scoped Logger, Pipeline Split

Pure core work. No files move between packages, so the tree stays green throughout.

**Files:**

- Modify: `internal/headroom/types.go` (remove `Pipeline`; add `Logger`, `RewritesCount`, `Log()`)
- Create: `internal/headroom/pipeline.go` (`Pipeline`, `NewPipeline`, `Run` moved out of `types.go`)
- Modify: `internal/headroom/walk.go` (export walkers; **receive `ErrorOrdinals`**)
- Modify: `internal/headroom/verbatim.go` (export `SkipVerbatim`, `WalkMessages`)
- Modify: `internal/headroom/engine.go` (export `FrozenPrefixIndex`)
- Modify: `internal/headroom/command_crusher.go` (delete `errorOrdinals`; call `ErrorOrdinals`)
- Modify: `internal/headroom/ccr_stage.go`, `smart_crusher.go`, `code_compressor.go`, `output_shaper.go` (call the exported names)
- Modify: `internal/headroom/types_test.go`, `walk_test.go`, `verbatim_test.go`, `command_crusher_test.go` (call the exported names)

**Interfaces produced:**

- `headroom.WalkToolResultText(req map[string]any, from int, fn func(idx, ord int, get func() string, set func(string)))`
- `headroom.WalkToolUseBlocks(req map[string]any, fn func(idx int, block map[string]any))`
- `headroom.WalkMessages(req map[string]any, fn func(msg map[string]any))`
- `headroom.ErrorOrdinals(req map[string]any) map[int]bool` — body moved from `command_crusher.go` into `walk.go`
- `headroom.SkipVerbatim(reqCtx *RequestContext, cfg *Config, ord int) bool`
- `headroom.FrozenPrefixIndex(req map[string]any, liveTurns int) int`
- `(*RequestContext).Log() *slog.Logger` and field `Logger *slog.Logger`
- `RequestContext.RewritesCount int`, incremented by `RecordRewrite`

- [ ] **Step 1: Write failing tests for `Log()` nil-safety and rewrite counting**

```go
// internal/headroom/types_test.go
func TestRequestContext_LogIsNilSafe(t *testing.T) {
	reqCtx := &RequestContext{}
	if reqCtx.Log() == nil {
		t.Fatal("Log() must never return nil")
	}
}

func TestRequestContext_RecordRewriteCountsAndAccounts(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	reqCtx := &RequestContext{Logger: logger}

	reqCtx.RecordRewrite("hello world 12345", "hello")

	if reqCtx.BytesBefore != 17 || reqCtx.BytesAfter != 5 || reqCtx.RewritesCount != 1 {
		t.Fatalf("unexpected telemetry: before=%d after=%d rewrites=%d",
			reqCtx.BytesBefore, reqCtx.BytesAfter, reqCtx.RewritesCount)
	}
	if reqCtx.Log() != logger {
		t.Fatal("Log() must return the injected logger")
	}
}
```

- [ ] **Step 2: Run to confirm failure**

Run: `go test ./internal/headroom -run TestRequestContext`
Expected: FAIL — `Log` and `RewritesCount` are undefined.

- [ ] **Step 3: Apply the core changes**

1. Move `Pipeline`, `NewPipeline`, and `(*Pipeline).Run` verbatim from `types.go` into a new `pipeline.go`.
2. Add `Logger *slog.Logger` and `RewritesCount int` to `RequestContext`; add `Log()`; increment `RewritesCount` in `RecordRewrite`.
3. Rename the six helpers to their exported names. Bodies unchanged.
4. Cut the `errorOrdinals` body out of `command_crusher.go` and paste it into `walk.go` as `ErrorOrdinals`. This must happen now — Task 3 deletes `command_crusher.go`, and leaving the function there would carry core AST logic into the crusher package.
5. Update every in-package caller and test.

- [ ] **Step 4: Verify green**

Run: `go test -race ./internal/headroom && go build ./...`
Expected: PASS. `grep -rn "errorOrdinals\|walkToolResultText\|skipVerbatim\|frozenPrefixIndex\|walkMessages\|walkToolUseBlocks" internal/headroom` returns only doc-comment mentions.

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/
git commit -m "refactor(headroom): export AST helpers, split pipeline.go, add request-scoped logger"
```

---

### Task 2: Decouple the Engine and Move Store Ownership to the Server

Still no file moves. `NewEngine` stops constructing stages and stops owning the CCR store; `server.go` becomes the composition root. The concrete stages are still in package `headroom`, so this compiles cleanly on its own.

**Files:**

- Modify: `internal/headroom/engine.go`
- Modify: `internal/headroom/engine_test.go` (keep only mock-stage unit tests)
- Create: `internal/headroom/integration_test.go` (`package headroom_test`)
- Modify: `internal/headroom/verbatim_test.go`, `ccr_test.go`, `continuation_test.go` (relocate their `NewEngine` tests)
- Modify: `internal/headroom/benchmark_test.go` (`BenchmarkEngine_Process`, `BenchmarkEngineProcess_WithVerbatim` relocate)
- Modify: `internal/api/server.go`
- Modify: `internal/api/headroom_ccr_test.go`, `server_test.go`, `kimi_proxy_test.go`, `claudecode_proxy_test.go`, `claudecode_observability_test.go`, `headroom_proxy_test.go`

**Interfaces produced:**

- `headroom.NewEngine(cfg Config, logger *slog.Logger, stages ...Stage) *Engine`
- `Engine.CCRStore()` — **removed**
- `Server.ccrStore *headroom.CCRStore` (retyped to `*ccr.CCRStore` in Task 7)

- [ ] **Step 1: Write failing engine tests for injection, correlation, and the summary record**

```go
// internal/headroom/engine_test.go
type mockStage struct {
	name     string
	executed bool
	rewrite  bool
}

func (m *mockStage) Name() string { return m.name }
func (m *mockStage) Execute(_ context.Context, reqCtx *RequestContext, _ *Config) error {
	m.executed = true
	if m.rewrite {
		reqCtx.RecordRewrite("aaaaaaaaaa", "a")
	}
	return nil
}

func TestEngine_ProcessRunsInjectedStagesAndLogsSummary(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	stage := &mockStage{name: "mock", rewrite: true}
	engine := NewEngine(Config{Enabled: true}, logger, stage)

	reqCtx, err := engine.Process(context.Background(), map[string]any{"messages": []any{}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stage.executed {
		t.Fatal("injected stage did not execute")
	}
	if reqCtx.RewritesCount != 1 {
		t.Fatalf("expected 1 rewrite, got %d", reqCtx.RewritesCount)
	}

	out := buf.String()
	for _, want := range []string{"headroom compression summary", "request_id", "saved_bytes", "duration_ms"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary log missing %q; got: %s", want, out)
		}
	}
}

func TestEngine_ProcessAssignsDistinctRequestIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	engine := NewEngine(Config{Enabled: true}, logger, &mockStage{name: "mock", rewrite: true})

	_, _ = engine.Process(context.Background(), map[string]any{"messages": []any{}})
	_, _ = engine.Process(context.Background(), map[string]any{"messages": []any{}})

	if strings.Count(buf.String(), "request_id=1") != 1 || strings.Count(buf.String(), "request_id=2") != 1 {
		t.Fatalf("expected request_id 1 and 2 exactly once each; got: %s", buf.String())
	}
}

func TestEngine_ProcessNilLoggerFallsBackToDefault(t *testing.T) {
	engine := NewEngine(Config{Enabled: true}, nil, &mockStage{name: "mock"})
	if _, err := engine.Process(context.Background(), map[string]any{"messages": []any{}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
```

- [ ] **Step 2: Run to confirm failure**

Run: `go test ./internal/headroom -run TestEngine_Process`
Expected: FAIL — `NewEngine` takes one argument.

- [ ] **Step 3: Rewrite `engine.go` per spec §3.4**

Drop the `store` field and `CCRStore()`. Add `logger`, `seq atomic.Uint64`. Take `logger` and `stages ...Stage`. Bind `request_id` onto `reqCtx.Logger`. Emit the DEBUG summary after `pipeline.Run` when `BytesBefore > 0 || EffortClamped`, and `Error` on pipeline failure. Do **not** add an early return on `!cfg.Enabled` — `Pipeline.Run` already short-circuits, and callers read `FrozenPrefixIndex` and `Verbatim` off the returned context.

- [ ] **Step 4: Relocate cross-stage tests into `package headroom_test`**

Create `internal/headroom/integration_test.go`:

```go
package headroom_test

import (
	"context"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/headroom"
)

// testStages returns the production stage list. Tasks 3-7 update this single
// helper as each stage moves into its own subpackage.
func testStages(store *headroom.CCRStore) []headroom.Stage {
	return []headroom.Stage{
		headroom.NewCCRStage(store),
		&headroom.CommandCrusherStage{},
		&headroom.SmartCrusherStage{},
		&headroom.CodeCompressorStage{},
		&headroom.OutputShaperStage{},
	}
}

func newTestEngine(cfg headroom.Config) *headroom.Engine {
	store := headroom.NewCCRStoreFromMB(cfg.CCR.MaxStoreMB)
	return headroom.NewEngine(cfg, nil, testStages(store)...)
}

// fullConfig and toolResultMsg move verbatim from engine_test.go, gaining
// only the headroom. qualification.
func fullConfig() headroom.Config {
	return headroom.Config{
		Enabled: true, SmartCrusher: true, CodeCompressor: true, LiveTurns: 2,
		OutputShaper: headroom.OutputShaperConfig{
			Enabled: true, VerbositySteering: true, EffortRouting: true,
			MechanicalThinkingBudget: 1024,
		},
	}
}

func toolResultMsg(payload string) any {
	return map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "tu_" + payload[:1], "content": payload},
	}}
}
```

Move into this file, changing only the package clause and adding `headroom.` qualifications: every test in `engine_test.go` that calls `NewEngine` with real stages, the two in `verbatim_test.go` (lines 16, 46), `ccr_test.go:183`, `continuation_test.go:351`, and `BenchmarkEngine_Process` / `BenchmarkEngineProcess_WithVerbatim`. `engine_test.go` keeps only the mock-stage tests from Step 1.

This is the step that makes the later moves possible: a `package headroom` file can never import `stages/*` without an import cycle, so the full-pipeline tests must live outside the package before any stage leaves it.

- [ ] **Step 5: Update `internal/api/server.go`**

```go
srv.ccrStore = headroom.NewCCRStoreFromMB(cfg.Headroom.CCR.MaxStoreMB)

srv.headroom = headroom.NewEngine(
	cfg.Headroom,
	srv.logger,
	headroom.NewCCRStage(srv.ccrStore),
	&headroom.CommandCrusherStage{},
	&headroom.SmartCrusherStage{},
	&headroom.CodeCompressorStage{},
	&headroom.OutputShaperStage{},
)
```

Add the `ccrStore *headroom.CCRStore` field to `Server`. Repoint `server.go:1907`, `:1911`, `:1914` from `server.headroom.CCRStore()` to `server.ccrStore`. In the `Process` call site (`server.go:736-752`), delete the `server.logger.Debug("headroom compressed request", ...)` block — the engine owns that record now — while keeping the `Warn` on error and the `tracker.RecordHeadroom` call. Guard the tracker call on `hrCtx.BytesBefore > 0 || hrCtx.EffortClamped` as it is today.

- [ ] **Step 6: Repoint the API test call sites**

Replace `srv.headroom.CCRStore()` / `server.headroom.CCRStore()` / `engine.CCRStore()` with the owning server's `ccrStore` field (or a locally held store where the test builds an engine directly) in: `headroom_ccr_test.go` (4 sites), `server_test.go` (2), `kimi_proxy_test.go` (2), `claudecode_proxy_test.go` (2), `claudecode_observability_test.go` (1). Update every `headroom.NewEngine(...)` call in `internal/api/*_test.go` to the three-argument form.

- [ ] **Step 7: Verify green**

Run: `go build ./... && go test -race ./...`
Expected: PASS across all packages.

- [ ] **Step 8: Commit**

```bash
git add internal/headroom/ internal/api/
git commit -m "refactor(headroom): inject stages and logger into Engine; move CCR store ownership to server"
```

---

### Task 3: Move CommandCrusher to `internal/headroom/stages/crusher`

From here on, one stage per task. Each task moves files, requalifies core helpers, adds guarded logging, updates the one `server.go` import, and ends green.

**Files:**

- Create: `internal/headroom/stages/crusher/stage.go` (from `command_crusher.go`: `CommandCrusherStage`, `CrushCommandOutput`, `Execute`)
- Create: `internal/headroom/stages/crusher/detector.go` (`signature`, `detectSignature`, `signatureScanCap`)
- Create: `internal/headroom/stages/crusher/lines.go` (`filterLines`, `dedupeLines`, `nextLine`, `isLowerHex`, `hasCommitLine`, `hasCargoVerbLine`, `cargoVerbs`)
- Move: `crusher_git.go`, `crusher_gorust.go`, `crusher_javascript.go`, `crusher_python.go` → `stages/crusher/`
- Move: `command_crusher_test.go` → `stages/crusher/crusher_test.go`
- Move: crusher benchmarks out of `benchmark_test.go` → `stages/crusher/bench_test.go`
- Delete: `internal/headroom/command_crusher.go`, `crusher_git.go`, `crusher_gorust.go`, `crusher_javascript.go`, `crusher_python.go`, `command_crusher_test.go`
- Modify: `internal/api/server.go` (import `stages/crusher`; `crusher.NewStage()`)
- Modify: `internal/headroom/integration_test.go` (point `testStages()` at the moved stage)

**Interfaces produced:** `crusher.NewStage() *CommandCrusherStage`, `crusher.CrushCommandOutput(text string) (string, bool)`

- [ ] **Step 1: Add the failing log-assertion test**

Append to the moved `crusher_test.go`:

```go
func TestCommandCrusherStage_LogsRewrite(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	req := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":    "tool_result",
						"content": "...........................\n=== 5 passed in 0.05s ===\n",
					},
				},
			},
		},
	}
	reqCtx := &headroom.RequestContext{Request: req, FrozenPrefixIndex: -1, Logger: logger}
	cfg := &headroom.Config{Enabled: true, CommandCrusher: true}

	if err := NewStage().Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"command_crusher compressed tool output", "signature", "ordinal", "bytes_before"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q; got: %s", want, out)
		}
	}
}

func TestCommandCrusherStage_SilentWhenDebugDisabled(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	req := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":    "tool_result",
						"content": "...........................\n=== 5 passed in 0.05s ===\n",
					},
				},
			},
		},
	}
	reqCtx := &headroom.RequestContext{Request: req, FrozenPrefixIndex: -1, Logger: logger}
	cfg := &headroom.Config{Enabled: true, CommandCrusher: true}

	if err := NewStage().Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reqCtx.RewritesCount == 0 {
		t.Fatal("expected the rewrite to still happen with logging off")
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no output above DEBUG, got: %s", buf.String())
	}
}
```

- [ ] **Step 2: Run to confirm failure**

Run: `go test ./internal/headroom/stages/crusher/...`
Expected: FAIL — no such package.

- [ ] **Step 3: Move the code**

`git mv` the four `crusher_*.go` files and the test into the new directory, then split `command_crusher.go` into `stage.go`, `detector.go`, and `lines.go`. Change the package clause to `crusher`. Requalify `ErrorOrdinals`, `WalkToolResultText`, `SkipVerbatim`, `RequestContext`, and `Config` as `headroom.X`. `detectSignature` returns the signature already; log `sig.String()` — add a `String()` method on `signature` if one does not exist.

The `Execute` body becomes:

```go
func (s *CommandCrusherStage) Execute(ctx context.Context, reqCtx *headroom.RequestContext, cfg *headroom.Config) error {
	if !cfg.CommandCrusher || reqCtx == nil || reqCtx.Request == nil {
		return nil
	}
	log := reqCtx.Log()
	dbg := log.Enabled(ctx, slog.LevelDebug)

	errOrds := headroom.ErrorOrdinals(reqCtx.Request)
	headroom.WalkToolResultText(reqCtx.Request, 0, func(_, ord int, get func() string, set func(string)) {
		if errOrds[ord] {
			return // invariant I4
		}
		if headroom.SkipVerbatim(reqCtx, cfg, ord) {
			reqCtx.VerbatimSkipped++
			return // invariant I2
		}
		before := get()
		after, changed := CrushCommandOutput(before)
		if !changed {
			return
		}
		set(after)
		reqCtx.RecordRewrite(before, after)
		if dbg {
			log.Debug("command_crusher compressed tool output",
				"stage", s.Name(),
				"signature", detectSignature(before).String(),
				"ordinal", ord,
				"bytes_before", len(before),
				"bytes_after", len(after))
		}
	})
	return nil
}
```

The guard ordering (`errOrds` then `SkipVerbatim` then rewrite) and the `VerbatimSkipped` increment must match the current `command_crusher.go:59-80` exactly — this is a move, not a rewrite.

- [ ] **Step 4: Rewire and verify**

Update `server.go` to `crusher.NewStage()`. Run:

```bash
go test -race ./internal/headroom/... && go test -race ./internal/api/... && go build ./...
```

Expected: PASS. The moved regression tests must pass **unmodified in substance** — if any assertion needs its expected output changed, stop: that is a behavior change and violates I5.

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/stages/crusher/ internal/api/server.go
git rm internal/headroom/command_crusher.go internal/headroom/crusher_git.go \
       internal/headroom/crusher_gorust.go internal/headroom/crusher_javascript.go \
       internal/headroom/crusher_python.go internal/headroom/command_crusher_test.go
git commit -m "feat(headroom): extract CommandCrusher into stages/crusher with guarded debug logging"
```

---

### Task 4: Move SmartCrusher to `internal/headroom/stages/smart`

**Files:**

- Create: `internal/headroom/stages/smart/stage.go` (from `smart_crusher.go`)
- Move: `tabular.go` → `stages/smart/tabular.go`
- Move: `smart_crusher_test.go` → `stages/smart/smart_test.go`; `tabular_test.go` → `stages/smart/tabular_test.go`
- Move: `BenchmarkSmartCrusher_CompactJSON`, `BenchmarkTabularConversion` → `stages/smart/bench_test.go`
- Delete: `internal/headroom/smart_crusher.go`, `tabular.go`, `smart_crusher_test.go`, `tabular_test.go`
- Modify: `internal/api/server.go`
- Modify: `internal/headroom/integration_test.go` (point `testStages()` at the moved stage)

**Interfaces produced:** `smart.NewStage() *SmartCrusherStage`, `smart.CompactJSON`, `smart.TryTabularConversion`, `smart.DefaultMinTabularSavings`

- [ ] **Step 1: Add the failing log-assertion test**

```go
func TestSmartCrusherStage_LogsCompaction(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	req := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":    "tool_result",
						"content": "{\n  \"foo\": \"bar\",\n  \"baz\": 123\n}",
					},
				},
			},
		},
	}
	reqCtx := &headroom.RequestContext{Request: req, FrozenPrefixIndex: -1, Logger: logger}
	cfg := &headroom.Config{Enabled: true, SmartCrusher: true}

	if err := NewStage().Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "smart_crusher compacted tool output") ||
		!strings.Contains(buf.String(), "mode=compact") {
		t.Fatalf("expected compaction log, got: %s", buf.String())
	}
}
```

- [ ] **Step 2: Run to confirm failure** — `go test ./internal/headroom/stages/smart/...`
- [ ] **Step 3: Move the code**, requalify core helpers, and add the guarded DEBUG record. `mode` distinguishes the two transforms this stage applies:

```go
log := reqCtx.Log()
dbg := log.Enabled(ctx, slog.LevelDebug)

headroom.WalkToolResultText(reqCtx.Request, 0, func(_, ord int, get func() string, set func(string)) {
	if headroom.SkipVerbatim(reqCtx, cfg, ord) {
		reqCtx.VerbatimSkipped++
		return
	}
	before := get()
	after, mode, changed := s.transform(before, cfg) // mode is "compact" or "tabular"
	if !changed {
		return
	}
	set(after)
	reqCtx.RecordRewrite(before, after)
	if dbg {
		log.Debug("smart_crusher compacted tool output",
			"stage", s.Name(), "mode", mode, "ordinal", ord,
			"bytes_before", len(before), "bytes_after", len(after))
	}
})
```

`transform` is the existing `CompactJSON` / `TryTabularConversion` decision in `smart_crusher.go:34-60`, extracted only so the mode label is available to the log. Its branch order and thresholds are unchanged.
- [ ] **Step 4: Rewire `server.go` to `smart.NewStage()`; run `go test -race ./... && go build ./...`**
- [ ] **Step 5: Commit**

```bash
git add internal/headroom/stages/smart/ internal/api/server.go
git rm internal/headroom/smart_crusher.go internal/headroom/tabular.go \
       internal/headroom/smart_crusher_test.go internal/headroom/tabular_test.go
git commit -m "feat(headroom): extract SmartCrusher into stages/smart with guarded debug logging"
```

---

### Task 5: Move CodeCompressor to `internal/headroom/stages/code`

**Files:**

- Create: `internal/headroom/stages/code/stage.go` (from `code_compressor.go`)
- Move: `code_compressor_test.go` → `stages/code/code_test.go`
- Move: `BenchmarkCodeCompressor_PruneText` → `stages/code/bench_test.go`
- Delete: `internal/headroom/code_compressor.go`, `code_compressor_test.go`
- Modify: `internal/api/server.go`
- Modify: `internal/headroom/integration_test.go` (point `testStages()` at the moved stage)

**Interfaces produced:** `code.NewStage() *CodeCompressorStage`, `code.PruneText(input string) string`

- [ ] **Step 1: Add the failing log-assertion test**

```go
func TestCodeCompressorStage_LogsPruning(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	req := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type":    "tool_result",
						"content": "function test() {\n\n\n\n  return true;\n\n\n\n}",
					},
				},
			},
		},
	}
	reqCtx := &headroom.RequestContext{Request: req, FrozenPrefixIndex: -1, Logger: logger}
	cfg := &headroom.Config{Enabled: true, CodeCompressor: true}

	if err := NewStage().Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "code_compressor pruned tool output") {
		t.Fatalf("expected pruning log, got: %s", buf.String())
	}
}
```

- [ ] **Step 2: Run to confirm failure** — `go test ./internal/headroom/stages/code/...`
- [ ] **Step 3: Move the code**, requalify core helpers, and add the guarded DEBUG record:

```go
log := reqCtx.Log()
dbg := log.Enabled(ctx, slog.LevelDebug)

headroom.WalkToolResultText(reqCtx.Request, 0, func(_, ord int, get func() string, set func(string)) {
	if headroom.SkipVerbatim(reqCtx, cfg, ord) {
		reqCtx.VerbatimSkipped++
		return
	}
	before := get()
	after := PruneText(before)
	if after == before {
		return
	}
	set(after)
	reqCtx.RecordRewrite(before, after)
	if dbg {
		log.Debug("code_compressor pruned tool output",
			"stage", s.Name(), "ordinal", ord,
			"bytes_before", len(before), "bytes_after", len(after))
	}
})
```
- [ ] **Step 4: Rewire `server.go` to `code.NewStage()`; run `go test -race ./... && go build ./...`**
- [ ] **Step 5: Commit**

```bash
git add internal/headroom/stages/code/ internal/api/server.go
git rm internal/headroom/code_compressor.go internal/headroom/code_compressor_test.go
git commit -m "feat(headroom): extract CodeCompressor into stages/code with guarded debug logging"
```

---

### Task 6: Move OutputShaper to `internal/headroom/stages/shaper`

**Files:**

- Create: `internal/headroom/stages/shaper/stage.go` (from `output_shaper.go`, including `continuationKind`, `classifyContinuation`, `isCodingToolName`, `looksLikeTestOutput`)
- Move: `output_shaper_test.go` → `stages/shaper/shaper_test.go`; `continuation_test.go` → `stages/shaper/continuation_test.go`
- Delete: `internal/headroom/output_shaper.go`, `output_shaper_test.go`, `continuation_test.go`
- Modify: `internal/api/server.go`
- Modify: `internal/headroom/integration_test.go` (point `testStages()` at the moved stage)

**Interfaces produced:** `shaper.NewStage() *OutputShaperStage`, `shaper.DefaultVerbosityPrompt`

`classifyContinuation` takes a `*ToolInspector`; that type and its `Lookup`/`IsVerbatimOrdinal`/`VerbatimCount` methods are already exported, so no further core changes are needed.

- [ ] **Step 1: Add the failing log-assertion test**

```go
func TestOutputShaperStage_LogsEffortClamp(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	req := map[string]any{
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 4096},
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "content": "mkdir /foo/bar"},
				},
			},
		},
	}
	reqCtx := &headroom.RequestContext{
		Request:  req,
		Logger:   logger,
		Verbatim: headroom.NewToolInspector(req),
	}
	cfg := &headroom.Config{
		Enabled: true,
		OutputShaper: headroom.OutputShaperConfig{
			Enabled:                  true,
			EffortRouting:            true,
			MechanicalThinkingBudget: 1024,
		},
	}

	if err := NewStage().Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reqCtx.EffortClamped {
		t.Fatal("expected effort clamp")
	}
	for _, want := range []string{"output_shaper clamped thinking budget", "continuation_kind", "original_budget", "clamped_budget"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("log missing %q; got: %s", want, buf.String())
		}
	}
}
```

- [ ] **Step 2: Run to confirm failure** — `go test ./internal/headroom/stages/shaper/...`
- [ ] **Step 3: Move the code**, requalify, and add the DEBUG record at the end of `clampEffort`. This fires at most once per request, so no `Enabled` guard is required:

```go
func (s *OutputShaperStage) clampEffort(reqCtx *headroom.RequestContext, cfg *headroom.Config) {
	// ... existing classification and clamping, unchanged ...
	reqCtx.EffortClamped = true
	reqCtx.OriginalThinking = original
	reqCtx.ClampedThinking = clamped
	reqCtx.ContinuationKind = kind.String()

	reqCtx.Log().Debug("output_shaper clamped thinking budget",
		"stage", s.Name(),
		"continuation_kind", kind.String(),
		"original_budget", original,
		"clamped_budget", clamped)
}
```

`clampEffort` currently takes `(reqCtx *RequestContext, cfg *Config)` and has no receiver use; keep the signature and add the receiver only if `s.Name()` is wanted in the record.
- [ ] **Step 4: Rewire `server.go` to `shaper.NewStage()`; run `go test -race ./... && go build ./...`**
- [ ] **Step 5: Commit**

```bash
git add internal/headroom/stages/shaper/ internal/api/server.go
git rm internal/headroom/output_shaper.go internal/headroom/output_shaper_test.go \
       internal/headroom/continuation_test.go
git commit -m "feat(headroom): extract OutputShaper into stages/shaper with structured logging"
```

---

### Task 7: Move CCR Stage and Store to `internal/headroom/stages/ccr`

Last stage to move, because the store is the widest-reaching symbol.

**Files:**

- Create: `internal/headroom/stages/ccr/stage.go` (from `ccr_stage.go`)
- Create: `internal/headroom/stages/ccr/store.go` (from `ccr_store.go`)
- Move: `ccr_test.go` → `stages/ccr/ccr_test.go`; `ccr_store_test.go` → `stages/ccr/store_test.go`
- Move: `BenchmarkCCRStore_PutGet` → `stages/ccr/bench_test.go`
- Delete: `internal/headroom/ccr_stage.go`, `ccr_store.go`, `ccr_test.go`, `ccr_store_test.go`
- Modify: `internal/api/server.go` (retype `ccrStore` to `*ccr.CCRStore`)
- Modify: `internal/headroom/integration_test.go` (point `testStages()` at the moved stage)
- Modify: `internal/api/headroom_ccr_test.go` (`headroom.ChunkID` → `ccr.ChunkID`, 4 sites)
- Modify: any `internal/api/*_test.go` that names `headroom.CCRStore` as a type

**Interfaces produced:** `ccr.NewStage(store *CCRStore) *CCRStage`, `ccr.NewCCRStore`, `ccr.NewCCRStoreFromMB`, `ccr.ChunkID`, `ccr.FormatChunkToken`, `ccr.RetrieveToolDefinition`

- [ ] **Step 1: Add the failing log-assertion test**

```go
func TestCCRStage_LogsChunkDemotion(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	bigPayload := strings.Repeat("hello world data\n", 500)
	req := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "content": bigPayload},
				},
			},
			map[string]any{"role": "assistant", "content": "turn 1"},
			map[string]any{"role": "user", "content": "turn 2 live"},
		},
	}
	reqCtx := &headroom.RequestContext{Request: req, FrozenPrefixIndex: 0, Logger: logger}
	cfg := &headroom.Config{
		Enabled: true,
		CCR:     headroom.CCRConfig{Enabled: true, MinChunkBytes: 100},
	}

	if err := NewStage(NewCCRStoreFromMB(10)).Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reqCtx.ChunksStored == 0 {
		t.Fatal("expected a chunk to be stored")
	}
	for _, want := range []string{"ccr demoted chunk", "chunk_id", "chunk_bytes", "store_bytes"} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("log missing %q; got: %s", want, buf.String())
		}
	}
}
```

- [ ] **Step 2: Run to confirm failure** — `go test ./internal/headroom/stages/ccr/...`
- [ ] **Step 3: Move the code**, requalify core helpers, and add the guarded DEBUG record inside the demotion callback (`ccr_stage.go:80-104`):

```go
log := reqCtx.Log()
dbg := log.Enabled(ctx, slog.LevelDebug)

// ... inside the existing WalkToolResultText callback, after set(token):
reqCtx.RecordRewrite(before, token)
reqCtx.ChunksStored++
if dbg {
	log.Debug("ccr demoted chunk",
		"stage", s.Name(), "chunk_id", id,
		"ordinal", ord, "message_index", idx,
		"chunk_bytes", len(before), "store_bytes", s.store.Bytes())
}
```

The `FrozenPrefixIndex` gate, the `SkipVerbatim` gate, the `minBytes` floor, and the retrieve-tool injection block below the walk all move unchanged.
- [ ] **Step 4: Rewire the server**

```go
srv.ccrStore = ccr.NewCCRStoreFromMB(cfg.Headroom.CCR.MaxStoreMB)
```

Retype the `Server.ccrStore` field to `*ccr.CCRStore` and switch `headroom.NewCCRStage(...)` to `ccr.NewStage(srv.ccrStore)`. Repoint `headroom.ChunkID` in `headroom_ccr_test.go`. In `integration_test.go`, `testStages` changes signature from `testStages(store *headroom.CCRStore)` to `testStages(store *ccr.CCRStore)`, and `newTestEngine` builds the store with `ccr.NewCCRStoreFromMB`. After this task no `headroom.CCRStore` identifier remains anywhere in the tree:

```bash
grep -rn "headroom\.CCRStore\|headroom\.ChunkID\|headroom\.NewCCRStage\|headroom\.NewCCRStore" internal || echo "OK: no stale CCR references"
```

- [ ] **Step 5: Verify** — `go test -race ./... && go build ./...`
- [ ] **Step 6: Commit**

```bash
git add internal/headroom/stages/ccr/ internal/api/
git rm internal/headroom/ccr_stage.go internal/headroom/ccr_store.go \
       internal/headroom/ccr_test.go internal/headroom/ccr_store_test.go
git commit -m "feat(headroom): extract CCR stage and store into stages/ccr with structured logging"
```

---

### Task 8: Final Verification

No production code changes. This task proves the invariants and cleans up whatever the moves left behind.

**Files:**

- Modify: `internal/headroom/benchmark_test.go` (keep only `BenchmarkEngine_Process`, `BenchmarkEngineProcess_WithVerbatim`, `BenchmarkToolInspector_Build`; the rest moved out in Tasks 3-7)

- [ ] **Step 1: Prove the core is clean (I7)**

```bash
go list -f '{{join .Imports "\n"}}' ./internal/headroom | grep 'headroom/stages' && echo "FAIL: core imports a stage" || echo "OK: core is stage-free"
```

- [ ] **Step 2: Prove nothing was orphaned**

```bash
ls internal/headroom/*.go
```

Expected exactly: `engine.go`, `pipeline.go`, `types.go`, `verbatim.go`, `walk.go`, plus `engine_test.go`, `integration_test.go`, `types_test.go`, `verbatim_test.go`, `walk_test.go`, `benchmark_test.go`.

- [ ] **Step 3: Full gate**

```bash
go build ./... && go vet ./... && go test -race ./... && gofmt -l internal | tee /dev/stderr | wc -l
```

Expected: build clean, vet clean, all tests pass, zero unformatted files.

- [ ] **Step 4: Prove I6 — no cost when DEBUG is off**

```bash
go test -run XXX -bench . -benchmem ./internal/headroom/... > /tmp/after.txt
```

Compare `allocs/op` against the pre-refactor baseline captured before Task 1. Per-block stage benchmarks must show no allocation increase at the default (non-DEBUG) level.

- [ ] **Step 5: Prove I5 — behavioral no-op**

Confirm the diff of every moved `_test.go` contains only: package clause, import block, `headroom.` qualifications, and appended log tests. Any changed expected-output literal is a behavior regression, not a refactor.

- [ ] **Step 6: Commit**

```bash
git add internal/headroom/benchmark_test.go
git commit -m "test(headroom): scope core benchmarks to engine and inspector after stage extraction"
```

---

## Baseline Capture (before Task 1)

```bash
go test -race ./... > /tmp/headroom-baseline-tests.txt 2>&1
go test -run XXX -bench . -benchmem ./internal/headroom/... > /tmp/headroom-baseline-bench.txt 2>&1
```

Task 8 Step 4 compares against `/tmp/headroom-baseline-bench.txt`.

---

## Plan Self-Review Checklist

- [x] **Ordering**: engine decoupling (Task 2) precedes every file move, so `engine.go` never references a deleted type. Every task ends compiling and green.
- [x] **Spec coverage**: five stages extracted, core made stage-free, store ownership moved to the server, guarded DEBUG logging plus a `request_id`-correlated summary, all invariants carried into explicit verification steps.
- [x] **Placeholder scan**: no TBD/TODO, no "similar to Task N", no prose-only code steps. Every stage-move task carries the actual `Execute` or log-call body it must produce. `pipeline.go` is created unconditionally in Task 1; `benchmark_test.go` is split across Tasks 3-7 and scoped in Task 8.
- [x] **Call-site completeness**: `Engine.CCRStore()` (3 non-test + 11 test sites) and `headroom.ChunkID` (4 sites) are named with their files. Task 7 Step 4 ends with a `grep` that fails loudly if any stale `headroom.CCRStore*` identifier survives.
- [x] **Test preservation**: every existing test file is moved and edited, never recreated; Task 8 Step 5 audits the diffs for it.
- [x] **Type consistency**: `NewEngine(cfg, logger, stages...)`, `Stage`, and `RequestContext` are identical across tasks. Two signatures change type mid-series and both are called out where it happens: `Server.ccrStore` and `testStages(store)` go from `*headroom.CCRStore` to `*ccr.CCRStore` in Task 7.
- [x] **Compilable snippets**: every Go block was written against the current signatures in `internal/headroom`.
- [x] **Task right-sizing**: eight tasks, 45 steps. Each task ends at a reviewable boundary — a passing `go test -race ./...` on a compiling tree — and a reviewer could reject any one stage move while accepting its neighbors.
