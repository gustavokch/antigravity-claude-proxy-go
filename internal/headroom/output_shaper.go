package headroom

import "context"

const DefaultVerbosityPrompt = "Respond with concise technical precision. Avoid conversational filler, preamble, and meta-commentary. Focus directly on answering questions and executing actions."

// minThinkingBudget is the Anthropic API floor for thinking.budget_tokens.
const minThinkingBudget = 1024

type OutputShaperStage struct{}

func (s *OutputShaperStage) Name() string { return "output_shaper" }

func (s *OutputShaperStage) Execute(ctx context.Context, reqCtx *RequestContext, cfg *Config) error {
	if !cfg.OutputShaper.Enabled {
		return nil
	}
	if cfg.OutputShaper.VerbositySteering {
		s.applySteering(reqCtx.Request, cfg)
	}
	if cfg.OutputShaper.EffortRouting && isMechanicalContinuation(reqCtx.Request) {
		s.clampEffort(reqCtx, cfg)
	}
	return nil
}

func (s *OutputShaperStage) applySteering(req map[string]any, cfg *Config) {
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

// isMechanicalContinuation reports whether the final message is a pure tool
// result turn with no errors: the model is resuming work, not being asked
// something new, and does not need a large thinking budget.
func isMechanicalContinuation(req map[string]any) bool {
	messages, ok := req["messages"].([]any)
	if !ok || len(messages) == 0 {
		return false
	}
	last, ok := messages[len(messages)-1].(map[string]any)
	if !ok || last["role"] != "user" {
		return false
	}
	blocks, ok := last["content"].([]any)
	if !ok || len(blocks) == 0 {
		return false
	}
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != "tool_result" {
			return false
		}
		if isErr, _ := block["is_error"].(bool); isErr {
			return false
		}
	}
	return true
}

func (s *OutputShaperStage) clampEffort(reqCtx *RequestContext, cfg *Config) {
	req := reqCtx.Request

	budget := cfg.OutputShaper.MechanicalThinkingBudget
	if budget < minThinkingBudget {
		budget = minThinkingBudget
	}

	// Anthropic shape. Only present-and-enabled thinking is clamped (I4).
	if thinking, ok := req["thinking"].(map[string]any); ok && thinking["type"] == "enabled" {
		if current, ok := thinking["budget_tokens"].(float64); ok && int(current) > budget {
			// max_tokens must stay strictly greater than budget_tokens.
			if maxTokens, ok := req["max_tokens"].(float64); !ok || int(maxTokens) > budget {
				thinking["budget_tokens"] = float64(budget)
				reqCtx.OriginalThinking = int(current)
				reqCtx.ClampedThinking = budget
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
}
