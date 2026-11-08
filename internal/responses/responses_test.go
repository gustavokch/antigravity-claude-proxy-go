package responses

import (
	"encoding/json"
	"testing"
)

// The JSON body of POST /v1/responses becomes an Anthropic message: output
// items flatten into content blocks, usage moves to the Anthropic convention,
// and stop_reason is derived (there is no finish_reason on this wire).
func TestToAnthropic_OutputItems(t *testing.T) {
	resp := map[string]any{
		"id": "resp_123",
		"output": []any{
			map[string]any{"type": "reasoning", "summary": []any{
				map[string]any{"type": "summary_text", "text": "thinking hard"},
			}},
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "listing"},
			}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "bash", "arguments": `{"command":"ls"}`},
		},
		"usage": map[string]any{
			"input_tokens":         100,
			"output_tokens":        20,
			"input_tokens_details": map[string]any{"cached_tokens": 40},
		},
	}

	out := ToAnthropic(resp, Options{Model: "gpt-5.5", ToolNames: map[string]string{"bash": "Bash"}, Drop: map[string]bool{"read": true}})

	if out["id"] != "msg_resp_123" {
		t.Errorf("id = %v, want msg_resp_123", out["id"])
	}
	if out["type"] != "message" || out["role"] != "assistant" {
		t.Errorf("type/role = %v/%v, want message/assistant", out["type"], out["role"])
	}
	if out["model"] != "gpt-5.5" {
		t.Errorf("model = %v, want gpt-5.5", out["model"])
	}
	if out["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use (a function_call is in the output)", out["stop_reason"])
	}
	content, _ := out["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content = %s, want thinking + text + tool_use", mustJSON(t, content))
	}
	think, _ := content[0].(map[string]any)
	if think["type"] != "thinking" || think["thinking"] != "thinking hard" {
		t.Errorf("content[0] = %s, want a thinking block", mustJSON(t, content[0]))
	}
	text, _ := content[1].(map[string]any)
	if text["type"] != "text" || text["text"] != "listing" {
		t.Errorf("content[1] = %s, want a text block", mustJSON(t, content[1]))
	}
	tool, _ := content[2].(map[string]any)
	if tool["type"] != "tool_use" || tool["id"] != "call_1" || tool["name"] != "Bash" {
		t.Errorf("content[2] = %s, want tool_use renamed back to Bash", mustJSON(t, content[2]))
	}
	if input, _ := tool["input"].(map[string]any); input["command"] != "ls" {
		t.Errorf("tool input = %s, want the decoded arguments", mustJSON(t, tool["input"]))
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"] != 60 || usage["cache_read_input_tokens"] != 40 || usage["output_tokens"] != 20 {
		t.Errorf("usage = %s, want input 60 (100-40 cached), cache_read 40, output 20", mustJSON(t, usage))
	}
}

// incomplete_details.reason == "max_output_tokens" is the only signal that the
// output cap stopped the model; with no function call in the output that maps
// to stop_reason "max_tokens".
func TestToAnthropic_StopReasons(t *testing.T) {
	textOnly := ToAnthropic(map[string]any{
		"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{
			map[string]any{"type": "output_text", "text": "hi"},
		}}},
	}, Options{Model: "gpt-5"})
	if textOnly["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", textOnly["stop_reason"])
	}

	truncated := ToAnthropic(map[string]any{
		"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{
			map[string]any{"type": "output_text", "text": "cut"},
		}}},
		"incomplete_details": map[string]any{"reason": "max_output_tokens"},
	}, Options{Model: "gpt-5"})
	if truncated["stop_reason"] != "max_tokens" {
		t.Errorf("stop_reason = %v, want max_tokens", truncated["stop_reason"])
	}
}

// A function call to a gate-injected tool the client never declared has no
// client-side tool to resolve, so it must not reach the client and must not
// claim stop_reason "tool_use".
func TestToAnthropic_DropsInjectedToolCalls(t *testing.T) {
	resp := map[string]any{
		"output": []any{
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "done"},
			}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "read", "arguments": `{}`},
		},
	}
	out := ToAnthropic(resp, Options{Model: "gpt-5", Drop: map[string]bool{"read": true}})
	content, _ := out["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %s, want only the text block (injected tool call dropped)", mustJSON(t, content))
	}
	if out["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn (no surviving tool call)", out["stop_reason"])
	}
}

// A refusal part is not silently dropped, and incomplete_details
// content_filter is not reported as a clean end_turn: Anthropic's real
// stop_reason for a declined generation is "refusal".
func TestToAnthropic_Refusal(t *testing.T) {
	refused := ToAnthropic(map[string]any{
		"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{
			map[string]any{"type": "refusal", "refusal": "I can't help with that."},
		}}},
	}, Options{Model: "gpt-5"})
	if refused["stop_reason"] != "refusal" {
		t.Errorf("stop_reason = %v, want refusal for a refusal part", refused["stop_reason"])
	}
	content, _ := refused["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %s, want the refusal text surfaced", mustJSON(t, content))
	}
	if part, _ := content[0].(map[string]any); part["type"] != "text" {
		t.Errorf("content[0] = %s, want a text block carrying the refusal", mustJSON(t, content[0]))
	}

	filtered := ToAnthropic(map[string]any{
		"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{
			map[string]any{"type": "output_text", "text": "partial"},
		}}},
		"incomplete_details": map[string]any{"reason": "content_filter"},
	}, Options{Model: "gpt-5"})
	if filtered["stop_reason"] != "refusal" {
		t.Errorf("stop_reason = %v, want refusal for incomplete_details.content_filter", filtered["stop_reason"])
	}
}

// Reasoning summary parts are separate blocks upstream; fusing them without a
// separator would glue words together ("firstsecond").
func TestToAnthropic_ReasoningMultiPartSeparator(t *testing.T) {
	resp := map[string]any{
		"output": []any{
			map[string]any{"type": "reasoning", "summary": []any{
				map[string]any{"type": "summary_text", "text": "first"},
				map[string]any{"type": "summary_text", "text": "second"},
			}},
		},
	}
	msg := ToAnthropic(resp, Options{Model: "gpt-5"})
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %s, want one thinking block", mustJSON(t, msg["content"]))
	}
	part, _ := content[0].(map[string]any)
	if thinking, _ := part["thinking"].(string); thinking != "first\n\nsecond" {
		t.Errorf("thinking = %q, want %q", thinking, "first\n\nsecond")
	}
}

// An upstream function_call with no call_id still needs a unique tool_use id:
// two parallel calls to the same tool would otherwise share one, and the client
// could not match either result to its call. The stream path already numbers by
// output index; the JSON path must not fall back to the tool name.
func TestToAnthropic_FallbackCallIDsAreUnique(t *testing.T) {
	out := ToAnthropic(map[string]any{"output": []any{
		map[string]any{"type": "function_call", "name": "read", "arguments": `{"filePath":"/a"}`},
		map[string]any{"type": "function_call", "name": "read", "arguments": `{"filePath":"/b"}`},
	}}, Options{Model: "gpt-5"})

	seen := map[string]bool{}
	for _, raw := range anySlice(out["content"]) {
		block, _ := raw.(map[string]any)
		id, _ := block["id"].(string)
		if id == "" || seen[id] {
			t.Errorf("tool_use id %q is empty or repeated: %s", id, mustJSON(t, out["content"]))
		}
		seen[id] = true
	}
	if len(seen) != 2 {
		t.Errorf("tool_use blocks = %d, want 2: %s", len(seen), mustJSON(t, out["content"]))
	}
}

// A reasoning item's encrypted_content is what the next turn hands back as an
// encrypted reasoning item, so it travels in the thinking block's signature.
// An item with a signature and no summary text still becomes a thinking block
// (empty text); an item without one keeps an empty signature, and an item with
// neither has nothing to show.
func TestToAnthropic_ReasoningSignature(t *testing.T) {
	out := ToAnthropic(map[string]any{"output": []any{
		map[string]any{"type": "reasoning", "summary": []any{
			map[string]any{"type": "summary_text", "text": "weighing it"},
		}, "encrypted_content": "gAAAAsummary"},
		map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": "gAAAAbare"},
		map[string]any{"type": "reasoning", "summary": []any{
			map[string]any{"type": "summary_text", "text": "unsigned"},
		}},
		map[string]any{"type": "reasoning", "summary": []any{}},
	}}, Options{Model: "gpt-5"})

	want := `[{"signature":"gAAAAsummary","thinking":"weighing it","type":"thinking"},` +
		`{"signature":"gAAAAbare","thinking":"","type":"thinking"},` +
		`{"signature":"","thinking":"unsigned","type":"thinking"}]`
	if got := mustJSON(t, out["content"]); got != want {
		t.Errorf("content = %s, want %s", got, want)
	}
}

// A web_search_call becomes Anthropic's server tool pair: the server_tool_use
// with the query the model ran, then the web_search_tool_result listing the
// sources (title falls back to the URL). The search ran upstream, so the turn
// still ends normally. An action with neither a query nor sources (open_page)
// has nothing to show.
func TestToAnthropic_WebSearchCall(t *testing.T) {
	out := ToAnthropic(map[string]any{"output": []any{
		map[string]any{"id": "ws_1", "type": "web_search_call", "status": "completed", "action": map[string]any{
			"type": "search", "query": "go 1.27 release",
			"sources": []any{
				map[string]any{"type": "url", "url": "https://go.dev/doc/go1.27", "title": "Go 1.27 Release Notes"},
				map[string]any{"type": "url", "url": "https://example.test/a"},
				map[string]any{"type": "url"},
			},
		}},
		map[string]any{"id": "ws_2", "type": "web_search_call", "status": "completed", "action": map[string]any{
			"type": "open_page", "url": "https://go.dev/doc/go1.27",
		}},
		map[string]any{"type": "message", "role": "assistant", "content": []any{
			map[string]any{"type": "output_text", "text": "Go 1.27 is out."},
		}},
	}}, Options{Model: "gpt-5"})

	want := `[{"id":"ws_1","input":{"query":"go 1.27 release"},"name":"web_search","type":"server_tool_use"},` +
		`{"content":[{"page_age":null,"title":"Go 1.27 Release Notes","type":"web_search_result","url":"https://go.dev/doc/go1.27"},` +
		`{"page_age":null,"title":"https://example.test/a","type":"web_search_result","url":"https://example.test/a"}],` +
		`"tool_use_id":"ws_1","type":"web_search_tool_result"},` +
		`{"text":"Go 1.27 is out.","type":"text"}]`
	if got := mustJSON(t, out["content"]); got != want {
		t.Errorf("content = %s\nwant      %s", got, want)
	}
	if out["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn (a server tool asks nothing of the client)", out["stop_reason"])
	}
}

// The Responses input_tokens includes both the cached prefix and the tokens
// written to the cache; Anthropic reports each separately, so both come out of
// input_tokens. cache_creation_tokens is the older spelling of the write count.
func TestUsage_CacheReadsAndWrites(t *testing.T) {
	for _, tc := range []struct {
		name    string
		details map[string]any
		want    string
	}{
		{"no details", nil,
			`{"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"input_tokens":100,"output_tokens":7}`},
		{"cache read", map[string]any{"cached_tokens": 60},
			`{"cache_creation_input_tokens":0,"cache_read_input_tokens":60,"input_tokens":40,"output_tokens":7}`},
		{"cache read and write", map[string]any{"cached_tokens": 60, "cache_write_tokens": 30},
			`{"cache_creation_input_tokens":30,"cache_read_input_tokens":60,"input_tokens":10,"output_tokens":7}`},
		{"older write spelling", map[string]any{"cache_creation_tokens": 25},
			`{"cache_creation_input_tokens":25,"cache_read_input_tokens":0,"input_tokens":75,"output_tokens":7}`},
		{"write count wins over the older spelling", map[string]any{"cache_write_tokens": 30, "cache_creation_tokens": 25},
			`{"cache_creation_input_tokens":30,"cache_read_input_tokens":0,"input_tokens":70,"output_tokens":7}`},
		{"reads clamp to the input", map[string]any{"cached_tokens": 150},
			`{"cache_creation_input_tokens":0,"cache_read_input_tokens":100,"input_tokens":0,"output_tokens":7}`},
		{"writes clamp to what the reads leave", map[string]any{"cached_tokens": 90, "cache_write_tokens": 30},
			`{"cache_creation_input_tokens":10,"cache_read_input_tokens":90,"input_tokens":0,"output_tokens":7}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := map[string]any{"input_tokens": float64(100), "output_tokens": float64(7)}
			if tc.details != nil {
				u["input_tokens_details"] = tc.details
			}
			if got := mustJSON(t, Usage(u)); got != tc.want {
				t.Errorf("Usage = %s, want %s", got, tc.want)
			}
		})
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
