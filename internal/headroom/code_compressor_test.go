package headroom

import (
	"context"
	"strings"
	"testing"
)

func TestPruneText(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{
			name: "trims trailing whitespace, keeps indentation",
			in:   "func main() {   \n\tprintln(1)\t\n}   ",
			want: "func main() {\n\tprintln(1)\n}",
		},
		{
			name: "collapses multiple blank lines to one",
			in:   "line 1\n\n\n\nline 2\n\nline 3",
			want: "line 1\n\nline 2\n\nline 3",
		},
		{
			name: "collapses repeated identical lines",
			in:   "start\ntick\ntick\ntick\ntick\nend",
			want: "start\ntick\n[... repeated 3 times ...]\nend",
		},
		{
			name: "leaves a two-line run alone",
			in:   "start\ntick\ntick\nend",
			want: "start\ntick\ntick\nend",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PruneText(tc.in); got != tc.want {
				t.Errorf("PruneText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPruneText_Idempotent(t *testing.T) {
	in := "a   \n\n\n\nb\nb\nb\nb\nb\nc"
	once := PruneText(in)
	if twice := PruneText(once); twice != once {
		t.Errorf("not idempotent: %q -> %q", once, twice)
	}
}

func TestCodeCompressor_OnlyTouchesToolResults(t *testing.T) {
	userText := "please run   \n\n\n\nthe thing"
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": userText},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": "out   \n\n\n\nput"},
		}},
	}}
	reqCtx := &RequestContext{Request: req}

	stage := &CodeCompressorStage{}
	if err := stage.Execute(context.Background(), reqCtx, &Config{Enabled: true, CodeCompressor: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	msgs := req["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != userText {
		t.Error("user prompt text must not be rewritten (invariant I3)")
	}
	got := msgs[1].(map[string]any)["content"].([]any)[0].(map[string]any)["content"]
	if got != "out\n\nput" {
		t.Errorf("tool_result not pruned, got %q", got)
	}
}

func TestCodeCompressor_LargeLogCollapses(t *testing.T) {
	log := "building\n" + strings.Repeat("  downloading...\n", 500) + "done\n"
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": log},
		}},
	}}
	reqCtx := &RequestContext{Request: req}

	stage := &CodeCompressorStage{}
	if err := stage.Execute(context.Background(), reqCtx, &Config{Enabled: true, CodeCompressor: true}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := req["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(got, "[... repeated 499 times ...]") {
		t.Errorf("expected repetition marker, got %q", got)
	}
	if len(got) > 200 {
		t.Errorf("expected large collapse, got %d bytes", len(got))
	}
}

func TestCodeCompressor_SkipsVerbatimReadResult(t *testing.T) {
	// Trailing spaces and a 5x repeated line: exactly what PruneText destroys.
	payload := "     1\tpackage main  \n" +
		"     2\t\n" +
		"     3\tline\n     4\tline\n     5\tline\n     6\tline\n     7\tline\n"
	req := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read",
				"input": map[string]any{"file_path": "/tmp/x.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": payload},
		}},
	}}
	reqCtx := &RequestContext{Request: req, Verbatim: NewToolInspector(req)}
	cfg := &Config{Enabled: true, CodeCompressor: true, PreserveVerbatimReads: true}

	stage := &CodeCompressorStage{}
	if err := stage.Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := toolResultText(t, req, 1)
	if got != payload {
		t.Errorf("verbatim payload must survive byte-identical, got %q", got)
	}
	if reqCtx.VerbatimSkipped != 1 {
		t.Errorf("expected VerbatimSkipped 1, got %d", reqCtx.VerbatimSkipped)
	}
}

func TestCodeCompressor_StillPrunesNonVerbatim(t *testing.T) {
	payload := "log line one   \nlog line two\n"
	req := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash",
				"input": map[string]any{"command": "make test"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": payload},
		}},
	}}
	reqCtx := &RequestContext{Request: req, Verbatim: NewToolInspector(req)}
	cfg := &Config{Enabled: true, CodeCompressor: true, PreserveVerbatimReads: true}

	stage := &CodeCompressorStage{}
	if err := stage.Execute(context.Background(), reqCtx, cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := toolResultText(t, req, 1)
	if got != "log line one\nlog line two\n" {
		t.Errorf("non-verbatim payload must still prune, got %q", got)
	}
}
