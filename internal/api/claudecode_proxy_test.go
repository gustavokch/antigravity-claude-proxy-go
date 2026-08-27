package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/headroom"
)

// fakeMsgServer returns an httptest server that emulates Anthropic /v1/messages.
func fakeMsgServer(t *testing.T, statusCode int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" {
			http.Error(w, "missing x-api-key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(body))
	}))
}

func TestForwardToClaudeCode_Success(t *testing.T) {
	upstream := fakeMsgServer(t, http.StatusOK, `{"id":"msg_01","type":"message","content":[{"type":"text","text":"hello"}]}`)
	defer upstream.Close()

	cfg := claudecode.Config{
		Enabled: true,
		BaseURL: upstream.URL,
		Mode:    "pool",
		Accounts: []claudecode.AccountConfig{
			{ID: "acc1", Token: "sk-ant-test", Enabled: true},
		},
		Allowlist: claudecode.DefaultAllowlist(),
		Routing:   claudecode.DefaultRoutingConfig(),
	}

	// Reset pool to pick up test upstream.
	ccPoolMu.Lock()
	ccPoolInst = nil
	ccHTTPClient = nil
	ccPoolMu.Unlock()

	pool, client := getOrCreateCCPool(cfg)
	_ = pool
	_ = client

	reqBody := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv := &Server{}
	srv.forwardToClaudeCode(w, req, cfg, []byte(reqBody), "claude-sonnet-5")

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["type"] != "message" {
		t.Errorf("expected type=message, got %v", resp["type"])
	}
}

func TestForwardToClaudeCode_RateLimitFailover(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_02","type":"message"}`))
	}))
	defer upstream.Close()

	cfg := claudecode.Config{
		Enabled: true,
		BaseURL: upstream.URL,
		Mode:    "pool",
		Accounts: []claudecode.AccountConfig{
			{ID: "acc-a", Token: "sk-ant-a", Enabled: true, Priority: 1},
			{ID: "acc-b", Token: "sk-ant-b", Enabled: true, Priority: 2},
		},
		Allowlist: claudecode.DefaultAllowlist(),
		Routing:   claudecode.DefaultRoutingConfig(),
	}

	ccPoolMu.Lock()
	ccPoolInst = nil
	ccHTTPClient = nil
	ccPoolMu.Unlock()

	reqBody := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	srv := &Server{}
	srv.forwardToClaudeCode(w, req, cfg, []byte(reqBody), "claude-sonnet-5")

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 after failover, got %d", w.Code)
	}
	if calls < 2 {
		t.Errorf("expected at least 2 upstream calls (1 rate-limited + 1 success), got %d", calls)
	}
}

func TestMatchClaudeCodeModel_AllowlistAndAlias(t *testing.T) {
	cfg := claudecode.Config{
		Allowlist: claudecode.DefaultAllowlist(),
	}

	cases := []struct {
		input string
		want  string // empty = no match
	}{
		{"claude-sonnet-5", "claude-sonnet-5"},
		{"claude-haiku-4-5", "claude-haiku-4-5-20251001"}, // alias
		{"gpt-4o", ""},                                     // not in allowlist
		{"", ""},
	}

	for _, tc := range cases {
		got := matchClaudeCodeModel(cfg, tc.input)
		if got != tc.want {
			t.Errorf("matchClaudeCodeModel(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestClaudeCodeConfigInDefaultConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.ClaudeCode.Enabled != false {
		t.Error("default ClaudeCode.Enabled should be false")
	}
	if cfg.ClaudeCode.BaseURL != claudecode.DefaultBaseURL {
		t.Errorf("default BaseURL = %q, want %q", cfg.ClaudeCode.BaseURL, claudecode.DefaultBaseURL)
	}
	if len(cfg.ClaudeCode.Allowlist) == 0 {
		t.Error("default allowlist should be non-empty")
	}
}

func TestForwardToClaudeCode_Non2xxStatusRecording(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"type":"api_error","message":"internal error"}}`))
	}))
	defer upstream.Close()

	cfg := claudecode.Config{
		Enabled: true,
		BaseURL: upstream.URL,
		Mode:    "pool",
		Accounts: []claudecode.AccountConfig{
			{ID: "acc-err", Token: "sk-ant-err", Enabled: true},
		},
		Allowlist: claudecode.DefaultAllowlist(),
		Routing:   claudecode.DefaultRoutingConfig(),
	}

	ccPoolMu.Lock()
	ccPoolInst = nil
	ccHTTPClient = nil
	ccPoolMu.Unlock()

	pool, _ := getOrCreateCCPool(cfg)

	reqBody := `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	srv := &Server{}
	srv.forwardToClaudeCode(w, req, cfg, []byte(reqBody), "claude-sonnet-5")

	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 status forwarded, got %d", w.Code)
	}

	acc, ok := pool.GetAccount("acc-err")
	if !ok {
		t.Fatalf("account not found in pool")
	}
	if acc.ConsecutiveFailures == 0 || acc.TotalErrors == 0 {
		t.Errorf("expected account failures/errors incremented on 500 upstream response, got failures=%d errors=%d", acc.ConsecutiveFailures, acc.TotalErrors)
	}
}

func TestForwardToClaudeCode_CCRHydration_Streaming(t *testing.T) {
	var chunkID string
	var callCount int32
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		curr := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)

		if curr == 1 {
			// Turn 1: headroom_retrieve
			fmt.Fprintf(w, "event: message_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_cc1\",\"role\":\"assistant\",\"model\":\"claude-sonnet-5\",\"usage\":{\"input_tokens\":40,\"output_tokens\":8}}}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_cc1\",\"name\":\"headroom_retrieve\",\"input\":{}}}\n\n")

			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"chunk_id\\\":\\\"%s\\\"}\"}}\n\n", chunkID)

			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

			fmt.Fprintf(w, "event: message_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":10}}\n\n")

			fmt.Fprintf(w, "event: message_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
		} else {
			// Turn 2: Hydrated text answer
			fmt.Fprintf(w, "event: message_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_cc2\",\"role\":\"assistant\",\"model\":\"claude-sonnet-5\",\"usage\":{\"input_tokens\":100,\"output_tokens\":15}}}\n\n")

			fmt.Fprintf(w, "event: content_block_start\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")

			fmt.Fprintf(w, "event: content_block_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Claude Code answer with hydrated context.\"}}\n\n")

			fmt.Fprintf(w, "event: content_block_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")

			fmt.Fprintf(w, "event: message_delta\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":15}}\n\n")

			fmt.Fprintf(w, "event: message_stop\n")
			fmt.Fprintf(w, "data: {\"type\":\"message_stop\"}\n\n")
		}
	}))
	defer mockUpstream.Close()

	cfg := claudecode.Config{
		Enabled:   true,
		BaseURL:   mockUpstream.URL,
		Accounts:  []claudecode.AccountConfig{{ID: "acc-cc", Token: "tok-cc", Enabled: true}},
		Allowlist: []claudecode.ModelConfig{{ID: "claude-sonnet-5", Enabled: true}},
		Routing:   claudecode.DefaultRoutingConfig(),
	}

	ccPoolMu.Lock()
	ccPoolInst = nil
	ccHTTPClient = nil
	ccPoolMu.Unlock()

	headroomEngine := headroom.NewEngine(headroom.Config{
		Enabled: true,
		CCR: headroom.CCRConfig{
			Enabled: true,
		},
	})
	var ok bool
	chunkID, ok = headroomEngine.CCRStore().Put("Secret Claude Code payload")
	if !ok {
		t.Fatalf("Failed to put chunk into CCRStore")
	}

	srv := &Server{
		headroom: headroomEngine,
	}

	reqBody := `{"model":"claude-sonnet-5","stream":true,"messages":[{"role":"user","content":"Fetch chunk"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	srv.forwardToClaudeCode(w, req, cfg, []byte(reqBody), "claude-sonnet-5")

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if atomic.LoadInt32(&callCount) != 2 {
		t.Fatalf("Expected 2 calls to Claude Code backend, got %d", callCount)
	}

	respBody := w.Body.String()
	if strings.Contains(respBody, "headroom_retrieve") {
		t.Fatalf("Leaked headroom_retrieve tool_use: %s", respBody)
	}
	if !strings.Contains(respBody, "Claude Code answer with hydrated context.") {
		t.Fatalf("Missing hydrated text in stream: %s", respBody)
	}
	if !strings.Contains(respBody, "\"output_tokens\":25") {
		t.Fatalf("Expected patched output_tokens 25, got: %s", respBody)
	}
}

func TestForwardToClaudeCode_CCRHydration_Unary(t *testing.T) {
	var chunkID string
	var callCount int32
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		curr := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if curr == 1 {
			resp := map[string]any{
				"id":    "msg_cc_u1",
				"type":  "message",
				"role":  "assistant",
				"model": "claude-sonnet-5",
				"content": []any{
					map[string]any{
						"type":  "tool_use",
						"id":    "toolu_cc_u1",
						"name":  "headroom_retrieve",
						"input": map[string]any{"chunk_id": chunkID},
					},
				},
				"stop_reason": "tool_use",
				"usage": map[string]any{
					"input_tokens":  60,
					"output_tokens": 10,
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		} else {
			resp := map[string]any{
				"id":    "msg_cc_u2",
				"type":  "message",
				"role":  "assistant",
				"model": "claude-sonnet-5",
				"content": []any{
					map[string]any{
						"type": "text",
						"text": "Unary Claude Code hydrated response",
					},
				},
				"stop_reason": "end_turn",
				"usage": map[string]any{
					"input_tokens":  120,
					"output_tokens": 14,
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		}
	}))
	defer mockUpstream.Close()

	cfg := claudecode.Config{
		Enabled:   true,
		BaseURL:   mockUpstream.URL,
		Accounts:  []claudecode.AccountConfig{{ID: "acc-cc", Token: "tok-cc", Enabled: true}},
		Allowlist: []claudecode.ModelConfig{{ID: "claude-sonnet-5", Enabled: true}},
		Routing:   claudecode.DefaultRoutingConfig(),
	}

	ccPoolMu.Lock()
	ccPoolInst = nil
	ccHTTPClient = nil
	ccPoolMu.Unlock()

	headroomEngine := headroom.NewEngine(headroom.Config{
		Enabled: true,
		CCR: headroom.CCRConfig{
			Enabled: true,
		},
	})
	var ok bool
	chunkID, ok = headroomEngine.CCRStore().Put("Secret Claude Code Unary payload")
	if !ok {
		t.Fatalf("Failed to put chunk into CCRStore")
	}

	srv := &Server{
		headroom: headroomEngine,
	}

	reqBody := `{"model":"claude-sonnet-5","stream":false,"messages":[{"role":"user","content":"Fetch chunk"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	srv.forwardToClaudeCode(w, req, cfg, []byte(reqBody), "claude-sonnet-5")

	if w.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	if atomic.LoadInt32(&callCount) != 2 {
		t.Fatalf("Expected 2 calls to Claude Code backend, got %d", callCount)
	}

	var respMap map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &respMap); err != nil {
		t.Fatalf("Failed to unmarshal unary response: %v", err)
	}

	content, _ := respMap["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("Expected 1 content block, got: %v", content)
	}
	firstBlock, _ := content[0].(map[string]any)
	if firstBlock["text"] != "Unary Claude Code hydrated response" {
		t.Fatalf("Unexpected content text: %v", firstBlock["text"])
	}

	usage, _ := respMap["usage"].(map[string]any)
	if usage["output_tokens"].(float64) != 24 { // 10 + 14 = 24
		t.Fatalf("Expected output_tokens 24, got %v", usage["output_tokens"])
	}
}
