package headroom

import (
	"context"
	"strings"
	"testing"
)

// TestVerbatim_EditExactMatchSurvivesPipeline runs the full engine over a
// request whose tool_result is a realistic Read payload (trailing whitespace, a
// blank-line run, and four identical lines) and asserts the payload is returned
// byte-for-byte. This is the property an Edit's old_string depends on.
func TestVerbatim_EditExactMatchSurvivesPipeline(t *testing.T) {
	payload := realisticReadPayload()

	engine := NewEngine(Config{
		Enabled:               true,
		SmartCrusher:          true,
		TabularArrays:         true,
		CodeCompressor:        true,
		LiveTurns:             2,
		PreserveVerbatimReads: true,
		CCR: CCRConfig{
			Enabled:       true,
			MinChunkBytes: 2048,
		},
	})

	req := readEditRequest(payload)

	if _, err := engine.Process(context.Background(), req); err != nil {
		t.Fatalf("Process error: %v", err)
	}

	got := toolResultText(t, req, 2)
	if got != payload {
		t.Errorf("Read payload mutated; Edit old_string drawn from it cannot match disk.\nfirst divergence:\n%s", firstDiff(payload, got))
	}
}

// TestVerbatim_DisabledByConfig restores pre-fix behaviour when the operator
// opts out: the same payload is demoted/mutated again.
func TestVerbatim_DisabledByConfig(t *testing.T) {
	payload := realisticReadPayload()

	engine := NewEngine(Config{
		Enabled:               true,
		SmartCrusher:          true,
		CodeCompressor:        true,
		LiveTurns:             2,
		PreserveVerbatimReads: false,
		CCR: CCRConfig{
			Enabled:       true,
			MinChunkBytes: 2048,
		},
	})

	req := readEditRequest(payload)
	if _, err := engine.Process(context.Background(), req); err != nil {
		t.Fatalf("Process error: %v", err)
	}

	got := toolResultText(t, req, 2)
	if got == payload {
		t.Error("with PreserveVerbatimReads=false the payload must be rewritten as before")
	}
}

// realisticReadPayload builds `cat -n` Read output with the three shapes the
// lossy stages destroy: trailing whitespace, a run of identical lines, and
// enough bytes to trip CCR demotion.
func realisticReadPayload() string {
	var b strings.Builder
	b.WriteString("     1\tpackage main\n")
	b.WriteString("     2\t\n")
	b.WriteString("     3\timport \"fmt\"   \n")
	for i := 4; i < 8; i++ {
		b.WriteString("     " + itoa(i) + "\t// repeated marker\n")
	}
	for lineNo := 8; b.Len() < 3000; lineNo++ {
		b.WriteString("     " + itoa(lineNo) + "\tfmt.Println(\"x\")  \n")
	}
	return b.String()
}

// readEditRequest is a five-message transcript: the Read call and its result
// sit in the frozen prefix; the edit request is live.
func readEditRequest(payload string) map[string]any {
	return map[string]any{
		"tools": []any{map[string]any{"name": "Read"}},
		"messages": []any{
			map[string]any{"role": "user", "content": "read the file"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read",
					"input": map[string]any{"file_path": "/tmp/main.go"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": payload},
			}},
			map[string]any{"role": "assistant", "content": "noted"},
			map[string]any{"role": "user", "content": "now edit line 3"},
		},
	}
}

func toolResultText(t *testing.T, req map[string]any, msgIdx int) string {
	t.Helper()
	msgs := req["messages"].([]any)
	block := msgs[msgIdx].(map[string]any)["content"].([]any)[0].(map[string]any)
	s, ok := block["content"].(string)
	if !ok {
		t.Fatalf("message %d tool_result content is not a string: %T", msgIdx, block["content"])
	}
	return s
}

// --- ToolInspector classification -----------------------------------------

func TestToolInspector_MatchesReadByName(t *testing.T) {
	names := []string{"Read", "read_file", "View", "mcp__filesystem__read_file", "fs.readFile"}
	var messages []any
	for i, name := range names {
		id := "toolu_" + itoa(i)
		messages = append(messages,
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": id, "name": name,
					"input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": id, "content": "x"},
			}},
		)
	}
	req := map[string]any{"messages": messages}

	insp := NewToolInspector(req)
	if insp.VerbatimCount() != len(names) {
		t.Errorf("expected %d verbatim ordinals, got %d", len(names), insp.VerbatimCount())
	}
	for i, name := range names {
		if !insp.IsVerbatimOrdinal(i) {
			t.Errorf("tool %q (ordinal %d) must classify verbatim", name, i)
		}
		info, ok := insp.Lookup("toolu_" + itoa(i))
		if !ok || !info.Verbatim {
			t.Errorf("Lookup for %q missing or not verbatim: %+v", name, info)
		}
	}
}

func TestToolInspector_MatchesByFilePathInput(t *testing.T) {
	req := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_9", "name": "mcp__acme__fetch_thing",
				"input": map[string]any{"file_path": "/a/b.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_9", "content": "x"},
		}},
	}}

	insp := NewToolInspector(req)
	if !insp.IsVerbatimOrdinal(0) {
		t.Error("unknown tool naming a file_path must classify verbatim (over-match policy)")
	}
}

func TestToolInspector_RejectsNonPathInput(t *testing.T) {
	inputs := []map[string]any{
		{"path": "user.name"},
		{"query": "foo"},
	}
	var messages []any
	for i, input := range inputs {
		id := "toolu_" + itoa(i)
		messages = append(messages,
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": id, "name": "mcp__acme__set_config", "input": input},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": id, "content": "ok"},
			}},
		)
	}
	req := map[string]any{"messages": messages}

	insp := NewToolInspector(req)
	if insp.VerbatimCount() != 0 {
		t.Errorf("non-path inputs must not classify verbatim, got %d", insp.VerbatimCount())
	}
}

func TestToolInspector_MatchesNumberedSourceShape(t *testing.T) {
	// No matching tool_use in the window: history was truncated upstream. The
	// payload shape alone must mark it verbatim.
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_gone",
				"content": "  1\tpackage x\n  2\t\n  3\tfunc f()"},
		}},
	}}

	insp := NewToolInspector(req)
	if !insp.IsVerbatimOrdinal(0) {
		t.Error("cat -n shaped payload with no matching tool_use must classify verbatim")
	}
}

func TestToolInspector_RejectsLogWithLeadingCounter(t *testing.T) {
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_gone",
				"content": "1 request ok\nsome text\n2 request ok"},
		}},
	}}

	insp := NewToolInspector(req)
	if insp.IsVerbatimOrdinal(0) {
		t.Error("non-consecutive leading counters are a log, not numbered source")
	}
}

func TestToolInspector_UnifiedDiffIsVerbatim(t *testing.T) {
	diff := "--- a/x.go\n+++ b/x.go\n@@ -1,2 +1,2 @@\n-old line\n+new line"
	req := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_gone", "content": diff},
		}},
	}}

	insp := NewToolInspector(req)
	if !insp.IsVerbatimOrdinal(0) {
		t.Error("unified diff payload must classify verbatim")
	}
}

func TestToolInspector_OrdinalsStableAcrossFrom(t *testing.T) {
	mk := func(s string) any {
		return map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "content": s},
		}}
	}
	req := map[string]any{"messages": []any{mk("a"), mk("b"), mk("c"), mk("d")}}

	fromZero := map[int]string{}
	walkToolResultText(req, 0, func(_, ord int, get func() string, _ func(string)) {
		fromZero[ord] = get()
	})
	fromTwo := map[int]string{}
	walkToolResultText(req, 2, func(_, ord int, get func() string, _ func(string)) {
		fromTwo[ord] = get()
	})

	if len(fromTwo) != 2 {
		t.Fatalf("walk from=2 must visit 2 payloads, got %d", len(fromTwo))
	}
	for ord, s := range fromTwo {
		if fromZero[ord] != s {
			t.Errorf("ordinal %d unstable across from: from0=%q from2=%q", ord, fromZero[ord], s)
		}
	}
}

func TestNormalizeToolName(t *testing.T) {
	cases := map[string]string{
		"Read":                        "read",
		"READ":                        "read",
		"read_file":                   "read_file",
		"mcp__filesystem__read_file":  "read_file",
		"filesystem:read_file":        "read_file",
		"fs.readFile":                 "readfile",
		"mcp__x__Read":                "read",
		"str_replace_editor":          "str_replace_editor",
		"mcp__acme__fetch_thing":      "fetch_thing",
	}
	for in, want := range cases {
		if got := normalizeToolName(in); got != want {
			t.Errorf("normalizeToolName(%q) = %q, want %q", in, got, want)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}

func firstDiff(want, got string) string {
	n := len(want)
	if len(got) < n {
		n = len(got)
	}
	for i := 0; i < n; i++ {
		if want[i] != got[i] {
			lo := i - 40
			if lo < 0 {
				lo = 0
			}
			hi := i + 40
			if hi > n {
				hi = n
			}
			return "byte " + itoa(i) + ":\nwant: " + want[lo:hi] + "\ngot:  " + got[lo:hi]
		}
	}
	if len(want) != len(got) {
		return "length: want " + itoa(len(want)) + " got " + itoa(len(got))
	}
	return ""
}

func TestToolInspector_MutatingToolsAreNotVerbatim(t *testing.T) {
	// inputLooksLikeFileRead over-matches on purpose, but every mutating tool
	// also names a file_path. Marking their confirmations verbatim pinned
	// unbounded text the model will never quote back, and made those turns
	// classify as coding.
	names := []string{"Edit", "Write", "MultiEdit", "NotebookEdit", "apply_patch", "str_replace", "Glob", "Grep"}
	var messages []any
	for i, name := range names {
		id := "toolu_m" + itoa(i)
		messages = append(messages,
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": id, "name": name,
					"input": map[string]any{"file_path": "/a/b.go"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": id, "content": "The file /a/b.go has been updated."},
			}},
		)
	}
	req := map[string]any{"messages": messages}

	insp := NewToolInspector(req)
	if insp.VerbatimCount() != 0 {
		t.Errorf("mutating tool results must not classify verbatim, got %d", insp.VerbatimCount())
	}

	// str_replace_editor is dual mode: its "view" command returns file content,
	// so the name match must still win over the mutating-tool guard.
	viewReq := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_v", "name": "str_replace_editor",
				"input": map[string]any{"path": "/a/b.go"}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_v", "content": "x"},
		}},
	}}
	if !NewToolInspector(viewReq).IsVerbatimOrdinal(0) {
		t.Error("str_replace_editor must stay verbatim by name")
	}
}
