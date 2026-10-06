package api

import "antigravity-go-proxy/internal/reasoning"

// anthropicEffortLevels are the output_config.effort values Anthropic defines.
var anthropicEffortLevels = [...]reasoning.Level{
	reasoning.LevelLow, reasoning.LevelMedium, reasoning.LevelHigh, reasoning.LevelXHigh, reasoning.LevelMax,
}

// applyOpenAIReasoning carries an OpenAI reasoning request onto the Anthropic
// fields the rest of the pipeline reads. OpenAI's reasoning_effort (Chat
// Completions) and reasoning.effort (Responses shape) become
// output_config.effort, with minimal riding the low level because Anthropic
// has none below it; "none" becomes thinking.type "disabled". The proxy-only
// reasoning_effort field is never forwarded: an Anthropic-shaped upstream
// rejects unknown top-level fields. An unrecognized spelling is dropped.
func applyOpenAIReasoning(anthropic, openaiRequest map[string]any) {
	params := reasoning.Parse(openaiRequest)
	switch {
	case params.Disabled:
		anthropic["thinking"] = map[string]any{"type": "disabled"}
	case params.Level != reasoning.LevelUnset:
		anthropic["output_config"] = map[string]any{
			"effort": string(params.Level.Supported(anthropicEffortLevels[:]...)),
		}
	}
}
