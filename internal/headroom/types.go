package headroom

import "context"

// CCRConfig controls Content-Conditioned Retrieval (Phase 2).
type CCRConfig struct {
	Enabled       bool `json:"enabled"`
	MaxStoreMB    int  `json:"maxStoreMB,omitempty"`
	MinChunkBytes int  `json:"minChunkBytes,omitempty"`
}

// OutputShaperConfig controls verbosity steering and effort routing.
type OutputShaperConfig struct {
	Enabled                  bool   `json:"enabled"`
	VerbositySteering        bool   `json:"verbositySteering,omitempty"`
	SteeringText             string `json:"steeringText,omitempty"` // empty = DefaultVerbosityPrompt
	EffortRouting            bool   `json:"effortRouting,omitempty"`
	MechanicalThinkingBudget int    `json:"mechanicalThinkingBudget,omitempty"`
}

type Config struct {
	Enabled        bool               `json:"enabled"`
	SmartCrusher   bool               `json:"smartCrusher,omitempty"`
	CodeCompressor bool               `json:"codeCompressor,omitempty"`
	// LiveTurns is the number of trailing messages CCR leaves untouched.
	// It has no effect on SmartCrusher/CodeCompressor, which are position
	// independent by design (see invariant I1).
	LiveTurns    int                `json:"liveTurns,omitempty"`
	CCR          CCRConfig          `json:"ccr,omitempty"`
	OutputShaper OutputShaperConfig `json:"outputShaper,omitempty"`
}

// RequestContext carries the in-flight request and pipeline telemetry.
// Request is the caller's decoded Anthropic request map and is mutated in
// place; callers must run the pipeline before marshalling for any provider.
type RequestContext struct {
	Request map[string]any

	// FrozenPrefixIndex is the highest message index CCR is allowed to demote.
	// Messages with index > FrozenPrefixIndex are the live turns and stay
	// inline. -1 means "everything is live".
	FrozenPrefixIndex int

	// Byte accounting over rewritten blocks only (not whole-request sizes).
	BytesBefore int
	BytesAfter  int

	// OutputShaper telemetry.
	EffortClamped    bool
	OriginalThinking int
	ClampedThinking  int

	// CCR telemetry (Phase 2).
	ChunksStored int
}

// RecordRewrite accumulates byte accounting for one rewritten block.
func (r *RequestContext) RecordRewrite(before, after string) {
	r.BytesBefore += len(before)
	r.BytesAfter += len(after)
}

type Stage interface {
	Name() string
	Execute(ctx context.Context, reqCtx *RequestContext, cfg *Config) error
}

type Pipeline struct {
	stages []Stage
}

func NewPipeline(stages ...Stage) *Pipeline {
	return &Pipeline{stages: stages}
}

func (p *Pipeline) Run(ctx context.Context, reqCtx *RequestContext, cfg *Config) error {
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	for _, stage := range p.stages {
		if err := stage.Execute(ctx, reqCtx, cfg); err != nil {
			return err
		}
	}
	return nil
}
