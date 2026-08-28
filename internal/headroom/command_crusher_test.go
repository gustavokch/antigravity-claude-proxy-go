package headroom

import (
	"context"
	"testing"
)

func crusherConfig() Config {
	return Config{Enabled: true, CommandCrusher: true, PreserveVerbatimReads: true}
}

func TestCommandCrusher_IsErrorUntouched(t *testing.T) {
	payload := "collected 1 items\n\ntest_a.py F [100%]\n\n=== 1 failed in 0.01s ==="
	req := map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "is_error": true, "content": payload},
	}}}}

	reqCtx := &RequestContext{Request: req}
	if err := (&CommandCrusherStage{}).Execute(context.Background(), reqCtx, &Config{Enabled: true, CommandCrusher: true}); err != nil {
		t.Fatal(err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if got != payload {
		t.Errorf("is_error payload must pass through byte-for-byte, got %q", got)
	}
}

func TestCommandCrusher_VerbatimSkipped(t *testing.T) {
	// cat -n numbered source that happens to contain a go-test-looking line.
	payload := "     1\tpackage main\n     2\t// === RUN fake\n     3\tfunc main() {}\n"
	req := map[string]any{"messages": []any{toolResultMsg(payload)}}
	cfg := crusherConfig()
	reqCtx := &RequestContext{Request: req, Verbatim: NewToolInspector(req)}

	if err := (&CommandCrusherStage{}).Execute(context.Background(), reqCtx, &cfg); err != nil {
		t.Fatal(err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if got != payload {
		t.Errorf("verbatim payload corrupted, got %q", got)
	}
	if reqCtx.VerbatimSkipped != 1 {
		t.Errorf("expected VerbatimSkipped=1, got %d", reqCtx.VerbatimSkipped)
	}
}

func TestCommandCrusher_I3_AssistantTextUntouched(t *testing.T) {
	assistantText := "collected 1 items\n=== 1 passed in 0.01s ==="
	req := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": assistantText},
		}},
		toolResultMsg("collected 1 items\n\ntest_a.py . [100%]\n\n=== 1 passed in 0.01s ==="),
	}}
	cfg := crusherConfig()
	reqCtx := &RequestContext{Request: req}

	if err := (&CommandCrusherStage{}).Execute(context.Background(), reqCtx, &cfg); err != nil {
		t.Fatal(err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if got != assistantText {
		t.Errorf("assistant text mutated (I3 violation), got %q", got)
	}
}

func TestErrorOrdinals_MixedBlocks(t *testing.T) {
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "a", "is_error": true, "content": "err"},
			map[string]any{"type": "tool_result", "tool_use_id": "b", "content": []any{
				map[string]any{"type": "text", "text": "one"},
				map[string]any{"type": "text", "text": "two"},
			}},
		}},
	}}
	errs := errorOrdinals(req)
	if !errs[0] || errs[1] || errs[2] {
		t.Errorf("expected only ordinal 0 marked, got %v", errs)
	}
}
