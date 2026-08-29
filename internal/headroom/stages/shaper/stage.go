package shaper

import (
	"context"
	"regexp"

	"antigravity-go-proxy/internal/headroom"
)

const DefaultVerbosityPrompt = "Respond with concise technical precision. Avoid conversational filler, preamble, and meta-commentary. Focus directly on answering questions and executing actions."

// minThinkingBudget is the Anthropic API floor for thinking.budget_tokens.
const minThinkingBudget = 1024

// defaultMechanicalMaxBytes is the payload ceiling under which a non-code
// tool-result turn still counts as mechanical.
const defaultMechanicalMaxBytes = 2048

// continuationKind classifies the trailing user turn for effort routing.
type continuationKind int

const (
	// kindInteractive is a fresh user request, an error result, or any turn this
	// classifier does not recognise. Never clamped.
	kindInteractive continuationKind = iota

	// kindCoding is a tool-result continuation carrying file content, a diff, or
	// the output of an edit, build, or test command. The model is mid-task and
	// still needs its requested reasoning budget.
	kindCoding

	// kindMechanical is a small, non-code tool-result continuation: the model is
	// resuming work and does not need a large thinking budget.
	kindMechanical

	// kindLarge is a tool-result continuation over the byte ceiling that carries
	// no code signal: a long log, a directory listing, a fetched page. Not
	// clamped, since the model still has to read it, but kept distinct from
	// kindCoding so the telemetry says which signal decided the turn.
	kindLarge
)

func (k continuationKind) String() string {
	switch k {
	case kindInteractive:
		return "interactive"
	case kindCoding:
		return "coding"
	case kindMechanical:
		return "mechanical"
	case kindLarge:
		return "large"
	default:
		return "unknown"
	}
}

// codingToolNames are tools whose results put the model mid-implementation:
// mutations to source, and commands that build, run, or test it.
var codingToolNames = map[string]bool{
	"edit": true, "multiedit": true, "write": true, "create_file": true,
	"str_replace": true, "str_replace_editor": true, "apply_patch": true,
	"notebookedit": true, "notebook_edit": true,
	"bash": true, "shell": true, "run_command": true, "run_terminal_cmd": true,
	"execute_command": true, "run_tests": true, "test": true,
	"task": true, "agent": true,
}

func isCodingToolName(name string) bool {
	return codingToolNames[headroom.NormalizeToolName(name)]
}

// testOutputPatterns are tuned for precision, not recall. A false positive
// classifies the turn as coding and skips the clamp, so a loose pattern does
// not merely add noise: it disables effort routing for every turn that happens
// to contain the word.
var testOutputPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bFAIL\b`),
	regexp.MustCompile(`\bPASS\b`),
	regexp.MustCompile(`(?m)^ok\s`),
	regexp.MustCompile(`(?m)^panic:`),
	regexp.MustCompile(`Traceback \(most recent call last\)`),
	regexp.MustCompile(`(?mi)^\s*(error|warning):`),
	// Compiler diagnostics: file:line[:col]: error|warning|note:
	regexp.MustCompile(`(?mi)^.+:\d+:(\d+:)?\s*(error|warning|note):`),
	regexp.MustCompile(`AssertionError`),
	regexp.MustCompile(`(?i)expected.*got`),
	regexp.MustCompile(`(?i)\d+\s+(passed|failed|errors?)\b`),
	regexp.MustCompile(`(?i)\bbuild failed\b`),
	regexp.MustCompile(`(?i)\bcompilation (failed|error)\b`),
}

func looksLikeTestOutput(text string) bool {
	for _, re := range testOutputPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

type OutputShaperStage struct{}

func NewStage() *OutputShaperStage {
	return &OutputShaperStage{}
}

func (s *OutputShaperStage) Name() string { return "output_shaper" }

func (s *OutputShaperStage) Execute(ctx context.Context, reqCtx *headroom.RequestContext, cfg *headroom.Config) error {
	if !cfg.OutputShaper.Enabled || reqCtx == nil || reqCtx.Request == nil {
		return nil
	}
	if cfg.OutputShaper.VerbositySteering {
		s.applySteering(reqCtx.Request, cfg)
	}
	if !cfg.OutputShaper.EffortRouting {
		return nil
	}

	kind := classifyContinuation(reqCtx.Request, reqCtx.Verbatim, cfg.OutputShaper.MechanicalMaxBytes)
	reqCtx.ContinuationKind = kind.String()

	// Coding continuations keep their requested budget: the model is mid-task
	// and clamping it to the 1024 floor is what makes it guess at edits.
	if kind == kindMechanical || (kind == kindCoding && cfg.OutputShaper.ClampCodingContinuations) {
		s.clampEffort(reqCtx, cfg, kind)
	}
	return nil
}

func (s *OutputShaperStage) applySteering(req map[string]any, cfg *headroom.Config) {
	text := cfg.OutputShaper.SteeringText
	if text == "" {
		text = DefaultVerbosityPrompt
	}
	// Appended at the tail so any cache_control breakpoint the client set on an
	// earlier system block keeps its exact bytes.
	switch sys := req["system"].(type) {
	case string:
		req["system"] = sys + "\n\n" + text
	case []any:
		req["system"] = append(sys, map[string]any{"type": "text", "text": text})
	case nil:
		req["system"] = text
	}
}

// classifyContinuation inspects the final message. inspector may be nil, in
// which case the verbatim signal is skipped and the text heuristics carry the
// classification on their own. A mechanicalMaxBytes of 0 selects
// defaultMechanicalMaxBytes.
func classifyContinuation(req map[string]any, inspector *headroom.ToolInspector, mechanicalMaxBytes int) continuationKind {
	messages, ok := req["messages"].([]any)
	if !ok || len(messages) == 0 {
		return kindInteractive
	}
	last, ok := messages[len(messages)-1].(map[string]any)
	if !ok || last["role"] != "user" {
		return kindInteractive
	}
	blocks, ok := last["content"].([]any)
	if !ok || len(blocks) == 0 {
		return kindInteractive
	}
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != "tool_result" {
			return kindInteractive
		}
		if isErr, _ := block["is_error"].(bool); isErr {
			return kindInteractive
		}
	}

	maxBytes := defaultMechanicalMaxBytes
	if mechanicalMaxBytes > 0 {
		maxBytes = mechanicalMaxBytes
	}

	// Calculate the starting ordinal for tool_result payloads in the last message.
	startOrd := 0
	for i := 0; i < len(messages)-1; i++ {
		msg, ok := messages[i].(map[string]any)
		if !ok {
			continue
		}
		cBlocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, raw := range cBlocks {
			b, ok := raw.(map[string]any)
			if !ok || b["type"] != "tool_result" {
				continue
			}
			startOrd += headroom.CountTextPayloads(b)
		}
	}

	totalBytes := 0
	currOrd := startOrd

	for _, raw := range blocks {
		block, _ := raw.(map[string]any)

		// 1 & 2. Check inspector for tool_use name or verbatim status.
		if id, _ := block["tool_use_id"].(string); id != "" && inspector != nil {
			if info, found := inspector.Lookup(id); found {
				if isCodingToolName(info.Name) {
					return kindCoding
				}
			}
		}

		numPayloads := headroom.CountTextPayloads(block)
		if inspector != nil {
			for j := currOrd; j < currOrd+numPayloads; j++ {
				if inspector.IsVerbatimOrdinal(j) {
					return kindCoding
				}
			}
		}
		currOrd += numPayloads

		// Check payload texts.
		switch content := block["content"].(type) {
		case string:
			totalBytes += len(content)
			if headroom.LooksLikeNumberedSource(content) || headroom.LooksLikeUnifiedDiff(content) || looksLikeTestOutput(content) {
				return kindCoding
			}
		case []any:
			for _, innerRaw := range content {
				inner, ok := innerRaw.(map[string]any)
				if !ok || inner["type"] != "text" {
					continue
				}
				if text, ok := inner["text"].(string); ok {
					totalBytes += len(text)
					if headroom.LooksLikeNumberedSource(text) || headroom.LooksLikeUnifiedDiff(text) || looksLikeTestOutput(text) {
						return kindCoding
					}
				}
			}
		}
	}

	// 3. Check byte ceiling. Size alone is not evidence of code, so this is the
	// one exit that does not claim the turn is a coding continuation.
	if totalBytes >= maxBytes {
		return kindLarge
	}

	return kindMechanical
}

func (s *OutputShaperStage) clampEffort(reqCtx *headroom.RequestContext, cfg *headroom.Config, kind continuationKind) {
	req := reqCtx.Request

	budget := cfg.OutputShaper.MechanicalThinkingBudget
	if budget < minThinkingBudget {
		budget = minThinkingBudget
	}

	original := 0
	clamped := 0

	// Anthropic shape. Only present-and-enabled thinking is clamped (I4).
	if thinking, ok := req["thinking"].(map[string]any); ok && thinking["type"] == "enabled" {
		if current, ok := thinking["budget_tokens"].(float64); ok && int(current) > budget {
			// max_tokens must stay strictly greater than budget_tokens.
			if maxTokens, ok := req["max_tokens"].(float64); !ok || int(maxTokens) > budget {
				thinking["budget_tokens"] = float64(budget)
				original = int(current)
				clamped = budget
				reqCtx.OriginalThinking = original
				reqCtx.ClampedThinking = clamped
				reqCtx.EffortClamped = true
			}
		}
	}

	// OpenAI-compatible shapes, for custom endpoints that speak them.
	if reasoning, ok := req["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort == "high" {
			reasoning["effort"] = "low"
			reqCtx.EffortClamped = true
		}
	}
	if effort, ok := req["reasoning_effort"].(string); ok && effort == "high" {
		req["reasoning_effort"] = "low"
		reqCtx.EffortClamped = true
	}

	if reqCtx.EffortClamped {
		reqCtx.Log().Debug("output_shaper clamped thinking budget",
			"stage", s.Name(),
			"request_id", reqCtx.RequestID,
			"continuation_kind", kind.String(),
			"original_budget", original,
			"clamped_budget", clamped)
	}
}
