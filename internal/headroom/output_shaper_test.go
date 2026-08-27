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
