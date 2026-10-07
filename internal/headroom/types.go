package headroom

import (
	"context"
	"log/slog"
)

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
	// PreserveVerbatimReads keeps file-read tool results byte-for-byte, so a
	// later Edit or patch call can quote them exactly. Default true.
	// No `,omitempty`: with a true default, omitempty would silently drop the
	// key from persisted config and reload would read false.
	PreserveVerbatimReads bool               `json:"preserveVerbatimReads"`
	OutputShaper          OutputShaperConfig `json:"outputShaper,omitempty"`
}

// RequestContext carries the in-flight request and pipeline telemetry.
// Request is the caller's decoded Anthropic request map and is mutated in
// place; callers must run the pipeline before marshalling for any provider.
type RequestContext struct {
	Request map[string]any

	// RequestID is the monotonic identifier assigned by Engine.Process,
	// logged as an inline attribute rather than bound via Logger.With — With
	// clones a handler on every request, which is the allocation this field
	// exists to avoid.
	RequestID uint64

	// Logger is the shared base logger (module=headroom), not cloned per
	// request.
	Logger *slog.Logger

	// Byte accounting over rewritten blocks only (not whole-request sizes).
	BytesBefore   int
	BytesAfter    int
	RewritesCount int

	// OutputShaper telemetry.
	EffortClamped    bool
	OriginalThinking int
	ClampedThinking  int
	ContinuationKind string

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
