package format

import (
	"strings"

	"antigravity-go-proxy/internal/reasoning"
)

// The effort-to-budget table is the proxy's own translation choice. Cloud
// Code's ThinkingConfig carries a token budget or a tier and never an effort
// level, so an Anthropic output_config.effort has to become a number here.
const (
	effortBudgetLow        = 1024
	effortBudgetMedium     = 8000
	effortBudgetHighClaude = 32000
	effortBudgetHighGemini = 16000
)

// Spelling of the Claude route's thinkingConfig keys. The proxy has always sent
// snake_case here while every other branch sends the proto JSON (camelCase)
// names. Whether the Claude route may use camelCase too is decided by probe GA
// and the agy capture (plan Task 12a); keep the two keys together.
const (
	claudeKeyIncludeThoughts = "include_thoughts"
	claudeKeyThinkingBudget  = "thinking_budget"
)

// budgetLevels are the effort levels the budget table distinguishes. A
// stronger request falls back to the highest level listed, the rule Claude
// Code documents for a level a model does not accept.
var budgetLevels = [...]reasoning.Level{reasoning.LevelLow, reasoning.LevelMedium, reasoning.LevelHigh}

// effortBudget returns the thinking budget for an effort level, 0 for none.
func effortBudget(family ModelFamily, level reasoning.Level) int {
	switch level.Supported(budgetLevels[:]...) {
	case reasoning.LevelLow:
		return effortBudgetLow
	case reasoning.LevelMedium:
		return effortBudgetMedium
	case reasoning.LevelHigh:
		if family == FamilyClaude {
			return effortBudgetHighClaude
		}
		return effortBudgetHighGemini
	}
	return 0
}

// thinkingBudget picks the token budget for a budget-style model. Precedence:
// an explicit reasoning_effort, then an explicit budget, then the ambient
// output_config.effort, then fallback. The result honors the catalog minimum.
func thinkingBudget(params reasoning.Params, family ModelFamily, fallback, minimum int) int {
	budget := 0
	switch {
	case params.Source == reasoning.SourceExplicit:
		budget = effortBudget(family, params.Level)
	case params.HasBudget:
		budget = params.Budget
	case params.Level != reasoning.LevelUnset:
		budget = effortBudget(family, params.Level)
	}
	if budget <= 0 {
		budget = fallback
	}
	if minimum > 0 && budget < minimum {
		budget = minimum
	}
	return budget
}

// geminiBudgetCeiling is the largest thinkingBudget Google documents for the
// Gemini 2.5 series (2.5 Pro 128-32768, 2.5 Flash and Flash-Lite up to 24576).
// Other families have no documented budget range, so they keep the proxy's
// historical 128000 ceiling. It applies only when no live catalog entry
// describes the model.
func geminiBudgetCeiling(model string) int {
	lower := strings.ToLower(model)
	switch {
	case strings.Contains(lower, "gemini-2.5-pro"):
		return 32768
	case strings.Contains(lower, "gemini-2.5"):
		return 24576
	}
	return 128000
}
