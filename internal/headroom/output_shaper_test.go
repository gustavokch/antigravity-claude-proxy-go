package headroom

import (
	"context"
	"strings"
	"testing"
)

func shaperCfg() *Config {
	return &Config{Enabled: true, OutputShaper: OutputShaperConfig{
		Enabled: true, VerbositySteering: true, EffortRouting: true,
		MechanicalThinkingBudget: 1024,
	}}
}

func TestOutputShaper_AppendsSteeringToStringSystem(t *testing.T) {
	req := map[string]any{"system": "You are a helpful assistant."}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), &RequestContext{Request: req}, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	system := req["system"].(string)
	if !strings.HasPrefix(system, "You are a helpful assistant.") {
		t.Error("original system prompt must be preserved as the prefix")
	}
	if !strings.Contains(system, DefaultVerbosityPrompt) {
		t.Errorf("steering text missing: %q", system)
	}
}

func TestOutputShaper_AppendsBlockToArraySystem(t *testing.T) {
	req := map[string]any{"system": []any{
		map[string]any{"type": "text", "text": "base", "cache_control": map[string]any{"type": "ephemeral"}},
	}}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), &RequestContext{Request: req}, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	blocks := req["system"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 system blocks, got %d", len(blocks))
	}
	first := blocks[0].(map[string]any)
	if first["text"] != "base" || first["cache_control"] == nil {
		t.Error("existing cached system block must be untouched")
	}
}

func TestOutputShaper_UsesCustomSteeringText(t *testing.T) {
	cfg := shaperCfg()
	cfg.OutputShaper.SteeringText = "Be terse."
	req := map[string]any{"system": "base"}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), &RequestContext{Request: req}, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(req["system"].(string), "Be terse.") {
		t.Errorf("custom steering text not applied: %q", req["system"])
	}
}

func toolContinuation(isError bool) map[string]any {
	block := map[string]any{"type": "tool_result", "content": "ok"}
	if isError {
		block["is_error"] = true
	}
	return map[string]any{"role": "user", "content": []any{block}}
}

func TestOutputShaper_ClampsThinkingOnMechanicalTurn(t *testing.T) {
	req := map[string]any{
		"max_tokens": float64(8192),
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": float64(16000)},
		"messages":   []any{toolContinuation(false)},
	}
	reqCtx := &RequestContext{Request: req}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), reqCtx, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req["thinking"].(map[string]any)["budget_tokens"]
	if got != float64(1024) {
		t.Errorf("expected clamp to 1024, got %v", got)
	}
	if !reqCtx.EffortClamped || reqCtx.OriginalThinking != 16000 || reqCtx.ClampedThinking != 1024 {
		t.Errorf("clamp telemetry not recorded: %+v", reqCtx)
	}
}

func TestOutputShaper_DoesNotClampOnErrorResult(t *testing.T) {
	req := map[string]any{
		"max_tokens": float64(8192),
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": float64(16000)},
		"messages":   []any{toolContinuation(true)},
	}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), &RequestContext{Request: req}, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req["thinking"].(map[string]any)["budget_tokens"] != float64(16000) {
		t.Error("must not clamp thinking when a tool result carries is_error")
	}
}

func TestOutputShaper_DoesNotClampOnUserTurn(t *testing.T) {
	req := map[string]any{
		"max_tokens": float64(8192),
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": float64(16000)},
		"messages":   []any{map[string]any{"role": "user", "content": "think hard about this"}},
	}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), &RequestContext{Request: req}, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req["thinking"].(map[string]any)["budget_tokens"] != float64(16000) {
		t.Error("must not clamp thinking on a real user turn")
	}
}

func TestOutputShaper_NeverInventsThinking(t *testing.T) {
	req := map[string]any{"max_tokens": float64(8192), "messages": []any{toolContinuation(false)}}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), &RequestContext{Request: req}, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, exists := req["thinking"]; exists {
		t.Error("must not add a thinking field the client did not send (invariant I4)")
	}
}

func TestOutputShaper_RespectsMaxTokensFloor(t *testing.T) {
	req := map[string]any{
		"max_tokens": float64(1024),
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": float64(16000)},
		"messages":   []any{toolContinuation(false)},
	}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), &RequestContext{Request: req}, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req["thinking"].(map[string]any)["budget_tokens"] != float64(16000) {
		t.Error("clamping to >= max_tokens would produce an invalid request; must skip")
	}
}

func TestOutputShaper_BypassWhenDisabled(t *testing.T) {
	req := map[string]any{"system": "Original"}
	stage := &OutputShaperStage{}
	cfg := &Config{Enabled: true, OutputShaper: OutputShaperConfig{Enabled: false, VerbositySteering: true}}
	if err := stage.Execute(context.Background(), &RequestContext{Request: req}, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req["system"] != "Original" {
		t.Error("system prompt must be untouched when the shaper is disabled")
	}
}

func TestOutputShaper_DoesNotClampCodingContinuation(t *testing.T) {
	req := map[string]any{
		"max_tokens": float64(8192),
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": float64(16000)},
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "tool_use", "id": "call_read", "name": "Read", "input": map[string]any{"file_path": "main.go"}},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "call_read", "content": "package main\n\nfunc main() {}\n"},
				},
			},
		},
	}
	reqCtx := &RequestContext{Request: req, Verbatim: NewToolInspector(req)}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), reqCtx, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req["thinking"].(map[string]any)["budget_tokens"]
	if got != float64(16000) {
		t.Errorf("expected 16000 budget_tokens to survive on coding continuation, got %v", got)
	}
	if reqCtx.EffortClamped {
		t.Error("EffortClamped must be false on coding continuation")
	}
}

func TestOutputShaper_ClampsMechanicalContinuation(t *testing.T) {
	req := map[string]any{
		"max_tokens": float64(8192),
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": float64(16000)},
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "tool_use", "id": "call_glob", "name": "Glob", "input": map[string]any{"pattern": "*.go"}},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "call_glob", "content": "main.go\n"},
				},
			},
		},
	}
	reqCtx := &RequestContext{Request: req, Verbatim: NewToolInspector(req)}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), reqCtx, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req["thinking"].(map[string]any)["budget_tokens"]
	if got != float64(1024) {
		t.Errorf("expected 1024 budget_tokens for mechanical continuation, got %v", got)
	}
	if !reqCtx.EffortClamped {
		t.Error("EffortClamped must be true on mechanical continuation")
	}
}

func TestOutputShaper_ClampCodingOptIn(t *testing.T) {
	cfg := shaperCfg()
	cfg.OutputShaper.ClampCodingContinuations = true
	req := map[string]any{
		"max_tokens": float64(8192),
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": float64(16000)},
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "tool_use", "id": "call_read", "name": "Read", "input": map[string]any{"file_path": "main.go"}},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "call_read", "content": "package main\n\nfunc main() {}\n"},
				},
			},
		},
	}
	reqCtx := &RequestContext{Request: req, Verbatim: NewToolInspector(req)}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req["thinking"].(map[string]any)["budget_tokens"]
	if got != float64(1024) {
		t.Errorf("expected ClampCodingContinuations to clamp to 1024, got %v", got)
	}
	if !reqCtx.EffortClamped {
		t.Error("EffortClamped must be true when ClampCodingContinuations is true")
	}
}

func TestOutputShaper_MechanicalMaxBytesRespected(t *testing.T) {
	cfg := shaperCfg()
	cfg.OutputShaper.MechanicalMaxBytes = 100

	// Payload under 100 bytes -> mechanical -> clamped
	underReq := map[string]any{
		"max_tokens": float64(8192),
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": float64(16000)},
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "content": strings.Repeat("x", 50)},
				},
			},
		},
	}
	underCtx := &RequestContext{Request: underReq}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), underCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !underCtx.EffortClamped {
		t.Error("expected payload under ceiling to be clamped as mechanical")
	}

	// Payload over 100 bytes -> coding -> NOT clamped
	overReq := map[string]any{
		"max_tokens": float64(8192),
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": float64(16000)},
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "content": strings.Repeat("x", 101)},
				},
			},
		},
	}
	overCtx := &RequestContext{Request: overReq}
	if err := stage.Execute(context.Background(), overCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if overCtx.EffortClamped {
		t.Error("expected payload over ceiling to NOT be clamped")
	}
}

func TestOutputShaper_ReasoningEffortNotDowngradedOnCoding(t *testing.T) {
	req := map[string]any{
		"reasoning_effort": "high",
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "tool_use", "id": "call_read", "name": "Read", "input": map[string]any{"file_path": "main.go"}},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "call_read", "content": "package main\n\nfunc main() {}\n"},
				},
			},
		},
	}
	reqCtx := &RequestContext{Request: req, Verbatim: NewToolInspector(req)}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), reqCtx, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req["reasoning_effort"] != "high" {
		t.Errorf("expected reasoning_effort 'high' to survive coding turn, got %v", req["reasoning_effort"])
	}
	if reqCtx.EffortClamped {
		t.Error("EffortClamped must be false on coding turn")
	}
}

func TestOutputShaper_RecordsContinuationKind(t *testing.T) {
	req := map[string]any{
		"max_tokens": float64(8192),
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": float64(16000)},
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "tool_use", "id": "call_read", "name": "Read", "input": map[string]any{"file_path": "main.go"}},
				},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "call_read", "content": "package main\n\nfunc main() {}\n"},
				},
			},
		},
	}
	reqCtx := &RequestContext{Request: req, Verbatim: NewToolInspector(req)}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), reqCtx, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reqCtx.ContinuationKind != "coding" {
		t.Errorf("expected reqCtx.ContinuationKind == %q, got %q", "coding", reqCtx.ContinuationKind)
	}
}

func TestLooksLikeTestOutput_ProseIsNotTestOutput(t *testing.T) {
	// Case-insensitive \bPASS\b, \berror:, and \bwarning: matched ordinary
	// prose, so almost every tool result classified as coding and effort
	// routing never clamped anything.
	//
	// The discriminator is position, not wording: a diagnostic prefix opens a
	// line, while prose mentions it mid-sentence. A line that genuinely starts
	// "warning:" is treated as a diagnostic even if what follows is short.
	prose := []string{
		"Please pass the auth token to the handler.",
		"The user does not have an error: field here",
		"Applied 1 edit to main.go",
		"Deleted 3 files.",
		"The first pass over the data is complete.",
	}
	for _, text := range prose {
		if looksLikeTestOutput(text) {
			t.Errorf("prose classified as test output: %q", text)
		}
	}
}

func TestLooksLikeTestOutput_RealRunnerOutput(t *testing.T) {
	outputs := []string{
		"--- FAIL: TestFoo (0.01s)",
		"ok  \tantigravity-go-proxy/internal/headroom\t0.007s",
		"PASS\nok  \texample/pkg\t0.2s",
		"panic: runtime error: index out of range",
		"Traceback (most recent call last):\n  File \"a.py\", line 1",
		"main.go:12:2: error: undefined: foo",
		"  warning: unused variable 'x'",
		"AssertionError: expected 1, got 2",
		"3 passed, 1 failed",
		"build failed",
	}
	for _, text := range outputs {
		if !looksLikeTestOutput(text) {
			t.Errorf("real runner output not detected: %q", text)
		}
	}
}

func TestClassifyContinuation_ZeroMaxBytesUsesDefault(t *testing.T) {
	// A zero ceiling means "unset", not "clamp everything". This pins that
	// meaning across the signature change from a variadic to a plain
	// parameter, where an omitted argument becomes an explicit 0.
	small := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "done"},
		}},
	}}
	if got := classifyContinuation(small, nil, 0); got != kindMechanical {
		t.Errorf("small payload with zero ceiling = %s, want mechanical", got)
	}

	large := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1",
				"content": strings.Repeat("x", defaultMechanicalMaxBytes+1)},
		}},
	}}
	if got := classifyContinuation(large, nil, 0); got == kindMechanical {
		t.Error("payload over the default ceiling with zero ceiling = mechanical, want the ceiling applied")
	}
}

func TestClassifyContinuation_LargeNonCodeIsLarge(t *testing.T) {
	// A payload over the ceiling with no code signal is not coding; it is only
	// big. Labelling it "coding" made the telemetry unreadable, since a real
	// edit loop and a long log line looked identical.
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1",
				"content": strings.Repeat("lorem ipsum dolor sit amet ", 200)},
		}},
	}}
	got := classifyContinuation(req, nil, 0)
	if got != kindLarge {
		t.Errorf("large non-code continuation = %s, want large", got)
	}
	if got.String() != "large" {
		t.Errorf("String() = %q, want %q", got.String(), "large")
	}
}

func TestOutputShaper_LargeContinuationIsNotClamped(t *testing.T) {
	req := map[string]any{
		"model":      "claude-opus-4",
		"max_tokens": float64(8192),
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": float64(4096)},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1",
					"content": strings.Repeat("lorem ipsum dolor sit amet ", 200)},
			}},
		},
	}
	reqCtx := &RequestContext{Request: req}
	stage := &OutputShaperStage{}
	if err := stage.Execute(context.Background(), reqCtx, shaperCfg()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reqCtx.ContinuationKind != "large" {
		t.Errorf("ContinuationKind = %q, want %q", reqCtx.ContinuationKind, "large")
	}
	if reqCtx.EffortClamped {
		t.Error("large continuation must not be clamped")
	}
}
