package main

import (
	"fmt"
	"strings"
	"time"

	proxyformat "antigravity-go-proxy/internal/format"
)

// Gate names tie a probe to the decision it unlocks in
// docs/superpowers/plans/2026-10-05-api-parameter-parity.md ("Decision gates").
const (
	GateCasing    = "GA" // Claude thinkingConfig key casing
	GateAdaptive  = "GB" // Claude thinkingConfig without a budget
	GateSampling  = "GC" // sampling parameters under thinking
	GateMaxBudget = "GD" // large Claude thinking budget; budget == maxOutputTokens
	GateOutputCap = "GE" // Gemini maxOutputTokens at the advertised limit
	GateMinimal   = "GF" // thinkingLevel MINIMAL
	GateBoth      = "GX" // thinkingLevel and thinkingBudget in one request
	GateEffort    = "GH" // whether the budget moves real thinking
)

const (
	okPrompt     = "Reply with exactly: OK"
	reasonPrompt = "What is the sum of the first 40 prime numbers? Reply with only the number."
)

// Models are the upstream routing IDs the matrix runs against. Both must be
// IDs the account's fetchAvailableModels catalog publishes.
type Models struct {
	Claude string // a Claude thinking route, e.g. claude-opus-4-6-thinking
	Tier   string // a thinkingLevel-style Gemini route, e.g. gemini-3.8-flash-high
}

// Case is one upstream call. Config replaces the request's generationConfig
// verbatim: the probe asks what Cloud Code accepts, not what the converter
// would emit.
type Case struct {
	ID       string
	Gate     string
	Question string
	Model    string
	Prompt   string
	Heavy    bool // spends real thinking tokens; needs -heavy
	Config   map[string]any
}

func claudeCase(m Models, id, gate, question string, config map[string]any) Case {
	return Case{ID: id, Gate: gate, Question: question, Model: m.Claude, Prompt: okPrompt, Config: config}
}

func tierCase(m Models, id, gate, question string, config map[string]any) Case {
	return Case{ID: id, Gate: gate, Question: question, Model: m.Tier, Prompt: okPrompt, Config: config}
}

func snake(budget any) map[string]any {
	config := map[string]any{"include_thoughts": true}
	if budget != nil {
		config["thinking_budget"] = budget
	}
	return config
}

// buildCases returns the fixed probe matrix. It does no I/O.
func buildCases(m Models) []Case {
	cases := []Case{
		claudeCase(m, "GA-claude-snake", GateCasing,
			"baseline: the snake_case thinkingConfig the proxy sends to Claude today",
			map[string]any{"maxOutputTokens": 512, "thinkingConfig": snake(1024)}),
		claudeCase(m, "GA-claude-camel", GateCasing,
			"does the Claude route also accept the proto JSON (camelCase) spelling?",
			map[string]any{"maxOutputTokens": 512, "thinkingConfig": map[string]any{"includeThoughts": true, "thinkingBudget": 1024}}),
		claudeCase(m, "GB-claude-no-budget", GateAdaptive,
			"does the Claude route accept thinkingConfig without a budget?",
			map[string]any{"maxOutputTokens": 512, "thinkingConfig": snake(nil)}),
		claudeCase(m, "GC-claude-temperature", GateSampling,
			"does Cloud Code reject a non-default temperature while Claude thinks?",
			map[string]any{"maxOutputTokens": 512, "temperature": 0.2, "thinkingConfig": snake(1024)}),
		claudeCase(m, "GC-claude-top-k", GateSampling,
			"does Cloud Code reject topK while Claude thinks?",
			map[string]any{"maxOutputTokens": 512, "topK": 10, "thinkingConfig": snake(1024)}),
		claudeCase(m, "GD-claude-budget-55808", GateMaxBudget,
			"is a budget of maxOutputTokens-8192 accepted (the largest sensible Claude budget)?",
			map[string]any{"maxOutputTokens": 64000, "thinkingConfig": snake(55808)}),
		claudeCase(m, "GD-claude-budget-equals-max", GateMaxBudget,
			"negative control: is a budget equal to maxOutputTokens rejected?",
			map[string]any{"maxOutputTokens": 4096, "thinkingConfig": snake(4096)}),
		tierCase(m, "GE-tier-65536", GateOutputCap,
			"is maxOutputTokens at the advertised 65536 accepted on a Gemini route?",
			map[string]any{"maxOutputTokens": 65536, "thinkingConfig": map[string]any{"includeThoughts": true, "thinkingLevel": "LOW"}}),
		tierCase(m, "GF-tier-minimal", GateMinimal,
			"is thinkingLevel MINIMAL accepted on the served Gemini tier?",
			map[string]any{"maxOutputTokens": 512, "thinkingConfig": map[string]any{"includeThoughts": true, "thinkingLevel": "MINIMAL"}}),
		tierCase(m, "GX-tier-level-and-budget", GateBoth,
			"is thinkingLevel together with thinkingBudget rejected?",
			map[string]any{"maxOutputTokens": 512, "thinkingConfig": map[string]any{"includeThoughts": true, "thinkingLevel": "LOW", "thinkingBudget": 1024}}),
	}
	for _, budget := range []int{1024, 8000, 32000} {
		cases = append(cases, Case{
			ID: fmt.Sprintf("GH-claude-effort-%d", budget), Gate: GateEffort,
			Question: "does the budget the effort table emits change how much Claude thinks?",
			Model:    m.Claude, Prompt: reasonPrompt, Heavy: true,
			Config: map[string]any{"maxOutputTokens": 40000, "thinkingConfig": snake(budget)},
		})
	}
	return cases
}

// Result is one probe's outcome. Status 0 means the call failed before a
// response; Error carries the detail.
type Result struct {
	ID             string    `json:"id"`
	Gate           string    `json:"gate"`
	Question       string    `json:"question"`
	Model          string    `json:"model"`
	Status         int       `json:"status"`
	Error          string    `json:"error,omitempty"`
	StopReason     string    `json:"stopReason,omitempty"`
	ThinkingBlocks int       `json:"thinkingBlocks"`
	ThinkingTokens int       `json:"thinkingTokens"`
	OutputTokens   int       `json:"outputTokens"`
	LatencyMS      int64     `json:"latencyMs"`
	At             time.Time `json:"at"`
}

// Outcome classifies a result for the decision gates. Only an upstream 400
// counts as a rejection: a 429 or a transport error says nothing about the
// request shape.
func (result Result) Outcome() string {
	switch result.Status {
	case 200:
		return "accepted"
	case 400:
		return "rejected"
	}
	return "inconclusive"
}

// Summary groups outcomes by gate: "GA: GA-claude-snake=accepted, ...".
func Summary(results []Result) []string {
	order := []string{}
	byGate := map[string][]string{}
	for _, result := range results {
		if _, seen := byGate[result.Gate]; !seen {
			order = append(order, result.Gate)
		}
		byGate[result.Gate] = append(byGate[result.Gate], result.ID+"="+result.Outcome())
	}
	lines := make([]string, 0, len(order))
	for _, gate := range order {
		lines = append(lines, gate+": "+strings.Join(byGate[gate], ", "))
	}
	return lines
}

// buildPayload wraps the case in the production Cloud Code envelope, then
// swaps in the case's generationConfig.
func buildPayload(c Case, project, email string) map[string]any {
	request := map[string]any{
		"model":    c.Model,
		"messages": []any{map[string]any{"role": "user", "content": c.Prompt}},
	}
	payload := proxyformat.NewBuilder().BuildCloudCodeRequestWithModel(request, project, email, proxyformat.ModelOptions{
		SupportsThinking: proxyformat.IsThinkingModel(c.Model),
	})
	inner, _ := payload["request"].(map[string]any)
	inner["generationConfig"] = c.Config
	return payload
}
