package headroom

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// defaultLiveTurns is how many trailing messages CCR leaves inline.
const defaultLiveTurns = 2

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

func (e *Engine) UpdateConfig(cfg Config) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.config = cfg
}

func (e *Engine) GetConfig() Config {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.config
}

// Process runs the pipeline over req, mutating it in place. The returned
// RequestContext carries telemetry only; the request itself is the caller's map.
func (e *Engine) Process(ctx context.Context, req map[string]any) (*RequestContext, error) {
	cfg := e.GetConfig()

	reqCtx := &RequestContext{
		Request:           req,
		RequestID:         e.seq.Add(1),
		FrozenPrefixIndex: FrozenPrefixIndex(req, cfg.LiveTurns),
		Logger:            e.logger,
	}
	// The inspector has two consumers: the verbatim skip guards, and the
	// continuation classifier's tool-name lookup. Build it when either needs
	// it — gating it on PreserveVerbatimReads alone made turning that flag off
	// silently demote coding continuations to mechanical. SkipVerbatim still
	// checks PreserveVerbatimReads, so the skip guards stay off.
	if cfg.PreserveVerbatimReads || (cfg.OutputShaper.Enabled && cfg.OutputShaper.EffortRouting) {
		reqCtx.Verbatim = NewToolInspector(req)
	}

	start := time.Now()
	if err := e.pipeline.Run(ctx, reqCtx, &cfg); err != nil {
		reqCtx.Log().Error("headroom pipeline failed", "request_id", reqCtx.RequestID, "error", err)
		return nil, err
	}

	// Guarded like the per-stage records (invariant I6): the varargs slice and
	// the boxing of every int/float below are built before Debug is called, so
	// an unguarded summary would allocate on every compressed request.
	if log := reqCtx.Log(); (reqCtx.BytesBefore > 0 || reqCtx.EffortClamped) && log.Enabled(ctx, slog.LevelDebug) {
		saved := reqCtx.BytesBefore - reqCtx.BytesAfter
		var savedPct float64
		if reqCtx.BytesBefore > 0 {
			savedPct = float64(saved) / float64(reqCtx.BytesBefore) * 100
		}
		log.Debug("headroom compression summary",
			"request_id", reqCtx.RequestID,
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

// FrozenPrefixIndex returns the highest message index outside the live window.
// -1 means every message is live.
func FrozenPrefixIndex(req map[string]any, liveTurns int) int {
	if liveTurns <= 0 {
		liveTurns = defaultLiveTurns
	}
	messages, ok := req["messages"].([]any)
	if !ok {
		return -1
	}
	if idx := len(messages) - liveTurns - 1; idx >= 0 {
		return idx
	}
	return -1
}
