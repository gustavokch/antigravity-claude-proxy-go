package ccidentity

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// streamedReply is a Messages API event stream in the shape Anthropic sends:
// a thinking block with its signature, a text block in two deltas, and a tool
// call.
const streamedReply = `event: message_start
data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","content":[],"model":"claude-sonnet-5","usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"The user wants the files."}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EqQBCkYIBxgCKkBsignature=="}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"I'll list "}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"the files."}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_01A","name":"Bash","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":30}}

event: message_stop
data: {"type":"message_stop"}

`

// The blocks of streamedReply as a client echoes them in its next request.
const (
	echoedThinking = `{"type":"thinking","thinking":"The user wants the files.","signature":"EqQBCkYIBxgCKkBsignature=="}`
	echoedText     = `{"type":"text","text":"I'll list the files."}`
	echoedToolUse  = `{"type":"tool_use","id":"toolu_01A","name":"Bash","input":{"command":"ls"}}`
)

// followUp is a request body whose latest assistant message has the given
// content, followed by the tool result that continues it.
func followUp(reply string) []byte {
	return []byte(`{"messages":[` +
		`{"role":"user","content":"list the files"},` +
		`{"role":"assistant","content":` + reply + `},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01A","content":"a.go"}]}]}`)
}

// readThrough reads a reply through Watch one byte at a time, so every event is
// split across reads, and returns the bytes the caller received.
func readThrough(t *testing.T, log *ReplyLog, scope, requestID, stream string) string {
	t.Helper()
	body := log.Watch(scope, requestID, io.NopCloser(iotest.OneByteReader(strings.NewReader(stream))))
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read the watched stream: %v", err)
	}
	return string(got)
}

// Each block alone identifies the reply: clients differ in what they echo
// (some drop thinking, some rewrite text), and a text-only reply has no tool
// call to go by.
func TestReplyLog_FollowUpNamesTheStreamedReplyItEchoes(t *testing.T) {
	log := NewReplyLog()
	scope := ReplyScope("claudecode:acc", "session-1")
	if got := readThrough(t, log, scope, "req_011Stream", streamedReply); got != streamedReply {
		t.Fatal("Watch changed the bytes of the stream it watched")
	}

	rewrittenText := `{"type":"text","text":"I will list the files."}`
	for _, tc := range []struct{ name, reply string }{
		{"the whole reply", `[` + echoedThinking + `,` + echoedText + `,` + echoedToolUse + `]`},
		{"its thinking", `[` + echoedThinking + `,` + rewrittenText + `]`},
		{"its text", `[` + echoedText + `]`},
		{"its tool call", `[` + rewrittenText + `,` + echoedToolUse + `]`},
	} {
		turn := TurnOf(followUp(tc.reply))
		if got := log.PrevRequestID(scope, turn); got != "req_011Stream" {
			t.Errorf("%s: PrevRequestID = %q, want req_011Stream", tc.name, got)
		}
	}
	if got := log.PrevRequestID(scope, TurnOf(followUp(`[`+rewrittenText+`]`))); got != "" {
		t.Errorf("a reply that shares no block with the stream was named %q", got)
	}
}

func TestReplyLog_FollowUpNamesTheUnaryReplyItEchoes(t *testing.T) {
	log := NewReplyLog()
	scope := ReplyScope("endpoint:https://api.anthropic.com", "session-1")
	log.RecordMessage(scope, "req_011Unary", []byte(`{"id":"msg_01","type":"message","role":"assistant",`+
		`"content":[{"type":"text","text":"Two files."}],"stop_reason":"end_turn"}`))

	// A text-only reply, echoed with string content, then a new prompt.
	body := []byte(`{"messages":[{"role":"user","content":"how many files?"},` +
		`{"role":"assistant","content":"Two files."},{"role":"user","content":"and tests?"}]}`)
	if got := log.PrevRequestID(scope, TurnOf(body)); got != "req_011Unary" {
		t.Errorf("PrevRequestID = %q, want req_011Unary", got)
	}
}

// The real client names the request behind its latest reply. When the log does
// not know the latest reply, an earlier reply's request is not the one before
// this request, so nothing is named.
func TestReplyLog_UnknownLatestReplyNamesNothing(t *testing.T) {
	log := NewReplyLog()
	scope := ReplyScope("claudecode:acc", "session-1")
	readThrough(t, log, scope, "req_011Stream", streamedReply)

	body := []byte(`{"messages":[{"role":"user","content":"list the files"},` +
		`{"role":"assistant","content":[` + echoedText + `,` + echoedToolUse + `]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01A","content":"a.go"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"a reply the log never saw"}]},` +
		`{"role":"user","content":"thanks"}]}`)
	if got := log.PrevRequestID(scope, TurnOf(body)); got != "" {
		t.Errorf("PrevRequestID = %q, want none: the latest reply is unknown", got)
	}
	if got := log.PrevRequestID(scope, TurnOf([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))); got != "" {
		t.Errorf("a conversation's first request named %q", got)
	}
}

func TestReplyLog_ScopeSeparatesAccountsAndSessions(t *testing.T) {
	log := NewReplyLog()
	readThrough(t, log, ReplyScope("claudecode:acc-a", "session-1"), "req_011Stream", streamedReply)
	turn := TurnOf(followUp(`[` + echoedToolUse + `]`))

	if got := log.PrevRequestID(ReplyScope("claudecode:acc-b", "session-1"), turn); got != "" {
		t.Errorf("account b was given account a's request id %q", got)
	}
	if got := log.PrevRequestID(ReplyScope("claudecode:acc-a", "session-2"), turn); got != "" {
		t.Errorf("session 2 was given session 1's request id %q", got)
	}
	for _, key := range []string{"", "  "} {
		if scope := ReplyScope("claudecode:acc-a", key); scope != "" {
			t.Errorf("ReplyScope with session key %q = %q; an unkeyed request has no conversation to link", key, scope)
		}
	}
}

func TestReplyLog_DropsTheOldestKeyWhenFull(t *testing.T) {
	log := newReplyLog(2)
	scope := ReplyScope("claudecode:acc", "session-1")
	for _, reply := range []struct{ id, text string }{
		{"req_1", "one"}, {"req_2", "two"}, {"req_3", "three"},
	} {
		log.RecordMessage(scope, reply.id, []byte(`{"content":[{"type":"text","text":"`+reply.text+`"}]}`))
	}

	for text, want := range map[string]string{"one": "", "two": "req_2", "three": "req_3"} {
		if got := log.PrevRequestID(scope, TurnOf(followUp(`"`+text+`"`))); got != want {
			t.Errorf("reply %q: PrevRequestID = %q, want %q", text, got, want)
		}
	}
}

// The request id comes from an upstream response header and is spliced into
// the billing line, so a value outside the client's own guard is never kept.
func TestReplyLog_RefusesRequestIDsTheClientWouldNotSend(t *testing.T) {
	log := NewReplyLog()
	scope := ReplyScope("claudecode:acc", "session-1")
	bad := "req_011; cc_turn_index=9"
	readThrough(t, log, scope, bad, streamedReply)
	log.RecordMessage(scope, bad, []byte(`{"content":[{"type":"text","text":"Two files."}]}`))

	for _, reply := range []string{`[` + echoedToolUse + `]`, `"Two files."`} {
		if got := log.PrevRequestID(scope, TurnOf(followUp(reply))); got != "" {
			t.Errorf("reply %s: PrevRequestID = %q, want none", reply, got)
		}
	}
}
