package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
)

// mcqSchema is the schema shape from the OpenRouter spike: a closed object
// (additionalProperties:false) as emitted by OpenAI strict mode.
func mcqSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"answer":    map[string]any{"type": "string", "enum": []any{"A", "B", "C", "D"}},
			"rationale": map[string]any{"type": "string"},
		},
		"required":             []any{"answer"},
		"additionalProperties": false,
	}
}

// TestTranslateOpenAIRequest_JSONSchemaForcesSyntheticTool covers the plain
// structured-output case: no client tools, so the schema is injected as a
// synthetic tool and forced, giving real enforcement on an upstream that has
// no response_format field.
func TestTranslateOpenAIRequest_JSONSchemaForcesSyntheticTool(t *testing.T) {
	in := map[string]any{
		"model":    "inclusionai/ling-3.0-flash-sante:free",
		"messages": []any{map[string]any{"role": "user", "content": "Which one?"}},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "final_answer_mcq",
				"strict": true,
				"schema": mcqSchema(),
			},
		},
	}
	out, err := translateOpenAIRequest(in)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 synthetic tool, got %d (%v)", len(tools), out["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if got := stringFrom(tool["name"]); got != "final_answer_mcq" {
		t.Errorf("synthetic tool name = %q, want final_answer_mcq", got)
	}
	schema, _ := tool["input_schema"].(map[string]any)
	if schema == nil {
		t.Fatalf("synthetic tool has no input_schema")
	}
	if _, present := schema["additionalProperties"]; present {
		t.Errorf("additionalProperties must be stripped, got %v", schema)
	}
	props, _ := schema["properties"].(map[string]any)
	if _, ok := props["answer"]; !ok {
		t.Errorf("schema properties lost: %v", schema)
	}

	choice, _ := out["tool_choice"].(map[string]any)
	if stringFrom(choice["type"]) != "tool" || stringFrom(choice["name"]) != "final_answer_mcq" {
		t.Errorf("tool_choice = %v, want forced tool final_answer_mcq", out["tool_choice"])
	}
}

// TestTranslateOpenAIRequest_JSONSchemaWithClientTools asserts the emulation
// never hijacks a client that is running its own tool loop: forcing a
// synthetic tool would make the client's tools unreachable, so the schema is
// described in the system prompt instead and the client's tools stand.
func TestTranslateOpenAIRequest_JSONSchemaWithClientTools(t *testing.T) {
	in := map[string]any{
		"model":    "x/y",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}},
		}},
		"response_format": map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": "final_answer_mcq", "schema": mcqSchema()},
		},
	}
	out, err := translateOpenAIRequest(in)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("client tools must be preserved untouched, got %v", out["tools"])
	}
	if name := stringFrom(tools[0].(map[string]any)["name"]); name != "lookup" {
		t.Errorf("tool name = %q, want lookup", name)
	}
	if _, forced := out["tool_choice"]; forced {
		t.Errorf("must not force a tool when the client has its own tools: %v", out["tool_choice"])
	}
	system := stringFrom(out["system"])
	if !strings.Contains(system, "final_answer_mcq") || !strings.Contains(system, "\"answer\"") {
		t.Errorf("system prompt must carry the schema, got %q", system)
	}
}

// TestTranslateOpenAIRequest_JSONObject covers response_format json_object,
// which carries no schema and so can only be a prompt instruction.
func TestTranslateOpenAIRequest_JSONObject(t *testing.T) {
	in := map[string]any{
		"model":           "x/y",
		"messages":        []any{map[string]any{"role": "user", "content": "hi"}},
		"system":          nil,
		"response_format": map[string]any{"type": "json_object"},
	}
	out, err := translateOpenAIRequest(in)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if _, present := out["tools"]; present {
		t.Errorf("json_object must not inject a tool: %v", out["tools"])
	}
	if system := stringFrom(out["system"]); !strings.Contains(strings.ToLower(system), "json") {
		t.Errorf("json_object must add a JSON instruction, got %q", system)
	}
}

// TestTranslateOpenAIRequest_StripsAdditionalPropertiesFromClientTools is the
// direct regression for the strict:true degradation. OpenAI strict mode emits
// additionalProperties:false; the proxy drops the accompanying strict flag,
// and nothing on the Anthropic upstream enforces the keyword, so it is pure
// schema weight. It must not reach upstream.
func TestTranslateOpenAIRequest_StripsAdditionalPropertiesFromClientTools(t *testing.T) {
	nested := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items": map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
			},
		},
		"additionalProperties": false,
	}
	in := map[string]any{
		"model":    "x/y",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{map[string]any{
			"type":     "function",
			"function": map[string]any{"name": "final_answer_mcq", "strict": true, "parameters": nested},
		}},
	}
	out, err := translateOpenAIRequest(in)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	encoded, _ := json.Marshal(out["tools"])
	if strings.Contains(string(encoded), "additionalProperties") {
		t.Errorf("additionalProperties survived at some depth: %s", encoded)
	}
	if !strings.Contains(string(encoded), "\"items\"") {
		t.Errorf("stripping damaged the schema: %s", encoded)
	}
}

// TestOpenAIChatCompletions_StructuredOutputUnary is the end-to-end proof: a
// client asking for json_schema gets JSON in message.content, not a tool call
// it never asked for.
func TestOpenAIChatCompletions_StructuredOutputUnary(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"tool_use","id":"toolu_1","name":"final_answer_mcq","input":{"answer":"B"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":7}}`))
	}))
	defer mockOR.Close()

	if _, err := config.Save(map[string]any{
		"openrouter": map[string]any{
			"enabled":   true,
			"apiKey":    "sk-or-v1-secret-123",
			"baseUrl":   mockOR.URL,
			"allowlist": []map[string]any{{"id": "inclusionai/ling-3.0-flash-sante:free", "enabled": true}},
		},
	}); err != nil {
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

	reqPayload := `{"model":"inclusionai/ling-3.0-flash-sante:free","max_tokens":256,` +
		`"messages":[{"role":"user","content":"Which one?"}],` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"final_answer_mcq","strict":true,` +
		`"schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}}}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-proxy-key")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Upstream must have been asked to force the synthetic tool.
	choice, _ := receivedBody["tool_choice"].(map[string]any)
	if stringFrom(choice["type"]) != "tool" || stringFrom(choice["name"]) != "final_answer_mcq" {
		t.Errorf("upstream tool_choice = %v, want forced tool", receivedBody["tool_choice"])
	}
	// The forced choice here is the proxy's own synthetic one, so the request
	// must also carry provider.require_parameters — the fail-open capability
	// filter would otherwise let it land on an endpoint that ignores it.
	providerBlock, _ := receivedBody["provider"].(map[string]any)
	if providerBlock == nil || providerBlock["require_parameters"] != true {
		t.Errorf("provider.require_parameters = %v, want true on the response_format path", receivedBody["provider"])
	}

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	choices, _ := got["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v", got["choices"])
	}
	first, _ := choices[0].(map[string]any)
	if reason := stringFrom(first["finish_reason"]); reason != "stop" {
		t.Errorf("finish_reason = %q, want stop (the client asked for content, not a tool call)", reason)
	}
	msg, _ := first["message"].(map[string]any)
	if _, leaked := msg["tool_calls"]; leaked {
		t.Errorf("synthetic tool leaked to the client as tool_calls: %v", msg)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stringFrom(msg["content"])), &payload); err != nil {
		t.Fatalf("message.content is not valid JSON (%q): %v", msg["content"], err)
	}
	if payload["answer"] != "B" {
		t.Errorf("content = %v, want answer B", payload)
	}
}

// TestOpenAIStreamState_StructuredOutput asserts the streaming path unwraps
// the synthetic tool into content deltas rather than tool_call deltas.
func TestOpenAIStreamState_StructuredOutput(t *testing.T) {
	state := newOpenAIStreamState("m")
	state.structuredToolName = "final_answer_mcq"

	state.HandleEvent("message_start", map[string]any{
		"message": map[string]any{"id": "msg_1", "usage": map[string]any{"input_tokens": 3}},
	})
	if chunks := state.HandleEvent("content_block_start", map[string]any{
		"index":         0,
		"content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "final_answer_mcq"},
	}); len(chunks) != 0 {
		t.Errorf("synthetic tool must not emit a tool_calls chunk, got %v", chunks)
	}

	chunks := state.HandleEvent("content_block_delta", map[string]any{
		"index": 0,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"answer":`},
	})
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %v", chunks)
	}
	delta, _ := chunks[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if _, leaked := delta["tool_calls"]; leaked {
		t.Errorf("structured deltas must not be tool_calls: %v", delta)
	}
	if delta["content"] != `{"answer":` {
		t.Errorf("content delta = %v, want the raw JSON fragment", delta["content"])
	}

	state.HandleEvent("message_delta", map[string]any{"delta": map[string]any{"stop_reason": "tool_use"}})
	stop := state.HandleEvent("message_stop", map[string]any{})
	if len(stop) != 1 {
		t.Fatalf("expected final chunk, got %v", stop)
	}
	if reason := stop[0]["choices"].([]any)[0].(map[string]any)["finish_reason"]; reason != "stop" {
		t.Errorf("finish_reason = %v, want stop", reason)
	}
	if !state.done {
		t.Errorf("state.done must be true after message_stop")
	}
}

// TestUnwrapStructuredOutput_EmptyArguments ensures that even when the synthetic
// tool call returns empty string arguments, it is unwrapped and removed from tool_calls.
func TestUnwrapStructuredOutput_EmptyArguments(t *testing.T) {
	completion := map[string]any{
		"choices": []any{
			map[string]any{
				"finish_reason": "tool_calls",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []any{
						map[string]any{
							"id":   "toolu_1",
							"type": "function",
							"function": map[string]any{
								"name":      "final_answer_mcq",
								"arguments": "",
							},
						},
					},
				},
			},
		},
	}
	unwrapStructuredOutput(completion, "final_answer_mcq")
	choice := completion["choices"].([]any)[0].(map[string]any)
	if reason := choice["finish_reason"]; reason != "stop" {
		t.Errorf("finish_reason = %v, want stop", reason)
	}
	msg := choice["message"].(map[string]any)
	if _, present := msg["tool_calls"]; present {
		t.Errorf("tool_calls should be deleted, got %v", msg["tool_calls"])
	}
	if content, ok := msg["content"].(string); !ok || content != "" {
		t.Errorf("message.content = %v, want empty string", msg["content"])
	}
}

// TestTranslateAnthropicMessageToOpenAI_MalformedContent asserts that a nil or
// non-slice content field does not panic and translates safely to an empty message.
func TestTranslateAnthropicMessageToOpenAI_MalformedContent(t *testing.T) {
	cases := []map[string]any{
		{"id": "msg_nil", "stop_reason": "end_turn"},
		{"id": "msg_null", "content": nil, "stop_reason": "end_turn"},
		{"id": "msg_str", "content": "not-a-slice", "stop_reason": "end_turn"},
	}
	for _, tc := range cases {
		out := translateAnthropicMessageToOpenAI(tc, "test-model", time.Now().Unix())
		choices, _ := out["choices"].([]any)
		if len(choices) != 1 {
			t.Fatalf("choices = %v, want 1 choice", choices)
		}
		msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
		if msg == nil {
			t.Fatalf("message missing in choices")
		}
		if msg["content"] != "" {
			t.Errorf("content = %v, want empty string", msg["content"])
		}
	}
}

// TestTranslateOpenAIRequest_ToolWithoutParameters covers the OpenAI shape
// where function.parameters is optional. The Anthropic tool shape requires
// input_schema, so translation must substitute an empty object schema instead
// of omitting the field — an omission the upstream rejects with 400.
func TestTranslateOpenAIRequest_ToolWithoutParameters(t *testing.T) {
	in := map[string]any{
		"model":    "x/y",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "ping"}},
			map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": nil}},
		},
	}
	out, err := translateOpenAIRequest(in)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("expected both tools translated, got %v", out["tools"])
	}
	for i, raw := range tools {
		tool, _ := raw.(map[string]any)
		schema, ok := tool["input_schema"].(map[string]any)
		if !ok {
			t.Fatalf("tool %d has no input_schema: %v", i, tool)
		}
		if schema["type"] != "object" {
			t.Errorf("tool %d input_schema.type = %v, want object", i, schema["type"])
		}
		if props, ok := schema["properties"].(map[string]any); !ok || len(props) != 0 {
			t.Errorf("tool %d input_schema.properties = %v, want empty object", i, schema["properties"])
		}
	}
}
