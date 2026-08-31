package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
)

// translateOpenAIRequest converts an OpenAI Chat Completions request body
// (already decoded into a map) into an Anthropic Messages request map. It
// returns an error for structurally invalid requests (e.g. tool_calls whose
// arguments are not valid JSON).
func TestTranslateOpenAIRequest(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string // expected Anthropic map, as JSON (compared via DeepEqual)
		wantErr string
	}{
		{
			name:  "basic user text message with defaults",
			input: `{"model":"kimi-k2","messages":[{"role":"user","content":"Hello"}]}`,
			want: `{
				"model": "kimi-k2",
				"messages": [{"role": "user", "content": [{"type": "text", "text": "Hello"}]}],
				"max_tokens": 4096
			}`,
		},
		{
			name: "system message hoisted to top-level system",
			input: `{"model":"m","messages":[
				{"role":"system","content":"Be terse."},
				{"role":"user","content":"Hi"}
			]}`,
			want: `{
				"model": "m",
				"system": "Be terse.",
				"messages": [{"role": "user", "content": [{"type": "text", "text": "Hi"}]}],
				"max_tokens": 4096
			}`,
		},
		{
			name: "multiple system messages joined in order",
			input: `{"model":"m","messages":[
				{"role":"system","content":"A"},
				{"role":"user","content":"Hi"},
				{"role":"system","content":"B"}
			]}`,
			want: `{
				"model": "m",
				"system": "A\n\nB",
				"messages": [{"role": "user", "content": [{"type": "text", "text": "Hi"}]}],
				"max_tokens": 4096
			}`,
		},
		{
			name: "multi-part user content becomes blocks",
			input: `{"model":"m","messages":[{"role":"user","content":[
				{"type":"text","text":"part one"},
				{"type":"text","text":"part two"}
			]}],"max_tokens":100}`,
			want: `{
				"model": "m",
				"messages": [{"role": "user", "content": [
					{"type": "text", "text": "part one"},
					{"type": "text", "text": "part two"}
				]}],
				"max_tokens": 100
			}`,
		},
		{
			name:  "max_completion_tokens maps to max_tokens",
			input: `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":77}`,
			want: `{
				"model": "m",
				"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}],
				"max_tokens": 77
			}`,
		},
		{
			name: "assistant tool_calls become tool_use blocks with object input",
			input: `{"model":"m","messages":[
				{"role":"user","content":"weather?"},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Lisbon\"}"}}
				]}
			],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`,
			want: `{
				"model": "m",
				"messages": [
					{"role": "user", "content": [{"type": "text", "text": "weather?"}]},
					{"role": "assistant", "content": [{"type": "tool_use", "id": "call_1", "name": "get_weather", "input": {"city": "Lisbon"}}]}
				],
				"tools": [{"name": "get_weather", "description": "Get weather", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}}}],
				"max_tokens": 4096
			}`,
		},
		{
			name:    "malformed tool_call arguments is an error",
			input:   `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","function":{"name":"f","arguments":"{not json"}}]}]}`,
			wantErr: "invalid_request_error",
		},
		{
			name: "tool result message becomes tool_result block in user message",
			input: `{"model":"m","messages":[
				{"role":"user","content":"weather?"},
				{"role":"assistant","tool_calls":[{"id":"call_1","function":{"name":"get_weather","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"sunny 22C"}
			]}`,
			want: `{
				"model": "m",
				"messages": [
					{"role": "user", "content": [{"type": "text", "text": "weather?"}]},
					{"role": "assistant", "content": [{"type": "tool_use", "id": "call_1", "name": "get_weather", "input": {}}]},
					{"role": "user", "content": [{"type": "tool_result", "tool_use_id": "call_1", "content": [{"type": "text", "text": "sunny 22C"}]}]}
				],
				"max_tokens": 4096
			}`,
		},
		{
			name:  "stop string becomes stop_sequences array",
			input: `{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":"END"}`,
			want: `{
				"model": "m",
				"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}],
				"stop_sequences": ["END"],
				"max_tokens": 4096
			}`,
		},
		{
			name:  "stop array passes through",
			input: `{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":["A","B"]}`,
			want: `{
				"model": "m",
				"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}],
				"stop_sequences": ["A", "B"],
				"max_tokens": 4096
			}`,
		},
		{
			name:  "temperature and top_p pass through, stream flag passes",
			input: `{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":0.5,"top_p":0.9,"stream":true}`,
			want: `{
				"model": "m",
				"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}],
				"temperature": 0.5,
				"top_p": 0.9,
				"stream": true,
				"max_tokens": 4096
			}`,
		},
		{
			name:  "tool_choice auto maps",
			input: `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":"auto"}`,
			want: `{
				"model": "m",
				"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}],
				"tool_choice": {"type": "auto"},
				"max_tokens": 4096
			}`,
		},
		{
			name:  "tool_choice none maps",
			input: `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":"none"}`,
			want: `{
				"model": "m",
				"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}],
				"tool_choice": {"type": "none"},
				"max_tokens": 4096
			}`,
		},
		{
			name:  "tool_choice required maps to any",
			input: `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":"required"}`,
			want: `{
				"model": "m",
				"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}],
				"tool_choice": {"type": "any"},
				"max_tokens": 4096
			}`,
		},
		{
			name:  "tool_choice named function maps to tool",
			input: `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"function","function":{"name":"get_weather"}}}`,
			want: `{
				"model": "m",
				"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}],
				"tool_choice": {"type": "tool", "name": "get_weather"},
				"max_tokens": 4096
			}`,
		},
		{
			name: "parallel tool results merge into one user message",
			input: `{"model":"m","messages":[
				{"role":"user","content":"weather and time?"},
				{"role":"assistant","tool_calls":[
					{"id":"call_1","function":{"name":"get_weather","arguments":"{}"}},
					{"id":"call_2","function":{"name":"get_time","arguments":"{}"}}
				]},
				{"role":"tool","tool_call_id":"call_1","content":"sunny"},
				{"role":"tool","tool_call_id":"call_2","content":"noon"}
			]}`,
			want: `{
				"model": "m",
				"messages": [
					{"role": "user", "content": [{"type": "text", "text": "weather and time?"}]},
					{"role": "assistant", "content": [
						{"type": "tool_use", "id": "call_1", "name": "get_weather", "input": {}},
						{"type": "tool_use", "id": "call_2", "name": "get_time", "input": {}}
					]},
					{"role": "user", "content": [
						{"type": "tool_result", "tool_use_id": "call_1", "content": [{"type": "text", "text": "sunny"}]},
						{"type": "tool_result", "tool_use_id": "call_2", "content": [{"type": "text", "text": "noon"}]}
					]}
				],
				"max_tokens": 4096
			}`,
		},
		{
			name: "consecutive same-role messages merge",
			input: `{"model":"m","messages":[
				{"role":"user","content":"a"},
				{"role":"user","content":"b"}
			]}`,
			want: `{
				"model": "m",
				"messages": [{"role": "user", "content": [
					{"type": "text", "text": "a"},
					{"type": "text", "text": "b"}
				]}],
				"max_tokens": 4096
			}`,
		},
		{
			name:  "empty string content tolerated as empty text block",
			input: `{"model":"m","messages":[{"role":"user","content":""}]}`,
			want: `{
				"model": "m",
				"messages": [{"role": "user", "content": [{"type": "text", "text": ""}]}],
				"max_tokens": 4096
			}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var input map[string]any
			if err := json.Unmarshal([]byte(tt.input), &input); err != nil {
				t.Fatalf("test fixture is not valid JSON: %v", err)
			}

			got, err := translateOpenAIRequest(input)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("translateOpenAIRequest() error = nil, want %q", tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("translateOpenAIRequest() unexpected error: %v", err)
			}

			var want map[string]any
			if err := json.Unmarshal([]byte(tt.want), &want); err != nil {
				t.Fatalf("test fixture want is not valid JSON: %v", err)
			}
			// Round-trip got through JSON so both sides use the same numeric
			// representation; the observable behavior is the encoded shape.
			gotJSON, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal got: %v", err)
			}
			var gotNorm map[string]any
			if err := json.Unmarshal(gotJSON, &gotNorm); err != nil {
				t.Fatalf("unmarshal got: %v", err)
			}
			if !reflect.DeepEqual(gotNorm, want) {
				gotPretty, _ := json.MarshalIndent(gotNorm, "", "  ")
				wantJSON, _ := json.MarshalIndent(want, "", "  ")
				t.Errorf("translation mismatch\n--- got ---\n%s\n--- want ---\n%s", gotPretty, wantJSON)
			}
		})
	}
}

// TestOpenAIChatCompletions_Unary posts an OpenAI Chat Completions body to
// /v1/chat/completions and asserts the upstream receives the Anthropic shape
// while the client receives an OpenAI chat.completion JSON.
func TestOpenAIChatCompletions_Unary(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	var receivedBody map[string]any
	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &receivedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_123","type":"message","role":"assistant","model":"anthropic/claude-3.7-sonnet","content":[{"type":"text","text":"Hello from OpenRouter!"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":34}}`))
	}))
	defer mockOR.Close()

	_, err := config.Save(map[string]any{
		"openrouter": map[string]any{
			"enabled": true,
			"apiKey":  "sk-or-v1-secret-123",
			"baseUrl": mockOR.URL,
			"allowlist": []map[string]any{
				{"id": "anthropic/claude-3.7-sonnet", "enabled": true},
			},
		},
	})
	if err != nil {
		t.Fatalf("config save error: %v", err)
	}

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	reqPayload := `{"model":"anthropic/claude-3.7-sonnet","messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-proxy-key")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	// Upstream must have received the Anthropic shape.
	if got := receivedBody["model"]; got != "anthropic/claude-3.7-sonnet" {
		t.Errorf("upstream model = %v, want anthropic/claude-3.7-sonnet", got)
	}
	upMessages, _ := receivedBody["messages"].([]any)
	if len(upMessages) != 1 {
		t.Fatalf("upstream messages length = %d, want 1", len(upMessages))
	}
	upFirst, _ := upMessages[0].(map[string]any)
	if upFirst["role"] != "user" {
		t.Errorf("upstream first message role = %v, want user", upFirst["role"])
	}

	// Client receives the OpenAI chat.completion envelope.
	var completion map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &completion); err != nil {
		t.Fatalf("response is not JSON: %v\nbody: %s", err, rec.Body.String())
	}
	if got := completion["object"]; got != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", got)
	}
	choices, _ := completion["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices length = %d, want 1", len(choices))
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message["role"] != "assistant" {
		t.Errorf("message role = %v, want assistant", message["role"])
	}
	if message["content"] != "Hello from OpenRouter!" {
		t.Errorf("message content = %v, want %q", message["content"], "Hello from OpenRouter!")
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want stop", choice["finish_reason"])
	}
	usage, _ := completion["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(12) || usage["completion_tokens"] != float64(34) {
		t.Errorf("usage = %v, want prompt 12 completion 34", usage)
	}
	if completion["model"] != "anthropic/claude-3.7-sonnet" {
		t.Errorf("model = %v, want echo of requested model", completion["model"])
	}
}

// TestTranslateAnthropicMessageToOpenAI covers the unary response mapping in
// isolation: content blocks, tool_use re-serialization, thinking, stop_reason
// and usage mapping.
func TestTranslateAnthropicMessageToOpenAI(t *testing.T) {
	tests := []struct {
		name      string
		anthropic string
		// checks run against the translated envelope
		checks []func(t *testing.T, completion map[string]any)
	}{
		{
			name:      "text response",
			anthropic: `{"id":"msg_a","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi there"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`,
			checks: []func(t *testing.T, completion map[string]any){
				func(t *testing.T, c map[string]any) {
					if got := c["id"]; got != "chatcmpl-msg_a" {
						t.Errorf("id = %v, want chatcmpl-msg_a", got)
					}
				},
			},
		},
		{
			name:      "tool_use becomes tool_calls with string arguments",
			anthropic: `{"id":"msg_b","type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Lisbon","days":3}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":6}}`,
			checks: []func(t *testing.T, completion map[string]any){
				func(t *testing.T, c map[string]any) {
					choice := c["choices"].([]any)[0].(map[string]any)
					if choice["finish_reason"] != "tool_calls" {
						t.Errorf("finish_reason = %v, want tool_calls", choice["finish_reason"])
					}
					message := choice["message"].(map[string]any)
					toolCalls, _ := message["tool_calls"].([]any)
					if len(toolCalls) != 1 {
						t.Fatalf("tool_calls length = %d, want 1", len(toolCalls))
					}
					call := toolCalls[0].(map[string]any)
					if call["id"] != "toolu_1" || call["type"] != "function" {
						t.Errorf("tool_call = %v, want id toolu_1 type function", call)
					}
					fn := call["function"].(map[string]any)
					if fn["name"] != "get_weather" {
						t.Errorf("function name = %v, want get_weather", fn["name"])
					}
					args, ok := fn["arguments"].(string)
					if !ok {
						t.Fatalf("arguments must be a JSON string, got %T", fn["arguments"])
					}
					var parsed any
					if err := json.Unmarshal([]byte(args), &parsed); err != nil {
						t.Fatalf("arguments not valid JSON: %v", err)
					}
					if !reflect.DeepEqual(parsed, map[string]any{"city": "Lisbon", "days": float64(3)}) {
						t.Errorf("arguments decoded = %v, want original object", parsed)
					}
				},
			},
		},
		{
			name:      "thinking blocks become reasoning_content",
			anthropic: `{"id":"msg_c","role":"assistant","content":[{"type":"thinking","thinking":"let me reason"},{"type":"text","text":"answer"}],"stop_reason":"end_turn"}`,
			checks: []func(t *testing.T, completion map[string]any){
				func(t *testing.T, c map[string]any) {
					message := c["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
					if message["reasoning_content"] != "let me reason" {
						t.Errorf("reasoning_content = %v", message["reasoning_content"])
					}
					if message["content"] != "answer" {
						t.Errorf("content = %v, want answer", message["content"])
					}
				},
			},
		},
		{
			name:      "finish_reason length for max_tokens",
			anthropic: `{"id":"msg_d","role":"assistant","content":[{"type":"text","text":"cut off"}],"stop_reason":"max_tokens"}`,
			checks: []func(t *testing.T, completion map[string]any){
				func(t *testing.T, c map[string]any) {
					choice := c["choices"].([]any)[0].(map[string]any)
					if choice["finish_reason"] != "length" {
						t.Errorf("finish_reason = %v, want length", choice["finish_reason"])
					}
				},
			},
		},
		{
			name:      "finish_reason stop for stop_sequence",
			anthropic: `{"id":"msg_e","role":"assistant","content":[{"type":"text","text":"a"}],"stop_reason":"stop_sequence"}`,
			checks: []func(t *testing.T, completion map[string]any){
				func(t *testing.T, c map[string]any) {
					choice := c["choices"].([]any)[0].(map[string]any)
					if choice["finish_reason"] != "stop" {
						t.Errorf("finish_reason = %v, want stop", choice["finish_reason"])
					}
				},
			},
		},
		{
			name:      "empty content becomes empty string",
			anthropic: `{"id":"msg_f","role":"assistant","content":[],"stop_reason":"end_turn"}`,
			checks: []func(t *testing.T, completion map[string]any){
				func(t *testing.T, c map[string]any) {
					message := c["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
					if message["content"] != "" {
						t.Errorf("content = %v, want empty string", message["content"])
					}
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var anthropic map[string]any
			if err := json.Unmarshal([]byte(tt.anthropic), &anthropic); err != nil {
				t.Fatalf("fixture invalid: %v", err)
			}
			completion := translateAnthropicMessageToOpenAI(anthropic, "requested-model", 1700000000)
			if completion["object"] != "chat.completion" {
				t.Errorf("object = %v, want chat.completion", completion["object"])
			}
			if completion["model"] != "requested-model" {
				t.Errorf("model = %v, want requested-model", completion["model"])
			}
			for _, check := range tt.checks {
				check(t, completion)
			}
		})
	}
}

// TestTranslateOpenAIStopReasonMatrix pins the full stop_reason mapping.
func TestTranslateOpenAIStopReasonMapping(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
	}
	for anthropicReason, want := range cases {
		if got := anthropicStopReasonToOpenAI(anthropicReason); got != want {
			t.Errorf("stop_reason %q = %q, want %q", anthropicReason, got, want)
		}
	}
}

// TestOpenAIChatCompletions_UpstreamError checks that an Anthropic error
// envelope emitted before stream start reaches the OpenAI client as an OpenAI
// error with the upstream status preserved.
func TestOpenAIChatCompletions_UpstreamError(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"upstream exploded"}}`))
	}))
	defer mockOR.Close()

	_, err := config.Save(map[string]any{
		"openrouter": map[string]any{
			"enabled": true,
			"apiKey":  "sk-or-v1-secret-123",
			"baseUrl": mockOR.URL,
			"allowlist": []map[string]any{
				{"id": "anthropic/claude-3.7-sonnet", "enabled": true},
			},
		},
	})
	if err != nil {
		t.Fatalf("config save error: %v", err)
	}

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	reqPayload := `{"model":"anthropic/claude-3.7-sonnet","messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqPayload))
	req.Header.Set("Authorization", "Bearer test-proxy-key")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("expected error status, got 200: %s", rec.Body.String())
	}
	var errBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("response is not JSON: %v\nbody: %s", err, rec.Body.String())
	}
	errObj, ok := errBody["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected OpenAI error envelope, got %v", errBody)
	}
	if errObj["message"] != "upstream error" && !strings.Contains(stringFrom(errObj["message"]), "upstream") {
		t.Errorf("error message = %v, want upstream error text", errObj["message"])
	}
	if errObj["type"] == nil || errObj["type"] == "" {
		t.Errorf("error type missing: %v", errBody)
	}
}

// TestOpenAIChatCompletions_SSE streams Anthropic SSE from the upstream and
// asserts the client receives chat.completion.chunk frames ending with
// data: [DONE].
func TestOpenAIChatCompletions_SSE(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		frames := []struct{ event, data string }{
			{"message_start", `{"type":"message_start","message":{"id":"msg_stream","role":"assistant","content":[]},"usage":{"input_tokens":9}}`},
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`},
			{"message_stop", `{"type":"message_stop"}`},
		}
		for _, frame := range frames {
			_, _ = w.Write([]byte("event: " + frame.event + "\ndata: " + frame.data + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer mockOR.Close()

	_, err := config.Save(map[string]any{
		"openrouter": map[string]any{
			"enabled": true,
			"apiKey":  "sk-or-v1-secret-123",
			"baseUrl": mockOR.URL,
			"allowlist": []map[string]any{
				{"id": "anthropic/claude-3.7-sonnet", "enabled": true},
			},
		},
	})
	if err != nil {
		t.Fatalf("config save error: %v", err)
	}

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	reqPayload := `{"model":"anthropic/claude-3.7-sonnet","messages":[{"role":"user","content":"Hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqPayload))
	req.Header.Set("Authorization", "Bearer test-proxy-key")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", rec.Header().Get("Content-Type"))
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Errorf("stream must end with data: [DONE], got:\n%s", body)
	}
	if strings.Contains(body, "message_start") || strings.Contains(body, "content_block_delta") {
		t.Errorf("raw Anthropic event names leaked to OpenAI client:\n%s", body)
	}

	var text strings.Builder
	var sawRoleChunk, sawFinishChunk bool
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk struct {
			Object  string `json:"object"`
			Model   string `json:"model"`
			Choices []struct {
				Delta        map[string]any `json:"delta"`
				FinishReason any            `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("chunk line is not JSON: %v\nline: %s", err, line)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Errorf("object = %v, want chat.completion.chunk", chunk.Object)
		}
		if chunk.Model != "anthropic/claude-3.7-sonnet" {
			t.Errorf("chunk model = %v, want requested model", chunk.Model)
		}
		if len(chunk.Choices) != 1 {
			t.Fatalf("chunk choices length = %d", len(chunk.Choices))
		}
		if chunk.Choices[0].Delta["role"] == "assistant" {
			sawRoleChunk = true
		}
		if textStr, ok := chunk.Choices[0].Delta["content"].(string); ok {
			text.WriteString(textStr)
		}
		if chunk.Choices[0].FinishReason == "stop" {
			sawFinishChunk = true
		}
	}
	if !sawRoleChunk {
		t.Errorf("missing first chunk with delta.role=assistant:\n%s", body)
	}
	if !sawFinishChunk {
		t.Errorf("missing final chunk with finish_reason=stop:\n%s", body)
	}
	if text.String() != "Hello" {
		t.Errorf("streamed content = %q, want %q", text.String(), "Hello")
	}
}

// TestOpenAIStreamState_ToolCalls pins tool_use streaming: block start
// announces the call, input_json_delta appends arguments, message_delta sets
// finish_reason, message_stop closes the stream.
func TestOpenAIStreamState_ToolCalls(t *testing.T) {
	state := newOpenAIStreamState("m")
	feed := func(t *testing.T, event string, data string) []map[string]any {
		var dataObj map[string]any
		if err := json.Unmarshal([]byte(data), &dataObj); err != nil {
			t.Fatalf("fixture invalid: %v", err)
		}
		return state.HandleEvent(event, dataObj)
	}

	// message_start emits the role chunk
	roleChunks := feed(t, "message_start", `{"type":"message_start","message":{"id":"msg_t","usage":{"input_tokens":3}}}`)
	if len(roleChunks) != 1 || roleChunks[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["role"] != "assistant" {
		t.Fatalf("message_start chunks = %v", roleChunks)
	}

	// tool_use block start announces the call
	startChunks := feed(t, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_9","name":"get_weather"}}`)
	if len(startChunks) != 1 {
		t.Fatalf("content_block_start emitted %d chunks, want 1", len(startChunks))
	}
	delta0 := startChunks[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	toolCalls0, _ := delta0["tool_calls"].([]any)
	if len(toolCalls0) != 1 {
		t.Fatalf("tool_calls announce length = %d, want 1", len(toolCalls0))
	}
	call0 := toolCalls0[0].(map[string]any)
	if !numberEq(call0["index"], 0) || call0["id"] != "toolu_9" || call0["type"] != "function" {
		t.Errorf("tool call announce = %v", call0)
	}
	if fn := call0["function"].(map[string]any); fn["name"] != "get_weather" {
		t.Errorf("announced name = %v, want get_weather", fn["name"])
	}

	// argument fragments stream under the same tool index
	argChunks := feed(t, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`)
	if len(argChunks) != 1 {
		t.Fatalf("input_json_delta emitted %d chunks, want 1", len(argChunks))
	}
	argChoice := argChunks[0]["choices"].([]any)[0].(map[string]any)
	argCalls, _ := argChoice["delta"].(map[string]any)["tool_calls"].([]any)
	if len(argCalls) != 1 {
		t.Fatalf("argument delta tool_calls length = %d, want 1", len(argCalls))
	}
	argCall := argCalls[0].(map[string]any)
	if !numberEq(argCall["index"], 0) {
		t.Errorf("argument delta index = %v, want 0", argCall["index"])
	}
	if argCall["arguments"] != "{\"city\":" {
		t.Errorf("argument fragment = %v, want {\"city\":", argCall["arguments"])
	}

	// message_delta carries no chunk; finish lands on the final chunk
	stopDelta := feed(t, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`)
	if len(stopDelta) != 0 {
		t.Errorf("message_delta emitted %d chunks, want 0 (finish applied to final chunk)", len(stopDelta))
	}
	finalChunks := feed(t, "message_stop", `{"type":"message_stop"}`)
	if len(finalChunks) != 1 {
		t.Fatalf("message_stop emitted %d chunks, want 1", len(finalChunks))
	}
	finalChoice := finalChunks[0]["choices"].([]any)[0].(map[string]any)
	if finalChoice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", finalChoice["finish_reason"])
	}
	if finalChoice["delta"].(map[string]any)["tool_calls"] != nil {
		t.Errorf("final chunk must not re-announce tool calls: %v", finalChoice["delta"])
	}
}

// TestSSELineParser_SplitFrames feeds SSE frames split across arbitrary Write
// boundaries (including mid-JSON) and requires each frame to dispatch exactly
// once, whole.
func TestSSELineParser_SplitFrames(t *testing.T) {
	type receivedEvent struct {
		event string
		data  map[string]any
	}
	var received []receivedEvent
	parser := sseLineParser{}
	handle := func(eventType string, data map[string]any) {
		received = append(received, receivedEvent{event: eventType, data: data})
	}

	parser.feed([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"te"), handle)
	parser.feed([]byte("xt\":\"split\"}}\n\nevent: ping\nda"), handle)
	parser.feed([]byte("ta: {\"type\":\"ping\"}\n\n"), handle)

	if len(received) != 2 {
		t.Fatalf("dispatched %d events, want 2: %+v", len(received), received)
	}
	if received[0].event != "content_block_delta" {
		t.Errorf("first event = %q", received[0].event)
	}
	if text := received[0].data["delta"].(map[string]any)["text"]; text != "split" {
		t.Errorf("reassembled text = %v, want split", text)
	}
	if received[1].event != "ping" {
		t.Errorf("second event = %v, want ping", received[1].event)
	}
}

// TestOpenAIStreamState_Thinking pins reasoning_content streaming.
func TestOpenAIStreamState_Thinking(t *testing.T) {
	state := newOpenAIStreamState("m")
	var dataObj map[string]any
	_ = json.Unmarshal([]byte(`{"type":"message_start","message":{"id":"msg_r"}}`), &dataObj)
	chunks := state.HandleEvent("message_start", dataObj)
	if len(chunks) != 1 {
		t.Fatalf("message_start chunks = %d, want 1", len(chunks))
	}

	dataObj = nil
	_ = json.Unmarshal([]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reasoning bit"}}`), &dataObj)
	chunks = state.HandleEvent("content_block_delta", dataObj)
	if len(chunks) != 1 {
		t.Fatalf("thinking delta emitted %d chunks, want 1", len(chunks))
	}
	if got := chunks[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["reasoning_content"]; got != "reasoning bit" {
		t.Errorf("reasoning_content = %v, want %q", got, "reasoning bit")
	}
}

// TestOpenAIChatCompletions_OversizedBody caps the request body the same way
// /v1/messages does: bodies beyond maxRequestBody are rejected with 413 and an
// OpenAI error envelope instead of being buffered in full.
func TestOpenAIChatCompletions_OversizedBody(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: &mockCloudCodeBackend{},
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	oversized := strings.Repeat("a", maxRequestBody+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(oversized))
	req.Header.Set("Authorization", "Bearer test-proxy-key")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	var errBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if _, ok := errBody["error"].(map[string]any); !ok {
		t.Errorf("expected OpenAI error envelope, got %v", errBody)
	}
}

// numberEq compares a decoded JSON number to an int regardless of whether the
// value came from json.Unmarshal (float64) or was built in memory (int).
func numberEq(value any, want int) bool {
	return numberToInt(value) == want
}
