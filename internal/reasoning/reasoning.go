// Package reasoning normalizes the reasoning-effort signals a client can put
// on an Anthropic /v1/messages request into one value that both the model
// catalog (tier routing) and the Cloud Code request builder (budget emission)
// read, so the two can never disagree about the same request.
//
// The level names are Anthropic's output_config.effort vocabulary (Effort
// doc: low, medium, high, xhigh, max). Minimal exists only because
// OpenAI-shaped clients send it.
package reasoning

import (
	"fmt"
	"strings"
)

// Level is a normalized reasoning-effort level.
type Level string

const (
	LevelUnset   Level = ""
	LevelMinimal Level = "minimal"
	LevelLow     Level = "low"
	LevelMedium  Level = "medium"
	LevelHigh    Level = "high"
	LevelXHigh   Level = "xhigh"
	LevelMax     Level = "max"
)

// order is ascending strength; Supported relies on it.
var order = [...]Level{LevelMinimal, LevelLow, LevelMedium, LevelHigh, LevelXHigh, LevelMax}

func rank(level Level) int {
	for i, candidate := range order {
		if candidate == level {
			return i
		}
	}
	return -1
}

// Supported returns the highest level in supported that is at or below level.
// That is the fallback Claude Code documents for a level a model does not
// accept ("xhigh runs as high on Opus 4.6"). A level below everything the
// model supports takes the lowest supported level; an unset level stays unset.
func (level Level) Supported(supported ...Level) Level {
	want := rank(level)
	if want < 0 || len(supported) == 0 {
		return LevelUnset
	}
	best, bestRank := LevelUnset, -1
	lowest, lowestRank := LevelUnset, len(order)
	for _, candidate := range supported {
		r := rank(candidate)
		if r < 0 {
			continue
		}
		if r <= want && r > bestRank {
			best, bestRank = candidate, r
		}
		if r < lowestRank {
			lowest, lowestRank = candidate, r
		}
	}
	if bestRank >= 0 {
		return best
	}
	return lowest
}

// Source records which request field produced Params.Level. Precedence
// between the fields differs by consumer, so it travels with the value.
type Source int

const (
	SourceNone Source = iota
	// SourceOutputConfig is output_config.effort. Claude Code sends it on
	// every request, so it is ambient: it must not override a tier the client
	// already chose by model name.
	SourceOutputConfig
	// SourceBudget is a level derived from a bare thinking budget.
	SourceBudget
	// SourceExplicit is reasoning_effort or reasoning, the proxy's own
	// request extension; a client sets it on purpose.
	SourceExplicit
)

// Params is the normalized reasoning intent of one request.
type Params struct {
	// Level is the requested effort, LevelUnset when the client expressed none.
	Level  Level
	Source Source
	// Budget is the explicit token budget (thinking.budget_tokens or
	// thinking_budget). It is meaningful only when HasBudget is set.
	Budget    int
	HasBudget bool
	// Disabled means the client turned thinking off.
	Disabled bool
	// Adaptive means thinking.type was "adaptive".
	Adaptive bool
}

// Parse reads every reasoning signal a request can carry. Precedence for the
// level, highest first: reasoning_effort / reasoning, a level derived from an
// explicit budget, output_config.effort.
func Parse(request map[string]any) Params {
	var p Params
	if request == nil {
		return p
	}

	effort := textOf(request["reasoning_effort"])
	if strings.TrimSpace(effort) == "" {
		effort = textOf(request["reasoning"])
	}
	level, off := normalize(effort)
	if level != LevelUnset {
		p.Level, p.Source = level, SourceExplicit
	}
	p.Disabled = off

	thinking, _ := request["thinking"].(map[string]any)
	switch strings.ToLower(textOf(thinking["type"])) {
	case "disabled":
		p.Disabled = true
	case "adaptive":
		p.Adaptive = true
	}
	if budget, ok := intOf(thinking["budget_tokens"]); ok {
		p.Budget, p.HasBudget = budget, true
	}
	if budget, ok := intOf(request["thinking_budget"]); ok {
		p.Budget, p.HasBudget = budget, true
	}

	// A non-positive budget switches thinking off unless the client said
	// "enabled" outright.
	if p.HasBudget && p.Budget <= 0 && !p.Disabled &&
		strings.ToLower(textOf(thinking["type"])) != "enabled" {
		p.Disabled = true
	}

	if p.Level == LevelUnset && !p.Disabled && p.HasBudget && p.Budget > 0 {
		p.Level, p.Source = levelForBudget(p.Budget), SourceBudget
	}

	if p.Level == LevelUnset {
		if config, ok := request["output_config"].(map[string]any); ok {
			if ambient, _ := normalize(textOf(config["effort"])); ambient != LevelUnset {
				p.Level, p.Source = ambient, SourceOutputConfig
			}
		}
	}
	return p
}

// levelForBudget back-maps a bare budget onto a level: at most 2048 tokens is
// low, under 12000 is medium, anything larger is high.
func levelForBudget(budget int) Level {
	switch {
	case budget <= 2048:
		return LevelLow
	case budget < 12000:
		return LevelMedium
	}
	return LevelHigh
}

// normalize maps a client spelling to a Level. off reports an explicit
// "thinking off" spelling. An unknown spelling is ignored, not an error.
func normalize(raw string) (level Level, off bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "none", "disabled", "off", "false", "0":
		return LevelUnset, true
	case "minimal":
		return LevelMinimal, false
	case "low":
		return LevelLow, false
	case "medium":
		return LevelMedium, false
	case "high":
		return LevelHigh, false
	case "xhigh", "extra-high", "very-high":
		return LevelXHigh, false
	case "max", "maximum", "extreme":
		return LevelMax, false
	}
	return LevelUnset, false
}

// textOf renders a scalar request value, or the effort field of the OpenAI
// Responses object form {"effort": "..."}.
func textOf(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case map[string]any:
		return textOf(typed["effort"])
	}
	return fmt.Sprint(value)
}

func intOf(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case int:
		return typed, true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	}
	return 0, false
}
