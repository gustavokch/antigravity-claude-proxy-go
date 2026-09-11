package format

import (
	"encoding/json"
	"testing"
)

// buildPiStyleHistory reproduces what a strict Anthropic client (the Pi coding
// agent) sends after `turns` tool-loop rounds: assistant tool_use blocks with no
// thoughtSignature and no thinking blocks, each followed by a tool_result.
func buildPiStyleHistory(turns int) []any {
	messages := []any{
		map[string]any{"role": "user", "content": "start the loop"},
	}
	for index := 0; index < turns; index++ {
		toolID := "toolu_" + string(rune('a'+index))
		messages = append(messages,
			map[string]any{
				"role": "assistant",
				"content": []any{map[string]any{
					"type": "tool_use", "id": toolID, "name": "read",
					"input": map[string]any{"path": "file.go"},
				}},
			},
			map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "tool_result", "tool_use_id": toolID, "content": "file body",
				}},
			},
		)
	}
	return messages
}

func convertPiTurn(t *testing.T, turns int, cache *SignatureCache) []any {
	t.Helper()
	request := map[string]any{
		"model":    "gemini-3.0-flash-high",
		"messages": buildPiStyleHistory(turns),
	}
	converted := ConvertAnthropicToGoogle(request, cache)
	return asSlice(converted["contents"])
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}

// TestGeminiToolLoopPrefixIsStableAcrossTurns asserts the invariant implicit
// context caching depends on: the contents array produced for turn N must be an
// exact prefix of the contents array produced for turn N+1.
func TestGeminiToolLoopPrefixIsStableAcrossTurns(t *testing.T) {
	t.Parallel()
	cache := NewSignatureCache()
	previous := convertPiTurn(t, 3, cache)
	next := convertPiTurn(t, 4, cache)

	if len(next) < len(previous) {
		t.Fatalf("turn N+1 shrank: %d < %d", len(next), len(previous))
	}
	for index := range previous {
		want := mustJSON(t, previous[index])
		got := mustJSON(t, next[index])
		if want != got {
			t.Fatalf("prefix diverged at index %d\n turn N:   %s\n turn N+1: %s", index, want, got)
		}
	}
}

// TestUsageMappersNeverReportNegativeInputTokens guards against a usageMetadata
// frame whose cached count exceeds the prompt count.
func TestUsageMappersNeverReportNegativeInputTokens(t *testing.T) {
	t.Parallel()
	response := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"parts": []any{map[string]any{"text": "hi"}}},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount":        100,
			"cachedContentTokenCount": 140,
			"candidatesTokenCount":    5,
		},
	}
	converted := ConvertGoogleToAnthropic(response, "gemini-3.0-flash-high", NewSignatureCache())
	usage := asMap(converted["usage"])
	if intValue(usage["input_tokens"], -1) != 0 {
		t.Fatalf("non-streaming input_tokens = %#v", usage["input_tokens"])
	}

	converter := NewStreamConverter("gemini-3.0-flash-high", NewSignatureCache(), "msg_test")
	frame := []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"hi"}]}}],"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":140,"candidatesTokenCount":5}}}`)
	events, err := converter.Consume(frame)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	for _, event := range events {
		if event["type"] != "message_start" {
			continue
		}
		streamUsage := asMap(asMap(event["message"])["usage"])
		if intValue(streamUsage["input_tokens"], -1) != 0 {
			t.Fatalf("streaming input_tokens = %#v", streamUsage["input_tokens"])
		}
	}
}
