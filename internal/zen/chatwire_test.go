package zen

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendChatStreamingToolCall(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("bad upstream call %s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{
			`{"id":"c1","choices":[{"delta":{"reasoning_content":"hmm"}}]}`,
			`{"id":"c1","choices":[{"delta":{"content":"Hi"}}]}`,
			`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"read","arguments":"{\"p\":"}}]}}]}`,
			`{"id":"c1","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}`,
			`{"id":"c1","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":40}}}`,
		} {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	body := `{"model":"opencode/glm-5.3","stream":true,"max_tokens":50,"system":[{"type":"text","text":"sys"}],
	"tools":[{"name":"read","description":"d","input_schema":{"type":"object"}},{"type":"web_search_20250305","name":"web_search"}],
	"tool_choice":{"type":"any"},
	"messages":[{"role":"user","content":"q"},
	{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"s"},{"type":"tool_use","id":"tu1","name":"read","input":{"p":0}}]},
	{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"text","text":"ok"}]},{"type":"text","text":"next"}]}]}`
	resp, err := SendChat(context.Background(), srv.Client(), srv.URL, "k", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	s := string(out)

	// Request translation.
	msgs := got["messages"].([]any)
	roles := []string{}
	for _, m := range msgs {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	if strings.Join(roles, ",") != "system,user,assistant,tool,user" {
		t.Fatalf("roles = %v", roles)
	}
	asst := msgs[2].(map[string]any)
	if asst["reasoning_content"] != "t" || asst["tool_calls"].([]any)[0].(map[string]any)["id"] != "tu1" {
		t.Fatalf("assistant = %v", asst)
	}
	if msgs[3].(map[string]any)["tool_call_id"] != "tu1" || got["model"] != "glm-5.3" || got["tool_choice"] != "required" {
		t.Fatalf("request = %v", got)
	}
	if got["stream"] != true {
		t.Fatalf("upstream stream = %v, want true (free-tier gate rejects non-streaming bodies)", got["stream"])
	}
	// The client defines "read" but not "bash"; the gate requires both, so
	// the captured OpenCode bash definition is injected.
	names := []string{}
	for _, tool := range got["tools"].([]any) {
		fn := tool.(map[string]any)["function"].(map[string]any)
		names = append(names, fn["name"].(string))
	}
	if strings.Join(names, ",") != "read,bash" {
		t.Fatalf("tools = %v, want [read bash]", names)
	}
	if params := gateToolDef("bash")["parameters"]; params == nil {
		t.Fatalf("injected bash definition missing parameters")
	}

	// Response translation.
	for _, want := range []string{
		`"thinking":"hmm","type":"thinking_delta"`,
		`"text":"Hi","type":"text_delta"`,
		`"content_block":{"id":"call_a","input":{},"name":"read","type":"tool_use"}`,
		`"partial_json":"{\"p\":"`,
		`"partial_json":"1}"`,
		`"stop_reason":"tool_use"`,
		`"cache_read_input_tokens":40,"input_tokens":60,"output_tokens":7`,
		`event: message_stop`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("stream missing %s\n%s", want, s)
		}
	}
	if strings.Count(s, "event: content_block_start") != 3 || strings.Count(s, "event: content_block_stop") != 3 {
		t.Errorf("block framing wrong:\n%s", s)
	}
}

func TestSendChatJSONAndError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req["model"] == "bad" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"c2","choices":[{"message":{"content":"yo","tool_calls":[{"id":"x","function":{"name":"f","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}))
	defer srv.Close()

	resp, err := SendChat(context.Background(), srv.Client(), srv.URL, "k", []byte(`{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&msg)
	content := msg["content"].([]any)
	if msg["stop_reason"] != "tool_use" || len(content) != 2 || content[1].(map[string]any)["input"].(map[string]any)["a"] != 1.0 {
		t.Fatalf("json = %v", msg)
	}

	resp, err = SendChat(context.Background(), srv.Client(), srv.URL, "k", []byte(`{"model":"bad","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 429 || !strings.Contains(string(b), `"type":"rate_limit_error"`) || !strings.Contains(string(b), "slow down") {
		t.Fatalf("error = %d %s", resp.StatusCode, b)
	}
}

// "length" with a tool call means the arguments were truncated: the client
// must see max_tokens, not a tool_use it would execute with {} input. Some
// backends report "stop" on tool-call turns; that must still promote.
func TestChatResponseToolCallStopReason(t *testing.T) {
	resp := func(finish string) map[string]any {
		return ChatResponseToAnthropic(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"tool_calls": []any{map[string]any{
				"id": "x", "function": map[string]any{"name": "Write", "arguments": `{"path":"/tmp/a","content":"trunc`},
			}}},
			"finish_reason": finish,
		}}}, "m", nil, nil)
	}
	if got := resp("length")["stop_reason"]; got != "max_tokens" {
		t.Errorf("length + tool_calls: stop_reason = %v, want max_tokens", got)
	}
	if got := resp("stop")["stop_reason"]; got != "tool_use" {
		t.Errorf("stop + tool_calls: stop_reason = %v, want tool_use", got)
	}
}

func TestSendChatNonJSONSuccessIs502(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>gateway</html>")
	}))
	defer srv.Close()

	resp, err := SendChat(context.Background(), srv.Client(), srv.URL, "k", []byte(`{"model":"glm-5.3","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(b), `"type":"api_error"`) {
		t.Fatalf("got %d %s, want 502 api_error", resp.StatusCode, b)
	}
}

// A stream is complete only if it carried [DONE] or a finish_reason; a bare
// clean EOF is a dropped connection and must not pass as end_turn.
func TestStreamTerminationRequiresMarker(t *testing.T) {
	content := "data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"The answer is\"}}]}\n\n"
	cases := []struct {
		name, in  string
		wantError bool
	}{
		{"clean EOF, no marker", content, true},
		{"finish_reason without [DONE]", content + "data: {\"id\":\"c\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", false},
		{"[DONE] without finish_reason", content + "data: [DONE]\n\n", false},
	}
	for _, c := range cases {
		var out bytes.Buffer
		if err := streamChatToAnthropic(strings.NewReader(c.in), &out, "m", nil, nil); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		s := out.String()
		gotError := strings.Contains(s, "event: error")
		gotStop := strings.Contains(s, "event: message_stop")
		if gotError != c.wantError || gotStop == c.wantError {
			t.Errorf("%s: error=%v message_stop=%v, want error=%v\n%s", c.name, gotError, gotStop, c.wantError, s)
		}
	}
}

func TestMapFinishReasonContentFilter(t *testing.T) {
	if got := mapFinishReason("content_filter"); got != "refusal" {
		t.Fatalf("content_filter = %q, want refusal", got)
	}
}

func TestToolResultImageLeavesPlaceholder(t *testing.T) {
	out := userToChat([]any{map[string]any{
		"type": "tool_result", "tool_use_id": "t1",
		"content": []any{map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": "image/png", "data": "AAAA",
		}}},
	}})
	tool := out[0].(map[string]any)
	if tool["role"] != "tool" || !strings.Contains(tool["content"].(string), "image omitted") {
		t.Fatalf("tool message = %v, want an image-omitted placeholder", tool)
	}
}

func TestUserToChatEmptyTurnPreserved(t *testing.T) {
	// document/file image sources have no Chat equivalent and are dropped.
	out := userToChat([]any{
		map[string]any{"type": "image", "source": map[string]any{"type": "file", "file_id": "f1"}},
	})
	if len(out) != 1 {
		t.Fatalf("dropped turn vanished: %v", out)
	}
	msg := out[0].(map[string]any)
	if msg["role"] != "user" || msg["content"] != "" {
		t.Fatalf("fallback message = %v", msg)
	}
}

func TestStreamLogsDroppedInterleavedArgs(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	in := "data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"f\",\"arguments\":\"{\"}}]}}]}\n\n" +
		"data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"b\",\"function\":{\"name\":\"g\",\"arguments\":\"{\"}}]}}]}\n\n" +
		"data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	var out bytes.Buffer
	if err := streamChatToAnthropic(strings.NewReader(in), &out, "m", nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "dropping interleaved tool_call arguments") {
		t.Fatalf("no drop logged:\n%s", buf.String())
	}
}

func TestChatErrorLogsFreeTierGate(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(prev)

	gate := `{"error":{"code":403,"message":"FreeTierError: OpenCode's free tier can only be used from within OpenCode"}}`
	out := chatErrorToAnthropic(http.StatusForbidden, []byte(gate), "mimo-v2.6-flash-free")
	if !strings.Contains(string(out), "permission_error") || !strings.Contains(string(out), "Zen: ") {
		t.Fatalf("envelope = %s", out)
	}
	if !strings.Contains(buf.String(), "zen free-tier gate rejected request") {
		t.Fatalf("no gate warning logged:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "model=mimo-v2.6-flash-free") {
		t.Fatalf("warning missing model attr:\n%s", buf.String())
	}

	buf.Reset()
	_ = chatErrorToAnthropic(http.StatusTooManyRequests, []byte(`{"error":{"message":"rate limited"}}`), "glm-5.3")
	if buf.Len() != 0 {
		t.Fatalf("unexpected log for non-gate error:\n%s", buf.String())
	}
}

// The free-tier gate rejects non-streaming upstream bodies (403
// FreeTierError), so the upstream request always carries stream:true and a
// client that asked for JSON gets the stream aggregated back into one
// Anthropic response.
func TestSendChatForcesStreamAndAggregatesJSON(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range []string{
			`{"id":"c9","choices":[{"delta":{"content":"hi "}}]}`,
			`{"id":"c9","choices":[{"delta":{"content":"there"}}]}`,
			`{"id":"c9","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_b","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]}}]}`,
			`{"id":"c9","choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"id":"c9","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":3}}`,
		} {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	// No stream flag, no tools: both are supplied for the gate.
	resp, err := SendChat(context.Background(), srv.Client(), srv.URL, "k",
		[]byte(`{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"q"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got["stream"] != true {
		t.Fatalf("upstream stream = %v, want true", got["stream"])
	}
	var names []string
	for _, tool := range got["tools"].([]any) {
		fn := tool.(map[string]any)["function"].(map[string]any)
		names = append(names, fn["name"].(string))
	}
	if strings.Join(names, ",") != "bash,read" {
		t.Fatalf("tools = %v, want [bash read]", names)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q, want application/json", ct)
	}
	var msg map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	// The client declared no tools, so bash/read were injected for the gate:
	// the upstream bash call must not come back as a tool_use block.
	content := msg["content"].([]any)
	if msg["stop_reason"] != "end_turn" || len(content) != 1 {
		t.Fatalf("message = %v, want end_turn with the injected bash call dropped", msg)
	}
	if content[0].(map[string]any)["text"] != "hi there" {
		t.Fatalf("text block = %v", content[0])
	}
	if usage := msg["usage"].(map[string]any); toInt(usage["input_tokens"]) != 9 || toInt(usage["output_tokens"]) != 3 {
		t.Fatalf("usage = %v", usage)
	}
}

// Anthropic-spelled client tools ("Bash"/"Read") travel upstream under the
// gate's lowercase names and come back under the client's names, in request
// bodies (history, tool_choice) and in responses (SSE tool_use blocks).
func TestSendChatRenamesBashRead(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_x\",\"function\":{\"name\":\"bash\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	body := `{"model":"mimo-v2.6-flash-free","stream":true,
	"tools":[{"name":"Bash","input_schema":{"type":"object"}},{"name":"Read","input_schema":{"type":"object"}}],
	"tool_choice":{"type":"tool","name":"Bash"},
	"messages":[{"role":"user","content":"q"},
	{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{}}]},
	{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"ok"}]}]}`
	resp, err := SendChat(context.Background(), srv.Client(), srv.URL, "k", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range got["tools"].([]any) {
		fn := tool.(map[string]any)["function"].(map[string]any)
		names = append(names, fn["name"].(string))
	}
	if strings.Join(names, ",") != "bash,read" {
		t.Fatalf("upstream tools = %v, want [bash read] (no duplicates from injection)", names)
	}
	if tc := got["tool_choice"].(map[string]any); tc["function"].(map[string]any)["name"] != "bash" {
		t.Fatalf("tool_choice = %v", got["tool_choice"])
	}
	asst := got["messages"].([]any)[1].(map[string]any)
	if call := asst["tool_calls"].([]any)[0].(map[string]any); call["function"].(map[string]any)["name"] != "bash" {
		t.Fatalf("history tool_calls = %v", call)
	}
	out, _ := io.ReadAll(resp.Body)
	s := string(out)
	if !strings.Contains(s, `"name":"Bash"`) {
		t.Errorf("response tool_use not renamed back to Bash:\n%s", s)
	}
	if strings.Contains(s, `"name":"bash"`) {
		t.Errorf("response leaked upstream name bash:\n%s", s)
	}
}

// A truncated upstream stream (no [DONE], no finish_reason) is a dropped
// connection; the non-streaming client must get an error, not a partial
// answer passed off as complete.
func TestSendChatNonStreamTruncatedStreamFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	}))
	defer srv.Close()

	resp, err := SendChat(context.Background(), srv.Client(), srv.URL, "k",
		[]byte(`{"model":"mimo-v2.6-flash-free","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(b), "stream ended before completion") {
		t.Fatalf("got %d %s, want 502 truncated-stream error", resp.StatusCode, b)
	}
}

// The gate-injected "bash"/"read" definitions the client never declared must
// not come back as tool_use blocks: Claude Code errors on an unknown tool
// name, and the reverse-rename map only covers real renames.
func TestSendChatDropsInjectedToolCallNonStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","choices":[{"message":{"content":"ok","tool_calls":[
			{"id":"call_a","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}},
			{"id":"call_b","function":{"name":"read","arguments":"{\"p\":1}"}}]},
			"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	resp, err := SendChat(context.Background(), srv.Client(), srv.URL, "k",
		[]byte(`{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"q"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	for _, block := range msg["content"].([]any) {
		if block.(map[string]any)["type"] == "tool_use" {
			t.Fatalf("gate-injected tool surfaced as tool_use: %v", block)
		}
	}
	// No tool_use block survived, so stop_reason must not claim one.
	if msg["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v, want end_turn with every call dropped", msg["stop_reason"])
	}
}

// Same drop on the streaming path: no tool_use content block opens for a
// gate-injected tool, and later argument fragments for it are ignored.
func TestSendChatDropsInjectedToolCallStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"function\":{\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\"}}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"ls}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	resp, err := SendChat(context.Background(), srv.Client(), srv.URL, "k",
		[]byte(`{"model":"mimo-v2.6-flash-free","stream":true,"messages":[{"role":"user","content":"q"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	s := string(out)
	if strings.Contains(s, `"type":"tool_use"`) {
		t.Errorf("gate-injected tool surfaced in stream:\n%s", s)
	}
	if strings.Contains(s, "input_json_delta") {
		t.Errorf("arguments for a dropped call were forwarded:\n%s", s)
	}
	if !strings.Contains(s, `"stop_reason":"end_turn"`) {
		t.Errorf("stream stop_reason not end_turn:\n%s", s)
	}
	if !strings.Contains(s, "event: message_stop") {
		t.Errorf("stream not finished:\n%s", s)
	}
}

// A client that declares both "Bash" and "bash" must not collapse them onto
// one upstream name: duplicate upstream tools and a wrong reverse lookup
// would misroute the response. The collision case skips the rename instead.
func TestToolRenames_CaseCollision(t *testing.T) {
	req := map[string]any{
		"model":    "glm-5.3",
		"messages": []any{},
		"tools": []any{
			map[string]any{"name": "Bash", "input_schema": map[string]any{"type": "object"}},
			map[string]any{"name": "bash", "input_schema": map[string]any{"type": "object"}},
		},
	}
	out, rev, injected := anthropicToChatRequest(req)

	var names []string
	for _, tool := range out["tools"].([]any) {
		fn := tool.(map[string]any)["function"].(map[string]any)
		names = append(names, fn["name"].(string))
	}
	seen := map[string]int{}
	for _, n := range names {
		seen[n]++
	}
	if len(names) != len(seen) {
		t.Fatalf("upstream tools = %v, want no duplicate names", names)
	}
	if seen["bash"] != 1 || seen["Bash"] != 1 {
		t.Fatalf("upstream tools = %v, want one bash and one Bash", names)
	}
	if !injected["read"] || len(injected) != 1 {
		t.Fatalf("injected = %v, want [read] only (bash present case-insensitively)", injected)
	}
	// Round trip: each upstream name the client declared resolves back to
	// its own spelling; the injected "read" is dropped instead.
	for _, upstream := range names {
		if injected[upstream] {
			continue
		}
		msg := ChatResponseToAnthropic(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"tool_calls": []any{map[string]any{
				"id": "x", "function": map[string]any{"name": upstream, "arguments": "{}"},
			}}},
			"finish_reason": "tool_calls",
		}}}, "m", rev, injected)
		block := msg["content"].([]any)[0].(map[string]any)
		if block["name"] != upstream {
			t.Errorf("round trip %q -> %v", upstream, block["name"])
		}
	}
}

// Claude Code opens its system prompt with an attribution line whose cch value
// differs on every request. A translated wire must not send it: it makes the
// head of each prompt unique, so the provider's prefix cache never hits, and it
// hands the provider a client-identity marker.
func TestSystemText_DropsClaudeCodeBillingHeader(t *testing.T) {
	const header = "x-anthropic-billing-header: cc_version=2.1.280.5c2; cc_entrypoint=cli; cch=3d0b8; cc_prompt_id=abc;"
	text := func(s string) any { return map[string]any{"type": "text", "text": s} }
	for _, tc := range []struct {
		name   string
		system any
		want   string
	}{
		{"header in its own block", []any{text(header), text("You are Claude Code.")}, "You are Claude Code."},
		{"header-only system", []any{text(header)}, ""},
		{"header line inside a string", header + "\nYou are Claude Code.", "You are Claude Code."},
		{"token match ignores case", "X-Anthropic-Billing-Header: cch=1\nbody", "body"},
		{"system without a header is untouched", []any{text("a"), text("b")}, "a\n\nb"},
	} {
		if got := systemText(tc.system); got != tc.want {
			t.Errorf("%s: systemText = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestTranslatedWires_OmitClaudeCodeBillingHeader(t *testing.T) {
	req := map[string]any{
		"model": "glm-5.3",
		"system": []any{
			map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.280.5c2; cch=3d0b8;"},
			map[string]any{"type": "text", "text": "You are Claude Code."},
		},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	chat, _, _ := anthropicToChatRequest(req)
	responses, _, _ := anthropicToResponsesRequest(req)
	for name, body := range map[string]map[string]any{"chat": chat, "responses": responses} {
		raw, _ := json.Marshal(body)
		if strings.Contains(strings.ToLower(string(raw)), "billing-header") {
			t.Errorf("%s wire body carries the billing header: %s", name, raw)
		}
		if !strings.Contains(string(raw), "You are Claude Code.") {
			t.Errorf("%s wire body lost the real system prompt: %s", name, raw)
		}
	}
}
