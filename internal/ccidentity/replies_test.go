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

// firstRequest is the request streamedReply answers; followUp continues it.
var firstRequest = []byte(`{"messages":[{"role":"user","content":"list the files"}]}`)

// scopeOf is the scope a caller computes for the request with this body.
func scopeOf(upstream, session string, body []byte) string {
	return ReplyScope(upstream, session, TurnOf(body))
}

// prevOf is what a caller asks the log for the request with this body: the id
// of the request that produced the reply its latest assistant message echoes.
func prevOf(log *ReplyLog, upstream, session string, body []byte) string {
	turn := TurnOf(body)
	return log.PrevRequestID(ReplyScope(upstream, session, turn), turn)
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
	scope := scopeOf("claudecode:acc", "session-1", firstRequest)
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
		if got := prevOf(log, "claudecode:acc", "session-1", followUp(tc.reply)); got != "req_011Stream" {
			t.Errorf("%s: PrevRequestID = %q, want req_011Stream", tc.name, got)
		}
	}
	if got := prevOf(log, "claudecode:acc", "session-1", followUp(`[`+rewrittenText+`]`)); got != "" {
		t.Errorf("a reply that shares no block with the stream was named %q", got)
	}
}

func TestReplyLog_FollowUpNamesTheUnaryReplyItEchoes(t *testing.T) {
	log := NewReplyLog()
	const upstream = "endpoint:https://api.anthropic.com"
	first := []byte(`{"messages":[{"role":"user","content":"how many files?"}]}`)
	log.RecordMessage(scopeOf(upstream, "session-1", first), "req_011Unary", []byte(`{"id":"msg_01","type":"message","role":"assistant",`+
		`"content":[{"type":"text","text":"Two files."}],"stop_reason":"end_turn"}`))

	// A text-only reply, echoed with string content, then a new prompt.
	body := []byte(`{"messages":[{"role":"user","content":"how many files?"},` +
		`{"role":"assistant","content":"Two files."},{"role":"user","content":"and tests?"}]}`)
	if got := prevOf(log, upstream, "session-1", body); got != "req_011Unary" {
		t.Errorf("PrevRequestID = %q, want req_011Unary", got)
	}
}

// The real client names the request behind its latest reply. When the log does
// not know the latest reply, an earlier reply's request is not the one before
// this request, so nothing is named.
func TestReplyLog_UnknownLatestReplyNamesNothing(t *testing.T) {
	log := NewReplyLog()
	readThrough(t, log, scopeOf("claudecode:acc", "session-1", firstRequest), "req_011Stream", streamedReply)

	body := []byte(`{"messages":[{"role":"user","content":"list the files"},` +
		`{"role":"assistant","content":[` + echoedText + `,` + echoedToolUse + `]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01A","content":"a.go"}]},` +
		`{"role":"assistant","content":[{"type":"text","text":"a reply the log never saw"}]},` +
		`{"role":"user","content":"thanks"}]}`)
	if got := prevOf(log, "claudecode:acc", "session-1", body); got != "" {
		t.Errorf("PrevRequestID = %q, want none: the latest reply is unknown", got)
	}
	if got := prevOf(log, "claudecode:acc", "session-1", firstRequest); got != "" {
		t.Errorf("a conversation's first request named %q", got)
	}
}

func TestReplyLog_ScopeSeparatesAccountsAndSessions(t *testing.T) {
	log := NewReplyLog()
	readThrough(t, log, scopeOf("claudecode:acc-a", "session-1", firstRequest), "req_011Stream", streamedReply)
	body := followUp(`[` + echoedToolUse + `]`)

	if got := prevOf(log, "claudecode:acc-a", "session-1", body); got != "req_011Stream" {
		t.Fatalf("the account and session that recorded the reply got %q, want req_011Stream", got)
	}
	if got := prevOf(log, "claudecode:acc-b", "session-1", body); got != "" {
		t.Errorf("account b was given account a's request id %q", got)
	}
	if got := prevOf(log, "claudecode:acc-a", "session-2", body); got != "" {
		t.Errorf("session 2 was given session 1's request id %q", got)
	}
	for _, key := range []string{"", "  "} {
		if scope := scopeOf("claudecode:acc-a", key, firstRequest); scope != "" {
			t.Errorf("ReplyScope with session key %q = %q; an unkeyed request has no conversation to link", key, scope)
		}
	}
}

// Claude Code keeps one session id for its main thread, its subagents and its
// side queries, so the session alone cannot tell two conversations apart. Two of
// them often end on the same words, such as "Done.", and each one's next request
// has to name its own reply. A client that rewrites its first prompt (it
// compacts or trims its history) names no request: a miss is all the log risks,
// and it never names another conversation's request.
func TestReplyLog_ConversationsOfOneSessionStayApart(t *testing.T) {
	const upstream, session = "claudecode:acc", "session-1"
	log := NewReplyLog()
	reply := []byte(`{"content":[{"type":"text","text":"Done."}]}`)
	opening := func(prompt string) []byte {
		return []byte(`{"messages":[{"role":"user","content":"` + prompt + `"}]}`)
	}
	continued := func(prompt string) []byte {
		return []byte(`{"messages":[{"role":"user","content":"` + prompt + `"},` +
			`{"role":"assistant","content":[{"type":"text","text":"Done."}]},{"role":"user","content":"and then?"}]}`)
	}

	log.RecordMessage(scopeOf(upstream, session, opening("main task")), "req_main", reply)
	log.RecordMessage(scopeOf(upstream, session, opening("subagent task")), "req_sub", reply)

	for _, tc := range []struct{ name, prompt, want string }{
		{"the main conversation", "main task", "req_main"},
		{"the subagent", "subagent task", "req_sub"},
		{"a conversation whose first prompt changed", "main task, trimmed", ""},
	} {
		if got := prevOf(log, upstream, session, continued(tc.prompt)); got != tc.want {
			t.Errorf("%s: PrevRequestID = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A client may put a <system-reminder> block on the latest message for one
// request and leave it off once the message is older. The conversation's first
// prompt is the latest message on request 1 and an old one on request 2, so the
// reminder it carried on request 1 must not move its conversation to another
// scope: the follow-up would miss the reply of the request it continues.
func TestReplyLog_FirstPromptKeepsItsScopeWhenItsReminderGoes(t *testing.T) {
	log := NewReplyLog()
	opening := []byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"<system-reminder>it is Tuesday</system-reminder>"},` +
		`{"type":"text","text":"list the files"}]}]}`)
	readThrough(t, log, scopeOf("claudecode:acc", "session-1", opening), "req_011Stream", streamedReply)

	// On request 2 the first message has no reminder; followUp's prompt is the
	// same words as a string.
	if got := prevOf(log, "claudecode:acc", "session-1", followUp(`[`+echoedToolUse+`]`)); got != "req_011Stream" {
		t.Errorf("PrevRequestID = %q, want req_011Stream", got)
	}

	other := []byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"<system-reminder>it is Tuesday</system-reminder>"},` +
		`{"type":"text","text":"list the tests"}]},` +
		`{"role":"assistant","content":[` + echoedToolUse + `]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01A","content":"a.go"}]}]}`)
	if got := prevOf(log, "claudecode:acc", "session-1", other); got != "" {
		t.Errorf("a conversation with another first prompt was named %q", got)
	}
}

func TestReplyLog_DropsTheOldestKeyWhenFull(t *testing.T) {
	log := newReplyLog(2)
	scope := scopeOf("claudecode:acc", "session-1", firstRequest)
	for _, reply := range []struct{ id, text string }{
		{"req_1", "one"}, {"req_2", "two"}, {"req_3", "three"},
	} {
		log.RecordMessage(scope, reply.id, []byte(`{"content":[{"type":"text","text":"`+reply.text+`"}]}`))
	}

	for text, want := range map[string]string{"one": "", "two": "req_2", "three": "req_3"} {
		if got := prevOf(log, "claudecode:acc", "session-1", followUp(`"`+text+`"`)); got != want {
			t.Errorf("reply %q: PrevRequestID = %q, want %q", text, got, want)
		}
	}
}

// The request id comes from an upstream response header and is spliced into
// the billing line, so a value outside the client's own guard is never kept.
func TestReplyLog_RefusesRequestIDsTheClientWouldNotSend(t *testing.T) {
	log := NewReplyLog()
	scope := scopeOf("claudecode:acc", "session-1", firstRequest)
	bad := "req_011; cc_turn_index=9"
	readThrough(t, log, scope, bad, streamedReply)
	log.RecordMessage(scope, bad, []byte(`{"content":[{"type":"text","text":"Two files."}]}`))

	for _, reply := range []string{`[` + echoedToolUse + `]`, `"Two files."`} {
		if got := prevOf(log, "claudecode:acc", "session-1", followUp(reply)); got != "" {
			t.Errorf("reply %s: PrevRequestID = %q, want none", reply, got)
		}
	}
}
