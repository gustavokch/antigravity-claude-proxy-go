# PR #44 Review Remediation — Headroom Stage Decoupling

**PR:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/44
**Branch:** `feat/headroom-decoupling-observability`
**Goal:** Resolve the review findings on the headroom stage extraction without changing pipeline behavior.

## Architecture

`internal/headroom` stays the pure orchestrator (engine, pipeline, walk, verbatim/ToolInspector).
`internal/headroom/stages/{ccr,crusher,smart,code,shaper}` hold the concrete stages and depend on
`headroom` one way only. Shared payload heuristics belong to `headroom`, not to a stage package.

## Tech Stack

Go 1.x, `log/slog`, standard `testing`. Gate: `go build ./... && go vet ./... && go test ./... -race`.

## Findings to resolve

| # | Location | Severity | Finding |
|---|----------|----------|---------|
| 1 | `stages/shaper/stage.go:188` | risk | `Execute` misses the nil `reqCtx`/`Request` guard; `applySteering` panics on a nil map |
| 2 | `engine.go:76` | risk | Summary `Debug` allocates varargs and boxes values when DEBUG is off (breaks I6) |
| 3 | `stages/ccr/store.go:73` | risk | `Put` returns a cached id on a 48-bit id match without comparing content |
| 4 | `stages/shaper/stage.go` + `verbatim.go` | risk | Six helpers duplicated across packages; ordinal accounting can drift |
| 5 | `stages/ccr/stage.go:117` | nit | Redundant `tools != nil` before `len(tools) > 0` |

Out of scope (reported, not fixed): CCR store is not resized by `applyHeadroomConfig`
(pre-existing); per-request `logger.With` allocation.

---

## Task 1 — Shaper nil guard

**Modify:** `internal/headroom/stages/shaper/stage.go`
**Test:** `internal/headroom/stages/shaper/shaper_test.go`

- Step 1 (Red): add `TestOutputShaperStage_NilRequestIsNoOp` calling `Execute` with a
  `&headroom.RequestContext{}` (nil `Request`) and verbatim steering enabled; expect no panic, no error.
- Step 2: `go test ./internal/headroom/stages/shaper/ -run NilRequest` — must panic/fail.
- Step 3 (Green): add `if reqCtx == nil || reqCtx.Request == nil { return nil }` to `Execute`,
  matching the other four stages.
- Step 4: re-run the test — pass.
- Step 5: `git commit -m "fix(headroom): guard shaper stage against nil request context"`

## Task 2 — Guard the pipeline summary log

**Modify:** `internal/headroom/engine.go`
**Test:** `internal/headroom/engine_test.go`

- Step 1 (Red): add `TestEngine_SummaryLogAllocatesNothingWhenDebugDisabled` using
  `testing.AllocsPerRun` over `Process` with an INFO-level handler and a stage that records a
  rewrite; assert the allocation count does not exceed the same run with a no-rewrite request.
- Step 2: `go test ./internal/headroom/ -run SummaryLog` — must fail.
- Step 3 (Green): wrap the summary block in `if reqCtx.Log().Enabled(ctx, slog.LevelDebug)`.
- Step 4: re-run — pass. Existing `TestEngine_DebugSummary*` tests must stay green.
- Step 5: `git commit -m "perf(headroom): skip pipeline summary log construction when debug is off"`

## Task 3 — Collision-safe CCR store writes

**Modify:** `internal/headroom/stages/ccr/store.go`
**Test:** `internal/headroom/stages/ccr/store_test.go`

- Step 1 (Red): add `TestCCRStore_IDCollisionKeepsNewestContent` driving the internal
  `putWithID` helper with one id and two different payloads; assert `Get(id)` returns the
  second payload and `Bytes()` accounts for it exactly once.
- Step 2: `go test ./internal/headroom/stages/ccr/ -run Collision` — must fail.
- Step 3 (Green): extract `putWithID(id, content)`; on a cache hit compare the stored value,
  replace it and adjust `currentBytes` when it differs, then promote.
- Step 4: re-run — pass, LRU and concurrency tests still green.
- Step 5: `git commit -m "fix(headroom): keep CCR store content-correct on truncated id collision"`

## Task 4 — De-duplicate shaper heuristics

**Modify:** `internal/headroom/verbatim.go`, `internal/headroom/walk.go`,
`internal/headroom/stages/shaper/stage.go`
**Test:** `internal/headroom/verbatim_test.go`, `internal/headroom/stages/shaper/*_test.go`

- Step 1 (Red): add `TestExportedPayloadHelpers` in `headroom` covering
  `CountTextPayloads`, `LooksLikeNumberedSource`, `LooksLikeUnifiedDiff`, `NormalizeToolName`
  against the shaper fixtures.
- Step 2: `go test ./internal/headroom/ -run ExportedPayloadHelpers` — must fail to compile.
- Step 3 (Green): export the four helpers from `headroom`, keep unexported aliases where the
  package already calls them, delete the shaper copies plus `numberedLineRe`/`minNumberedLines`
  and call the exported versions.
- Step 4: `go test ./internal/headroom/... -race` — all green, shaper classification unchanged.
- Step 5: `git commit -m "refactor(headroom): share payload heuristics between engine and shaper"`

## Task 5 — CCR tools nit

**Modify:** `internal/headroom/stages/ccr/stage.go`

- Step 1: no new test; `TestCCR_ToolInjection*` already covers both branches.
- Step 2 (Green): `if tools, ok := reqCtx.Request["tools"].([]any); ok && len(tools) > 0 {`.
- Step 3: `go test ./internal/headroom/... ./internal/api/...` — green.
- Step 4: `git commit -m "style(headroom): drop redundant nil check on CCR tools list"`

## Verification

1. `go build ./...`
2. `go vet ./...`
3. `go test ./... -race`
4. `go test ./internal/headroom/... -bench . -benchmem` — no allocation regression.
5. `git push origin feat/headroom-decoupling-observability`
