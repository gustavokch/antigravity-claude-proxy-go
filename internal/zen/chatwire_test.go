package zen

import (
	"context"
	"encoding/json"
	"io"
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
	if n := len(got["tools"].([]any)); n != 1 {
		t.Fatalf("server tool not dropped: %d tools", n)
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

func TestMapFinishReasonContentFilter(t *testing.T) {
	if got := mapFinishReason("content_filter"); got != "refusal" {
		t.Fatalf("content_filter = %q, want refusal", got)
	}
}
