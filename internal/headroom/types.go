package headroom

import (
	"context"
	"log/slog"
)

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

	// ClampCodingContinuations opts back into clamping tool-result turns that
	// carry code, diffs, or test output. Off by default: clamping those is what
	// starves a mid-task model of reasoning budget.
	ClampCodingContinuations bool `json:"clampCodingContinuations,omitempty"`

	// MechanicalMaxBytes is the total payload ceiling under which a non-code
	// tool-result turn still counts as mechanical. 0 = defaultMechanicalMaxBytes (2048).
	MechanicalMaxBytes int `json:"mechanicalMaxBytes,omitempty"`
}

type Config struct {
	Enabled        bool `json:"enabled"`
	CommandCrusher bool `json:"commandCrusher,omitempty"`
	SmartCrusher   bool `json:"smartCrusher,omitempty"`
	TabularArrays  bool `json:"tabularArrays,omitempty"`
	CodeCompressor bool `json:"codeCompressor,omitempty"`
	// LiveTurns is the number of trailing messages CCR leaves untouched.
	// It has no effect on SmartCrusher/CodeCompressor, which are position
	// independent by design (see invariant I1).
	LiveTurns int `json:"liveTurns,omitempty"`
	// PreserveVerbatimReads keeps file-read tool results byte-for-byte, so a
	// later Edit or patch call can quote them exactly. Default true.
	// No `,omitempty`: with a true default, omitempty would silently drop the
	// key from persisted config and reload would read false.
	PreserveVerbatimReads bool               `json:"preserveVerbatimReads"`
	CCR                   CCRConfig          `json:"ccr,omitempty"`
	OutputShaper          OutputShaperConfig `json:"outputShaper,omitempty"`
}

// RequestContext carries the in-flight request and pipeline telemetry.
// Request is the caller's decoded Anthropic request map and is mutated in
// place; callers must run the pipeline before marshalling for any provider.
type RequestContext struct {
	Request map[string]any

	// Logger is the request-scoped logger configured with request_id and other context.
	Logger *slog.Logger

	// FrozenPrefixIndex is the highest message index CCR is allowed to demote.
	// Messages with index > FrozenPrefixIndex are the live turns and stay
	// inline. -1 means "everything is live".
	FrozenPrefixIndex int

	// Byte accounting over rewritten blocks only (not whole-request sizes).
	BytesBefore   int
	BytesAfter    int
	RewritesCount int

	// OutputShaper telemetry.
	EffortClamped    bool
	OriginalThinking int
	ClampedThinking  int
	ContinuationKind string

	// CCR telemetry (Phase 2).
	ChunksStored int

	// Verbatim classifies which tool_result payloads must not be rewritten. It
	// is built before the first stage runs.
	Verbatim *ToolInspector

	// VerbatimSkipped counts payloads a stage left untouched on that basis.
	VerbatimSkipped int
}

// Log returns the request-scoped logger, or slog.Default() if nil.
func (r *RequestContext) Log() *slog.Logger {
	if r == nil || r.Logger == nil {
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

type Stage interface {
	Name() string
	Execute(ctx context.Context, reqCtx *RequestContext, cfg *Config) error
}
