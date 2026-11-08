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
	part, _ := sysParts[0].(map[string]any)
	text, _ := part["text"].(string)
	if part["type"] != "input_text" || !strings.HasPrefix(text, "be terse") || !hasGateEnv(text) {
		t.Errorf("system part = %v, want input_text starting with the client prompt and carrying the gate env block", sysParts[0])
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

	if gotPath != "/openai/v1/responses" {
		t.Errorf("upstream path = %q, want /openai/v1/responses", gotPath)
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
	if len(items) != 4 {
		t.Fatalf("input has %d items, want 4 (gate system, user, assistant text, user): %s", len(items), mustJSON(t, items))
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
	if len(items) != 3 {
		t.Fatalf("input has %d items, want gate system, function_call_output, user message: %s", len(items), mustJSON(t, items))
	}
	result, _ := items[1].(map[string]any)
	if result["type"] != "function_call_output" || result["call_id"] != "toolu_1" || result["output"] != "Error: boom" {
		t.Errorf("tool result item = %s, want function_call_output toolu_1 with an Error: prefix", mustJSON(t, result))
	}
	user, _ := items[2].(map[string]any)
	want := `[{"text":"look","type":"input_text"},` +
		`{"image_url":"data:image/png;base64,QUJD","type":"input_image"},` +
		`{"image_url":"https://example.test/a.png","type":"input_image"}]`
	if got := mustJSON(t, user["content"]); got != want {
		t.Errorf("user content = %s, want %s", got, want)
	}
}
