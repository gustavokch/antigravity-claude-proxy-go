package format

import (
	"encoding/json"
	"strings"
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

// TestGeminiToolSignatureIsIndependentOfSignatureCache asserts conversion does
// not depend on process-local ephemeral state: a warm and a cold signature cache
// must produce the same historical functionCall part.
func TestGeminiToolSignatureIsIndependentOfSignatureCache(t *testing.T) {
	t.Parallel()
	signature := strings.Repeat("g", MinSignatureLength)
	warm := NewSignatureCache()
	warm.CacheTool("toolu_a", signature)

	history := []any{
		map[string]any{"role": "user", "content": "start"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{
			"type": "tool_use", "id": "toolu_a", "name": "read", "input": map[string]any{},
		}}},
		map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": "toolu_a", "content": "body",
		}}},
	}
	convert := func(cache *SignatureCache) string {
		converted := ConvertAnthropicToGoogle(map[string]any{
			"model": "gemini-3.0-flash-high", "messages": history,
		}, cache)
		return mustJSON(t, asSlice(converted["contents"])[1])
	}
	withCache := convert(warm)
	withoutCache := convert(NewSignatureCache())
	if withCache != withoutCache {
		t.Fatalf("history part changed when the signature cache expired\n warm: %s\n cold: %s", withCache, withoutCache)
	}
}
