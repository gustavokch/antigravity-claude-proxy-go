package tokencount

import (
	"encoding/json"
	"testing"
)

func decodeRequest(t *testing.T, body string) map[string]any {
	t.Helper()
	var request map[string]any
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return request
}

// Claude 4.7 brought a new tokenizer, so it and every later Claude learn their
// own ratio.
func TestFamily(t *testing.T) {
	for model, want := range map[string]string{
		"claude-opus-4-6-thinking":   "claude",
		"claude-sonnet-4-5":          "claude",
		"claude-3-7-sonnet-20250219": "claude",
		"claude-opus-4-7":            "claude-4.7",
		"claude-sonnet-5-5":          "claude-4.7",
		"claude-sonnet-5.5-high":     "claude-4.7",
		"gemini-3.8-flash-high":      "gemini",
		"gemini-3.1-pro":             "gemini",
		"gpt-oss-120b-medium":        "gpt-oss",
		"mimo-v2.6-flash-free":       "mimo-v2.6-flash-free",
		"kimi-k2.6":                  "kimi-k2.6",
	} {
		if got := Family(model); got != want {
			t.Errorf("Family(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestMeasureRequestCountsEveryTextSource(t *testing.T) {
	request := decodeRequest(t, `{
		"model": "claude-sonnet-4-6",
		"system": [{"type": "text", "text": "abcd", "cache_control": {"type": "ephemeral"}}],
		"messages": [
			{"role": "user", "content": "héllo"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "think", "signature": "c2lnbmF0dXJl"},
				{"type": "redacted_thinking", "data": "b3BhcXVl"},
				{"type": "text", "text": "ok"},
				{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": {"path": "a<b"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "out"},
				{"type": "tool_result", "tool_use_id": "toolu_2", "content": [{"type": "text", "text": "xy"}]},
				{"type": "text", "text": "z", "cache_control": {"type": "ephemeral"}}
			]}
		],
		"tools": [{"name": "f", "input_schema": {"type": "object"}}]
	}`)

	got := measureRequest(pricingFor("claude-sonnet-4-6"), request)

	// system 4, "héllo" 5 runes (6 bytes), thinking 5, text 2,
	// tool_use input {"path":"a<b"} 14 (19 if < were HTML-escaped),
	// tool_result string 3, tool_result block 2, text 1,
	// tool {"input_schema":{"type":"object"},"name":"f"} 45.
	want := measurement{chars: 4 + 5 + 5 + 2 + 14 + 3 + 2 + 1 + 45}
	if got != want {
		t.Fatalf("measureRequest = %+v, want %+v", got, want)
	}
}

func TestMeasureRequestReadsAStringSystem(t *testing.T) {
	request := decodeRequest(t, `{"system": "abc", "messages": [{"role": "user", "content": "hi"}]}`)
	if got := measureRequest(pricingFor("claude-sonnet-4-6"), request); got != (measurement{chars: 5}) {
		t.Fatalf("measureRequest = %+v, want 5 chars", got)
	}
}
