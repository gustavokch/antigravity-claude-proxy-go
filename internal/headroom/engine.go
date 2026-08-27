package headroom

import (
	"context"
	"sync"
)

// defaultLiveTurns is how many trailing messages CCR leaves inline.
const defaultLiveTurns = 2

type Engine struct {
	mu       sync.RWMutex
	config   Config
	pipeline *Pipeline
	store    *CCRStore
}

func NewEngine(cfg Config) *Engine {
	store := NewCCRStoreFromMB(cfg.CCR.MaxStoreMB)
	return &Engine{
		config: cfg,
		store:  store,
		pipeline: NewPipeline(
			NewCCRStage(store),
			&SmartCrusherStage{},
			&CodeCompressorStage{},
			&OutputShaperStage{},
		),
	}
}

// CCRStore returns the chunk store used by the Engine.
func (e *Engine) CCRStore() *CCRStore {
	return e.store
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
		FrozenPrefixIndex: frozenPrefixIndex(req, cfg.LiveTurns),
	}
	if err := e.pipeline.Run(ctx, reqCtx, &cfg); err != nil {
		return nil, err
	}
	return reqCtx, nil
}

// frozenPrefixIndex returns the highest message index outside the live window.
// -1 means every message is live.
func frozenPrefixIndex(req map[string]any, liveTurns int) int {
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
