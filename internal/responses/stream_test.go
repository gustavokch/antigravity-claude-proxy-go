package responses

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A streaming client receives the Anthropic SSE event sequence: text as
// text_delta, a tool call as a tool_use block with input_json_delta, and a
// final message_delta carrying the derived stop_reason and Anthropic usage.
func TestStream_EmitsAnthropicEvents(t *testing.T) {
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
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5", ToolNames: map[string]string{"bash": "Bash"}}); err != nil {
		t.Fatalf("Stream: %v", err)
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
func TestStream_DropsInjectedToolCalls(t *testing.T) {
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
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5", Drop: map[string]bool{"read": true}}); err != nil {
		t.Fatalf("Stream: %v", err)
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
func TestStream_EmitsArgumentsFromCompletedItem(t *testing.T) {
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
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
		t.Fatalf("Stream: %v", err)
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
func TestStream_DoesNotRepeatDeliveredArguments(t *testing.T) {
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
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := out.String()
	if n := strings.Count(got, `input_json_delta`); n != 2 {
		t.Errorf("input_json_delta frames = %d, want 2 (the two streamed fragments only):\n%s", n, got)
	}
}

// A stream that fails mid-flight reports an Anthropic error event instead of
// a clean stop, so the client does not treat a partial answer as complete.
func TestStream_FailedStreamEmitsError(t *testing.T) {
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
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "event: error") || !strings.Contains(got, "upstream exploded") {
		t.Errorf("failed stream must emit an error event carrying the reason:\n%s", got)
	}
	if strings.Contains(got, "event: message_stop") {
		t.Errorf("a failed stream must not emit message_stop:\n%s", got)
	}
}

// A streamed refusal reports stop_reason refusal, never end_turn, and its text
// reaches the client as text_delta. The two signals are exercised separately:
// a refusal delta on its own, and incomplete_details content_filter on a
// stream that carried ordinary text.
func TestStream_RefusalStopReason(t *testing.T) {
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
		if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
			t.Fatalf("Stream: %v", err)
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
		if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
			t.Fatalf("Stream: %v", err)
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

// Anthropic SSE requires message_start to be the first event on the wire: a
// client cannot attribute a content block to a message that has not opened.
// A Responses stream that opens with something other than
// response.created — a delta, or the response.in_progress the API also
// sends — must still emit it first.
func TestStream_MessageStartPrecedesContentBlocks(t *testing.T) {
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
			if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
				t.Fatalf("Stream: %v", err)
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

// The aggregator recovers text, reasoning and refusals that only the
// completed item carries; the stream must too, or a streaming client gets an
// empty turn (or, for a refusal, a clean end_turn).
func TestStream_EmitsContentCarriedOnlyByDoneItem(t *testing.T) {
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
			if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
				t.Fatalf("Stream: %v", err)
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
func TestStream_DoesNotRepeatDeliveredText(t *testing.T) {
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
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
		t.Fatalf("Stream: %v", err)
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
func TestStream_CompletedOutputFallback(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_f"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_f","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello world"}]}],"usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")
	var out bytes.Buffer
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	text := streamedDeltas(t, out.String(), "text_delta", "text")
	if text != "hello world" {
		t.Errorf("client text = %q, want %q", text, "hello world")
	}
}

// A function call that arrives only in response.completed's output still
// streams as a tool_use block.
func TestStream_CompletedOutputFallbackFunctionCall(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_fc"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_fc","output":[{"type":"function_call","name":"get_weather","call_id":"call_0","arguments":"{\"city\":\"Sampa\"}"}],"usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")
	var out bytes.Buffer
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if !strings.Contains(out.String(), `"tool_use"`) || !strings.Contains(out.String(), "get_weather") {
		t.Errorf("stream missing tool_use get_weather:\n%s", out.String())
	}
	if got := streamedDeltas(t, out.String(), "input_json_delta", "partial_json"); got != `{"city":"Sampa"}` {
		t.Errorf("tool arguments = %q, want %q", got, `{"city":"Sampa"}`)
	}
	if !strings.Contains(out.String(), `"stop_reason":"tool_use"`) {
		t.Errorf("stream missing tool_use stop:\n%s", out.String())
	}
}

// A completed-output call to a gate-injected tool stays dropped.
func TestStream_CompletedOutputFallbackDropsInjectedCall(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_inj"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_inj","output":[{"type":"function_call","name":"gate_secret","call_id":"call_0","arguments":"{}"}],"usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")
	var out bytes.Buffer
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5", Drop: map[string]bool{"gate_secret": true}}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if strings.Contains(out.String(), "gate_secret") {
		t.Errorf("stream leaked gate-injected call:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"stop_reason":"end_turn"`) {
		t.Errorf("expected end_turn after drop:\n%s", out.String())
	}
}

// Deltas plus an identical completed-output restatement stream exactly once.
func TestStream_CompletedOutputNoDuplicate(t *testing.T) {
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
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	text := streamedDeltas(t, out.String(), "text_delta", "text")
	if text != "hello world" {
		t.Errorf("client text = %q, want exactly one delivery", text)
	}
}

// A done-only multi-part reasoning summary streams joined with the same
// separator as the aggregate and JSON paths.
func TestStream_ReasoningMultiPartSeparator(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_r"}}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"first"},{"type":"summary_text","text":"second"}]}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_r","usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")
	var out bytes.Buffer
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	thinking := streamedDeltas(t, out.String(), "thinking_delta", "thinking")
	if thinking != "first\n\nsecond" {
		t.Errorf("client thinking = %q, want %q", thinking, "first\n\nsecond")
	}
}

// A restatement that extends partial deltas must still deliver the rest, with
// the part separator, instead of dropping it or gluing the parts together.
func TestStream_ReasoningPartialDeltasPreserved(t *testing.T) {
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
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	thinking := streamedDeltas(t, out.String(), "thinking_delta", "thinking")
	if thinking != "first\n\nsecond" {
		t.Errorf("client thinking = %q, want %q (no loss, parts separated)", thinking, "first\n\nsecond")
	}
}

// OpenAI tags every reasoning_summary_text.delta with the summary part it
// belongs to. Parts are separate blocks upstream, so a part change in the live
// deltas must read like the joined restatement, not glue words together, and
// the restatement that follows must add nothing.
func TestStream_ReasoningSummaryIndexSeparator(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_r"}}`,
		``,
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"fir"}`,
		``,
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"st"}`,
		``,
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":1,"delta":"second"}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"first"},{"type":"summary_text","text":"second"}]}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_r","usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")
	var out bytes.Buffer
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	thinking := streamedDeltas(t, out.String(), "thinking_delta", "thinking")
	if thinking != "first\n\nsecond" {
		t.Errorf("client thinking = %q, want %q", thinking, "first\n\nsecond")
	}
}

// Without summary_index nothing marks a part boundary, so deltas arrive fused.
// The restatement must then neither repeat what was streamed nor drop the rest.
func TestStream_ReasoningUntaggedDeltasNoLossNoRepeat(t *testing.T) {
	delta := func(text string) string {
		return `data: {"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"` + text + `"}` + "\n"
	}
	cases := []struct{ name, deltas, want string }{
		{"fully streamed", delta("first") + delta("second"), "firstsecond"},
		{"cut inside the second part", delta("firstsec"), "firstsecond"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sse := `data: {"type":"response.created","response":{"id":"resp_r"}}` + "\n" + tc.deltas +
				`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"first"},{"type":"summary_text","text":"second"}]}}` + "\n" +
				`data: {"type":"response.completed","response":{"id":"resp_r","usage":{"input_tokens":5,"output_tokens":2}}}` + "\n"
			var out bytes.Buffer
			if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
				t.Fatalf("Stream: %v", err)
			}
			if got := streamedDeltas(t, out.String(), "thinking_delta", "thinking"); got != tc.want {
				t.Errorf("client thinking = %q, want %q", got, tc.want)
			}
		})
	}
}

// streamedDeltas concatenates one field of every content_block_delta of the
// given delta type ("text_delta"/"text", "thinking_delta"/"thinking",
// "input_json_delta"/"partial_json") across an Anthropic SSE transcript: what
// the client would assemble.
func streamedDeltas(t *testing.T, raw, deltaType, field string) string {
	t.Helper()
	var b strings.Builder
	for _, ev := range anthropicEvents(t, raw) {
		if ev["type"] != "content_block_delta" {
			continue
		}
		delta, _ := ev["delta"].(map[string]any)
		if delta["type"] != deltaType {
			continue
		}
		part, _ := delta[field].(string)
		b.WriteString(part)
	}
	return b.String()
}

// A function_call delivered only in output_item.done — no output_item.added and
// no restatement in response.completed — must still reach the client: dropping
// it would end the agent loop on a clean end_turn.
func TestStream_FunctionCallOnlyInItemDone(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_o"}}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","name":"get_weather","call_id":"call_0","arguments":"{\"city\":\"Sampa\"}"}}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_o","usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")
	var out bytes.Buffer
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := streamedDeltas(t, out.String(), "input_json_delta", "partial_json"); got != `{"city":"Sampa"}` {
		t.Errorf("tool arguments = %q, want %q", got, `{"city":"Sampa"}`)
	}
	if stop := streamStopReason(t, sse); stop != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", stop)
	}
}

// A fully streamed call that response.completed restates must not open a second
// tool_use block or repeat its arguments: a duplicated call would run twice.
func TestStream_CompletedOutputNoDuplicateCall(t *testing.T) {
	call := `{"type":"function_call","name":"get_weather","call_id":"call_0","arguments":"{\"city\":\"Sampa\"}"}`
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_d"}}`,
		``,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","name":"get_weather","call_id":"call_0","arguments":""}}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"city\":"}`,
		``,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"Sampa\"}"}`,
		``,
		`data: {"type":"response.output_item.done","output_index":0,"item":` + call + `}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_d","output":[` + call + `],"usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")
	var out bytes.Buffer
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	starts := 0
	for _, ev := range anthropicEvents(t, out.String()) {
		if block, _ := ev["content_block"].(map[string]any); ev["type"] == "content_block_start" && block["type"] == "tool_use" {
			starts++
		}
	}
	if starts != 1 {
		t.Errorf("tool_use blocks = %d, want 1", starts)
	}
	if got := streamedDeltas(t, out.String(), "input_json_delta", "partial_json"); got != `{"city":"Sampa"}` {
		t.Errorf("tool arguments = %q, want %q delivered once", got, `{"city":"Sampa"}`)
	}
}

// Some items stream live while others appear only in response.completed: each
// is delivered exactly once. Block order is not asserted; a late item can only
// land after what already streamed.
func TestStream_CompletedOutputMixedDeliveredOnce(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_m"}}`,
		``,
		`data: {"type":"response.output_text.delta","output_index":1,"delta":"hello"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"resp_m","output":[` +
			`{"type":"reasoning","summary":[{"type":"summary_text","text":"pondering"}]},` +
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},` +
			`{"type":"function_call","name":"get_weather","call_id":"call_2","arguments":"{\"city\":\"Sampa\"}"}` +
			`],"usage":{"input_tokens":5,"output_tokens":2}}}`,
		``,
	}, "\n")
	var out bytes.Buffer
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	raw := out.String()
	if got := streamedDeltas(t, raw, "text_delta", "text"); got != "hello" {
		t.Errorf("client text = %q, want %q once", got, "hello")
	}
	if got := streamedDeltas(t, raw, "thinking_delta", "thinking"); got != "pondering" {
		t.Errorf("client thinking = %q, want %q once", got, "pondering")
	}
	if got := streamedDeltas(t, raw, "input_json_delta", "partial_json"); got != `{"city":"Sampa"}` {
		t.Errorf("tool arguments = %q, want %q once", got, `{"city":"Sampa"}`)
	}
	if stop := streamStopReason(t, sse); stop != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", stop)
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
	if err := Stream(strings.NewReader(upstream), &out, Options{Model: "gpt-5"}); err != nil {
		t.Fatalf("Stream: %v", err)
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
	agg, err := Aggregate(strings.NewReader(upstream))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	reason, _ := ToAnthropic(agg, Options{Model: "gpt-5"})["stop_reason"].(string)
	return reason
}

// Both directions translate the same upstream events, so they must agree on
// why the turn stopped; a refusal outranks every other reason.
func TestStopReasonAgreesAcrossDirections(t *testing.T) {
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

// Parallel tool calls arrive as consecutive function_call items. Each must
// become its own tool_use block carrying exactly its own arguments, in order.
func TestStream_ParallelToolCalls(t *testing.T) {
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
	if err := Stream(strings.NewReader(upstream), &out, Options{Model: "gpt-5", ToolNames: map[string]string{"bash": "Bash"}}); err != nil {
		t.Fatalf("Stream: %v", err)
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

// blockEvents renders the content-block events of an Anthropic SSE transcript
// as "start <block type>@i", "delta <delta type>@i" and "stop@i": the sequence
// a client assembles its content blocks from.
func blockEvents(t *testing.T, raw string) []string {
	t.Helper()
	var seq []string
	for _, ev := range anthropicEvents(t, raw) {
		i := toInt(ev["index"])
		switch ev["type"] {
		case "content_block_start":
			block, _ := ev["content_block"].(map[string]any)
			seq = append(seq, fmt.Sprintf("start %v@%d", block["type"], i))
		case "content_block_delta":
			delta, _ := ev["delta"].(map[string]any)
			seq = append(seq, fmt.Sprintf("delta %v@%d", delta["type"], i))
		case "content_block_stop":
			seq = append(seq, fmt.Sprintf("stop@%d", i))
		}
	}
	return seq
}

// blockStart returns the content_block a content_block_start opened at index.
func blockStart(t *testing.T, raw string, index int) map[string]any {
	t.Helper()
	for _, ev := range anthropicEvents(t, raw) {
		if ev["type"] == "content_block_start" && toInt(ev["index"]) == index {
			block, _ := ev["content_block"].(map[string]any)
			return block
		}
	}
	t.Fatalf("no content_block_start at %d:\n%s", index, raw)
	return nil
}

// blockArgs concatenates the input_json_delta fragments of one block.
func blockArgs(t *testing.T, raw string, index int) string {
	t.Helper()
	var b strings.Builder
	for _, ev := range anthropicEvents(t, raw) {
		if ev["type"] != "content_block_delta" || toInt(ev["index"]) != index {
			continue
		}
		delta, _ := ev["delta"].(map[string]any)
		if part, ok := delta["partial_json"].(string); ok {
			b.WriteString(part)
		}
	}
	return b.String()
}

func assertBlockEvents(t *testing.T, raw string, want []string) {
	t.Helper()
	if got := blockEvents(t, raw); strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Errorf("block events = %v\nwant           %v\n%s", got, want, raw)
	}
}

// A reasoning item's encrypted_content reaches the client as a signature_delta
// right before its thinking block's content_block_stop. The block closes with
// the item, so the answer opens its own block, and the completed response that
// restates the item adds no second signature.
func TestStream_SignatureClosesThinkingBlock(t *testing.T) {
	reasoning := `{"id":"rs_1","type":"reasoning","summary":[{"type":"summary_text","text":"weighing it"}],"encrypted_content":"gAAAAsummary"}`
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_s"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}`,
		`data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`,
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"weighing"}`,
		`data: {"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":" it"}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":` + reasoning + `}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
		`data: {"type":"response.output_text.delta","output_index":1,"delta":"done"}`,
		`data: {"type":"response.completed","response":{"id":"resp_s","output":[` + reasoning + `,{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}}`,
	}, "\n\n") + "\n\n"

	var out bytes.Buffer
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	raw := out.String()
	assertBlockEvents(t, raw, []string{
		"start thinking@0", "delta thinking_delta@0", "delta thinking_delta@0", "delta signature_delta@0", "stop@0",
		"start text@1", "delta text_delta@1", "stop@1",
	})
	if got := streamedDeltas(t, raw, "signature_delta", "signature"); got != "gAAAAsummary" {
		t.Errorf("signature = %q, want the item's encrypted_content once", got)
	}
	if got := streamedDeltas(t, raw, "thinking_delta", "thinking"); got != "weighing it" {
		t.Errorf("thinking = %q, want %q", got, "weighing it")
	}
}

// A reasoning item with no summary text (Codex sends none unless one is
// requested) still delivers its encrypted_content: a thinking block with empty
// text and the signature, whether the item arrives on output_item.done or only
// in the completed response.
func TestStream_SignatureOnlyReasoning(t *testing.T) {
	item := `{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"gAAAAbare"}`
	for _, tc := range []struct {
		name   string
		events []string
	}{
		{"output_item.done", []string{
			`data: {"type":"response.created","response":{"id":"resp_b"}}`,
			`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[]}}`,
			`data: {"type":"response.output_item.done","output_index":0,"item":` + item + `}`,
			`data: {"type":"response.completed","response":{"id":"resp_b"}}`,
		}},
		{"completed output only", []string{
			`data: {"type":"response.created","response":{"id":"resp_b"}}`,
			`data: {"type":"response.completed","response":{"id":"resp_b","output":[` + item + `]}}`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := Stream(strings.NewReader(strings.Join(tc.events, "\n\n")+"\n\n"), &out, Options{Model: "gpt-5"}); err != nil {
				t.Fatalf("Stream: %v", err)
			}
			raw := out.String()
			assertBlockEvents(t, raw, []string{"start thinking@0", "delta signature_delta@0", "stop@0"})
			if got := mustJSON(t, blockStart(t, raw, 0)); got != `{"signature":"","thinking":"","type":"thinking"}` {
				t.Errorf("thinking block = %s, want an empty thinking block", got)
			}
			if got := streamedDeltas(t, raw, "signature_delta", "signature"); got != "gAAAAbare" {
				t.Errorf("signature = %q, want gAAAAbare", got)
			}
		})
	}
}

// Two calls whose argument fragments interleave each get one tool_use block
// with exactly their own arguments: the second block opens only after the
// first closes, and its fragments wait for it instead of being dropped.
func TestStream_InterleavedCallArgumentsAreBuffered(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_i"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_a","type":"function_call","call_id":"call_a","name":"bash","arguments":""}}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"fc_b","type":"function_call","call_id":"call_b","name":"read","arguments":""}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"command\":"}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"filePath\":\"/a\"}"}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"ls\"}"}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_a","type":"function_call","call_id":"call_a","name":"bash","arguments":"{\"command\":\"ls\"}"}}`,
		`data: {"type":"response.output_item.done","output_index":1,"item":{"id":"fc_b","type":"function_call","call_id":"call_b","name":"read","arguments":"{\"filePath\":\"/a\"}"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_i"}}`,
	}, "\n\n") + "\n\n"

	var out bytes.Buffer
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5", ToolNames: map[string]string{"bash": "Bash"}}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	raw := out.String()
	assertBlockEvents(t, raw, []string{
		"start tool_use@0", "delta input_json_delta@0", "delta input_json_delta@0", "stop@0",
		"start tool_use@1", "delta input_json_delta@1", "stop@1",
	})
	for i, want := range []struct{ id, name, args string }{
		{"call_a", "Bash", `{"command":"ls"}`},
		{"call_b", "read", `{"filePath":"/a"}`},
	} {
		block := blockStart(t, raw, i)
		if block["id"] != want.id || block["name"] != want.name {
			t.Errorf("block %d = %s, want id %s name %s", i, mustJSON(t, block), want.id, want.name)
		}
		if got := blockArgs(t, raw, i); got != want.args {
			t.Errorf("block %d arguments = %q, want %q", i, got, want.args)
		}
	}
	if !strings.Contains(raw, `"stop_reason":"tool_use"`) {
		t.Errorf("stop_reason tool_use missing:\n%s", raw)
	}
}

// Text for another item that arrives while a tool_use block is still
// receiving arguments waits until the call is done: opening the text block
// would close the call's block and cut its arguments short.
func TestStream_EventsWaitForOpenToolUse(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_w"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_a","type":"function_call","call_id":"call_a","name":"bash","arguments":""}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"command\":"}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
		`data: {"type":"response.output_text.delta","output_index":1,"delta":"running"}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"ls\"}"}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_a","type":"function_call","call_id":"call_a","name":"bash","arguments":"{\"command\":\"ls\"}"}}`,
		`data: {"type":"response.completed","response":{"id":"resp_w"}}`,
	}, "\n\n") + "\n\n"

	var out bytes.Buffer
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	raw := out.String()
	assertBlockEvents(t, raw, []string{
		"start tool_use@0", "delta input_json_delta@0", "delta input_json_delta@0", "stop@0",
		"start text@1", "delta text_delta@1", "stop@1",
	})
	if got := blockArgs(t, raw, 0); got != `{"command":"ls"}` {
		t.Errorf("arguments = %q, want %q", got, `{"command":"ls"}`)
	}
	if got := streamedDeltas(t, raw, "text_delta", "text"); got != "running" {
		t.Errorf("text = %q, want %q", got, "running")
	}
}

// Argument fragments that arrive before their call's output_item.added are
// held for the call and sent once its block opens.
func TestStream_ArgumentsBeforeEnvelopeAreBuffered(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_e"}}`,
		`data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"command\":\"ls\"}"}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_a","type":"function_call","call_id":"call_a","name":"bash","arguments":""}}`,
		`data: {"type":"response.completed","response":{"id":"resp_e"}}`,
	}, "\n\n") + "\n\n"

	var out bytes.Buffer
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	raw := out.String()
	assertBlockEvents(t, raw, []string{"start tool_use@0", "delta input_json_delta@0", "stop@0"})
	if got := blockArgs(t, raw, 0); got != `{"command":"ls"}` {
		t.Errorf("arguments = %q, want %q", got, `{"command":"ls"}`)
	}
}

// A finished web search streams as Anthropic's server tool pair: a
// server_tool_use block whose query arrives as one input_json_delta, then the
// web_search_tool_result block with the sources. The completed response that
// restates the search adds nothing, and the turn still ends normally.
func TestStream_WebSearchCall(t *testing.T) {
	search := `{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","query":"go 1.27 release","sources":[{"type":"url","url":"https://go.dev/doc/go1.27","title":"Go 1.27 Release Notes"},{"type":"url","url":"https://example.test/a"}]}}`
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_w"}}`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"ws_1","type":"web_search_call","status":"in_progress"}}`,
		`data: {"type":"response.web_search_call.searching","output_index":0,"item_id":"ws_1"}`,
		`data: {"type":"response.output_item.done","output_index":0,"item":` + search + `}`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"id":"msg_1","type":"message","role":"assistant","content":[]}}`,
		`data: {"type":"response.output_text.delta","output_index":1,"delta":"Go 1.27 is out."}`,
		`data: {"type":"response.completed","response":{"id":"resp_w","output":[` + search + `,{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Go 1.27 is out."}]}]}}`,
	}, "\n\n") + "\n\n"

	var out bytes.Buffer
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	raw := out.String()
	assertBlockEvents(t, raw, []string{
		"start server_tool_use@0", "delta input_json_delta@0", "stop@0",
		"start web_search_tool_result@1", "stop@1",
		"start text@2", "delta text_delta@2", "stop@2",
	})
	if got := mustJSON(t, blockStart(t, raw, 0)); got != `{"id":"ws_1","input":{},"name":"web_search","type":"server_tool_use"}` {
		t.Errorf("server_tool_use = %s", got)
	}
	if got := blockArgs(t, raw, 0); got != `{"query":"go 1.27 release"}` {
		t.Errorf("server_tool_use input = %q, want the query", got)
	}
	want := `{"content":[{"page_age":null,"title":"Go 1.27 Release Notes","type":"web_search_result","url":"https://go.dev/doc/go1.27"},` +
		`{"page_age":null,"title":"https://example.test/a","type":"web_search_result","url":"https://example.test/a"}],` +
		`"tool_use_id":"ws_1","type":"web_search_tool_result"}`
	if got := mustJSON(t, blockStart(t, raw, 1)); got != want {
		t.Errorf("web_search_tool_result = %s\nwant                     %s", got, want)
	}
	if stop := streamStopReason(t, sse); stop != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", stop)
	}
}

// The final message_delta reports the turn's usage the Anthropic way: cache
// reads and cache writes each have their own count and are not input_tokens.
func TestStream_UsageReportsCacheWrites(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_u"}}`,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}`,
		`data: {"type":"response.completed","response":{"id":"resp_u","usage":{"input_tokens":100,"output_tokens":7,"input_tokens_details":{"cached_tokens":60,"cache_write_tokens":30}}}}`,
	}, "\n\n") + "\n\n"

	var out bytes.Buffer
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5"}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for _, ev := range anthropicEvents(t, out.String()) {
		if ev["type"] != "message_delta" {
			continue
		}
		want := `{"cache_creation_input_tokens":30,"cache_read_input_tokens":60,"input_tokens":10,"output_tokens":7}`
		if got := mustJSON(t, ev["usage"]); got != want {
			t.Errorf("usage = %s, want %s", got, want)
		}
		return
	}
	t.Fatalf("no message_delta:\n%s", out.String())
}

// response.done is the terminal event's other spelling: a stream that ends
// with it finishes cleanly instead of being reported as truncated.
func TestStream_ResponseDoneIsTerminal(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_d"}}`,
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"hi"}`,
		`data: {"type":"response.done","response":{"id":"resp_d","usage":{"input_tokens":5,"output_tokens":1}}}`,
	}, "\n\n") + "\n\n"

	var out bytes.Buffer
	if err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5", ErrorLabel: "Codex"}); err != nil {
		t.Fatalf("Stream: %v (response.done must end the stream)", err)
	}
	raw := out.String()
	if strings.Contains(raw, "event: error") {
		t.Errorf("response.done reported as an error:\n%s", raw)
	}
	for _, want := range []string{`"stop_reason":"end_turn"`, "event: message_stop"} {
		if !strings.Contains(raw, want) {
			t.Errorf("stream missing %s:\n%s", want, raw)
		}
	}
}

// A stream cut before its terminal event tells the client in-band, naming the
// upstream through ErrorLabel, and returns ErrStreamTruncated for the caller.
func TestStream_TruncationIsReported(t *testing.T) {
	sse := `data: {"type":"response.output_text.delta","output_index":0,"delta":"partial"}` + "\n\n"
	var out bytes.Buffer
	err := Stream(strings.NewReader(sse), &out, Options{Model: "gpt-5", ErrorLabel: "Codex"})
	if !errors.Is(err, ErrStreamTruncated) {
		t.Fatalf("err = %v, want ErrStreamTruncated", err)
	}
	if !strings.Contains(out.String(), "Codex stream ended before completion") {
		t.Errorf("in-band error must name the upstream:\n%s", out.String())
	}
	if strings.Contains(out.String(), "event: message_stop") {
		t.Errorf("a truncated stream must not end cleanly:\n%s", out.String())
	}
}
