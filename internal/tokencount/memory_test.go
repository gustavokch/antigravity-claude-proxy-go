package tokencount

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	readTool  = map[string]any{"name": "Read", "input_schema": map[string]any{"type": "object"}}  // 48 chars
	writeTool = map[string]any{"name": "Write", "input_schema": map[string]any{"type": "object"}} // 49 chars
)

// chatRequest builds a decoded request whose messages alternate user and
// assistant text. Like Claude Code, it puts the cache breakpoint on the
// newest message only, so the next turn sends this turn's last message
// without it.
func chatRequest(system string, tools []any, texts ...string) map[string]any {
	messages := make([]any, len(texts))
	for i, text := range texts {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		block := map[string]any{"type": "text", "text": text}
		if i == len(texts)-1 {
			block["cache_control"] = map[string]any{"type": "ephemeral"}
		}
		messages[i] = map[string]any{"role": role, "content": []any{block}}
	}
	return map[string]any{"system": system, "tools": tools, "messages": messages}
}

// firstTurn is 71 chars: "be brief" 8, "fix the bug" 11, "done" 4, Read 48.
// With no memory it estimates ⌈71/4⌉ = 18 tokens.
func firstTurn() map[string]any {
	return chatRequest("be brief", []any{readTool}, "fix the bug", "done")
}

func TestEstimateContinuesFromTheLastRealCount(t *testing.T) {
	counter := New(nil)
	observed := firstTurn()
	counter.Observe("session-a", "claude-sonnet-4-6", observed, 5000)

	if got := counter.Estimate("session-a", "claude-sonnet-4-6", firstTurn()); got != 5000 {
		t.Errorf("same conversation = %d tokens, want the real 5000", got)
	}

	next := chatRequest("be brief", []any{readTool}, "fix the bug", "done", strings.Repeat("b", 400))
	newest := next["messages"].([]any)[2].(map[string]any)
	newest["content"] = append(newest["content"].([]any), base64ImageBlock("image/png", encodedImage(t, "png", 200, 200)))
	// 400 new chars at 4 per token and a 64-token image, on top of the real
	// count; the earlier messages lost their breakpoint and still match.
	if got := counter.Estimate("session-a", "claude-opus-4-6", next); got != 5000+100+64 {
		t.Errorf("next turn = %d tokens, want 5164", got)
	}

	last := observed["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	if _, ok := last["cache_control"]; !ok {
		t.Error("Observe removed cache_control from the caller's request")
	}
}

func TestEstimateRestartsWhenTheConversationChanged(t *testing.T) {
	counter := New(nil)
	counter.Observe("session-a", "claude-sonnet-4-6", firstTurn(), 5000)

	for _, tc := range []struct {
		name    string
		session string
		model   string
		request map[string]any
		want    int
	}{
		{"continued", "session-a", "claude-sonnet-4-6",
			chatRequest("be brief", []any{readTool}, "fix the bug", "done", "next"), 5000 + 1},
		{"earlier message edited", "session-a", "claude-sonnet-4-6",
			chatRequest("be brief", []any{readTool}, "fix the bugs", "done", "next"), 19}, // 76 chars
		{"compacted to fewer messages", "session-a", "claude-sonnet-4-6",
			chatRequest("be brief", []any{readTool}, "summary"), 16}, // 63 chars
		{"tool added", "session-a", "claude-sonnet-4-6",
			chatRequest("be brief", []any{readTool, writeTool}, "fix the bug", "done", "next"), 31}, // 124 chars
		{"system changed", "session-a", "claude-sonnet-4-6",
			chatRequest("be terse", []any{readTool}, "fix the bug", "done", "next"), 19}, // 75 chars
		{"other session", "session-b", "claude-sonnet-4-6",
			chatRequest("be brief", []any{readTool}, "fix the bug", "done", "next"), 19},
		{"other family", "session-a", "gemini-3.1-pro",
			chatRequest("be brief", []any{readTool}, "fix the bug", "done", "next"), 19},
		{"no session", "", "claude-sonnet-4-6",
			chatRequest("be brief", []any{readTool}, "fix the bug", "done", "next"), 19},
	} {
		if got := counter.Estimate(tc.session, tc.model, tc.request); got != tc.want {
			t.Errorf("%s: %d tokens, want %d", tc.name, got, tc.want)
		}
	}
}

func TestObserveRemembersOnlyWithASessionAndARealCount(t *testing.T) {
	counter := New(nil)
	// No session: the ratio learns (2,000 chars in 1,000 tokens, 4.0 → 3.6)
	// and nothing is remembered, so a session estimate is ⌈2000/3.6⌉ = 556,
	// not 1,000.
	counter.Observe("", "claude-sonnet-4-6", textRequest(2000), 1000)
	if got := counter.Estimate("session-a", "claude-sonnet-4-6", textRequest(2000)); got != 556 {
		t.Fatalf("after a sessionless observation = %d tokens, want 556", got)
	}
	// A reply without usage teaches neither.
	counter.Observe("session-a", "claude-sonnet-4-6", textRequest(2000), 0)
	if got := counter.Estimate("session-a", "claude-sonnet-4-6", textRequest(2000)); got != 556 {
		t.Fatalf("after an observation without a count = %d tokens, want 556", got)
	}
}

func TestConversationMemoryExpiresAfterAnIdleHour(t *testing.T) {
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	now := start
	counter := New(func() time.Time { return now })
	counter.Observe("session-a", "claude-sonnet-4-6", firstTurn(), 5000)

	now = start.Add(50 * time.Minute)
	counter.Observe("session-a", "claude-sonnet-4-6", firstTurn(), 6000)

	now = start.Add(50*time.Minute + time.Hour)
	if got := counter.Estimate("session-a", "claude-sonnet-4-6", firstTurn()); got != 6000 {
		t.Fatalf("one hour after the last observation = %d tokens, want the remembered 6000", got)
	}
	now = now.Add(time.Nanosecond)
	if got := counter.Estimate("session-a", "claude-sonnet-4-6", firstTurn()); got != 18 {
		t.Fatalf("past one idle hour = %d tokens, want the full estimate 18", got)
	}
}

func TestConversationMemoryEvictsTheOldestAboveTheCap(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	counter := New(func() time.Time { return now })
	observe := func(session string) {
		now = now.Add(time.Millisecond)
		counter.Observe(session, "claude-sonnet-4-6", firstTurn(), 5000)
	}
	for i := range maxConversations {
		observe(fmt.Sprintf("session-%d", i))
	}
	observe("session-0") // now the latest; session-1 is the oldest
	observe("session-new")

	for session, want := range map[string]int{
		"session-0":   5000,
		"session-1":   18,
		"session-2":   5000,
		"session-new": 5000,
	} {
		if got := counter.Estimate(session, "claude-sonnet-4-6", firstTurn()); got != want {
			t.Errorf("%s = %d tokens, want %d", session, got, want)
		}
	}
	if held := len(counter.conversations); held != maxConversations {
		t.Errorf("memory holds %d conversations, want the cap %d", held, maxConversations)
	}
}

func TestCounterIsSafeForConcurrentUse(t *testing.T) {
	counter := New(nil)
	var wg sync.WaitGroup
	results := make(chan int, 8*100)
	for g := range 8 {
		wg.Go(func() {
			session := fmt.Sprintf("session-%d", g%2)
			for range 100 {
				counter.Observe(session, "claude-sonnet-4-6", firstTurn(), 5000)
				results <- counter.Estimate(session, "claude-sonnet-4-6", firstTurn())
				counter.Observe("", "gemini-3.1-pro", textRequest(2000), 1000)
			}
		})
	}
	wg.Wait()
	close(results)

	for got := range results {
		if got != 5000 {
			t.Fatalf("estimate after its own observation = %d tokens, want 5000", got)
		}
	}
	// 800 observations at 2.0 chars per token leave the gemini ratio at 2.0.
	if got := counter.Estimate("", "gemini-3.1-pro", textRequest(4000)); got != 2000 {
		t.Fatalf("gemini 4000 chars = %d tokens, want 2000", got)
	}
}
