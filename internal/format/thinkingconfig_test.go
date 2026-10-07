package format

import (
	"strings"
	"testing"
)

// claudeOpts mirrors a live Cloud Code entry for Claude Opus 4.6 (Thinking):
// output cap 64000. Its catalog default budget (2048) is deliberately not one
// of the effort table's values, so a row that expects 1024 proves effort moved
// the budget instead of the catalog default standing.
var claudeOpts = ModelOptions{SupportsThinking: true, ThinkingBudget: 2048, MaxOutputTokens: 64000}

func convertWith(request map[string]any, options *ModelOptions) map[string]any {
	request["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
	if options == nil {
		return asMap(ConvertAnthropicToGoogle(request, NewSignatureCache())["generationConfig"])
	}
	return asMap(ConvertAnthropicToGoogleWithModel(request, NewSignatureCache(), *options)["generationConfig"])
}

func claudeRequestFor(model string, extra map[string]any) map[string]any {
	request := map[string]any{"model": model, "max_tokens": float64(64000)}
	for k, v := range extra {
		request[k] = v
	}
	return request
}

func claudeRequest(extra map[string]any) map[string]any {
	return claudeRequestFor("claude-opus-4-6-thinking", extra)
}

func budgetOf(t *testing.T, generation map[string]any) int {
	t.Helper()
	config := asMap(generation["thinkingConfig"])
	if config == nil {
		t.Fatalf("no thinkingConfig in %#v", generation)
	}
	for _, key := range []string{"thinking_budget", "thinkingBudget"} {
		if value, ok := config[key]; ok {
			return intValue(value, -1)
		}
	}
	t.Fatalf("thinkingConfig carries no budget: %#v", config)
	return 0
}

func effortConfig(level string) map[string]any { return map[string]any{"effort": level} }

// Claude Code sends output_config.effort on every request; on a budget-style
// model it is the only dial the client has, so it must move the budget.
func TestOutputConfigEffortSetsBudgetOnBudgetStyleModels(t *testing.T) {
	t.Parallel()
	adaptive := map[string]any{"type": "adaptive"}
	tests := []struct {
		name    string
		model   string
		options ModelOptions
		extra   map[string]any
		want    int
	}{
		{"claude: low", "claude-opus-4-6-thinking", claudeOpts, map[string]any{"thinking": adaptive, "output_config": effortConfig("low")}, 1024},
		{"claude: medium", "claude-opus-4-6-thinking", claudeOpts, map[string]any{"thinking": adaptive, "output_config": effortConfig("medium")}, 8000},
		{"claude: high", "claude-opus-4-6-thinking", claudeOpts, map[string]any{"thinking": adaptive, "output_config": effortConfig("high")}, 32000},
		{"claude: xhigh falls back to high", "claude-opus-4-6-thinking", claudeOpts, map[string]any{"thinking": adaptive, "output_config": effortConfig("xhigh")}, 32000},
		{"claude: max falls back to high", "claude-opus-4-6-thinking", claudeOpts, map[string]any{"thinking": adaptive, "output_config": effortConfig("max")}, 32000},
		{"claude: no effort keeps the catalog default", "claude-opus-4-6-thinking", claudeOpts, map[string]any{"thinking": adaptive}, 2048},
		{"claude: explicit budget beats ambient effort", "claude-opus-4-6-thinking", claudeOpts,
			map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": float64(20000)}, "output_config": effortConfig("low")}, 20000},
		{"claude: reasoning_effort beats an explicit budget", "claude-opus-4-6-thinking", claudeOpts,
			map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": float64(20000)}, "reasoning_effort": "low"}, 1024},
		{"claude: top-level thinking_budget is honored", "claude-opus-4-6-thinking", claudeOpts, map[string]any{"thinking_budget": float64(5000)}, 5000},
		{"gemini budget model: medium", "gemini-3.1-pro", ModelOptions{SupportsThinking: true, ThinkingBudget: 10001, MaxOutputTokens: 65535},
			map[string]any{"output_config": effortConfig("medium")}, 8000},
		{"gemini budget model: max falls back to high", "gemini-3.1-pro", ModelOptions{SupportsThinking: true, ThinkingBudget: 10001, MaxOutputTokens: 65535},
			map[string]any{"output_config": effortConfig("max")}, 16000},
		{"gpt-oss: low", "gpt-oss-120b", ModelOptions{SupportsThinking: true, ThinkingBudget: 8192, MaxOutputTokens: 32768},
			map[string]any{"output_config": effortConfig("low")}, 1024},

		// A tier named in the model ID keeps its own catalog budget: ambient
		// effort never overrides it (plan decision D1), a deliberate
		// reasoning_effort still does.
		{"gemini named tier: ambient low keeps the catalog budget", "gemini-3.1-pro-high", ModelOptions{SupportsThinking: true, ThinkingBudget: 10001, MaxOutputTokens: 65535},
			map[string]any{"output_config": effortConfig("low")}, 10001},
		{"gemini named tier: ambient max keeps the catalog budget", "gemini-3.1-pro-high", ModelOptions{SupportsThinking: true, ThinkingBudget: 10001, MaxOutputTokens: 65535},
			map[string]any{"output_config": effortConfig("max")}, 10001},
		{"gpt-oss named tier: ambient low keeps the catalog budget", "gpt-oss-120b-medium", ModelOptions{SupportsThinking: true, ThinkingBudget: 8192, MaxOutputTokens: 32768},
			map[string]any{"output_config": effortConfig("low")}, 8192},
		{"gemini named tier: reasoning_effort overrides the catalog budget", "gemini-3.1-pro-high", ModelOptions{SupportsThinking: true, ThinkingBudget: 10001, MaxOutputTokens: 65535},
			map[string]any{"output_config": effortConfig("high"), "reasoning_effort": "low"}, 1024},
		{"gemini named tier: an explicit budget still wins", "gemini-3.1-pro-high", ModelOptions{SupportsThinking: true, ThinkingBudget: 10001, MaxOutputTokens: 65535},
			map[string]any{"output_config": effortConfig("low"), "thinking_budget": float64(5000)}, 5000},
	}
	for _, tc := range tests {
		options := tc.options
		generation := convertWith(claudeRequestFor(tc.model, tc.extra), &options)
		if got := budgetOf(t, generation); got != tc.want {
			t.Errorf("%s: thinking budget = %d, want %d (%#v)", tc.name, got, tc.want, generation)
		}
	}
}

// A tiered model carries its level in the catalog; effort selects the tier in
// the catalog, so the converter must not second-guess it from the request.
func TestOutputConfigEffortDoesNotChangeTieredLevel(t *testing.T) {
	t.Parallel()
	options := ModelOptions{SupportsThinking: true, ThinkingLevel: "LOW", MaxOutputTokens: 65536}
	generation := convertWith(map[string]any{
		"model": "gemini-3.8-flash-low", "max_tokens": float64(1024), "output_config": effortConfig("max"),
	}, &options)
	config := asMap(generation["thinkingConfig"])
	if config["thinkingLevel"] != "LOW" {
		t.Fatalf("thinkingConfig = %#v", config)
	}
	if _, hasBudget := config["thinkingBudget"]; hasBudget {
		t.Fatalf("a thinkingLevel config must not also carry a budget: %#v", config)
	}
}

// The legacy single-credential path has no catalog entry. It used to overwrite
// every effort-derived budget with the clamped budget_tokens, so effort and a
// top-level thinking_budget were silently ignored for Gemini.
func TestLegacyGeminiPathHonorsEffortAndTopLevelBudget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		extra map[string]any
		want  int
	}{
		{"reasoning_effort low", map[string]any{"reasoning_effort": "low"}, 1024},
		{"output_config medium", map[string]any{"output_config": effortConfig("medium")}, 8000},
		{"top-level thinking_budget", map[string]any{"thinking_budget": float64(5000)}, 5000},
		{"no signal uses the Gemini default", map[string]any{}, DefaultGeminiThinkBudget},
	}
	for _, tc := range tests {
		generation := convertWith(claudeRequestFor("gemini-3.1-pro", tc.extra), nil)
		if got := budgetOf(t, generation); got != tc.want {
			t.Errorf("%s: budget = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Google documents different thinkingBudget ceilings per 2.5 model, and none
// for the later families.
func TestGeminiBudgetCeilingByModel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		model  string
		budget float64
		want   int
	}{
		{"gemini-2.5-pro-thinking", 30000, 30000},
		{"gemini-2.5-pro-thinking", 40000, 32768},
		{"gemini-2.5-flash-thinking", 20000, 20000},
		{"gemini-2.5-flash-thinking", 30000, 24576},
		{"gemini-3.1-pro-high", 100000, 100000},
		{"gemini-3.1-pro-high", 200000, 128000},
	}
	for _, tc := range tests {
		generation := convertWith(claudeRequestFor(tc.model, map[string]any{
			"thinking": map[string]any{"type": "enabled", "budget_tokens": tc.budget},
		}), nil)
		if got := budgetOf(t, generation); got != tc.want {
			t.Errorf("%s budget_tokens=%v: budget = %d, want %d", tc.model, tc.budget, got, tc.want)
		}
	}
}

// A non-positive bare budget turns thinking off in the catalog's tier routing;
// the converter used to disagree and emit the default budget anyway.
func TestZeroThinkingBudgetTurnsThinkingOffOnBudgetStyleModels(t *testing.T) {
	t.Parallel()
	options := claudeOpts
	generation := convertWith(claudeRequest(map[string]any{"thinking_budget": float64(0)}), &options)
	if _, has := generation["thinkingConfig"]; has {
		t.Fatalf("thinkingConfig = %#v, want none", generation["thinkingConfig"])
	}
}

// Real Claude Code requests carry thinking.display and, after a model switch,
// redacted_thinking blocks from an Anthropic-native backend. Neither may leak
// upstream: Cloud Code cannot replay another backend's encrypted payload.
func TestRedactedThinkingFromAnotherBackendDoesNotReachCloudCode(t *testing.T) {
	t.Parallel()
	request := map[string]any{
		"model": "claude-opus-4-6-thinking", "max_tokens": float64(4096),
		"thinking": map[string]any{"type": "adaptive", "display": "summarized"},
		"messages": []any{
			map[string]any{"role": "user", "content": "run it"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "redacted_thinking", "data": strings.Repeat("R", 200)},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "bash", "input": map[string]any{"command": "ls"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"},
			}},
		},
	}
	options := claudeOpts
	converted := ConvertAnthropicToGoogleWithModel(request, NewSignatureCache(), options)
	encoded := stringValue(converted)
	for _, forbidden := range []string{"redacted_thinking", strings.Repeat("R", 50)} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("converted request leaks %q: %s", forbidden, encoded)
		}
	}
	if asMap(asMap(converted["generationConfig"])["thinkingConfig"]) == nil {
		t.Fatalf("thinking must stay enabled: %#v", converted["generationConfig"])
	}
}

// The body fields real Claude Code sends (.reference/claude-code-headers-20260923.txt:
// context_management, diagnostics, max_tokens, messages, metadata, model,
// output_config, stream, system, thinking, tools; thinking carries type+display,
// output_config carries effort; no sampling fields) must convert to a
// generationConfig that holds only what Cloud Code understands.
func TestClaudeCodeCapturedBodyShapeConvertsToACleanGenerationConfig(t *testing.T) {
	t.Parallel()
	request := map[string]any{
		"model":              "claude-opus-4-6-thinking",
		"max_tokens":         float64(64000),
		"stream":             true,
		"thinking":           map[string]any{"type": "adaptive", "display": "summarized"},
		"output_config":      map[string]any{"effort": "high"},
		"context_management": map[string]any{"edits": []any{map[string]any{"type": "clear_thinking_20251015", "keep": "all"}}},
		"diagnostics":        map[string]any{"client": "claude-code"},
		"metadata":           map[string]any{"user_id": "{}"},
		"messages":           []any{map[string]any{"role": "user", "content": "hi"}},
	}
	options := claudeOpts
	converted := ConvertAnthropicToGoogleWithModel(request, NewSignatureCache(), options)
	generation := asMap(converted["generationConfig"])
	if len(generation) != 2 || intValue(generation["maxOutputTokens"], 0) != 64000 || budgetOf(t, generation) != 32000 {
		t.Fatalf("generationConfig = %#v, want only maxOutputTokens 64000 and thinking_budget 32000", generation)
	}
	encoded := stringValue(converted)
	for _, leaked := range []string{"output_config", "context_management", "diagnostics", "display", "clear_thinking"} {
		if strings.Contains(encoded, leaked) {
			t.Errorf("Cloud Code request leaks the client-only field %q: %s", leaked, encoded)
		}
	}
}

// Anthropic rejects budget_tokens >= max_tokens, and Google counts thought
// tokens against maxOutputTokens, so the budget must stay below the final
// maxOutputTokens however the model cap and the client's value interact.
func TestThinkingBudgetStaysBelowMaxOutputTokens(t *testing.T) {
	t.Parallel()
	enabled := func(budget float64) map[string]any {
		return map[string]any{"type": "enabled", "budget_tokens": budget}
	}
	tests := []struct {
		name       string
		maxTokens  float64
		budget     float64
		cap        int
		wantMax    int
		wantBudget int
	}{
		{"max_tokens above budget is untouched", 64000, 20000, 64000, 64000, 20000},
		{"max_tokens at the budget is raised by the headroom", 16000, 16000, 64000, 24192, 16000},
		{"max_tokens below the budget is raised by the headroom", 16000, 20000, 64000, 28192, 20000},
		{"the cap stops the raise and the budget shrinks", 80000, 70000, 64000, 64000, 55808},
		{"a small cap halves the room instead", 5000, 10000, 8192, 8192, 4096},
		{"no cap: only the raise applies", 16000, 20000, 0, 28192, 20000},
	}
	for _, tc := range tests {
		options := ModelOptions{SupportsThinking: true, ThinkingBudget: 1024, MaxOutputTokens: tc.cap}
		generation := convertWith(map[string]any{
			"model": "claude-opus-4-6-thinking", "max_tokens": tc.maxTokens, "thinking": enabled(tc.budget),
		}, &options)
		gotMax, gotBudget := intValue(generation["maxOutputTokens"], 0), budgetOf(t, generation)
		if gotMax != tc.wantMax || gotBudget != tc.wantBudget {
			t.Errorf("%s: maxOutputTokens=%d budget=%d, want %d and %d", tc.name, gotMax, gotBudget, tc.wantMax, tc.wantBudget)
		}
		if gotBudget >= gotMax {
			t.Errorf("%s: budget %d is not below maxOutputTokens %d", tc.name, gotBudget, gotMax)
		}
	}
}

// /v1/models advertises the catalog's max output, so a client that sends
// exactly that must not be cut to a smaller proxy-side ceiling. A live limit
// takes precedence, while an entry with no live limit falls back to the ceiling.
func TestGeminiOutputIsCappedByTheLiveLimitNotAFixedCeiling(t *testing.T) {
	t.Parallel()
	high := ModelOptions{SupportsThinking: true, ThinkingLevel: "HIGH", MaxOutputTokens: 65536}
	low := ModelOptions{SupportsThinking: true, ThinkingLevel: "HIGH", MaxOutputTokens: 8192}
	tests := []struct {
		name      string
		maxTokens float64
		options   *ModelOptions
		want      int
	}{
		{"the advertised limit goes through", 65536, &high, 65536},
		{"above the limit is cut to the limit", 100000, &high, 65536},
		{"below the limit is untouched", 4096, &high, 4096},
		{"a live limit below the ceiling still applies", 16000, &low, 8192},
		{"no live limit falls back to the ceiling", 65536, nil, GeminiMaxOutputTokens},
		{"an entry without a limit falls back to the ceiling", 65536, &ModelOptions{SupportsThinking: true, ThinkingLevel: "HIGH"}, GeminiMaxOutputTokens},
	}
	for _, tc := range tests {
		generation := convertWith(map[string]any{"model": "gemini-3.8-flash-high", "max_tokens": tc.maxTokens}, tc.options)
		if got := intValue(generation["maxOutputTokens"], 0); got != tc.want {
			t.Errorf("%s: maxOutputTokens = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Google counts thought tokens against maxOutputTokens, so an effort-derived
// budget is capped to leave room for the answer when output headroom is tight.
// A budget the client or the catalog chose is not touched.
func TestEffortDerivedBudgetKeepsRoomForTheAnswer(t *testing.T) {
	t.Parallel()
	gemini := ModelOptions{SupportsThinking: true, ThinkingBudget: 10001, MaxOutputTokens: 65535}
	gptOSS := ModelOptions{SupportsThinking: true, ThinkingBudget: 8192, MaxOutputTokens: 32768}
	tests := []struct {
		name       string
		model      string
		options    ModelOptions
		extra      map[string]any
		wantMax    int
		wantBudget int
	}{
		{"ambient high has room under live cap", "gemini-3.1-pro", gemini,
			map[string]any{"output_config": effortConfig("high")}, 64000, 16000},
		{"ambient medium is below the limit and kept", "gemini-3.1-pro", gemini,
			map[string]any{"output_config": effortConfig("medium")}, 64000, 8000},
		{"explicit reasoning_effort high has room under live cap", "gemini-3.1-pro", gemini,
			map[string]any{"reasoning_effort": "high"}, 64000, 16000},
		{"an explicit client budget is kept", "gemini-3.1-pro", gemini,
			map[string]any{"thinking_budget": float64(16000)}, 64000, 16000},
		{"a small max_tokens halves the room", "gemini-3.1-pro", gemini,
			map[string]any{"output_config": effortConfig("high"), "max_tokens": float64(4096)}, 4096, 2048},
		{"gpt-oss has room under its own cap", "gpt-oss-120b", gptOSS,
			map[string]any{"output_config": effortConfig("high")}, 32768, 16000},
	}
	for _, tc := range tests {
		options := tc.options
		generation := convertWith(claudeRequestFor(tc.model, tc.extra), &options)
		gotMax, gotBudget := intValue(generation["maxOutputTokens"], 0), budgetOf(t, generation)
		if gotMax != tc.wantMax || gotBudget != tc.wantBudget {
			t.Errorf("%s: maxOutputTokens=%d budget=%d, want %d and %d", tc.name, gotMax, gotBudget, tc.wantMax, tc.wantBudget)
		}
	}
}
