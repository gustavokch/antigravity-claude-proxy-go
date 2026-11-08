package responses

import (
	"errors"
	"strings"
	"testing"
)

// A non-streaming client gets the upstream stream folded back into the single
// JSON body a non-streaming call would have returned: text, reasoning, and
// tool-call arguments all reconstructed from the deltas.
func TestAggregate_ReconstructsOutput(t *testing.T) {
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

	agg, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
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
func TestAggregate_TruncatedStreamErrors(t *testing.T) {
	sse := "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"partial\"}\n\n"
	if _, err := Aggregate(strings.NewReader(sse)); err == nil {
		t.Fatal("truncated stream must not aggregate silently")
	}
}

// The gate-injected drop is not a property of the streaming emitter alone: in
// the aggregate direction the same call must not become a tool_use block. With
// the injected check removed this fails on both the content and the stop reason.
func TestAggregate_DropsInjectedToolCalls(t *testing.T) {
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

	agg, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg := ToAnthropic(agg, Options{Model: "gpt-5", Drop: map[string]bool{"read": true}})
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

// A refusal in the aggregate direction survives the fold: the deltas become a
// refusal part the non-streaming path maps, and content_filter still reaches
// the client as stop_reason refusal.
func TestAggregate_RefusalReachesClient(t *testing.T) {
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

	agg, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg := ToAnthropic(agg, Options{Model: "gpt-5"})
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

// A refusal that arrives only in the output_item.done envelope — with no
// refusal delta to stream — must still reach the client as a refusal, not as
// an empty successful end_turn.
func TestAggregate_RefusalOnlyInItemDone(t *testing.T) {
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

	agg, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg := ToAnthropic(agg, Options{Model: "gpt-5"})
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
func TestAggregate_CompletedOutputFallback(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_f"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_f","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello world"}]}],"usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")

	agg, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg := ToAnthropic(agg, Options{Model: "gpt-5"})
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
func TestAggregate_CompletedOutputFallbackIgnoresEmptyPlaceholder(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_p"}}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":""}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_p","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello world"}]}],"usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")
	agg, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg := ToAnthropic(agg, Options{Model: "gpt-5"})
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %s, want fallback text despite empty delta", mustJSON(t, msg["content"]))
	}
	got, _ := content[0].(map[string]any)
	if text, _ := got["text"].(string); !strings.Contains(text, "hello world") {
		t.Errorf("content[0] = %s, want hello world", mustJSON(t, content[0]))
	}
}

// The aggregate fold joins multi-part reasoning summaries with the same
// separator as the JSON translation.
func TestAggregate_ReasoningMultiPartSeparator(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_r"}}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"first"},{"type":"summary_text","text":"second"}]}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_r","usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")

	agg, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
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
func TestAggregate_FunctionCallWithoutItemDone(t *testing.T) {
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

	agg, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg := ToAnthropic(agg, Options{Model: "gpt-5"})
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

// A function call that arrives only in response.completed's output still
// surfaces as a tool_use block in the aggregate path.
func TestAggregate_CompletedOutputFallbackFunctionCall(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_fc"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_fc","output":[{"type":"function_call","name":"get_weather","call_id":"call_0","arguments":"{\"city\":\"Sampa\"}"}],"usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")
	agg, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg := ToAnthropic(agg, Options{Model: "gpt-5"})
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %s, want one tool_use", mustJSON(t, msg["content"]))
	}
	got, _ := content[0].(map[string]any)
	if got["type"] != "tool_use" || got["name"] != "get_weather" {
		t.Errorf("content[0] = %s, want get_weather tool_use", mustJSON(t, content[0]))
	}
	if input, _ := got["input"].(map[string]any); input["city"] != "Sampa" {
		t.Errorf("tool_use input = %s, want city=Sampa: the arguments must survive the fallback", mustJSON(t, got["input"]))
	}
	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", msg["stop_reason"])
	}
}

// The aggregate fold separates tagged parts the same way.
func TestAggregate_ReasoningSummaryIndexSeparator(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_r"}}`,
		``,
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"first"}`,
		``,
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":1,"delta":"second"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_r","usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")
	agg, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg := ToAnthropic(agg, Options{Model: "gpt-5"})
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %s, want one thinking block", mustJSON(t, msg["content"]))
	}
	part, _ := content[0].(map[string]any)
	if thinking, _ := part["thinking"].(string); thinking != "first\n\nsecond" {
		t.Errorf("thinking = %q, want %q", thinking, "first\n\nsecond")
	}
}

// A stream cut before its terminal event reports ErrStreamTruncated, so the
// caller can tell a dropped connection from an upstream failure.
func TestAggregate_TruncationIsErrStreamTruncated(t *testing.T) {
	sse := `data: {"type":"response.output_text.delta","output_index":0,"delta":"partial"}` + "\n\n"
	if _, err := Aggregate(strings.NewReader(sse)); !errors.Is(err, ErrStreamTruncated) {
		t.Errorf("err = %v, want ErrStreamTruncated", err)
	}
	failed := `data: {"type":"response.failed","response":{"error":{"message":"upstream exploded"}}}` + "\n\n"
	if _, err := Aggregate(strings.NewReader(failed)); err == nil || errors.Is(err, ErrStreamTruncated) || err.Error() != "upstream exploded" {
		t.Errorf("err = %v, want the upstream reason, not a truncation", err)
	}
}

// The fold keeps each reasoning item's encrypted_content, including an item
// that carried no summary, so the non-streaming client gets the signatures the
// next turn replays.
func TestAggregate_KeepsReasoningSignature(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_s"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}`,
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"weighing it"}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"weighing it"}],"encrypted_content":"gAAAAsummary"}}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"rs_2","type":"reasoning","summary":[]}}`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"rs_2","type":"reasoning","summary":[],"encrypted_content":"gAAAAbare"}}`,
		`data: {"type":"response.output_text.delta","output_index":2,"delta":"done"}`,
		`data: {"type":"response.completed","response":{"id":"resp_s"}}`,
	}, "\n\n") + "\n\n"

	agg, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg := ToAnthropic(agg, Options{Model: "gpt-5"})
	want := `[{"signature":"gAAAAsummary","thinking":"weighing it","type":"thinking"},` +
		`{"signature":"gAAAAbare","thinking":"","type":"thinking"},` +
		`{"text":"done","type":"text"}]`
	if got := mustJSON(t, msg["content"]); got != want {
		t.Errorf("content = %s, want %s", got, want)
	}
}

// A web search streams no deltas; the fold keeps its completed item whole so
// the non-streaming client gets the same server tool pair as the stream.
func TestAggregate_KeepsWebSearchCall(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_w"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"ws_1","type":"web_search_call","status":"in_progress"}}`,
		`data: {"type":"response.web_search_call.searching","output_index":0,"item_id":"ws_1"}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","query":"go 1.27 release","sources":[{"type":"url","url":"https://go.dev/doc/go1.27","title":"Go 1.27 Release Notes"}]}}}`,
		`data: {"type":"response.output_text.delta","output_index":1,"delta":"Go 1.27 is out."}`,
		`data: {"type":"response.completed","response":{"id":"resp_w"}}`,
	}, "\n\n") + "\n\n"

	agg, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg := ToAnthropic(agg, Options{Model: "gpt-5"})
	want := `[{"id":"ws_1","input":{"query":"go 1.27 release"},"name":"web_search","type":"server_tool_use"},` +
		`{"content":[{"page_age":null,"title":"Go 1.27 Release Notes","type":"web_search_result","url":"https://go.dev/doc/go1.27"}],` +
		`"tool_use_id":"ws_1","type":"web_search_tool_result"},` +
		`{"text":"Go 1.27 is out.","type":"text"}]`
	if got := mustJSON(t, msg["content"]); got != want {
		t.Errorf("content = %s\nwant      %s", got, want)
	}
}

// response.done is the terminal event's other spelling: a stream that ends
// with it is complete, and its usage is the turn's usage.
func TestAggregate_ResponseDoneIsTerminal(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_d"}}`,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}`,
		`data: {"type":"response.done","response":{"id":"resp_d","usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":60,"cache_write_tokens":30}}}}`,
	}, "\n\n") + "\n\n"

	agg, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v (response.done must end the stream)", err)
	}
	msg := ToAnthropic(agg, Options{Model: "gpt-5"})
	if msg["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", msg["stop_reason"])
	}
	want := `{"cache_creation_input_tokens":30,"cache_read_input_tokens":60,"input_tokens":10,"output_tokens":7}`
	if got := mustJSON(t, msg["usage"]); got != want {
		t.Errorf("usage = %s, want %s", got, want)
	}
}

// Argument fragments of two calls that interleave are folded per call, the
// non-streaming counterpart of the stream's buffered tool_use blocks.
func TestAggregate_InterleavedCallArguments(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_i"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_a","type":"function_call","call_id":"call_a","name":"bash","arguments":""}}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_b","type":"function_call","call_id":"call_b","name":"read","arguments":""}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"command\":"}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"filePath\":\"/a\"}"}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"ls\"}"}`,
		`data: {"type":"response.completed","response":{"id":"resp_i"}}`,
	}, "\n\n") + "\n\n"

	agg, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg := ToAnthropic(agg, Options{Model: "gpt-5"})
	want := `[{"id":"call_a","input":{"command":"ls"},"name":"bash","type":"tool_use"},` +
		`{"id":"call_b","input":{"filePath":"/a"},"name":"read","type":"tool_use"}]`
	if got := mustJSON(t, msg["content"]); got != want {
		t.Errorf("content = %s, want %s", got, want)
	}
}
