package corpus

import (
	"strings"
	"testing"
)

// stage2Body mirrors the block layout captured in
// docs/classifier-fallback-notes.md: a literal <transcript> block, one block
// per turn as single-line JSON, a literal </transcript> block, then the footer.
const stage2Body = `{
  "model": "claude-opus-4-5",
  "system": [
    {"type": "text", "text": "x-anthropic-billing-header: cc_version=1"},
    {"type": "text", "text": "You are a security monitor for autonomous AI coding agents.\nRules follow."}
  ],
  "messages": [{"role": "user", "content": [
    {"type": "text", "text": "<transcript>"},
    {"type": "text", "text": "{\"user\":\"clean the build dir\"}"},
    {"type": "text", "text": "{\"Bash\":\"ls build/\"}"},
    {"type": "text", "text": "{\"Bash\":\"rm -rf build/\"}"},
    {"type": "text", "text": "</transcript>"},
    {"type": "text", "text": "Use <thinking> first, then respond with <severity>N</severity>, plus <category>"}
  ]}]
}`

func TestParseRequestExtractsActionAndContext(t *testing.T) {
	parsed, err := ParseRequest([]byte(stage2Body), 2)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if parsed.Action != `{"Bash":"rm -rf build/"}` {
		t.Errorf("Action = %q, want the last transcript entry", parsed.Action)
	}
	want := []string{`{"user":"clean the build dir"}`, `{"Bash":"ls build/"}`}
	if len(parsed.Context) != len(want) {
		t.Fatalf("Context has %d entries, want %d: %v", len(parsed.Context), len(want), parsed.Context)
	}
	for i := range want {
		if parsed.Context[i] != want[i] {
			t.Errorf("Context[%d] = %q, want %q (oldest first)", i, parsed.Context[i], want[i])
		}
	}
}

func TestParseRequestContextEntriesZeroYieldsEmptyContext(t *testing.T) {
	parsed, err := ParseRequest([]byte(stage2Body), 0)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if len(parsed.Context) != 0 {
		t.Errorf("Context = %v, want empty", parsed.Context)
	}
}

func TestParseRequestContextEntriesBeyondAvailable(t *testing.T) {
	parsed, err := ParseRequest([]byte(stage2Body), 20)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	// Only two entries precede the action; <transcript> itself is not one.
	if len(parsed.Context) != 2 {
		t.Errorf("Context has %d entries, want 2: %v", len(parsed.Context), parsed.Context)
	}
}

func TestParseRequestHashesAreStableAndDistinct(t *testing.T) {
	first, err := ParseRequest([]byte(stage2Body), 2)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	second, err := ParseRequest([]byte(stage2Body), 2)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if first.SystemSHA256 != second.SystemSHA256 {
		t.Error("SystemSHA256 is not stable across two parses of the same body")
	}
	if len(first.SystemSHA256) != 64 {
		t.Errorf("SystemSHA256 = %q, want 64 lowercase hex characters", first.SystemSHA256)
	}
	if len(first.FooterSHA256) != 64 {
		t.Errorf("FooterSHA256 = %q, want 64 lowercase hex characters", first.FooterSHA256)
	}
	if first.SystemSHA256 == first.FooterSHA256 {
		t.Error("SystemSHA256 and FooterSHA256 hash the same block")
	}
}

func TestParseRequestErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "not json", body: `not json at all`},
		{name: "no messages", body: `{"system":[],"messages":[]}`},
		{
			name: "content is a plain string",
			body: `{"messages":[{"role":"user","content":"<transcript></transcript>"}]}`,
		},
		{
			name: "no closing transcript block",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"<transcript>"}]}]}`,
		},
		{
			name: "transcript is empty",
			body: `{"messages":[{"role":"user","content":[
				{"type":"text","text":"<transcript>"},
				{"type":"text","text":"</transcript>"},
				{"type":"text","text":"footer"}]}]}`,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := ParseRequest([]byte(testCase.body), 2); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}

func TestExtractActionDelegatesToParseRequest(t *testing.T) {
	action, context, err := ExtractAction([]byte(stage2Body), 1)
	if err != nil {
		t.Fatalf("ExtractAction: %v", err)
	}
	if action != `{"Bash":"rm -rf build/"}` {
		t.Errorf("action = %q", action)
	}
	if len(context) != 1 || !strings.Contains(context[0], "ls build/") {
		t.Errorf("context = %v, want the single entry before the action", context)
	}
}
