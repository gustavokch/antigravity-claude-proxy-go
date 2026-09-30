package zen

import (
	"bufio"
	"bytes"
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
		"temperature":    0.3,
		"top_p":          0.9,
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
	if out["temperature"] != 0.3 || out["top_p"] != 0.9 {
		t.Errorf("sampling params = %v/%v, want 0.3/0.9", out["temperature"], out["top_p"])
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
