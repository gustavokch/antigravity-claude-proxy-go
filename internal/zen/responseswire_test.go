package zen

import (
	"encoding/json"
	"testing"
)

func responsesInputItems(t *testing.T, body map[string]any) []any {
	t.Helper()
	items, ok := body["input"].([]any)
	if !ok {
		t.Fatalf("input is %T, want []any", body["input"])
	}
	return items
}

func TestAnthropicToResponsesRequest_ConversationShape(t *testing.T) {
	req := map[string]any{
		"model":      "opencode/gpt-5.5",
		"max_tokens": float64(512),
		"system":     "be terse",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "checking"},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ls"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "a.txt"},
				map[string]any{"type": "text", "text": "thanks"},
			}},
		},
		"tools": []any{
			map[string]any{"name": "Bash", "description": "run", "input_schema": map[string]any{"type": "object"}},
		},
		"tool_choice": map[string]any{"type": "auto"},
		"stream":      false,
	}

	out, rev, injected := anthropicToResponsesRequest(req)

	if out["model"] != "gpt-5.5" {
		t.Errorf("model = %v, want gpt-5.5 (opencode/ prefix stripped)", out["model"])
	}
	if out["max_output_tokens"] != float64(512) {
		t.Errorf("max_output_tokens = %v, want 512", out["max_output_tokens"])
	}
	if out["stream"] != true {
		t.Errorf("stream = %v, want true (gate rejects non-streaming bodies)", out["stream"])
	}
	if _, ok := out["max_tokens"]; ok {
		t.Error("max_tokens must not leak into a Responses body")
	}

	items := responsesInputItems(t, out)
	if len(items) != 6 {
		t.Fatalf("input has %d items, want 6 (system, user, assistant, function_call, function_call_output, user): %s", len(items), mustJSON(t, items))
	}

	sys, _ := items[0].(map[string]any)
	if sys["role"] != "system" {
		t.Errorf("input[0].role = %v, want system", sys["role"])
	}
	sysParts, _ := sys["content"].([]any)
	if len(sysParts) != 1 {
		t.Fatalf("system content = %v, want one input_text part", sysParts)
	}
	if part, _ := sysParts[0].(map[string]any); part["type"] != "input_text" || part["text"] != "be terse" {
		t.Errorf("system part = %v, want input_text/be terse", sysParts[0])
	}

	user0, _ := items[1].(map[string]any)
	if user0["role"] != "user" {
		t.Errorf("input[1].role = %v, want user", user0["role"])
	}

	assistant, _ := items[2].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("input[2].role = %v, want assistant: %s", assistant["role"], mustJSON(t, items[2]))
	}
	aparts, _ := assistant["content"].([]any)
	if len(aparts) != 1 {
		t.Fatalf("assistant content = %v, want one output_text part", aparts)
	}
	if part, _ := aparts[0].(map[string]any); part["type"] != "output_text" || part["text"] != "checking" {
		t.Errorf("assistant part = %v, want output_text/checking", aparts[0])
	}

	call, _ := items[3].(map[string]any)
	if call["type"] != "function_call" {
		t.Fatalf("input[3].type = %v, want function_call: %s", call["type"], mustJSON(t, items[3]))
	}
	if call["name"] != "bash" {
		t.Errorf("function_call name = %v, want bash (gate spelling; client declared Bash)", call["name"])
	}
	if call["call_id"] != "toolu_1" {
		t.Errorf("function_call call_id = %v, want toolu_1", call["call_id"])
	}
	if args, _ := call["arguments"].(string); args != `{"command":"ls"}` {
		t.Errorf("function_call arguments = %q, want the JSON-encoded input", args)
	}

	// A function_call_output must directly follow its function_call, so the
	// user turn's tool_result items lead the turn; the turn's remaining text
	// then becomes the trailing user message.
	answer, _ := items[4].(map[string]any)
	if answer["type"] != "function_call_output" {
		t.Fatalf("input[4].type = %v, want function_call_output: %s", answer["type"], mustJSON(t, items[4]))
	}
	if answer["call_id"] != "toolu_1" {
		t.Errorf("function_call_output call_id = %v, want toolu_1", answer["call_id"])
	}
	if answer["output"] != "a.txt" {
		t.Errorf("function_call_output output = %v, want a.txt", answer["output"])
	}

	closing, _ := items[5].(map[string]any)
	if closing["role"] != "user" {
		t.Fatalf("input[5].role = %v, want user: %s", closing["role"], mustJSON(t, items[5]))
	}
	cparts, _ := closing["content"].([]any)
	if len(cparts) != 1 {
		t.Fatalf("closing user content = %v, want one input_text part", cparts)
	}
	if part, _ := cparts[0].(map[string]any); part["type"] != "input_text" || part["text"] != "thanks" {
		t.Errorf("closing user part = %v, want input_text/thanks", cparts[0])
	}

	tools, _ := out["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		tool, _ := tl.(map[string]any)
		if tool["type"] != "function" {
			t.Errorf("tool = %v, want type function (Responses tools are flat)", tool)
		}
		n, _ := tool["name"].(string)
		names[n] = true
		if _, wrapped := tool["function"]; wrapped {
			t.Errorf("tool %q is chat-wrapped; Responses tools are flat %s", n, mustJSON(t, tool))
		}
	}
	if !names["bash"] {
		t.Error("tools must always contain bash (gate)")
	}
	if !names["read"] {
		t.Error("tools must always contain read (gate)")
	}
	if rev["bash"] != "Bash" {
		t.Errorf("reverse rename = %v, want bash→Bash", rev)
	}
	if !injected["read"] {
		t.Error("read was not declared by the client, so it must be reported as injected")
	}
	if injected["bash"] {
		t.Error("bash was declared (as Bash), so it must not be reported as injected")
	}
}

// Server tools (no input_schema) have no Responses equivalent and are dropped,
// and stop_sequences has no Responses field at all.
func TestAnthropicToResponsesRequest_DropsUnsupportedFields(t *testing.T) {
	req := map[string]any{
		"model": "gpt-5",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"stop_sequences": []any{"END"},
		"temperature":    0.3,
		"top_p":          0.9,
		"thinking":       map[string]any{"type": "enabled"},
		"stream":         true,
		"tools": []any{
			map[string]any{"type": "web_search_20250305", "name": "web_search"},
		},
	}

	out, _, injected := anthropicToResponsesRequest(req)

	if _, ok := out["stop_sequences"]; ok {
		t.Error("stop_sequences must be dropped: the Responses API has no equivalent field")
	}
	if out["temperature"] != 0.3 || out["top_p"] != 0.9 {
		t.Errorf("sampling params = %v/%v, want 0.3/0.9", out["temperature"], out["top_p"])
	}
	if _, ok := out["thinking"]; ok {
		t.Error("thinking must be dropped: the Responses API has no thinking field")
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 2 {
		t.Errorf("tools = %s, want only the two injected gate tools (server tools dropped)", mustJSON(t, tools))
	}
	if len(injected) != 2 {
		t.Errorf("injected = %v, want bash and read", injected)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
