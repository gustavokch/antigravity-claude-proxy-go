package zen

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func responsesInputItems(t *testing.T, body map[string]any) []any {
	t.Helper()
	items, ok := body["input"].([]any)
	if !ok {
		t.Fatalf("input is %T, want []any", body["input"])
	}
	return items
}

func TestAnthropicToResponsesRequest_ConversationShape(t *testing.T) {
	req := map[string]any{
		"model":      "opencode/gpt-5.5",
		"max_tokens": float64(512),
		"system":     "be terse",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "checking"},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ls"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "a.txt"},
				map[string]any{"type": "text", "text": "thanks"},
			}},
		},
		"tools": []any{
			map[string]any{"name": "Bash", "description": "run", "input_schema": map[string]any{"type": "object"}},
		},
		"tool_choice": map[string]any{"type": "auto"},
		"stream":      false,
	}

	out, rev, injected := anthropicToResponsesRequest(req)

	if out["model"] != "gpt-5.5" {
		t.Errorf("model = %v, want gpt-5.5 (opencode/ prefix stripped)", out["model"])
	}
	if out["max_output_tokens"] != float64(512) {
		t.Errorf("max_output_tokens = %v, want 512", out["max_output_tokens"])
	}
	if out["stream"] != true {
		t.Errorf("stream = %v, want true (gate rejects non-streaming bodies)", out["stream"])
	}
	if _, ok := out["max_tokens"]; ok {
		t.Error("max_tokens must not leak into a Responses body")
	}

	items := responsesInputItems(t, out)
	if len(items) != 6 {
		t.Fatalf("input has %d items, want 6 (system, user, assistant, function_call, function_call_output, user): %s", len(items), mustJSON(t, items))
	}

	sys, _ := items[0].(map[string]any)
	if sys["role"] != "system" {
		t.Errorf("input[0].role = %v, want system", sys["role"])
	}
	sysParts, _ := sys["content"].([]any)
	if len(sysParts) != 1 {
		t.Fatalf("system content = %v, want one input_text part", sysParts)
	}
	if part, _ := sysParts[0].(map[string]any); part["type"] != "input_text" || part["text"] != "be terse" {
		t.Errorf("system part = %v, want input_text/be terse", sysParts[0])
	}

	user0, _ := items[1].(map[string]any)
	if user0["role"] != "user" {
		t.Errorf("input[1].role = %v, want user", user0["role"])
	}

	assistant, _ := items[2].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("input[2].role = %v, want assistant: %s", assistant["role"], mustJSON(t, items[2]))
	}
	aparts, _ := assistant["content"].([]any)
	if len(aparts) != 1 {
		t.Fatalf("assistant content = %v, want one output_text part", aparts)
	}
	if part, _ := aparts[0].(map[string]any); part["type"] != "output_text" || part["text"] != "checking" {
		t.Errorf("assistant part = %v, want output_text/checking", aparts[0])
	}

	call, _ := items[3].(map[string]any)
	if call["type"] != "function_call" {
		t.Fatalf("input[3].type = %v, want function_call: %s", call["type"], mustJSON(t, items[3]))
	}
	if call["name"] != "bash" {
		t.Errorf("function_call name = %v, want bash (gate spelling; client declared Bash)", call["name"])
	}
	if call["call_id"] != "toolu_1" {
		t.Errorf("function_call call_id = %v, want toolu_1", call["call_id"])
	}
	if args, _ := call["arguments"].(string); args != `{"command":"ls"}` {
		t.Errorf("function_call arguments = %q, want the JSON-encoded input", args)
	}

	// A function_call_output must directly follow its function_call, so the
	// user turn's tool_result items lead the turn; the turn's remaining text
	// then becomes the trailing user message.
	answer, _ := items[4].(map[string]any)
	if answer["type"] != "function_call_output" {
		t.Fatalf("input[4].type = %v, want function_call_output: %s", answer["type"], mustJSON(t, items[4]))
	}
	if answer["call_id"] != "toolu_1" {
		t.Errorf("function_call_output call_id = %v, want toolu_1", answer["call_id"])
	}
	if answer["output"] != "a.txt" {
		t.Errorf("function_call_output output = %v, want a.txt", answer["output"])
	}

	closing, _ := items[5].(map[string]any)
	if closing["role"] != "user" {
		t.Fatalf("input[5].role = %v, want user: %s", closing["role"], mustJSON(t, items[5]))
	}
	cparts, _ := closing["content"].([]any)
	if len(cparts) != 1 {
		t.Fatalf("closing user content = %v, want one input_text part", cparts)
	}
	if part, _ := cparts[0].(map[string]any); part["type"] != "input_text" || part["text"] != "thanks" {
		t.Errorf("closing user part = %v, want input_text/thanks", cparts[0])
	}

	tools, _ := out["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		tool, _ := tl.(map[string]any)
		if tool["type"] != "function" {
			t.Errorf("tool = %v, want type function (Responses tools are flat)", tool)
		}
		n, _ := tool["name"].(string)
		names[n] = true
		if _, wrapped := tool["function"]; wrapped {
			t.Errorf("tool %q is chat-wrapped; Responses tools are flat %s", n, mustJSON(t, tool))
		}
	}
	if !names["bash"] {
		t.Error("tools must always contain bash (gate)")
	}
	if !names["read"] {
		t.Error("tools must always contain read (gate)")
	}
	if rev["bash"] != "Bash" {
		t.Errorf("reverse rename = %v, want bash→Bash", rev)
	}
	if !injected["read"] {
		t.Error("read was not declared by the client, so it must be reported as injected")
	}
	if injected["bash"] {
		t.Error("bash was declared (as Bash), so it must not be reported as injected")
	}
	if out["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v, want auto", out["tool_choice"])
	}
}

// tool_choice rides to the Responses wire: "any" becomes "required" (the
// Responses spelling) and a forced tool must name the renamed upstream
// function, not the client's spelling, or the upstream call names a function
// that is not in the tools array.
func TestAnthropicToResponsesRequest_ToolChoice(t *testing.T) {
	tools := []any{
		map[string]any{"name": "Bash", "description": "run", "input_schema": map[string]any{"type": "object"}},
	}
	for _, tc := range []struct {
		name   string
		choice map[string]any
		want   any
	}{
		{"auto", map[string]any{"type": "auto"}, "auto"},
		{"none", map[string]any{"type": "none"}, "none"},
		{"any becomes required", map[string]any{"type": "any"}, "required"},
		{"forced tool names the upstream function",
			map[string]any{"type": "tool", "name": "Bash"},
			map[string]any{"type": "function", "name": "bash"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, _, _ := anthropicToResponsesRequest(map[string]any{
				"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
				"tools":       tools,
				"tool_choice": tc.choice,
			})
			if got, want := mustJSON(t, out["tool_choice"]), mustJSON(t, tc.want); got != want {
				t.Errorf("tool_choice = %s, want %s", got, want)
			}
		})
	}
}

// Server tools (no input_schema) have no Responses equivalent and are dropped,
// and stop_sequences has no Responses field at all.
func TestAnthropicToResponsesRequest_DropsUnsupportedFields(t *testing.T) {
	req := map[string]any{
		"model": "gpt-5",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		},
		"stop_sequences": []any{"END"},
		"thinking":       map[string]any{"type": "enabled"},
		"stream":         true,
		"tools": []any{
			map[string]any{"type": "web_search_20250305", "name": "web_search"},
		},
	}

	out, _, injected := anthropicToResponsesRequest(req)

	if _, ok := out["stop_sequences"]; ok {
		t.Error("stop_sequences must be dropped: the Responses API has no equivalent field")
	}
	if _, ok := out["thinking"]; ok {
		t.Error("thinking must be dropped: the Responses API has no thinking field")
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 2 {
		t.Errorf("tools = %s, want only the two injected gate tools (server tools dropped)", mustJSON(t, tools))
	}
	if len(injected) != 2 {
		t.Errorf("injected = %v, want bash and read", injected)
	}
}

// The Responses API retains every response server-side unless told otherwise,
// and the genuine OpenCode harness sends store:false for every model on this
// wire. store must be an explicit false, and because the body is built from an
// allowlist a client-supplied value must never reach the wire.
func TestAnthropicToResponsesRequest_DisablesStorage(t *testing.T) {
	cases := []struct {
		name string
		req  map[string]any
	}{
		{
			name: "field absent",
			req: map[string]any{
				"model":    "gpt-5",
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			},
		},
		{
			name: "client asks for store true",
			req: map[string]any{
				"model":    "gpt-5",
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
				"store":    true,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _, _ := anthropicToResponsesRequest(tc.req)
			v, present := out["store"]
			if !present {
				t.Fatalf("store missing from the upstream body (OpenAI stores by default): %s", mustJSON(t, out))
			}
			if v != false {
				t.Errorf("store = %v (%T), want the boolean false", v, v)
			}
		})
	}
}

// store:false is only safe because the replay is stateless: every turn resends
// the whole conversation, no replayed item references a stored id, and the
// body never chains on previous_response_id. An item carrying an id would make
// the upstream look it up and fail with "Item with id ... not found" once
// storage is off - an error that reads like a ban, not a bug.
func TestAnthropicToResponsesRequest_ReplayIsStateless(t *testing.T) {
	out, _, _ := anthropicToResponsesRequest(map[string]any{
		"model": "gpt-5",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "checking"},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{"command": "ls"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "a.txt"},
			}},
		},
	})
	if _, ok := out["previous_response_id"]; ok {
		t.Error("previous_response_id must never be sent: the stored response it names does not exist with store:false")
	}
	for _, raw := range responsesInputItems(t, out) {
		item, _ := raw.(map[string]any)
		if _, ok := item["id"]; ok {
			t.Errorf("replayed input item carries an id the upstream never stored: %s", mustJSON(t, item))
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// The JSON body of POST /v1/responses becomes an Anthropic message: output
// items flatten into content blocks, usage moves to the Anthropic convention,
// and stop_reason is derived (there is no finish_reason on this wire).
func TestResponsesResponseToAnthropic_OutputItems(t *testing.T) {
	resp := map[string]any{
		"id": "resp_123",
		"output": []any{
			map[string]any{"type": "reasoning", "summary": []any{
				map[string]any{"type": "summary_text", "text": "thinking hard"},
			}},
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "listing"},
			}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "bash", "arguments": `{"command":"ls"}`},
		},
		"usage": map[string]any{
			"input_tokens":         100,
			"output_tokens":        20,
			"input_tokens_details": map[string]any{"cached_tokens": 40},
		},
	}

	out := ResponsesResponseToAnthropic(resp, "gpt-5.5", map[string]string{"bash": "Bash"}, map[string]bool{"read": true})

	if out["id"] != "msg_resp_123" {
		t.Errorf("id = %v, want msg_resp_123", out["id"])
	}
	if out["type"] != "message" || out["role"] != "assistant" {
		t.Errorf("type/role = %v/%v, want message/assistant", out["type"], out["role"])
	}
	if out["model"] != "gpt-5.5" {
		t.Errorf("model = %v, want gpt-5.5", out["model"])
	}
	if out["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use (a function_call is in the output)", out["stop_reason"])
	}
	content, _ := out["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content = %s, want thinking + text + tool_use", mustJSON(t, content))
	}
	think, _ := content[0].(map[string]any)
	if think["type"] != "thinking" || think["thinking"] != "thinking hard" {
		t.Errorf("content[0] = %s, want a thinking block", mustJSON(t, content[0]))
	}
	text, _ := content[1].(map[string]any)
	if text["type"] != "text" || text["text"] != "listing" {
		t.Errorf("content[1] = %s, want a text block", mustJSON(t, content[1]))
	}
	tool, _ := content[2].(map[string]any)
	if tool["type"] != "tool_use" || tool["id"] != "call_1" || tool["name"] != "Bash" {
		t.Errorf("content[2] = %s, want tool_use renamed back to Bash", mustJSON(t, content[2]))
	}
	if input, _ := tool["input"].(map[string]any); input["command"] != "ls" {
		t.Errorf("tool input = %s, want the decoded arguments", mustJSON(t, tool["input"]))
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"] != 60 || usage["cache_read_input_tokens"] != 40 || usage["output_tokens"] != 20 {
		t.Errorf("usage = %s, want input 60 (100-40 cached), cache_read 40, output 20", mustJSON(t, usage))
	}
}

// incomplete_details.reason == "max_output_tokens" is the only signal that the
// output cap stopped the model; with no function call in the output that maps
// to stop_reason "max_tokens".
func TestResponsesResponseToAnthropic_StopReasons(t *testing.T) {
	textOnly := ResponsesResponseToAnthropic(map[string]any{
		"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{
			map[string]any{"type": "output_text", "text": "hi"},
		}}},
	}, "gpt-5", nil, nil)
	if textOnly["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", textOnly["stop_reason"])
	}

	truncated := ResponsesResponseToAnthropic(map[string]any{
		"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{
			map[string]any{"type": "output_text", "text": "cut"},
		}}},
		"incomplete_details": map[string]any{"reason": "max_output_tokens"},
	}, "gpt-5", nil, nil)
	if truncated["stop_reason"] != "max_tokens" {
		t.Errorf("stop_reason = %v, want max_tokens", truncated["stop_reason"])
	}
}

// A function call to a gate-injected tool the client never declared has no
// client-side tool to resolve, so it must not reach the client and must not
// claim stop_reason "tool_use".
func TestResponsesResponseToAnthropic_DropsInjectedToolCalls(t *testing.T) {
	resp := map[string]any{
		"output": []any{
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "done"},
			}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "read", "arguments": `{}`},
		},
	}
	out := ResponsesResponseToAnthropic(resp, "gpt-5", nil, map[string]bool{"read": true})
	content, _ := out["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %s, want only the text block (injected tool call dropped)", mustJSON(t, content))
	}
	if out["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn (no surviving tool call)", out["stop_reason"])
	}
}

// An upstream error status becomes the Anthropic error envelope with the
// status-mapped kind, so the client reports it as an error rather than
// parsing it as a message.
func TestTranslateResponsesResponse_ErrorStatusBecomesAnthropicError(t *testing.T) {
	upstream := &http.Response{
		StatusCode: 429,
		Status:     "429 Too Many Requests",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"slow down"}}`)),
	}
	out := translateResponsesResponse(upstream, "gpt-5", false, nil, nil)
	raw, _ := io.ReadAll(out.Body)
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("error body not JSON: %v; body = %s", err, raw)
	}
	if out.StatusCode != 429 {
		t.Errorf("status = %d, want 429 preserved", out.StatusCode)
	}
	errObj, _ := env["error"].(map[string]any)
	if errObj["type"] != "rate_limit_error" {
		t.Errorf("error type = %v, want rate_limit_error", errObj["type"])
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "slow down") {
		t.Errorf("error message = %q, want the upstream message", msg)
	}
}

// A non-streaming client that receives a non-streaming JSON body gets the
// translated message back as JSON.
func TestTranslateResponsesResponse_JSONBodyForNonStreamClient(t *testing.T) {
	body := `{"id":"resp_9","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"pong"}]}],"usage":{"input_tokens":3,"output_tokens":1}}`
	upstream := &http.Response{
		StatusCode: 200,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	out := translateResponsesResponse(upstream, "gpt-5", false, nil, nil)
	if ct := out.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	raw, _ := io.ReadAll(out.Body)
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("body not JSON: %v; body = %s", err, raw)
	}
	if msg["type"] != "message" || msg["stop_reason"] != "end_turn" {
		t.Errorf("response = %s, want an Anthropic message", raw)
	}
	content, _ := msg["content"].([]any)
	if part, _ := content[0].(map[string]any); part["text"] != "pong" {
		t.Errorf("content = %s, want pong", raw)
	}
}

// trackedBody records that the upstream body was closed, so a test can prove a
// rewritten response did not orphan the connection.
type trackedBody struct {
	io.Reader
	once   sync.Once
	closed chan struct{}
}

func newTrackedBody(s string) *trackedBody {
	return &trackedBody{Reader: strings.NewReader(s), closed: make(chan struct{})}
}

func (b *trackedBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return nil
}

// waitClosed asserts the body was closed, without hanging the suite when it
// was not.
func (b *trackedBody) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-b.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream body was not closed: the connection is leaked")
	}
}

// sseUpstream wraps an event stream as an upstream 200 response.
func sseUpstream(body *trackedBody) *http.Response {
	return &http.Response{
		StatusCode:    200,
		Status:        "200 OK",
		Header:        http.Header{"Content-Type": []string{"text/event-stream"}, "Content-Encoding": []string{"gzip"}, "Content-Length": []string{"42"}},
		ContentLength: 42,
		Body:          body,
	}
}

// A non-streaming client gets the upstream stream folded back into the single
// JSON body a non-streaming call would have returned: text, reasoning, and
// tool-call arguments all reconstructed from the deltas.
func TestAggregateResponsesStream_ReconstructsOutput(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_7"}}`,
		``,
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}`,
		``,
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"pondering"}`,
		``,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":1,"delta":"hel"}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":1,"delta":"lo"}`,
		``,
		`data: {"type":"response.output_item.added","output_index":2,"item":{"id":"fc_1","type":"function_call","call_id":"call_9","name":"bash","arguments":""}}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"command\":"}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","output_index":2,"delta":"\"ls\"}"}`,
		``,
		`data: {"type":"response.output_item.done","output_index":2,"item":{"id":"fc_1","type":"function_call","call_id":"call_9","name":"bash","arguments":"{\"command\":\"ls\"}"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_7","usage":{"input_tokens":10,"output_tokens":4}}}`,
		``,
	}, "\n")

	agg, err := aggregateResponsesStream(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("aggregateResponsesStream: %v", err)
	}
	if agg["id"] != "resp_7" {
		t.Errorf("id = %v, want resp_7", agg["id"])
	}
	output := anySlice(agg["output"])
	if len(output) != 3 {
		t.Fatalf("output = %s, want reasoning + message + function_call", mustJSON(t, output))
	}
	reason, _ := output[0].(map[string]any)
	if summary := anySlice(reason["summary"]); len(summary) != 1 {
		t.Fatalf("reasoning summary = %s, want one part", mustJSON(t, reason["summary"]))
	} else if part, _ := summary[0].(map[string]any); part["text"] != "pondering" {
		t.Errorf("reasoning text = %v, want pondering", part["text"])
	}
	message, _ := output[1].(map[string]any)
	parts := anySlice(message["content"])
	if len(parts) != 1 {
		t.Fatalf("message content = %s, want one output_text part", mustJSON(t, message["content"]))
	}
	if part, _ := parts[0].(map[string]any); part["text"] != "hello" {
		t.Errorf("message text = %v, want the deltas joined (hello)", part["text"])
	}
	call, _ := output[2].(map[string]any)
	if call["arguments"] != `{"command":"ls"}` {
		t.Errorf("function_call arguments = %v, want the deltas joined", call["arguments"])
	}
	usage, _ := agg["usage"].(map[string]any)
	if usage["input_tokens"] != 10.0 || usage["output_tokens"] != 4.0 {
		t.Errorf("usage = %s, want input 10 output 4", mustJSON(t, usage))
	}
}

// A stream that ends without response.completed is a dropped connection, not a
// complete answer: finishing it would pass a partial reply off as complete.
func TestAggregateResponsesStream_TruncatedStreamErrors(t *testing.T) {
	sse := "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"partial\"}\n\n"
	if _, err := aggregateResponsesStream(strings.NewReader(sse)); err == nil {
		t.Fatal("truncated stream must not aggregate silently")
	}
}

// A streaming client receives the Anthropic SSE event sequence: text as
// text_delta, a tool call as a tool_use block with input_json_delta, and a
// final message_delta carrying the derived stop_reason and Anthropic usage.
func TestStreamResponsesToAnthropic_EmitsAnthropicEvents(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_7"}}`,
		``,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}`,
		``,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_9","name":"bash","arguments":""}}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"command\":\"ls\"}"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_7","usage":{"input_tokens":10,"output_tokens":4}}}`,
		``,
	}, "\n")

	var out bytes.Buffer
	if err := streamResponsesToAnthropic(strings.NewReader(sse), &out, "gpt-5", map[string]string{"bash": "Bash"}, nil); err != nil {
		t.Fatalf("streamResponsesToAnthropic: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_start",
		`"type":"text_delta"`,
		`"type":"tool_use"`,
		`"name":"Bash"`,
		`"type":"input_json_delta"`,
		`"partial_json":"{\"command\":\"ls\"}"`,
		"event: content_block_stop",
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("event stream missing %s:\n%s", want, got)
		}
	}
}

// A tool call to a gate-injected tool must not open a tool_use block: the
// client never declared it, so it could not resolve the call.
func TestStreamResponsesToAnthropic_DropsInjectedToolCalls(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_8"}}`,
		``,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"done"}`,
		``,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_2","name":"read","arguments":""}}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{}"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_8","usage":{"input_tokens":1,"output_tokens":1}}}`,
		``,
	}, "\n")

	var out bytes.Buffer
	if err := streamResponsesToAnthropic(strings.NewReader(sse), &out, "gpt-5", nil, map[string]bool{"read": true}); err != nil {
		t.Fatalf("streamResponsesToAnthropic: %v", err)
	}
	got := out.String()
	if strings.Contains(got, `"type":"tool_use"`) {
		t.Errorf("injected tool call must not open a tool_use block:\n%s", got)
	}
	if !strings.Contains(got, `"stop_reason":"end_turn"`) {
		t.Errorf("stop_reason = end_turn required when no tool call survives:\n%s", got)
	}
}

// A function call whose arguments arrive only with response.output_item.done
// — no argument deltas at all — must still reach the client. The completed
// item carries the whole argument string; without it the tool_use block would
// hand the client an empty input it cannot run.
func TestStreamResponsesToAnthropic_EmitsArgumentsFromCompletedItem(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_9"}}`,
		``,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_3","name":"Bash","arguments":""}}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_3","name":"Bash","arguments":"{\"command\":\"ls\"}"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_9","usage":{"input_tokens":3,"output_tokens":1}}}`,
		``,
	}, "\n")

	var out bytes.Buffer
	if err := streamResponsesToAnthropic(strings.NewReader(sse), &out, "gpt-5", nil, nil); err != nil {
		t.Fatalf("streamResponsesToAnthropic: %v", err)
	}
	got := out.String()
	for _, want := range []string{`"type":"input_json_delta"`, `"partial_json":"{\"command\":\"ls\"}"`} {
		if !strings.Contains(got, want) {
			t.Errorf("completed item arguments must reach the client, missing %s:\n%s", want, got)
		}
	}
}

// A call that streamed its arguments as deltas must not have them repeated
// when response.output_item.done restates the completed value: the client
// concatenates partial_json, so a second copy would corrupt the input.
func TestStreamResponsesToAnthropic_DoesNotRepeatDeliveredArguments(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_10"}}`,
		``,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_4","name":"Bash","arguments":""}}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"command\":"}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"ls\"}"}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_4","name":"Bash","arguments":"{\"command\":\"ls\"}"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_10","usage":{"input_tokens":3,"output_tokens":1}}}`,
		``,
	}, "\n")

	var out bytes.Buffer
	if err := streamResponsesToAnthropic(strings.NewReader(sse), &out, "gpt-5", nil, nil); err != nil {
		t.Fatalf("streamResponsesToAnthropic: %v", err)
	}
	got := out.String()
	if n := strings.Count(got, `input_json_delta`); n != 2 {
		t.Errorf("input_json_delta frames = %d, want 2 (the two streamed fragments only):\n%s", n, got)
	}
}

// A stream that fails mid-flight reports an Anthropic error event instead of
// a clean stop, so the client does not treat a partial answer as complete.
func TestStreamResponsesToAnthropic_FailedStreamEmitsError(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_9"}}`,
		``,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"partial"}`,
		``,
		`data: {"type":"response.failed","response":{"error":{"message":"upstream exploded"}}}`,
		``,
	}, "\n")

	var out bytes.Buffer
	if err := streamResponsesToAnthropic(strings.NewReader(sse), &out, "gpt-5", nil, nil); err != nil {
		t.Fatalf("streamResponsesToAnthropic: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "event: error") || !strings.Contains(got, "upstream exploded") {
		t.Errorf("failed stream must emit an error event carrying the reason:\n%s", got)
	}
	if strings.Contains(got, "event: message_stop") {
		t.Errorf("a failed stream must not emit message_stop:\n%s", got)
	}
}

// The gate-injected drop is not a property of the streaming emitter alone: in
// the aggregate direction the same call must not become a tool_use block. With
// the injected check removed this fails on both the content and the stop reason.
func TestAggregateResponsesStream_DropsInjectedToolCalls(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_8"}}`,
		``,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"done"}`,
		``,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_2","name":"read","arguments":""}}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{}"}`,
		``,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_2","name":"read","arguments":"{}"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_8","usage":{"input_tokens":1,"output_tokens":1}}}`,
		``,
	}, "\n")

	agg, err := aggregateResponsesStream(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("aggregateResponsesStream: %v", err)
	}
	msg := ResponsesResponseToAnthropic(agg, "gpt-5", nil, map[string]bool{"read": true})
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %s, want only the text block (injected tool call dropped)", mustJSON(t, content))
	}
	if part, _ := content[0].(map[string]any); part["type"] != "text" || part["text"] != "done" {
		t.Errorf("content[0] = %s, want the text block", mustJSON(t, content[0]))
	}
	if msg["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn (no surviving tool call)", msg["stop_reason"])
	}
}

// A refusal part is not silently dropped, and incomplete_details
// content_filter is not reported as a clean end_turn: Anthropic's real
// stop_reason for a declined generation is "refusal".
func TestResponsesResponseToAnthropic_Refusal(t *testing.T) {
	refused := ResponsesResponseToAnthropic(map[string]any{
		"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{
			map[string]any{"type": "refusal", "refusal": "I can't help with that."},
		}}},
	}, "gpt-5", nil, nil)
	if refused["stop_reason"] != "refusal" {
		t.Errorf("stop_reason = %v, want refusal for a refusal part", refused["stop_reason"])
	}
	content, _ := refused["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %s, want the refusal text surfaced", mustJSON(t, content))
	}
	if part, _ := content[0].(map[string]any); part["type"] != "text" {
		t.Errorf("content[0] = %s, want a text block carrying the refusal", mustJSON(t, content[0]))
	}

	filtered := ResponsesResponseToAnthropic(map[string]any{
		"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{
			map[string]any{"type": "output_text", "text": "partial"},
		}}},
		"incomplete_details": map[string]any{"reason": "content_filter"},
	}, "gpt-5", nil, nil)
	if filtered["stop_reason"] != "refusal" {
		t.Errorf("stop_reason = %v, want refusal for incomplete_details.content_filter", filtered["stop_reason"])
	}
}

// A refusal in the aggregate direction survives the fold: the deltas become a
// refusal part the non-streaming path maps, and content_filter still reaches
// the client as stop_reason refusal.
func TestAggregateResponsesStream_RefusalReachesClient(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_r"}}`,
		``,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
		``,
		`data: {"type":"response.refusal.delta","output_index":0,"delta":"I can't "}`,
		``,
		`data: {"type":"response.refusal.delta","output_index":0,"delta":"help with that."}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_r","incomplete_details":{"reason":"content_filter"},"usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")

	agg, err := aggregateResponsesStream(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("aggregateResponsesStream: %v", err)
	}
	msg := ResponsesResponseToAnthropic(agg, "gpt-5", nil, nil)
	if msg["stop_reason"] != "refusal" {
		t.Errorf("stop_reason = %v, want refusal (content_filter through the aggregate path)", msg["stop_reason"])
	}
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %s, want the refusal text folded in", mustJSON(t, content))
	}
	part, _ := content[0].(map[string]any)
	text, _ := part["text"].(string)
	if part["type"] != "text" || !strings.Contains(text, "I can't help with that.") {
		t.Errorf("content[0] = %s, want the joined refusal deltas", mustJSON(t, content[0]))
	}
}

// A streamed refusal reports stop_reason refusal, never end_turn, and its text
// reaches the client as text_delta. The two signals are exercised separately:
// a refusal delta on its own, and incomplete_details content_filter on a
// stream that carried ordinary text.
func TestStreamResponsesToAnthropic_RefusalStopReason(t *testing.T) {
	t.Run("refusal delta", func(t *testing.T) {
		sse := strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_r"}}`,
			``,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
			``,
			`data: {"type":"response.refusal.delta","output_index":0,"delta":"I can't help with that."}`,
			``,
			`data: {"type":"response.completed","response":{"id":"resp_r","usage":{"input_tokens":5,"output_tokens":2}}}`,
			``,
		}, "\n")

		var out bytes.Buffer
		if err := streamResponsesToAnthropic(strings.NewReader(sse), &out, "gpt-5", nil, nil); err != nil {
			t.Fatalf("streamResponsesToAnthropic: %v", err)
		}
		got := out.String()
		if !strings.Contains(got, `"stop_reason":"refusal"`) {
			t.Errorf("a refusal delta must not report end_turn:\n%s", got)
		}
		if !strings.Contains(got, `"type":"text_delta"`) || !strings.Contains(got, "I can't help with that.") {
			t.Errorf("refusal text must reach the client as text_delta:\n%s", got)
		}
	})

	t.Run("content_filter without a refusal delta", func(t *testing.T) {
		sse := strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_r"}}`,
			``,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
			``,
			`data: {"type":"response.output_text.delta","output_index":0,"delta":"half an ans"}`,
			``,
			`data: {"type":"response.incomplete","response":{"id":"resp_r","incomplete_details":{"reason":"content_filter"},"usage":{"input_tokens":5,"output_tokens":2}}}`,
			``,
		}, "\n")

		var out bytes.Buffer
		if err := streamResponsesToAnthropic(strings.NewReader(sse), &out, "gpt-5", nil, nil); err != nil {
			t.Fatalf("streamResponsesToAnthropic: %v", err)
		}
		got := out.String()
		if !strings.Contains(got, `"stop_reason":"refusal"`) {
			t.Errorf("incomplete_details.content_filter must report refusal, not end_turn:\n%s", got)
		}
		if strings.Contains(got, `"stop_reason":"end_turn"`) {
			t.Errorf("a filtered turn must not report end_turn:\n%s", got)
		}
	})
}

// A streaming client gets the upstream event stream piped through as Anthropic
// SSE: the transport headers must not still claim a buffered JSON body.
func TestTranslateResponsesResponse_StreamsAnthropicEventsForStreamClient(t *testing.T) {
	up := newTrackedBody(strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_7"}}`,
		``,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"pong"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_7","usage":{"input_tokens":3,"output_tokens":1}}}`,
		``,
	}, "\n"))
	resp := sseUpstream(up)

	out := translateResponsesResponse(resp, "gpt-5", true, nil, nil)

	if ct := out.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if out.ContentLength != -1 {
		t.Errorf("ContentLength = %d, want -1 for a streamed body", out.ContentLength)
	}
	if out.Header.Get("Content-Length") != "" || out.Header.Get("Content-Encoding") != "" {
		t.Errorf("streamed body must drop Content-Length/Content-Encoding, got %v", out.Header)
	}
	raw, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("read piped body: %v", err)
	}
	got := string(raw)
	for _, want := range []string{"event: message_start", `"type":"text_delta"`, `"stop_reason":"end_turn"`, "event: message_stop"} {
		if !strings.Contains(got, want) {
			t.Errorf("piped stream missing %s:\n%s", want, got)
		}
	}
	_ = out.Body.Close()
	up.waitClosed(t)
}

// A non-streaming client gets the same event stream folded into one JSON
// message instead of an untranslatable event stream.
func TestTranslateResponsesResponse_AggregatesStreamForNonStreamClient(t *testing.T) {
	up := newTrackedBody(strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_7"}}`,
		``,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"po"}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"ng"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_7","usage":{"input_tokens":3,"output_tokens":1}}}`,
		``,
	}, "\n"))
	resp := sseUpstream(up)

	out := translateResponsesResponse(resp, "gpt-5", false, nil, nil)

	if ct := out.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	raw, _ := io.ReadAll(out.Body)
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("body not JSON: %v; body = %s", err, raw)
	}
	if msg["type"] != "message" || msg["stop_reason"] != "end_turn" {
		t.Errorf("response = %s, want an aggregated Anthropic message", raw)
	}
	content, _ := msg["content"].([]any)
	if part, _ := content[0].(map[string]any); part["text"] != "pong" {
		t.Errorf("content = %s, want the deltas joined", raw)
	}
	up.waitClosed(t)
}

// A stream that cannot be folded is an error response, and the upstream body
// is closed on the way out: failResponse replaces resp.Body, so a body closed
// after the rewrite would leak the connection.
func TestTranslateResponsesResponse_StreamFailureStillClosesBody(t *testing.T) {
	truncated := newTrackedBody(`data: {"type":"response.output_text.delta","output_index":0,"delta":"partial"}` + "\n\n")
	out := translateResponsesResponse(sseUpstream(truncated), "gpt-5", false, nil, nil)
	if out.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for a truncated stream", out.StatusCode)
	}
	truncated.waitClosed(t)

	failed := newTrackedBody(`data: {"type":"response.created","response":{"id":"resp_9"}}` + "\n\n" +
		`data: {"type":"response.failed","response":{"error":{"message":"upstream exploded"}}}` + "\n\n")
	out = translateResponsesResponse(sseUpstream(failed), "gpt-5", false, nil, nil)
	if out.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for a failed stream", out.StatusCode)
	}
	raw, _ := io.ReadAll(out.Body)
	if !strings.Contains(string(raw), "upstream exploded") {
		t.Errorf("body = %s, want the upstream reason", raw)
	}
	failed.waitClosed(t)
}

// The streaming branch must hand events to the client as the upstream produces
// them, not buffer the body and emit it at the end. The upstream here stalls
// mid-stream: a buffered body delivers nothing until the stall is released, so
// receiving message_start while the upstream is still open is what
// distinguishes streaming from buffering.
func TestTranslateResponsesResponse_StreamBranchDeliversIncrementally(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, ev := range []string{
			`data: {"type":"response.created","response":{"id":"resp_s"}}`,
			``,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
			``,
			`data: {"type":"response.output_text.delta","output_index":0,"delta":"hello "}`,
			``,
		} {
			io.WriteString(w, ev+"\n")
			flusher.Flush()
		}
		<-release
		io.WriteString(w, `data: {"type":"response.output_text.delta","output_index":0,"delta":"world"}`+"\n\n")
		io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_s","usage":{"input_tokens":4,"output_tokens":2}}}`+"\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	out := translateResponsesResponse(resp, "gpt-5", true, nil, nil)

	br := bufio.NewReader(out.Body)
	first := make(chan string, 1)
	go func() {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				first <- "read error: " + err.Error()
				return
			}
			if strings.HasPrefix(line, "event:") {
				first <- strings.TrimSpace(line)
				return
			}
		}
	}()

	var firstEvent string
	select {
	case firstEvent = <-first:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("no event reached the client while the upstream was still open: the body was buffered")
	}
	close(release)
	if firstEvent != "event: message_start" {
		t.Fatalf("first event = %q, want message_start", firstEvent)
	}

	rest, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := string(rest); !strings.Contains(got, `"type":"text_delta"`) || !strings.Contains(got, "hello ") {
		t.Errorf("remaining events missing the first text delta:\n%s", got)
	}
	_ = out.Body.Close()
}

// Anthropic SSE requires message_start to be the first event on the wire: a
// client cannot attribute a content block to a message that has not opened.
// A Responses stream that opens with something other than
// response.created — a delta, or the response.in_progress the API also
// sends — must still emit it first.
func TestStreamResponsesToAnthropic_MessageStartPrecedesContentBlocks(t *testing.T) {
	completed := `data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"
	for _, tc := range []struct {
		name  string
		first string
	}{
		{"no response.created", `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}` + "\n\n"},
		{"response.in_progress first", `data: {"type":"response.in_progress","response":{"id":"resp_1"}}` + "\n\n"},
		{"delta first", `data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}` + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sse := tc.first +
				`data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}` + "\n\n" +
				completed

			var out bytes.Buffer
			if err := streamResponsesToAnthropic(strings.NewReader(sse), &out, "gpt-5", nil, nil); err != nil {
				t.Fatalf("streamResponsesToAnthropic: %v", err)
			}
			got := out.String()
			start := strings.Index(got, "event: message_start")
			block := strings.Index(got, "event: content_block_start")
			if start < 0 {
				t.Fatalf("no message_start emitted:\n%s", got)
			}
			if block < 0 {
				t.Fatalf("no content block emitted:\n%s", got)
			}
			if start > block {
				t.Errorf("message_start at %d must precede content_block_start at %d:\n%s", start, block, got)
			}
		})
	}
}

// A refusal that arrives only in the output_item.done envelope — with no
// refusal delta to stream — must still reach the client as a refusal, not as
// an empty successful end_turn.
func TestAggregateResponsesStream_RefusalOnlyInItemDone(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_d"}}`,
		``,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"refusal","refusal":"I can't help with that."}]}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_d","usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")

	agg, err := aggregateResponsesStream(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("aggregateResponsesStream: %v", err)
	}
	msg := ResponsesResponseToAnthropic(agg, "gpt-5", nil, nil)
	if msg["stop_reason"] != "refusal" {
		t.Errorf("stop_reason = %v, want refusal (refusal carried only in output_item.done)", msg["stop_reason"])
	}
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %s, want the refusal text surfaced", mustJSON(t, content))
	}
	part, _ := content[0].(map[string]any)
	if text, _ := part["text"].(string); !strings.Contains(text, "I can't help with that.") {
		t.Errorf("content[0] = %s, want the refusal explanation", mustJSON(t, content[0]))
	}
}

// A stream that carries no deltas and no item envelopes, but whose
// response.completed restates the answer in response.output, must still
// reach the client instead of an empty end_turn.
func TestAggregateResponsesStream_CompletedOutputFallback(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_f"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_f","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello world"}]}],"usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")

	agg, err := aggregateResponsesStream(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("aggregateResponsesStream: %v", err)
	}
	msg := ResponsesResponseToAnthropic(agg, "gpt-5", nil, nil)
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %s, want the completed-output text surfaced", mustJSON(t, msg["content"]))
	}
	got, _ := content[0].(map[string]any)
	if text, _ := got["text"].(string); !strings.Contains(text, "hello world") {
		t.Errorf("content[0] = %s, want hello world", mustJSON(t, content[0]))
	}
	if msg["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", msg["stop_reason"])
	}
}

// An empty delta still registers a placeholder index via mark(); the
// completed-output fallback must ignore placeholders and adopt the answer.
func TestAggregateResponsesStream_CompletedOutputFallbackIgnoresEmptyPlaceholder(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_p"}}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":""}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_p","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello world"}]}],"usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")
	agg, err := aggregateResponsesStream(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("aggregateResponsesStream: %v", err)
	}
	msg := ResponsesResponseToAnthropic(agg, "gpt-5", nil, nil)
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %s, want fallback text despite empty delta", mustJSON(t, msg["content"]))
	}
	got, _ := content[0].(map[string]any)
	if text, _ := got["text"].(string); !strings.Contains(text, "hello world") {
		t.Errorf("content[0] = %s, want hello world", mustJSON(t, content[0]))
	}
}

// Reasoning summary parts are separate blocks upstream; fusing them without a
// separator would glue words together ("firstsecond").
func TestResponsesResponseToAnthropic_ReasoningMultiPartSeparator(t *testing.T) {
	resp := map[string]any{
		"output": []any{
			map[string]any{"type": "reasoning", "summary": []any{
				map[string]any{"type": "summary_text", "text": "first"},
				map[string]any{"type": "summary_text", "text": "second"},
			}},
		},
	}
	msg := ResponsesResponseToAnthropic(resp, "gpt-5", nil, nil)
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %s, want one thinking block", mustJSON(t, msg["content"]))
	}
	part, _ := content[0].(map[string]any)
	if thinking, _ := part["thinking"].(string); thinking != "first\n\nsecond" {
		t.Errorf("thinking = %q, want %q", thinking, "first\n\nsecond")
	}
}

// The aggregate fold joins multi-part reasoning summaries with the same
// separator as the JSON translation.
func TestAggregateResponsesStream_ReasoningMultiPartSeparator(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_r"}}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"first"},{"type":"summary_text","text":"second"}]}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_r","usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")

	agg, err := aggregateResponsesStream(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("aggregateResponsesStream: %v", err)
	}
	output := anySlice(agg["output"])
	if len(output) != 1 {
		t.Fatalf("output = %s, want one reasoning item", mustJSON(t, agg["output"]))
	}
	item, _ := output[0].(map[string]any)
	summary := anySlice(item["summary"])
	if len(summary) != 1 {
		t.Fatalf("summary = %s, want one folded part", mustJSON(t, item["summary"]))
	}
	if text, _ := summary[0].(map[string]any)["text"].(string); text != "first\n\nsecond" {
		t.Errorf("summary text = %q, want %q", text, "first\n\nsecond")
	}
}

// A function call whose arguments stream but whose response.output_item.done
// never arrives keeps the envelope from response.output_item.added: dropping it
// would lose a tool call and end the agent loop on a clean end_turn.
func TestAggregateResponsesStream_FunctionCallWithoutItemDone(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_t"}}`,
		``,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_7","name":"bash","arguments":""}}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"command\":"}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"ls\"}"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_t","usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")

	agg, err := aggregateResponsesStream(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("aggregateResponsesStream: %v", err)
	}
	msg := ResponsesResponseToAnthropic(agg, "gpt-5", nil, nil)
	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use (a streamed tool call must not be lost)", msg["stop_reason"])
	}
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %s, want the tool_use block", mustJSON(t, content))
	}
	tool, _ := content[0].(map[string]any)
	if tool["type"] != "tool_use" || tool["id"] != "call_7" || tool["name"] != "bash" {
		t.Errorf("content[0] = %s, want tool_use from the added envelope", mustJSON(t, content[0]))
	}
	if input, _ := tool["input"].(map[string]any); input["command"] != "ls" {
		t.Errorf("tool input = %s, want the argument deltas joined", mustJSON(t, tool["input"]))
	}
}

// A non-streaming client produces a Responses request with "stream": true and
// a bare gate tool set, and gets the upstream stream folded back into one
// Anthropic JSON message.
func TestSendResponses_NonStreamClientGetsJSON(t *testing.T) {
	var gotPath, gotAuth, gotAccept, gotUA string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotAccept = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Accept")
		gotUA = r.Header.Get("User-Agent")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_1"}}`,
			``,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
			``,
			`data: {"type":"response.output_text.delta","output_index":0,"delta":"pong"}`,
			``,
			`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":4,"output_tokens":2}}}`,
			``,
		}, "\n"))
	}))
	defer upstream.Close()

	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"ping"}],"max_tokens":64,"stream":false}`)
	resp, err := SendResponses(context.Background(), upstream.Client(), upstream.URL, "sk-zen-test", body)
	if err != nil {
		t.Fatalf("SendResponses: %v", err)
	}
	defer resp.Body.Close()

	if gotPath != "/v1/responses" {
		t.Errorf("upstream path = %q, want /v1/responses", gotPath)
	}
	if gotAuth != "Bearer sk-zen-test" {
		t.Errorf("Authorization = %q, want Bearer sk-zen-test", gotAuth)
	}
	if gotAccept != "*/*" {
		t.Errorf("Accept = %q, want */* for a non-streaming client", gotAccept)
	}
	if gotBody["stream"] != true {
		t.Errorf("upstream stream = %v, want true (gate)", gotBody["stream"])
	}
	if gotBody["max_output_tokens"] != float64(64) {
		t.Errorf("upstream max_output_tokens = %v, want 64", gotBody["max_output_tokens"])
	}
	tools, _ := gotBody["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		tool, _ := tl.(map[string]any)
		if name, _ := tool["name"].(string); name != "" {
			names[name] = true
		}
	}
	if !names["bash"] || !names["read"] {
		t.Errorf("upstream tools = %s, want bash and read (gate)", mustJSON(t, tools))
	}
	if gotUA == "" {
		t.Error("harness identity missing: the OpenCode User-Agent must be sent")
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (folded stream for a non-stream client)", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("body not JSON: %v; body = %s", err, raw)
	}
	if msg["type"] != "message" {
		t.Errorf("response = %s, want an Anthropic message", raw)
	}
	content, _ := msg["content"].([]any)
	if part, _ := content[0].(map[string]any); part["text"] != "pong" {
		t.Errorf("content = %s, want pong", raw)
	}
	usage, _ := msg["usage"].(map[string]any)
	if usage["input_tokens"] != 4.0 || usage["output_tokens"] != 2.0 {
		t.Errorf("usage = %s, want input 4 output 2", mustJSON(t, usage))
	}
}

// A streaming client keeps the upstream stream as Anthropic SSE.
func TestSendResponses_StreamClientGetsAnthropicSSE(t *testing.T) {
	var gotAccept string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join([]string{
			`data: {"type":"response.created","response":{"id":"resp_2"}}`,
			``,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
			``,
			`data: {"type":"response.output_text.delta","output_index":0,"delta":"streamy"}`,
			``,
			`data: {"type":"response.completed","response":{"id":"resp_2","usage":{"input_tokens":1,"output_tokens":1}}}`,
			``,
		}, "\n"))
	}))
	defer upstream.Close()

	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"ping"}],"max_tokens":64,"stream":true}`)
	resp, err := SendResponses(context.Background(), upstream.Client(), upstream.URL, "sk-zen-test", body)
	if err != nil {
		t.Fatalf("SendResponses: %v", err)
	}
	defer resp.Body.Close()

	if gotAccept != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream for a streaming client", gotAccept)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	raw, _ := io.ReadAll(resp.Body)
	for _, want := range []string{"event: message_start", `"type":"text_delta"`, "event: message_stop"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("SSE missing %s:\n%s", want, raw)
		}
	}
	if !strings.HasPrefix(string(raw), "event: message_start") {
		t.Errorf("message_start must be the first event:\n%s", raw)
	}
}

// ForwardResponses copies the translated response to w, running modify first.
func TestForwardResponses_CopiesTranslatedBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_3","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"ping"}],"max_tokens":32}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	modified := false
	ForwardResponses(rec, req, upstream.URL, "sk-zen-test", body, func(resp *http.Response) error {
		modified = true
		if resp.StatusCode != 200 {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		return nil
	})

	if !modified {
		t.Error("modify hook was not called")
	}
	if rec.Code != 200 {
		t.Fatalf("client status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"text":"ok"`) {
		t.Errorf("body = %s, want the translated message", rec.Body.String())
	}
}

// An upstream connection failure is a 502 with an api_error envelope, matching
// the rest of the proxy.
func TestForwardResponses_UpstreamFailureReturns502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := upstream.URL
	upstream.Close()

	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"ping"}],"max_tokens":32}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	ForwardResponses(rec, req, deadURL, "sk-zen-test", body, nil)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"api_error"`) {
		t.Errorf("body = %s, want an api_error envelope", rec.Body.String())
	}
}

// A gate rejection reaches the client as an Anthropic error envelope with the
// upstream status preserved, and modify still sees it so the caller can
// observe the gate.
func TestForwardResponses_GateRejectionIsAnAnthropicError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"type":"FreeTierError","message":"a newer version is required to use the free tier"}}`)
	}))
	defer upstream.Close()

	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"ping"}],"max_tokens":32}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	observed := 0
	ForwardResponses(rec, req, upstream.URL, "sk-zen-test", body, func(resp *http.Response) error {
		observed++
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("modify saw status %d, want 403", resp.StatusCode)
		}
		// A gate observer reads the body and restores it, as
		// ObserveFreeTierGate does; the client copy must still see the
		// envelope.
		raw, _ := io.ReadAll(resp.Body)
		if len(raw) == 0 {
			t.Error("modify saw an empty body: the gate reason is unobservable")
		}
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	})

	if observed != 1 {
		t.Fatalf("modify called %d times, want 1", observed)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("client status = %d, want 403 preserved; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"type":"error"`) {
		t.Errorf("body = %s, want an Anthropic error envelope", rec.Body.String())
	}
}

// A tool_use whose input is null must never reach the wire as the string
// "null": json.Marshal(nil) succeeds, so the function_call would carry
// arguments:"null", which is not a JSON object and which the hardened chat
// wire already rewrites to "{}". The Messages API does not let a client send
// "input": null, but a replayed assistant turn can carry one, and the object
// the client gets back must stay inside Anthropic's schema either way.
func TestSendResponses_ToolUseNullInputEncodesEmptyObject(t *testing.T) {
	var gotArgs string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("upstream body not JSON: %v", err)
		}
		if items, ok := got["input"].([]any); ok {
			for _, raw := range items {
				item, _ := raw.(map[string]any)
				if item["type"] != "function_call" {
					continue
				}
				gotArgs, _ = item["arguments"].(string)
			}
		}
		// Echo the call back the way a model replaying the turn would, so the
		// client-side assertion reads a real upstream answer rather than a
		// fixture chosen to pass.
		b, _ := json.Marshal(map[string]any{
			"type":         "response.output_item.done",
			"output_index": 0,
			"item": map[string]any{
				"type": "function_call", "call_id": "call_1",
				"name": "bash", "arguments": gotArgs,
			},
		})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join([]string{
			"data: " + string(b),
			``,
			`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":1,"output_tokens":1}}}`,
			``,
		}, "\n"))
	}))
	defer upstream.Close()
	body := []byte(`{"model":"gpt-5","max_tokens":64,"messages":[` +
		`{"role":"user","content":"run it"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":null}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}],` +
		`"tools":[{"name":"Bash","input_schema":{"type":"object"}}]}`)
	resp, err := SendResponses(context.Background(), upstream.Client(), upstream.URL, "sk-zen-test", body)
	if err != nil {
		t.Fatalf("SendResponses: %v", err)
	}
	defer resp.Body.Close()

	if gotArgs != "{}" {
		t.Errorf("upstream function_call arguments = %q, want {} (a null input must encode as an empty object)", gotArgs)
	}
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), `"input":null`) {
		t.Errorf("client body carries \"input\":null, which is outside Anthropic's object schema: %s", raw)
	}
	var msg map[string]any
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("client body not JSON: %v; body = %s", err, raw)
	}
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %s, want one tool_use block", mustJSON(t, content))
	}
	block, _ := content[0].(map[string]any)
	if block["type"] != "tool_use" || block["name"] != "Bash" {
		t.Fatalf("content[0] = %s, want a tool_use for the client's Bash", mustJSON(t, block))
	}
	input, ok := block["input"].(map[string]any)
	if !ok || len(input) != 0 {
		t.Errorf("tool_use input = %#v, want an empty object", block["input"])
	}
}

// A replayed thinking block must not become a reasoning input item: the
// Responses schema requires reasoning items to carry the id the upstream
// issued, and this translator never has one to give back.
func TestAnthropicToResponsesRequest_DropsReplayedThinking(t *testing.T) {
	out, _, _ := anthropicToResponsesRequest(map[string]any{
		"model": "gpt-5",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "pondering", "signature": ""},
				map[string]any{"type": "text", "text": "hello"},
			}},
			map[string]any{"role": "user", "content": "again"},
		},
	})
	items := responsesInputItems(t, out)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["type"] == "reasoning" {
			t.Fatalf("reasoning item replayed without an id: %s", mustJSON(t, item))
		}
	}
	if len(items) != 3 {
		t.Fatalf("input has %d items, want 3 (user, assistant text, user): %s", len(items), mustJSON(t, items))
	}
}

// Responses function tools default to strict mode when `strict` is omitted,
// which normalises the schema (optional params become required). The Chat
// wire this mirrors was non-strict, so every tool — declared or injected
// for the gate — must say so explicitly.
func TestAnthropicToResponsesRequest_ToolsAreNonStrict(t *testing.T) {
	out, _, _ := anthropicToResponsesRequest(map[string]any{
		"model":    "gpt-5",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{
			map[string]any{"name": "Edit", "input_schema": map[string]any{"type": "object"}},
		},
	})
	tools, _ := out["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("tools = %s, want Edit plus the injected bash and read", mustJSON(t, tools))
	}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if strict, ok := tool["strict"].(bool); !ok || strict {
			t.Errorf("tool %v strict = %v, want an explicit false", tool["name"], tool["strict"])
		}
	}
}

// The aggregator recovers text, reasoning and refusals that only the
// completed item carries; the stream must too, or a streaming client gets an
// empty turn (or, for a refusal, a clean end_turn).
func TestStreamResponsesToAnthropic_EmitsContentCarriedOnlyByDoneItem(t *testing.T) {
	for _, tc := range []struct {
		name      string
		item      string
		wantDelta string
		wantStop  string
	}{
		{"text", `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello there"}]}`, `"text":"hello there"`, "end_turn"},
		{"refusal", `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"refusal","refusal":"no can do"}]}`, `"text":"no can do"`, "refusal"},
		{"reasoning", `{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"weighing it"}]}`, `"thinking":"weighing it"`, "end_turn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sse := strings.Join([]string{
				`data: {"type":"response.created","response":{"id":"resp_d"}}`,
				``,
				`data: {"type":"response.output_item.done","output_index":0,"item":` + tc.item + `}`,
				``,
				`data: {"type":"response.completed","response":{"id":"resp_d","usage":{"input_tokens":3,"output_tokens":1}}}`,
				``,
			}, "\n")
			var out bytes.Buffer
			if err := streamResponsesToAnthropic(strings.NewReader(sse), &out, "gpt-5", nil, nil); err != nil {
				t.Fatalf("streamResponsesToAnthropic: %v", err)
			}
			got := out.String()
			if !strings.Contains(got, tc.wantDelta) {
				t.Errorf("done-only content missing %s:\n%s", tc.wantDelta, got)
			}
			if !strings.Contains(got, `"stop_reason":"`+tc.wantStop+`"`) {
				t.Errorf("stop_reason want %s:\n%s", tc.wantStop, got)
			}
		})
	}
}

// Content already streamed as deltas must not be repeated when the completed
// item restates it, and a completed item that extends the deltas contributes
// only the missing suffix: the client concatenates text_delta frames.
func TestStreamResponsesToAnthropic_DoesNotRepeatDeliveredText(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_e"}}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"hel"}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"lo"}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello world"}]}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_e","usage":{"input_tokens":3,"output_tokens":1}}}`,
		``,
	}, "\n")
	var out bytes.Buffer
	if err := streamResponsesToAnthropic(strings.NewReader(sse), &out, "gpt-5", nil, nil); err != nil {
		t.Fatalf("streamResponsesToAnthropic: %v", err)
	}
	var text strings.Builder
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, `"text_delta"`) {
			continue
		}
		var ev struct {
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		text.WriteString(ev.Delta.Text)
	}
	if text.String() != "hello world" {
		t.Errorf("client text = %q, want %q", text.String(), "hello world")
	}
}

// A stream that carries no deltas and no item envelopes, but whose
// response.completed restates the answer in response.output, must still
// stream the text instead of an empty end_turn.
func TestStreamResponsesToAnthropic_CompletedOutputFallback(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_f"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_f","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello world"}]}],"usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")
	var out bytes.Buffer
	if err := streamResponsesToAnthropic(strings.NewReader(sse), &out, "gpt-5", nil, nil); err != nil {
		t.Fatalf("streamResponsesToAnthropic: %v", err)
	}
	var text strings.Builder
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, `"text_delta"`) {
			continue
		}
		var ev struct {
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		text.WriteString(ev.Delta.Text)
	}
	if text.String() != "hello world" {
		t.Errorf("client text = %q, want %q", text.String(), "hello world")
	}
}

// A function call that arrives only in response.completed's output still
// surfaces as a tool_use block in the aggregate path.
func TestAggregateResponsesStream_CompletedOutputFallbackFunctionCall(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_fc"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_fc","output":[{"type":"function_call","name":"get_weather","call_id":"call_0","arguments":"{\"city\":\"Sampa\"}"}],"usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")
	agg, err := aggregateResponsesStream(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("aggregateResponsesStream: %v", err)
	}
	msg := ResponsesResponseToAnthropic(agg, "gpt-5", nil, nil)
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %s, want one tool_use", mustJSON(t, msg["content"]))
	}
	got, _ := content[0].(map[string]any)
	if got["type"] != "tool_use" || got["name"] != "get_weather" {
		t.Errorf("content[0] = %s, want get_weather tool_use", mustJSON(t, content[0]))
	}
	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", msg["stop_reason"])
	}
}

// A function call that arrives only in response.completed's output still
// streams as a tool_use block.
func TestStreamResponsesToAnthropic_CompletedOutputFallbackFunctionCall(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_fc"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_fc","output":[{"type":"function_call","name":"get_weather","call_id":"call_0","arguments":"{\"city\":\"Sampa\"}"}],"usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")
	var out bytes.Buffer
	if err := streamResponsesToAnthropic(strings.NewReader(sse), &out, "gpt-5", nil, nil); err != nil {
		t.Fatalf("streamResponsesToAnthropic: %v", err)
	}
	if !strings.Contains(out.String(), `"tool_use"`) || !strings.Contains(out.String(), "get_weather") {
		t.Errorf("stream missing tool_use get_weather:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"stop_reason":"tool_use"`) {
		t.Errorf("stream missing tool_use stop:\n%s", out.String())
	}
}

// A completed-output call to a gate-injected tool stays dropped.
func TestStreamResponsesToAnthropic_CompletedOutputFallbackDropsInjectedCall(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_inj"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_inj","output":[{"type":"function_call","name":"gate_secret","call_id":"call_0","arguments":"{}"}],"usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")
	var out bytes.Buffer
	if err := streamResponsesToAnthropic(strings.NewReader(sse), &out, "gpt-5", nil, map[string]bool{"gate_secret": true}); err != nil {
		t.Fatalf("streamResponsesToAnthropic: %v", err)
	}
	if strings.Contains(out.String(), "gate_secret") {
		t.Errorf("stream leaked gate-injected call:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"stop_reason":"end_turn"`) {
		t.Errorf("expected end_turn after drop:\n%s", out.String())
	}
}

// Deltas plus an identical completed-output restatement stream exactly once.
func TestStreamResponsesToAnthropic_CompletedOutputNoDuplicate(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_d"}}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"hello world"}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello world"}]}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_d","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello world"}]}],"usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")
	var out bytes.Buffer
	if err := streamResponsesToAnthropic(strings.NewReader(sse), &out, "gpt-5", nil, nil); err != nil {
		t.Fatalf("streamResponsesToAnthropic: %v", err)
	}
	var text strings.Builder
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, `"text_delta"`) {
			continue
		}
		var ev struct {
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		text.WriteString(ev.Delta.Text)
	}
	if text.String() != "hello world" {
		t.Errorf("client text = %q, want exactly one delivery", text.String())
	}
}

// A done-only multi-part reasoning summary streams joined with the same
// separator as the aggregate and JSON paths.
func TestStreamResponsesToAnthropic_ReasoningMultiPartSeparator(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_r"}}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"first"},{"type":"summary_text","text":"second"}]}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_r","usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")
	var out bytes.Buffer
	if err := streamResponsesToAnthropic(strings.NewReader(sse), &out, "gpt-5", nil, nil); err != nil {
		t.Fatalf("streamResponsesToAnthropic: %v", err)
	}
	var thinking strings.Builder
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, `"thinking_delta"`) {
			continue
		}
		var ev struct {
			Delta struct {
				Thinking string `json:"thinking"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		thinking.WriteString(ev.Delta.Thinking)
	}
	if thinking.String() != "first\n\nsecond" {
		t.Errorf("client thinking = %q, want %q", thinking.String(), "first\n\nsecond")
	}
}

// Deltas carry no part boundaries, so a restatement that extends partial
// deltas stays fused: the suffix must still reach the client, not be dropped.
func TestStreamResponsesToAnthropic_ReasoningPartialDeltasPreserved(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_r"}}`,
		``,
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"first"}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"first"},{"type":"summary_text","text":"second"}]}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_r","usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")
	var out bytes.Buffer
	if err := streamResponsesToAnthropic(strings.NewReader(sse), &out, "gpt-5", nil, nil); err != nil {
		t.Fatalf("streamResponsesToAnthropic: %v", err)
	}
	var thinking strings.Builder
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, `"thinking_delta"`) {
			continue
		}
		var ev struct {
			Delta struct {
				Thinking string `json:"thinking"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		thinking.WriteString(ev.Delta.Thinking)
	}
	if thinking.String() != "firstsecond" {
		t.Errorf("client thinking = %q, want fused %q (no loss)", thinking.String(), "firstsecond")
	}
}

// anthropicEvents decodes the data: frames of an Anthropic SSE stream in order.
func anthropicEvents(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(raw, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		events = append(events, ev)
	}
	return events
}

// streamStopReason returns the stop_reason the streaming emitter reports.
func streamStopReason(t *testing.T, upstream string) string {
	t.Helper()
	var out bytes.Buffer
	if err := streamResponsesToAnthropic(strings.NewReader(upstream), &out, "gpt-5", nil, nil); err != nil {
		t.Fatalf("streamResponsesToAnthropic: %v", err)
	}
	for _, ev := range anthropicEvents(t, out.String()) {
		if ev["type"] == "message_delta" {
			delta, _ := ev["delta"].(map[string]any)
			reason, _ := delta["stop_reason"].(string)
			return reason
		}
	}
	t.Fatalf("no message_delta in stream:\n%s", out.String())
	return ""
}

// aggregateStopReason returns the stop_reason a non-streaming client gets for
// the same upstream events.
func aggregateStopReason(t *testing.T, upstream string) string {
	t.Helper()
	agg, err := aggregateResponsesStream(strings.NewReader(upstream))
	if err != nil {
		t.Fatalf("aggregateResponsesStream: %v", err)
	}
	reason, _ := ResponsesResponseToAnthropic(agg, "gpt-5", nil, nil)["stop_reason"].(string)
	return reason
}

// Both directions translate the same upstream events, so they must agree on
// why the turn stopped; a refusal outranks every other reason.
func TestResponsesStopReasonAgreesAcrossDirections(t *testing.T) {
	const (
		created   = `data: {"type":"response.created","response":{"id":"resp_p"}}`
		message   = `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`
		text      = `data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}`
		refusal   = `data: {"type":"response.refusal.delta","output_index":0,"delta":"no"}`
		call      = `data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"bash","arguments":""}}`
		callArgs  = `data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{}"}`
		completed = `data: {"type":"response.completed","response":{"id":"resp_p"}}`
		truncated = `data: {"type":"response.incomplete","response":{"id":"resp_p","incomplete_details":{"reason":"max_output_tokens"}}}`
		filtered  = `data: {"type":"response.incomplete","response":{"id":"resp_p","incomplete_details":{"reason":"content_filter"}}}`
	)
	for _, tc := range []struct {
		name   string
		events []string
		want   string
	}{
		{"plain text", []string{created, message, text, completed}, "end_turn"},
		{"output cap", []string{created, message, text, truncated}, "max_tokens"},
		{"content filter", []string{created, message, text, filtered}, "refusal"},
		{"refusal part", []string{created, message, refusal, completed}, "refusal"},
		{"refusal then output cap", []string{created, message, refusal, truncated}, "refusal"},
		{"tool call", []string{created, call, callArgs, completed}, "tool_use"},
		{"tool call cut by output cap", []string{created, call, callArgs, truncated}, "max_tokens"},
		{"refusal beside a tool call", []string{created, message, refusal, call, callArgs, completed}, "refusal"},
		{"content filter beside a tool call", []string{created, call, callArgs, filtered}, "refusal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := strings.Join(tc.events, "\n\n") + "\n\n"
			if got := streamStopReason(t, upstream); got != tc.want {
				t.Errorf("stream stop_reason = %q, want %q", got, tc.want)
			}
			if got := aggregateStopReason(t, upstream); got != tc.want {
				t.Errorf("aggregate stop_reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// OpenAI documents temperature and top_p as unsupported for reasoning models
// unless effort is none, and the reference OpenCode client sends neither for
// any Responses-wire id, so the body must not carry them.
func TestAnthropicToResponsesRequest_DropsSamplingParams(t *testing.T) {
	out, _, _ := anthropicToResponsesRequest(map[string]any{
		"model":       "gpt-6-astra",
		"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
		"temperature": 1.0,
		"top_p":       0.9,
	})
	for _, key := range []string{"temperature", "top_p"} {
		if v, ok := out[key]; ok {
			t.Errorf("%s = %v reached the Responses body; it must be dropped", key, v)
		}
	}
}

// An upstream function_call with no call_id still needs a unique tool_use id:
// two parallel calls to the same tool would otherwise share one, and the client
// could not match either result to its call. The stream path already numbers by
// output index; the JSON path must not fall back to the tool name.
func TestResponsesResponseToAnthropic_FallbackCallIDsAreUnique(t *testing.T) {
	out := ResponsesResponseToAnthropic(map[string]any{"output": []any{
		map[string]any{"type": "function_call", "name": "read", "arguments": `{"filePath":"/a"}`},
		map[string]any{"type": "function_call", "name": "read", "arguments": `{"filePath":"/b"}`},
	}}, "gpt-5", nil, nil)

	seen := map[string]bool{}
	for _, raw := range anySlice(out["content"]) {
		block, _ := raw.(map[string]any)
		id, _ := block["id"].(string)
		if id == "" || seen[id] {
			t.Errorf("tool_use id %q is empty or repeated: %s", id, mustJSON(t, out["content"]))
		}
		seen[id] = true
	}
	if len(seen) != 2 {
		t.Errorf("tool_use blocks = %d, want 2: %s", len(seen), mustJSON(t, out["content"]))
	}
}

// A user turn carries tool results, text and images: results lead as
// function_call_output items (an error result is prefixed, as on the Chat
// wire), and the remaining parts become one user message whose images are
// input_image items holding a data URL or the original URL.
func TestAnthropicToResponsesRequest_UserTurnBlocks(t *testing.T) {
	out, _, _ := anthropicToResponsesRequest(map[string]any{
		"model": "gpt-5",
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "is_error": true, "content": "boom"},
			map[string]any{"type": "text", "text": "look"},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "QUJD"}},
			map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.test/a.png"}},
		}}},
	})

	items := responsesInputItems(t, out)
	if len(items) != 2 {
		t.Fatalf("input has %d items, want function_call_output then user message: %s", len(items), mustJSON(t, items))
	}
	result, _ := items[0].(map[string]any)
	if result["type"] != "function_call_output" || result["call_id"] != "toolu_1" || result["output"] != "Error: boom" {
		t.Errorf("tool result item = %s, want function_call_output toolu_1 with an Error: prefix", mustJSON(t, result))
	}
	user, _ := items[1].(map[string]any)
	want := `[{"text":"look","type":"input_text"},` +
		`{"image_url":"data:image/png;base64,QUJD","type":"input_image"},` +
		`{"image_url":"https://example.test/a.png","type":"input_image"}]`
	if got := mustJSON(t, user["content"]); got != want {
		t.Errorf("user content = %s, want %s", got, want)
	}
}

// Parallel tool calls arrive as consecutive function_call items. Each must
// become its own tool_use block carrying exactly its own arguments, in order.
func TestStreamResponsesToAnthropic_ParallelToolCalls(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_pc"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_a","name":"bash","arguments":""}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"command\":"}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"ls\"}"}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_a","name":"bash","arguments":"{\"command\":\"ls\"}"}}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_2","type":"function_call","call_id":"call_b","name":"read","arguments":""}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"filePath\":\"/a\"}"}`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"fc_2","type":"function_call","call_id":"call_b","name":"read","arguments":"{\"filePath\":\"/a\"}"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_pc"}}`,
	}, "\n\n") + "\n\n"

	var out bytes.Buffer
	if err := streamResponsesToAnthropic(strings.NewReader(upstream), &out, "gpt-5", map[string]string{"bash": "Bash"}, nil); err != nil {
		t.Fatalf("streamResponsesToAnthropic: %v", err)
	}

	type block struct{ id, name, args string }
	blocks := map[int]*block{}
	var order []int
	for _, ev := range anthropicEvents(t, out.String()) {
		idx := toInt(ev["index"])
		switch ev["type"] {
		case "content_block_start":
			cb, _ := ev["content_block"].(map[string]any)
			b := &block{}
			b.id, _ = cb["id"].(string)
			b.name, _ = cb["name"].(string)
			blocks[idx] = b
			order = append(order, idx)
		case "content_block_delta":
			delta, _ := ev["delta"].(map[string]any)
			part, _ := delta["partial_json"].(string)
			blocks[idx].args += part
		}
	}
	if len(order) != 2 {
		t.Fatalf("tool_use blocks = %d, want 2:\n%s", len(order), out.String())
	}
	for i, want := range []block{
		{"call_a", "Bash", `{"command":"ls"}`},
		{"call_b", "read", `{"filePath":"/a"}`},
	} {
		if got := *blocks[order[i]]; got != want {
			t.Errorf("block %d = %+v, want %+v", i, got, want)
		}
	}
	if !strings.Contains(out.String(), `"stop_reason":"tool_use"`) {
		t.Errorf("stop_reason tool_use missing:\n%s", out.String())
	}
}
