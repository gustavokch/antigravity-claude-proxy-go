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

// TestOpenRouterForward_RequireParametersForForcedTool asserts that a request
// carrying a forced tool_choice reaches OpenRouter with
// provider.require_parameters set, so the endpoint chosen upstream is one that
// actually honours forced tool choice. Without it, an endpoint with no
// capability metadata passes the proxy's fail-open filter and can silently
// ignore the constraint — the tool bypass observed in the spike.
func TestOpenRouterForward_RequireParametersForForcedTool(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"tool_use","id":"t1","name":"final_answer_mcq","input":{"answer":"C"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`))
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

	reqPayload := `{"model":"inclusionai/ling-3.0-flash-sante:free","max_tokens":128,` +
		`"messages":[{"role":"user","content":"Which one?"}],` +
		`"tools":[{"type":"function","function":{"name":"final_answer_mcq","strict":true,` +
		`"parameters":{"type":"object","properties":{"answer":{"type":"string"}},"additionalProperties":false}}}],` +
		`"tool_choice":{"type":"function","function":{"name":"final_answer_mcq"}}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-proxy-key")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	providerBlock, _ := receivedBody["provider"].(map[string]any)
	if providerBlock == nil {
		t.Fatalf("no provider block forwarded: %v", receivedBody)
	}
	if providerBlock["require_parameters"] != true {
		t.Errorf("provider.require_parameters = %v, want true", providerBlock["require_parameters"])
	}

	// The client declared this tool itself, so it must still come back as a
	// tool call — the structured-output unwrap must not touch it.
	encoded, _ := json.Marshal(receivedBody["tools"])
	if strings.Contains(string(encoded), "additionalProperties") {
		t.Errorf("additionalProperties reached upstream: %s", encoded)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	first := got["choices"].([]any)[0].(map[string]any)
	if stringFrom(first["finish_reason"]) != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls for a client-declared tool", first["finish_reason"])
	}
	msg := first["message"].(map[string]any)
	if _, ok := msg["tool_calls"]; !ok {
		t.Errorf("client-declared tool must surface as tool_calls: %v", msg)
	}
}

// TestOpenRouterForward_NoRequireParametersForAutoToolChoice guards the blast
// radius of the change above: ordinary agentic traffic carries tools with an
// absent or "auto" tool_choice, is not broken by an endpoint that ignores the
// field, and must not be narrowed to endpoints advertising it — that would
// only expose working traffic to 404s from an incomplete catalog.
func TestOpenRouterForward_NoRequireParametersForAutoToolChoice(t *testing.T) {
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
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
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

	reqPayload := `{"model":"inclusionai/ling-3.0-flash-sante:free","max_tokens":128,` +
		`"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],` +
		`"tool_choice":"auto"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqPayload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-proxy-key")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if providerBlock, ok := receivedBody["provider"].(map[string]any); ok {
		if providerBlock["require_parameters"] == true {
			t.Errorf("auto tool_choice must not set require_parameters: %v", providerBlock)
		}
	}
}
