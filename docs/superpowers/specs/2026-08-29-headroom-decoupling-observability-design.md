# Design Specification: Headroom Engine Decoupling & Observability Enhancement

**Date**: 2026-08-29  
**Status**: Approved  
**Scope**: Architectural Refactoring & Observability

---

## 1. Overview & Goals

This specification defines the architectural refactoring to decouple the **Headroom Engine** orchestration core from individual transformation stages (most notably the **RTK Suite / CommandCrusher** pattern compression engine) into dedicated modular subpackages under `internal/headroom/stages/`. Additionally, it establishes structured observability across all transformation stages and the core pipeline.

### Core Objectives
1. **Decouple Headroom Engine Core**: Ensure `internal/headroom` has zero direct imports of transformation stages, functioning purely as a pluggable pipeline coordinator.
2. **Modular Stage Isolation**: Package CommandCrusher (RTK Suite), SmartCrusher, CodeCompressor, OutputShaper, and CCR into self-contained subpackages under `internal/headroom/stages/`.
3. **Structured Observability**: Instrument the pipeline and all stages with `*slog.Logger`, emitting detailed debug logs for individual block rewrites, and summary info logs for pipeline execution checkpoints.
4. **Preserve All Safety Invariants**: Maintain strict preservation of `is_error` tool results, verbatim file-read payloads, and position-independent prompt cache stability.

---

## 2. Architecture & Package Layout

```
internal/
├── headroom/                          # Core Orchestration Engine
│   ├── engine.go                      # Engine lifecycle, Process(), UpdateConfig()
│   ├── pipeline.go                    # Pipeline runner, Stage execution loop
│   ├── types.go                       # Stage interface, RequestContext, Config types
│   ├── verbatim.go                    # ToolInspector & verbatim file-read protection
│   ├── walk.go                        # Fast JSON AST / tool_result message tree walker
│   └── engine_test.go                 # Engine orchestration unit tests
└── headroom/stages/                   # Pluggable Compression & Transformation Stages
    ├── crusher/                       # RTK CommandCrusher (CLI Tool Output Optimization)
    │   ├── stage.go                   # CommandCrusherStage (implements headroom.Stage)
    │   ├── detector.go                # Fast signature detection (git, pytest, cargo, etc.)
    │   ├── crusher_git.go             # git status, git log
    │   ├── crusher_gorust.go          # go test, golangci-lint, cargo test, cargo build
    │   ├── crusher_javascript.go      # jest, mocha, tsc, eslint
    │   ├── crusher_python.go          # pytest, unittest, ruff
    │   └── crusher_test.go            # Comprehensive signature & regression tests
    ├── smart/                         # SmartCrusher (JSON compactor & tabular array stage)
    │   ├── stage.go                   # SmartCrusherStage
    │   ├── tabular.go                 # Tabular array transformer
    │   └── smart_test.go
    ├── code/                          # CodeCompressor (whitespace & empty line stripper)
    │   ├── stage.go                   # CodeCompressorStage
    │   └── code_test.go
    ├── shaper/                        # OutputShaper (Effort routing & thinking budget)
    │   ├── stage.go                   # OutputShaperStage
    │   └── shaper_test.go
    └── ccr/                           # Content-Conditioned Retrieval (Phase 2 Chunking)
        ├── stage.go                   # CCRStage
        ├── store.go                   # Memory chunk store
        └── ccr_test.go
```

---

## 3. Core Interfaces & Engine Contract

### 3.1 Stage Interface
```go
package headroom

import "context"

type Stage interface {
	Name() string
	Execute(ctx context.Context, reqCtx *RequestContext, cfg *Config) error
}
```

### 3.2 RequestContext & Telemetry
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
	Logger            *slog.Logger

	// Stage-specific telemetry
	ChunksStored     int
	EffortClamped    bool
	OriginalThinking int
	ClampedThinking  int
	ContinuationKind string
}

func (r *RequestContext) RecordRewrite(before, after string) {
	r.BytesBefore += len(before)
	r.BytesAfter += len(after)
	r.RewritesCount++
}
```

### 3.3 Engine Construction & Execution
```go
package headroom

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type Engine struct {
	mu       sync.RWMutex
	config   Config
	pipeline *Pipeline
	logger   *slog.Logger
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
	if !cfg.Enabled {
		return &RequestContext{Request: req}, nil
	}

	start := time.Now()
	reqCtx := &RequestContext{
		Request:           req,
		FrozenPrefixIndex: FrozenPrefixIndex(req, cfg.LiveTurns),
		Logger:            e.logger,
	}

	if cfg.PreserveVerbatimReads || (cfg.OutputShaper.Enabled && cfg.OutputShaper.EffortRouting) {
		reqCtx.Verbatim = NewToolInspector(req)
	}

	if err := e.pipeline.Run(ctx, reqCtx, &cfg); err != nil {
		e.logger.Error("headroom pipeline failed", "error", err)
		return nil, err
	}

	duration := time.Since(start)
	if reqCtx.RewritesCount > 0 || reqCtx.ChunksStored > 0 || reqCtx.EffortClamped {
		savedBytes := reqCtx.BytesBefore - reqCtx.BytesAfter
		var savedPct float64
		if reqCtx.BytesBefore > 0 {
			savedPct = float64(savedBytes) / float64(reqCtx.BytesBefore) * 100
		}
		e.logger.Info("headroom compression summary",
			"rewrites", reqCtx.RewritesCount,
			"bytes_before", reqCtx.BytesBefore,
			"bytes_after", reqCtx.BytesAfter,
			"saved_bytes", savedBytes,
			"saved_pct", savedPct,
			"duration_ms", float64(duration.Microseconds())/1000.0,
		)
	}

	return reqCtx, nil
}
```

---

## 4. Subpackage Stage Implementation

### 4.1 RTK CommandCrusher (`internal/headroom/stages/crusher`)
- Implements `headroom.Stage`.
- Houses signature detection (`SigGitStatus`, `SigPytest`, `SigJest`, `SigGoTest`, `SigCargoTest`, etc.) and compression functions.
- Directly logs signature matches and compression yields when a rewrite occurs.
- Completely isolated from other stages.

### 4.2 SmartCrusher (`internal/headroom/stages/smart`)
- Compresses JSON outputs into compact single-line JSON and tabular arrays.
- Respects `cfg.SmartCrusher` and `cfg.TabularArrays`.

### 4.3 CodeCompressor (`internal/headroom/stages/code`)
- Trims excess blank lines and leading/trailing indentation noise in code blocks.
- Respects `cfg.CodeCompressor`.

### 4.4 OutputShaper (`internal/headroom/stages/shaper`)
- Applies verbosity steering prompts and effort routing (thinking budget clamping for mechanical tasks).
- Respects `cfg.OutputShaper.Enabled`.

### 4.5 Content-Conditioned Retrieval (`internal/headroom/stages/ccr`)
- Manages `CCRStore` for chunk offloading and hydration.
- Respects `cfg.CCR.Enabled`.

---

## 5. Wiring in `internal/api/server.go`

`internal/api/server.go` acts as the composition root, initializing stages and injecting them into `headroom.NewEngine`:

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

---

## 6. Observability Matrix

| Level | Component | Event | Attributes |
|---|---|---|---|
| `DEBUG` | `crusher` | Signature match & compression | `signature`, `bytes_before`, `bytes_after`, `saved_bytes`, `saved_pct` |
| `DEBUG` | `smart` | JSON compaction | `bytes_before`, `bytes_after`, `saved_bytes` |
| `DEBUG` | `code` | Code stripping | `lines_stripped`, `bytes_before`, `bytes_after` |
| `DEBUG` | `shaper` | Effort routing & clamp | `continuation_kind`, `original_budget`, `clamped_budget` |
| `DEBUG` | `verbatim` | File read skipped | `tool_name`, `file_path`, `ordinal` |
| `INFO` | `headroom` | Pipeline execution summary | `rewrites`, `bytes_before`, `bytes_after`, `saved_bytes`, `saved_pct`, `duration_ms` |
| `INFO` | `ccr` | Store / Retrieval | `action`, `chunk_id`, `chunk_bytes`, `store_total_mb` |
| `WARN` | `headroom` | Unparseable / malformed payload bypassed | `stage`, `block_index`, `reason` |
| `ERROR` | `headroom` | Pipeline stage failure | `stage`, `error` |

---

## 7. Verification & Invariants

1. **Deterministic Compression (Invariant I1)**: Output compression is pure and deterministic.
2. **Verbatim File Read Safety (Invariant I2)**: Payloads corresponding to verbatim file reads are strictly preserved untouched.
3. **Assistant Text Untouched (Invariant I3)**: Assistant messages are never modified.
4. **Tool Error Safety (Invariant I4)**: Tool results with `is_error: true` are never modified.
5. **No Performance Degradation**: Fast-path signature checks and zero-allocation logging when DEBUG level is disabled.
6. **100% Test Coverage & Race-Free**: All existing unit and integration tests pass with `go test -race ./...`.
