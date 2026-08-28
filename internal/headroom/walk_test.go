package headroom

import "testing"

func TestWalkToolResults_VisitsStringAndArrayForms(t *testing.T) {
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "raw user text"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "assistant prose"},
			map[string]any{"type": "thinking", "thinking": "private", "signature": "sig"},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": "string form"},
			map[string]any{"type": "tool_result", "content": []any{
				map[string]any{"type": "text", "text": "array form"},
				map[string]any{"type": "image", "source": map[string]any{"data": "b64"}},
			}},
		}},
	}}

	var seen []string
	walkToolResultText(req, 0, func(_, _ int, get func() string, set func(string)) {
		seen = append(seen, get())
		set(get() + "!")
	})

	if len(seen) != 2 || seen[0] != "string form" || seen[1] != "array form" {
		t.Fatalf("unexpected visits: %#v", seen)
	}

	msgs := req["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != "raw user text" {
		t.Error("must not touch top-level user text (invariant I3)")
	}
	assistant := msgs[1].(map[string]any)["content"].([]any)
	if assistant[0].(map[string]any)["text"] != "assistant prose" {
		t.Error("must not touch assistant text (invariant I3)")
	}
	if assistant[1].(map[string]any)["signature"] != "sig" {
		t.Error("must not touch thinking signatures (invariant I3)")
	}
	blocks := msgs[2].(map[string]any)["content"].([]any)
	if blocks[0].(map[string]any)["content"] != "string form!" {
		t.Error("string-form rewrite not applied")
	}
	inner := blocks[1].(map[string]any)["content"].([]any)
	if inner[0].(map[string]any)["text"] != "array form!" {
		t.Error("array-form rewrite not applied")
	}
	if _, ok := inner[1].(map[string]any)["text"]; ok {
		t.Error("image block must be left alone")
	}
}

func TestWalkToolResults_RespectsFromIndex(t *testing.T) {
	mk := func(s string) any {
		return map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": s},
		}}
	}
	req := map[string]any{"messages": []any{mk("a"), mk("b"), mk("c")}}

	var seen []string
	walkToolResultText(req, 2, func(_, _ int, get func() string, set func(string)) {
		seen = append(seen, get())
	})
	if len(seen) != 1 || seen[0] != "c" {
		t.Fatalf("expected only index 2, got %#v", seen)
	}
}

func TestWalkToolResults_OrdinalMonotonic(t *testing.T) {
	mk := func(s string) any {
		return map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": s},
			map[string]any{"type": "tool_result", "content": []any{
				map[string]any{"type": "text", "text": s + "-inner"},
			}},
		}}
	}
	req := map[string]any{"messages": []any{mk("a"), mk("b")}}

	var ords []int
	var payloads []string
	walkToolResultText(req, 0, func(_, ord int, get func() string, _ func(string)) {
		ords = append(ords, ord)
		payloads = append(payloads, get())
	})

	wantOrds := []int{0, 1, 2, 3}
	wantPayloads := []string{"a", "a-inner", "b", "b-inner"}
	if len(ords) != len(wantOrds) {
		t.Fatalf("expected %d payloads, got %d", len(wantOrds), len(ords))
	}
	for i := range wantOrds {
		if ords[i] != wantOrds[i] || payloads[i] != wantPayloads[i] {
			t.Errorf("position %d: got (ord=%d, %q), want (ord=%d, %q)",
				i, ords[i], payloads[i], wantOrds[i], wantPayloads[i])
		}
	}
}
