package headroom

import (
	"context"
	"testing"
)

func TestCompactJSON(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		changed bool
	}{
		{
			name:    "pretty object compacts and preserves key order",
			in:      "{\n  \"name\": \"example\",\n  \"items\": [\n    {\"id\": 1, \"value\": \"a\"}\n  ]\n}",
			want:    `{"name":"example","items":[{"id":1,"value":"a"}]}`,
			changed: true,
		},
		{
			name:    "preserves large integer literals exactly",
			in:      "{\n  \"id\": 12345678901234567890\n}",
			want:    `{"id":12345678901234567890}`,
			changed: true,
		},
		{
			name:    "non-JSON passes through untouched",
			in:      "total 12\ndrwxr-xr-x  2 user user 4096 file",
			want:    "total 12\ndrwxr-xr-x  2 user user 4096 file",
			changed: false,
		},
		{
			name:    "malformed JSON passes through untouched",
			in:      `{"unterminated": `,
			want:    `{"unterminated": `,
			changed: false,
		},
		{
			name:    "already compact is not rewritten",
			in:      `{"a":1}`,
			want:    `{"a":1}`,
			changed: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := CompactJSON(tc.in)
			if got != tc.want || changed != tc.changed {
				t.Errorf("CompactJSON(%q) = (%q, %v), want (%q, %v)", tc.in, got, changed, tc.want, tc.changed)
			}
		})
	}
}

func TestCompactJSON_Idempotent(t *testing.T) {
	in := "{\n  \"a\": [1, 2, 3]\n}"
	once, _ := CompactJSON(in)
	twice, changed := CompactJSON(once)
	if twice != once || changed {
		t.Errorf("not idempotent: %q -> %q (changed=%v)", once, twice, changed)
	}
}

func TestSmartCrusher_CompactsHistoryNotJustLastTurn(t *testing.T) {
	pretty := "{\n  \"ok\": true\n}"
	mk := func() any {
		return map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": pretty},
		}}
	}
	req := map[string]any{"messages": []any{mk(), mk(), mk()}}
	reqCtx := &RequestContext{Request: req, FrozenPrefixIndex: 0}

	stage := &SmartCrusherStage{}
	if err := stage.Execute(context.Background(), reqCtx, &Config{Enabled: true, SmartCrusher: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i, raw := range req["messages"].([]any) {
		blocks := raw.(map[string]any)["content"].([]any)
		got := blocks[0].(map[string]any)["content"].(string)
		if got != `{"ok":true}` {
			t.Errorf("message %d not compacted: %q", i, got)
		}
	}
	if reqCtx.BytesAfter >= reqCtx.BytesBefore || reqCtx.BytesBefore == 0 {
		t.Errorf("byte accounting not recorded: before=%d after=%d", reqCtx.BytesBefore, reqCtx.BytesAfter)
	}
}

func TestSmartCrusher_DisabledIsNoOp(t *testing.T) {
	pretty := "{\n  \"ok\": true\n}"
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": pretty},
		}},
	}}
	stage := &SmartCrusherStage{}
	if err := stage.Execute(context.Background(), &RequestContext{Request: req}, &Config{Enabled: true, SmartCrusher: false}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"]
	if got != pretty {
		t.Errorf("expected no-op when disabled, got %q", got)
	}
}

func TestSmartCrusher_SkipsVerbatimJSONFileRead(t *testing.T) {
	prettyJSON := "{\n  \"key\": \"value\",\n  \"list\": [\n    1,\n    2\n  ]\n}"
	req := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read",
				"input": map[string]any{"file_path": "/tmp/config.json"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": prettyJSON},
		}},
	}}
	reqCtx := &RequestContext{Request: req, Verbatim: NewToolInspector(req)}
	cfg := &Config{Enabled: true, SmartCrusher: true, PreserveVerbatimReads: true}

	stage := &SmartCrusherStage{}
	if err := stage.Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := toolResultText(t, req, 1)
	if got != prettyJSON {
		t.Errorf("verbatim JSON file read must not be compacted, got %q", got)
	}
}

func TestSmartCrusher_SkipsTabularOnVerbatim(t *testing.T) {
	// Uniform object array with long keys: TryTabularConversion would convert.
	payload := `[
  {"a_very_long_column_name": 1, "another_long_column_name": "alpha"},
  {"a_very_long_column_name": 2, "another_long_column_name": "beta"},
  {"a_very_long_column_name": 3, "another_long_column_name": "gamma"},
  {"a_very_long_column_name": 4, "another_long_column_name": "delta"}
]`
	if _, converted := TryTabularConversion(payload, DefaultMinTabularSavings); !converted {
		t.Fatal("fixture must be tabular-convertible, or the test proves nothing")
	}

	req := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read",
				"input": map[string]any{"file_path": "/tmp/data.json"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": payload},
		}},
	}}
	reqCtx := &RequestContext{Request: req, Verbatim: NewToolInspector(req)}
	cfg := &Config{Enabled: true, SmartCrusher: true, TabularArrays: true, PreserveVerbatimReads: true}

	stage := &SmartCrusherStage{}
	if err := stage.Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := toolResultText(t, req, 1)
	if got != payload {
		t.Errorf("tabular conversion must not fire on verbatim payload, got %q", got)
	}
}
