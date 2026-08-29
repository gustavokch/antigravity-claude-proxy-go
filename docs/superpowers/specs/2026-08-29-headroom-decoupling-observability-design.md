# Design Specification: Headroom Engine Decoupling & Observability Enhancement

**Date**: 2026-08-29
**Status**: Approved (revision 2)
**Scope**: Architectural Refactoring & Observability

---

## 1. Overview & Goals

This specification defines the architectural refactoring that decouples the **Headroom Engine** orchestration core from the individual transformation stages (most notably the **RTK Suite / CommandCrusher** pattern compression engine) into dedicated modular subpackages under `internal/headroom/stages/`. It also establishes structured observability across all transformation stages and the core pipeline.

### Core Objectives

1. **Decouple the Headroom Engine core**: `internal/headroom` imports no package under `internal/headroom/stages/`. It becomes a pluggable pipeline coordinator only.
2. **Modular stage isolation**: CommandCrusher, SmartCrusher, CodeCompressor, OutputShaper, and CCR each move into a self-contained subpackage under `internal/headroom/stages/`.
3. **Structured observability**: Instrument the pipeline and every stage with `*slog.Logger`, emitting per-rewrite DEBUG records and a per-request DEBUG summary, all correlated by `request_id`.
4. **Preserve every safety invariant**: `is_error` tool results, verbatim file-read payloads, and position-independent prompt cache stability remain exactly as they are today.

### Non-Goals

- No new metrics. `stats.HeadroomSample` and the existing tracker plumbing are unchanged; per-stage byte savings are exposed through logs only.
- No change to compression behavior. Every stage produces byte-identical output before and after the move. This refactor is observably a no-op on the wire.
- No new configuration surface. Log verbosity is controlled by the server's existing `*slog.Logger` level.

---

## 2. Architecture & Package Layout

```
internal/
├── headroom/                          # Core orchestration engine (imports no stage)
│   ├── engine.go                      # Engine lifecycle, Process(), UpdateConfig(), request_id
│   ├── pipeline.go                    # Pipeline runner (extracted from types.go)
│   ├── types.go                       # Stage interface, RequestContext, Config types
│   ├── verbatim.go                    # ToolInspector, SkipVerbatim
│   ├── walk.go                        # WalkToolResultText, WalkToolUseBlocks, ErrorOrdinals
│   ├── engine_test.go                 # Engine orchestration tests (mock stages)
│   ├── integration_test.go            # package headroom_test — full-pipeline tests
│   ├── types_test.go
│   ├── walk_test.go
│   ├── verbatim_test.go
│   └── benchmark_test.go              # Engine + ToolInspector benchmarks only
└── headroom/stages/                   # Pluggable transformation stages
    ├── crusher/                       # RTK CommandCrusher (CLI tool output optimization)
    │   ├── stage.go                   # CommandCrusherStage, CrushCommandOutput
    │   ├── detector.go                # signature enum + detectSignature
    │   ├── lines.go                   # filterLines, dedupeLines, nextLine, isLowerHex,
    │   │                              #   hasCommitLine, hasCargoVerbLine
    │   ├── crusher_git.go             # git status, git log
    │   ├── crusher_gorust.go          # go test, golangci-lint, cargo test, cargo build
    │   ├── crusher_javascript.go      # jest, mocha, tsc, eslint
    │   ├── crusher_python.go          # pytest, unittest, ruff
    │   ├── crusher_test.go            # moved from internal/headroom/command_crusher_test.go
    │   └── bench_test.go
    ├── smart/                         # SmartCrusher (JSON compaction & tabular arrays)
    │   ├── stage.go                   # SmartCrusherStage, CompactJSON
    │   ├── tabular.go                 # TryTabularConversion
    │   ├── smart_test.go
    │   ├── tabular_test.go
    │   └── bench_test.go
    ├── code/                          # CodeCompressor (whitespace & repeat-line pruning)
    │   ├── stage.go                   # CodeCompressorStage, PruneText
    │   ├── code_test.go
    │   └── bench_test.go
    ├── shaper/                        # OutputShaper (verbosity steering & effort routing)
    │   ├── stage.go                   # OutputShaperStage, classifyContinuation
    │   ├── shaper_test.go
    │   └── continuation_test.go
    └── ccr/                           # Content-Conditioned Retrieval (Phase 2 chunking)
        ├── stage.go                   # CCRStage, FormatChunkToken, RetrieveToolDefinition
        ├── store.go                   # CCRStore, ChunkID
        ├── ccr_test.go
        ├── store_test.go
        └── bench_test.go
```

Test files move with the code they cover. They are edited (package clause, core helper requalification, new log assertions), never rewritten from scratch: `command_crusher_test.go` alone carries ~24 KB of signature regression coverage that must survive the move intact.

Fourteen existing tests exercise the whole pipeline through `NewEngine`. A file declaring `package headroom` cannot import a stage subpackage — `stages/*` imports `headroom`, so that is an import cycle — therefore those tests move to `package headroom_test` in `internal/headroom/integration_test.go`. Being an external test package, it may import both the core and every stage. `engine_test.go` stays in `package headroom` and covers orchestration against mock stages only.

---

## 3. Core Interfaces & Engine Contract

### 3.1 Stage interface (unchanged)

```go
package headroom

import "context"

type Stage interface {
	Name() string
	Execute(ctx context.Context, reqCtx *RequestContext, cfg *Config) error
}
```

### 3.2 RequestContext & telemetry

`Logger` is a plain field so callers may leave it nil. Every stage reads it through `Log()`, which is nil-safe — dozens of existing tests construct `&RequestContext{Request: req}` literals and must keep working.

```go
package headroom

import "log/slog"

type RequestContext struct {
	Request           map[string]any
	FrozenPrefixIndex int
	BytesBefore       int
	BytesAfter        int
	RewritesCount     int
	Verbatim          *ToolInspector
	VerbatimSkipped   int

	// Logger is the request-scoped logger, pre-bound with request_id by the
	// Engine. May be nil; read it through Log().
	Logger *slog.Logger

	// Stage-specific telemetry.
	ChunksStored     int
	EffortClamped    bool
	OriginalThinking int
	ClampedThinking  int
	ContinuationKind string
}

// Log returns the request-scoped logger, falling back to the default logger
// when none was injected.
func (r *RequestContext) Log() *slog.Logger {
	if r.Logger == nil {
		return slog.Default()
	}
	return r.Logger
}

// RecordRewrite accumulates byte accounting for one rewritten block.
func (r *RequestContext) RecordRewrite(before, after string) {
	r.BytesBefore += len(before)
	r.BytesAfter += len(after)
	r.RewritesCount++
}
```

### 3.3 Exported core helpers

The stages live outside the package, so the AST helpers they share must be exported. Bodies are unchanged; only names and, in one case, the file they live in.

| Today (unexported)                     | After (exported)                        | File          |
| -------------------------------------- | --------------------------------------- | ------------- |
| `walkToolResultText` (`walk.go`)       | `WalkToolResultText`                    | `walk.go`     |
| `walkToolUseBlocks` (`walk.go`)        | `WalkToolUseBlocks`                     | `walk.go`     |
| `errorOrdinals` (`command_crusher.go`) | `ErrorOrdinals` — **moves to walk.go**  | `walk.go`     |
| `skipVerbatim` (`verbatim.go`)         | `SkipVerbatim`                          | `verbatim.go` |
| `walkMessages` (`verbatim.go`)         | `WalkMessages`                          | `verbatim.go` |
| `frozenPrefixIndex` (`engine.go`)      | `FrozenPrefixIndex`                     | `engine.go`   |

`ErrorOrdinals` is core AST logic that happens to live in `command_crusher.go` today. It must be relocated into `walk.go` in the first task, before the crusher files are deleted, otherwise the crusher move takes the function with it.

### 3.4 Engine construction & execution

The engine no longer builds any stage and no longer owns the CCR store. It receives both the logger and the stage list from the composition root.

```go
package headroom

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

type Engine struct {
	mu       sync.RWMutex
	config   Config
	pipeline *Pipeline
	logger   *slog.Logger
	seq      atomic.Uint64
}

func NewEngine(cfg Config, logger *slog.Logger, stages ...Stage) *Engine {
	if logger == nil {
		logger = slog.Default()
	}
	return &Engine{
		config:   cfg,
		logger:   logger.With("module", "headroom"),
		pipeline: NewPipeline(stages...),
	}
}

func (e *Engine) Process(ctx context.Context, req map[string]any) (*RequestContext, error) {
	cfg := e.GetConfig()

	reqCtx := &RequestContext{
		Request:           req,
		FrozenPrefixIndex: FrozenPrefixIndex(req, cfg.LiveTurns),
		Logger:            e.logger.With("request_id", e.seq.Add(1)),
	}
	if cfg.PreserveVerbatimReads || (cfg.OutputShaper.Enabled && cfg.OutputShaper.EffortRouting) {
		reqCtx.Verbatim = NewToolInspector(req)
	}

	start := time.Now()
	if err := e.pipeline.Run(ctx, reqCtx, &cfg); err != nil {
		reqCtx.Log().Error("headroom pipeline failed", "error", err)
		return nil, err
	}

	if reqCtx.BytesBefore > 0 || reqCtx.EffortClamped {
		saved := reqCtx.BytesBefore - reqCtx.BytesAfter
		var savedPct float64
		if reqCtx.BytesBefore > 0 {
			savedPct = float64(saved) / float64(reqCtx.BytesBefore) * 100
		}
		reqCtx.Log().Debug("headroom compression summary",
			"rewrites", reqCtx.RewritesCount,
			"bytes_before", reqCtx.BytesBefore,
			"bytes_after", reqCtx.BytesAfter,
			"saved_bytes", saved,
			"saved_pct", savedPct,
			"chunks_stored", reqCtx.ChunksStored,
			"effort_clamped", reqCtx.EffortClamped,
			"continuation", reqCtx.ContinuationKind,
			"verbatim_skipped", reqCtx.VerbatimSkipped,
			"duration_ms", float64(time.Since(start).Microseconds())/1000.0,
		)
	}
	return reqCtx, nil
}
```

Notes on this contract:

- **`request_id`** is a per-engine monotonic counter, not a transport ID. Nothing in the proxy path currently threads a request identifier through `context.Context`; a counter is sufficient to disambiguate interleaved concurrent DEBUG output, and it costs one atomic add per request.
- **No early return on `!cfg.Enabled`.** `Pipeline.Run` already short-circuits on a disabled config, and `Process` must keep returning a `RequestContext` whose `FrozenPrefixIndex` and `Verbatim` are populated. Adding an early return would change behavior for callers that read those fields.
- **Summary log level is DEBUG.** The equivalent server-side log at `internal/api/server.go:740` is removed in the same change, so there is exactly one summary record per request and one place that owns its field names. `server.go` keeps the `Warn` on pipeline failure and the `tracker.RecordHeadroom` call.

### 3.5 Store ownership

`Engine.CCRStore()` is removed. The server owns the store, constructs it, hands it to the CCR stage, and serves retrieval from its own field.

- `internal/api/server.go` gains a `ccrStore *ccr.CCRStore` field.
- `server.go:1907`, `:1911`, `:1914` read `server.ccrStore` instead of `server.headroom.CCRStore()`.
- Test call sites move from `srv.headroom.CCRStore()` to `srv.ccrStore`: `headroom_ccr_test.go`, `server_test.go`, `kimi_proxy_test.go`, `claudecode_proxy_test.go`, `claudecode_observability_test.go`.
- `headroom.ChunkID` becomes `ccr.ChunkID`; `internal/api/headroom_ccr_test.go` has four call sites.

---

## 4. Subpackage Stage Implementations

Each stage keeps its current behavior exactly. The only additions are the package boundary, requalified core helper calls, and the logging described in §5.

### 4.1 `stages/crusher` — RTK CommandCrusher

Signature detection (`SigGitStatus`, `SigPytest`, `SigJest`, `SigGoTest`, `SigCargoTest`, …) and the per-tool compression functions. Consumes `headroom.ErrorOrdinals`, `headroom.WalkToolResultText`, `headroom.SkipVerbatim`. Exports `NewStage() *CommandCrusherStage` and `CrushCommandOutput`.

### 4.2 `stages/smart` — SmartCrusher

Compacts JSON tool output to single-line form and converts uniform object arrays to tabular form. Respects `cfg.SmartCrusher` and `cfg.TabularArrays`. Exports `NewStage()`, `CompactJSON`, `TryTabularConversion`, `DefaultMinTabularSavings`.

### 4.3 `stages/code` — CodeCompressor

Collapses runs of blank lines and repeated lines. Respects `cfg.CodeCompressor`. Exports `NewStage()`, `PruneText`.

### 4.4 `stages/shaper` — OutputShaper

Verbosity steering prompt injection and effort routing (thinking budget clamping for mechanical continuations). Consumes `headroom.ToolInspector`. Respects `cfg.OutputShaper`. Exports `NewStage()`, `DefaultVerbosityPrompt`.

### 4.5 `stages/ccr` — Content-Conditioned Retrieval

Chunk demotion, store management, and retrieve-tool injection. Exports `NewStage(store *CCRStore)`, `NewCCRStore`, `NewCCRStoreFromMB`, `ChunkID`, `FormatChunkToken`, `RetrieveToolDefinition`.

---

## 5. Observability

### 5.1 Hot-path rule

Per-block DEBUG records fire once per `tool_result` text payload, so they run inside the walk loop. Every stage resolves the level once per `Execute` and guards the loop body:

```go
func (s *CommandCrusherStage) Execute(ctx context.Context, reqCtx *RequestContext, cfg *Config) error {
	log := reqCtx.Log()
	dbg := log.Enabled(ctx, slog.LevelDebug)
	// ...
	headroom.WalkToolResultText(reqCtx.Request, 0, func(_, ord int, get func() string, set func(string)) {
		// ...
		if dbg {
			log.Debug("command_crusher compressed tool output",
				"stage", s.Name(), "signature", sig.String(), "ordinal", ord,
				"bytes_before", len(before), "bytes_after", len(after))
		}
	})
	return nil
}
```

An unguarded `log.Debug("msg", "k", v)` still allocates the variadic slice and boxes each scalar even when the level is off. The guard is what makes the "no cost when DEBUG is disabled" claim literally true, and it is verified by the stage benchmarks.

`stage` is passed as an inline attribute rather than bound with `Logger.With(...)` per `Execute`: `With` allocates a handler even when nothing is ever logged, which would put a per-request cost back on the disabled path.

### 5.2 Event matrix

Every record inherits `module=headroom` and `request_id` from the request-scoped logger. Every stage record additionally carries `stage`, the value of that stage's `Name()`.

| Level   | Component | Event                        | Attributes                                                                                                                                 |
| ------- | --------- | ---------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| `DEBUG` | `crusher` | Signature match & compression | `stage`, `signature`, `ordinal`, `bytes_before`, `bytes_after`                                                                                |
| `DEBUG` | `smart`   | JSON compaction / tabular     | `stage`, `mode` (`compact`\|`tabular`), `ordinal`, `bytes_before`, `bytes_after`                                                              |
| `DEBUG` | `code`    | Whitespace & repeat pruning   | `stage`, `ordinal`, `bytes_before`, `bytes_after`                                                                                            |
| `DEBUG` | `shaper`  | Effort routing & clamp        | `stage`, `continuation_kind`, `original_budget`, `clamped_budget`                                                                             |
| `DEBUG` | `ccr`     | Chunk demotion                | `stage`, `chunk_id`, `ordinal`, `message_index`, `chunk_bytes`, `store_bytes`                                                                 |
| `DEBUG` | `headroom`| Pipeline summary              | `rewrites`, `bytes_before`, `bytes_after`, `saved_bytes`, `saved_pct`, `chunks_stored`, `effort_clamped`, `continuation`, `verbatim_skipped`, `duration_ms` |
| `ERROR` | `headroom`| Pipeline stage failure        | `error`                                                                                                                                       |

The `WARN` "unparseable payload bypassed" row from revision 1 is dropped. `WalkToolResultText` skips malformed blocks silently by design; a record there would fire on every non-text content block of every request and cost an allocation to say nothing actionable.

---

## 6. Wiring in `internal/api/server.go`

`server.go` is the composition root. It builds the store, builds the stages, and injects them alongside the logger:

```go
srv.ccrStore = ccr.NewCCRStoreFromMB(cfg.Headroom.CCR.MaxStoreMB)

srv.headroom = headroom.NewEngine(
	cfg.Headroom,
	srv.logger,
	ccr.NewStage(srv.ccrStore),
	crusher.NewStage(),
	smart.NewStage(),
	code.NewStage(),
	shaper.NewStage(),
)
```

Stage order is unchanged from today's `NewEngine`: CCR, crusher, smart, code, shaper. `srv.logger` is assigned in the `&Server{...}` literal above this call, so it is non-nil at construction time.

---

## 7. Verification & Invariants

1. **I1 — Deterministic compression**: compression is pure and position-independent.
2. **I2 — Verbatim file-read safety**: payloads classified verbatim are preserved byte-for-byte.
3. **I3 — Assistant text untouched**: assistant messages, thinking blocks, signatures, and `tool_use` inputs are never rewritten.
4. **I4 — Tool error safety**: `tool_result` blocks with `is_error: true` are never rewritten.
5. **I5 — Behavioral no-op**: the refactor changes no compressed output. Every moved test passes unmodified except for its package clause and helper qualification.
6. **I6 — No cost when DEBUG is off**: per-block logging sits behind a `Logger.Enabled` guard resolved once per `Execute`; stage benchmarks show no regression against the pre-move baseline.
7. **I7 — Core stays clean**: `go list -f '{{join .Imports "\n"}}' ./internal/headroom` contains no `internal/headroom/stages` entry.
8. **I8 — Green suite**: `go test -race ./...` passes at every commit, not only at the end of the series.

Build target: Go 1.27rc2 (`go.mod`), `log/slog`, standard library `testing`.
