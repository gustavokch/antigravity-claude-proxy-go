package format

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// buildPiStyleHistory reproduces what a strict Anthropic client (the Pi coding
// agent) sends after `turns` tool-loop rounds: assistant tool_use blocks with no
// thoughtSignature and no thinking blocks, each followed by a tool_result.
func buildPiStyleHistory(turns int) []any {
	messages := []any{
		map[string]any{"role": "user", "content": "start the loop"},
	}
	for index := 0; index < turns; index++ {
		toolID := "toolu_" + strconv.Itoa(index)
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
//
// Each turn gets its own SignatureCache. Sharing one would hide exactly the
// class of bug this test exists to catch, because a conversion that reads
// process-local state would still agree with itself.
func TestGeminiToolLoopPrefixIsStableAcrossTurns(t *testing.T) {
	t.Parallel()
	previous := convertPiTurn(t, 3, NewSignatureCache())
	next := convertPiTurn(t, 4, NewSignatureCache())

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

// TestThinkingBlockConversionSurvivesCacheExpiry pins the same invariant for
// thinking blocks that TestGeminiToolLoopPrefixIsStableAcrossTurns pins for tool
// calls: an identical client history must convert identically regardless of what
// the process-local signature cache happens to remember. A cache entry that has
// aged past signatureCacheTTL — or a proxy restart, which is indistinguishable
// from expiry — must not silently drop the thinking part and shift every later
// index in the contents array.
func TestThinkingBlockConversionSurvivesCacheExpiry(t *testing.T) {
	t.Parallel()
	signature := strings.Repeat("g", MinSignatureLength)
	history := []any{
		map[string]any{"role": "user", "content": "start"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking", "thinking": "plan", "signature": signature},
			map[string]any{"type": "text", "text": "done"},
		}},
		map[string]any{"role": "user", "content": "continue"},
	}
	convert := func(cache *SignatureCache) []any {
		request := map[string]any{"model": "gemini-3.0-flash-high", "messages": history}
		return asSlice(ConvertAnthropicToGoogle(request, cache)["contents"])
	}

	warm := NewSignatureCache()
	warm.CacheThinking(signature, FamilyGemini)

	expired := NewSignatureCache()
	expired.CacheThinking(signature, FamilyGemini)
	start := expired.now()
	expired.now = func() time.Time { return start.Add(signatureCacheTTL + time.Millisecond) }

	if want, got := mustJSON(t, convert(warm)), mustJSON(t, convert(expired)); want != got {
		t.Fatalf("cache age changed the conversion\n warm:    %s\n expired: %s", want, got)
	}
}

// TestClaudeThinkingBlockIsStillDroppedForGemini pins the other half of the
// rule: an explicit family mismatch is still a drop, so a Claude-origin
// signature is never forwarded to a Gemini backend while the cache remembers it.
func TestClaudeThinkingBlockIsStillDroppedForGemini(t *testing.T) {
	t.Parallel()
	signature := strings.Repeat("c", MinSignatureLength)
	cache := NewSignatureCache()
	cache.CacheThinking(signature, FamilyClaude)
	request := map[string]any{
		"model": "gemini-3.0-flash-high",
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "plan", "signature": signature},
				map[string]any{"type": "text", "text": "done"},
			}},
		},
	}
	contents := asSlice(ConvertAnthropicToGoogle(request, cache)["contents"])
	parts := asSlice(asMap(contents[len(contents)-1])["parts"])
	for _, rawPart := range parts {
		if asMap(rawPart)["thought"] == true {
			t.Fatalf("claude thinking part reached a gemini request: %#v", parts)
		}
	}
}

// TestUnknownThinkingSignatureReachesGeminiVerbatim documents what
// scripts/verify-gemini-cache-prefix.sh turn 3 actually exercises. A signature
// the proxy has never issued resolves to FamilyUnknown, and the keep-on-unknown
// rule forwards it to the backend untouched. The script probes whether Gemini
// rejects that; if it does, provenance has to travel in the value rather than in
// the process-local cache.
func TestUnknownThinkingSignatureReachesGeminiVerbatim(t *testing.T) {
	t.Parallel()
	foreign := strings.Repeat("F", MinSignatureLength)
	request := map[string]any{
		"model": "gemini-3.0-flash-high",
		"messages": []any{
			map[string]any{"role": "user", "content": "start"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "prior reasoning", "signature": foreign},
				map[string]any{"type": "text", "text": "done"},
			}},
			map[string]any{"role": "user", "content": "continue"},
		},
	}
	contents := asSlice(ConvertAnthropicToGoogle(request, NewSignatureCache())["contents"])
	for _, rawContent := range contents {
		for _, rawPart := range asSlice(asMap(rawContent)["parts"]) {
			part := asMap(rawPart)
			if part["thought"] != true {
				continue
			}
			if part["thoughtSignature"] != foreign {
				t.Fatalf("thoughtSignature = %#v, want the client value verbatim", part["thoughtSignature"])
			}
			return
		}
	}
	t.Fatal("no thought part survived conversion; the script probe would test nothing")
}

func TestGeminiToolSignaturePreservesSnakeCaseClientSignature(t *testing.T) {
	t.Parallel()
	cache := NewSignatureCache()
	signature := "custom-signature-12345678901234567890123456789012345678901234567890"
	request := map[string]any{
		"model": "gemini-3.0-flash-high",
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type":              "tool_use",
						"id":                "t1",
						"name":              "read",
						"input":             map[string]any{"path": "file.go"},
						"thought_signature": signature,
					},
				},
			},
		},
	}
	converted := ConvertAnthropicToGoogle(request, cache)
	contents := asSlice(converted["contents"])
	part := asMap(asSlice(asMap(contents[len(contents)-1])["parts"])[0])
	if part["thoughtSignature"] != signature {
		t.Fatalf("thoughtSignature = %#v, want %#v", part["thoughtSignature"], signature)
	}
}

func TestGeminiToolLoopPrefixIsStableWithClientSignatures(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		snakeCase bool
	}{
		{name: "camelCase thoughtSignature", snakeCase: false},
		{name: "snake_case thought_signature", snakeCase: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cache := NewSignatureCache()

			makeReq := func(turns int) []any {
				messages := []any{map[string]any{"role": "user", "content": "start the loop"}}
				for i := 0; i < turns; i++ {
					toolID := "toolu_" + strconv.Itoa(i)
					sigKey := "thoughtSignature"
					if tc.snakeCase {
						sigKey = "thought_signature"
					}
					sigVal := "custom-signature-" + strconv.Itoa(i) + "-1234567890123456789012345678901234567890"
					messages = append(messages,
						map[string]any{
							"role": "assistant",
							"content": []any{map[string]any{
								"type":  "tool_use",
								"id":    toolID,
								"name":  "read",
								"input": map[string]any{"path": "file.go"},
								sigKey:  sigVal,
							}},
						},
						map[string]any{
							"role": "user",
							"content": []any{map[string]any{
								"type":        "tool_result",
								"tool_use_id": toolID,
								"content":     "file body",
							}},
						},
					)
				}
				req := map[string]any{
					"model":    "gemini-3.0-flash-high",
					"messages": messages,
				}
				return asSlice(ConvertAnthropicToGoogle(req, cache)["contents"])
			}

			prev := makeReq(3)
			next := makeReq(4)

			if len(next) < len(prev) {
				t.Fatalf("turn N+1 shrank: %d < %d", len(next), len(prev))
			}
			for i := range prev {
				want := mustJSON(t, prev[i])
				got := mustJSON(t, next[i])
				if want != got {
					t.Fatalf("prefix diverged at %d\nwant: %s\ngot:  %s", i, want, got)
				}
			}
		})
	}
}


