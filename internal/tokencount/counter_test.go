package tokencount

import (
	"strings"
	"testing"
)

func textRequest(chars int) map[string]any {
	return map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": strings.Repeat("a", chars)},
	}}
}

// withImage adds a 200×200 PNG, 64 tokens on Claude's standard tier, to the
// request's first message.
func withImage(t *testing.T, request map[string]any) map[string]any {
	t.Helper()
	message := request["messages"].([]any)[0].(map[string]any)
	message["content"] = []any{
		map[string]any{"type": "text", "text": message["content"]},
		base64ImageBlock("image/png", encodedImage(t, "png", 200, 200)),
	}
	return request
}

func TestEstimateStartsAtFourCharsPerToken(t *testing.T) {
	counter := New(nil)
	if got := counter.Estimate("", "claude-sonnet-4-6", textRequest(4001)); got != 1001 {
		t.Errorf("4001 chars = %d tokens, want 1001", got)
	}
	if got := counter.Estimate("", "claude-sonnet-4-6", withImage(t, textRequest(4000))); got != 1000+64 {
		t.Errorf("4000 chars and a 200x200 image = %d tokens, want 1064", got)
	}
}

func TestObserveLearnsTheRatioOfItsFamilyOnly(t *testing.T) {
	counter := New(nil)
	// 2,000 chars and a 64-token image answered with 1,064 tokens: the text
	// took 1,000, an observed ratio of 2.0, so the ratio moves from 4.0 to
	// 0.8×4.0 + 0.2×2.0 = 3.6. Had the image stayed in, it would be 3.576.
	counter.Observe("", "claude-sonnet-4-6", withImage(t, textRequest(2000)), 1064)

	if got := counter.Estimate("", "claude-opus-4-6", textRequest(3601)); got != 1001 {
		t.Errorf("claude after one observation: 3601 chars = %d tokens, want 1001", got)
	}
	if got := counter.Estimate("", "gemini-3.1-pro", textRequest(4000)); got != 1000 {
		t.Errorf("gemini: 4000 chars = %d tokens, want 1000 at the starting ratio", got)
	}

	counter.Observe("", "mimo-v2.6-flash-free", textRequest(2000), 1000)
	if got := counter.Estimate("", "kimi-k2.6", textRequest(4000)); got != 1000 {
		t.Errorf("kimi-k2.6 learned from another model: 4000 chars = %d tokens, want 1000", got)
	}
}

func TestObserveSkipsRequestsThatCannotTeachTheRatio(t *testing.T) {
	counter := New(nil)
	counter.Observe("", "claude-sonnet-4-6", textRequest(999), 100)
	counter.Observe("", "claude-sonnet-4-6", withImage(t, textRequest(2000)), 64)
	counter.Observe("", "claude-sonnet-4-6", textRequest(2000), 0)
	if got := counter.Estimate("", "claude-sonnet-4-6", textRequest(4000)); got != 1000 {
		t.Fatalf("after skipped observations: 4000 chars = %d tokens, want 1000", got)
	}

	// 1,000 measured chars is enough: 1,000 chars in 1,000 tokens moves the
	// ratio to 0.8×4.0 + 0.2×1.0 = 3.4.
	counter.Observe("", "claude-sonnet-4-6", textRequest(1000), 1000)
	if got := counter.Estimate("", "claude-sonnet-4-6", textRequest(3401)); got != 1001 {
		t.Fatalf("after a 1000-char observation: 3401 chars = %d tokens, want 1001", got)
	}
}

func TestRatioConvergesOnSyntheticObservations(t *testing.T) {
	counter := New(nil)
	for range 30 {
		counter.Observe("", "gemini-3.8-flash-high", textRequest(3000), 1000)
	}
	// 3 + 0.8^30 ≈ 3.00124 chars per token.
	if got := counter.Estimate("", "gemini-3.8-flash-high", textRequest(30000)); got != 9996 {
		t.Fatalf("30000 chars after 30 observations at 3.0 = %d tokens, want 9996", got)
	}
}
