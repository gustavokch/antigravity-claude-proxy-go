package headroom

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func fullConfig() Config {
	return Config{
		Enabled: true, SmartCrusher: true, CodeCompressor: true, LiveTurns: 2,
		OutputShaper: OutputShaperConfig{Enabled: true, VerbositySteering: true, EffortRouting: true, MechanicalThinkingBudget: 1024},
	}
}

func toolResultMsg(payload string) any {
	return map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "tu_" + payload[:1], "content": payload},
	}}
}

func TestEngine_CompressesToolResults(t *testing.T) {
	engine := NewEngine(fullConfig())
	req := map[string]any{"messages": []any{toolResultMsg("{\n  \"a\": 1\n}")}}

	reqCtx, err := engine.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"]
	if got != `{"a":1}` {
		t.Errorf("expected compacted tool_result, got %q", got)
	}
	if reqCtx.BytesBefore <= reqCtx.BytesAfter {
		t.Errorf("expected savings, got before=%d after=%d", reqCtx.BytesBefore, reqCtx.BytesAfter)
	}
}

func TestEngine_DisabledIsCompleteBypass(t *testing.T) {
	engine := NewEngine(Config{Enabled: false, SmartCrusher: true, CodeCompressor: true})
	original := "{\n  \"a\": 1\n}"
	req := map[string]any{"system": "base", "messages": []any{toolResultMsg(original)}}

	if _, err := engine.Process(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"]
	if got != original || req["system"] != "base" {
		t.Error("disabled engine must not modify the request at all")
	}
}

// TestEngine_PrefixBytesStableAcrossTurns is the executable form of invariant
// I1: the serialized bytes of the shared conversation prefix must be identical
// between turn N and turn N+1, or the provider prompt cache misses every turn.
func TestEngine_PrefixBytesStableAcrossTurns(t *testing.T) {
	payloads := []string{
		"{\n  \"first\": true\n}",
		"log line   \n\n\n\nmore log",
		"{\n  \"third\": [1, 2, 3]\n}",
	}
	build := func(n int) map[string]any {
		msgs := make([]any, 0, n)
		for i := 0; i < n; i++ {
			msgs = append(msgs, toolResultMsg(payloads[i%len(payloads)]))
		}
		return map[string]any{"system": "base", "messages": msgs}
	}

	engine := NewEngine(fullConfig())

	turn1 := build(3)
	if _, err := engine.Process(context.Background(), turn1); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	turn2 := build(5)
	if _, err := engine.Process(context.Background(), turn2); err != nil {
		t.Fatalf("turn 2: %v", err)
	}

	prefix1, _ := json.Marshal(turn1["messages"].([]any))
	prefix2, _ := json.Marshal(turn2["messages"].([]any)[:3])
	if string(prefix1) != string(prefix2) {
		t.Errorf("prefix diverged between turns; prompt cache would miss\nturn1: %s\nturn2: %s", prefix1, prefix2)
	}
	if sys1, sys2 := turn1["system"], turn2["system"]; sys1 != sys2 {
		t.Errorf("system prompt diverged between turns: %v vs %v", sys1, sys2)
	}
}

func TestEngine_Idempotent(t *testing.T) {
	engine := NewEngine(fullConfig())
	req := map[string]any{"messages": []any{toolResultMsg("{\n  \"a\": 1\n}"), toolResultMsg("b   \n\n\n\nb2")}}

	if _, err := engine.Process(context.Background(), req); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	once, _ := json.Marshal(req["messages"])
	if _, err := engine.Process(context.Background(), req); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	twice, _ := json.Marshal(req["messages"])
	if string(once) != string(twice) {
		t.Errorf("pipeline is not idempotent\nonce:  %s\ntwice: %s", once, twice)
	}
}

func TestEngine_UpdateConfigTakesEffect(t *testing.T) {
	engine := NewEngine(Config{Enabled: false})
	engine.UpdateConfig(fullConfig())
	if !engine.GetConfig().Enabled {
		t.Fatal("expected updated config to be live")
	}
	req := map[string]any{"messages": []any{toolResultMsg("{\n  \"a\": 1\n}")}}
	if _, err := engine.Process(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"]
	if got != `{"a":1}` {
		t.Errorf("updated config not applied, got %q", got)
	}
}

func TestEngine_ConcurrentProcessIsSafe(t *testing.T) {
	engine := NewEngine(fullConfig())
	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			req := map[string]any{"messages": []any{toolResultMsg(strings.Repeat("{\n \"x\": 1\n}", 1))}}
			_, _ = engine.Process(context.Background(), req)
		}()
	}
	for i := 0; i < 16; i++ {
		<-done
	}
}
