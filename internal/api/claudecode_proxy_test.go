package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/headroom"
	"antigravity-go-proxy/internal/headroom/stages/ccr"
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
		{"sonnet-5", "claude-sonnet-5"},
		{"claude-opus-5", "claude-opus-5"},
		{"opus-5", "claude-opus-5"},
		{"claude-fable-5", "claude-fable-5"},
		{"fable-5", "claude-fable-5"},
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001"},
		{"claude-haiku-4-5", "claude-haiku-4-5-20251001"},
		{"haiku-4-5", "claude-haiku-4-5-20251001"},
		{"claude-haiku-4.5", "claude-haiku-4-5-20251001"},
		{"haiku-4.5", "claude-haiku-4-5-20251001"},
		{"claude-3-7-sonnet-20250219", "claude-3-7-sonnet-20250219"},
		{"claude-3-7-sonnet", "claude-3-7-sonnet-20250219"},
		{"claude-3.7-sonnet", "claude-3-7-sonnet-20250219"},
		{"sonnet-3-7", "claude-3-7-sonnet-20250219"},
		{"sonnet-3.7", "claude-3-7-sonnet-20250219"},
		{"claude-3-5-sonnet", "claude-3-5-sonnet-20241022"},
		{"claude-3.5-sonnet", "claude-3-5-sonnet-20241022"},
		{"sonnet-3-5", "claude-3-5-sonnet-20241022"},
		{"claude-3-5-haiku", "claude-3-5-haiku-20241022"},
		{"haiku-3-5", "claude-3-5-haiku-20241022"},
		{"claude-3-opus", "claude-3-opus-20240229"},
		{"opus-3", "claude-3-opus-20240229"},
		{"gpt-4o", ""}, // not in allowlist
		{"", ""},
	}

	for _, tc := range cases {
		got := matchClaudeCodeModel(cfg, tc.input)
		if got != tc.want {
			t.Errorf("matchClaudeCodeModel(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}

	// Test fallback when allowlist is empty in config (defaults to DefaultAllowlist)
	emptyCfg := claudecode.Config{}
	if got := matchClaudeCodeModel(emptyCfg, "claude-sonnet-5"); got != "claude-sonnet-5" {
		t.Errorf("expected empty cfg to match default allowlist, got %q", got)
	}
	if got := matchClaudeCodeModel(emptyCfg, "sonnet-5"); got != "claude-sonnet-5" {
		t.Errorf("expected empty cfg to match alias on default allowlist, got %q", got)
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

	store := ccr.NewCCRStore(1024 * 1024)
	headroomEngine := headroom.NewEngine(headroom.Config{
		Enabled: true,
		CCR: headroom.CCRConfig{
			Enabled: true,
		},
	}, nil, ccr.NewStage(store))
	var ok bool
	chunkID, ok = store.Put("Secret Claude Code payload")
	if !ok {
		t.Fatalf("Failed to put chunk into CCRStore")
	}

	srv := &Server{
		headroom: headroomEngine,
		ccrStore: store,
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

	store := ccr.NewCCRStore(1024 * 1024)
	headroomEngine := headroom.NewEngine(headroom.Config{
		Enabled: true,
		CCR: headroom.CCRConfig{
			Enabled: true,
		},
	}, nil, ccr.NewStage(store))
	var ok bool
	chunkID, ok = store.Put("Secret Claude Code Unary payload")
	if !ok {
		t.Fatalf("Failed to put chunk into CCRStore")
	}

	srv := &Server{
		headroom: headroomEngine,
		ccrStore: store,
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

func TestForwardToClaudeCode_AutoRefreshOn401(t *testing.T) {
	reqCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		authHdr := r.Header.Get("Authorization")
		if reqCount == 1 {
			// First request returns 401 expired token
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"OAuth token has expired"}}`))
			return
		}
		// Second request with refreshed token succeeds
		if authHdr != "Bearer sk-ant-oat01-refreshed-token" {
			t.Errorf("expected Bearer sk-ant-oat01-refreshed-token, got %s", authHdr)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_123","type":"message","usage":{"input_tokens":10,"output_tokens":20}}`))
	}))
	defer ts.Close()

	oauthMgr := auth.NewClaudeCodeOAuthManager()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "sk-ant-oat01-refreshed-token",
			"refresh_token": "new-refresh-token",
			"expires_in":    3600,
		})
	}))
	defer tokenSrv.Close()
	oauthMgr.SetEndpoints("", tokenSrv.URL, "", nil)

	srv, err := New(Options{
		APIKey: "test-key",
		Credentials: func(ctx context.Context) (auth.Credentials, error) {
			return auth.Credentials{AccessToken: "token"}, nil
		},
		NewUpstream:        func(s string) Upstream { return nil },
		ClaudeCodeOAuthMgr: oauthMgr,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	ccPoolMu.Lock()
	ccPoolInst = nil
	ccHTTPClient = nil
	ccPoolMu.Unlock()

	cfg := claudecode.Config{
		Enabled: true,
		BaseURL: ts.URL,
		Accounts: []claudecode.AccountConfig{
			{
				ID:           "acc-oauth",
				Name:         "OAuth Acc",
				Token:        "expired-token",
				RefreshToken: "valid-refresh",
				Type:         "oauth",
				Priority:     1,
				Enabled:      true,
			},
		},
		Allowlist: claudecode.DefaultAllowlist(),
		Routing:   claudecode.DefaultRoutingConfig(),
	}

	reqBody := []byte(`{"model":"claude-3-7-sonnet-20250219","messages":[{"role":"user","content":"hello"}]}`)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))
	w := httptest.NewRecorder()

	srv.forwardToClaudeCode(w, httpReq, cfg, reqBody, "claude-3-7-sonnet-20250219")

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200 OK after auto-refresh retry, got %d: %s", w.Code, w.Body.String())
	}
	if reqCount != 2 {
		t.Errorf("expected 2 upstream requests (initial 401 + retry), got %d", reqCount)
	}
}

func TestSyncRefreshedAccountToConfig_PreservesAllowlistAndRouting(t *testing.T) {
	origCfg := config.Get()
	defer config.SetForTest(origCfg)

	allowlist := []claudecode.ModelConfig{
		{
			ID:      "claude-custom-1",
			Aliases: []string{"custom-1"},
			Enabled: true,
		},
	}
	routing := claudecode.RoutingConfig{
		Retry429Max:      7,
		BackoffBaseMs:    1500,
		BackoffCapMs:     40000,
		RequestBudgetMs:  180000,
		CooldownDuration: 45000,
	}

	config.SetForTest(config.Config{
		ClaudeCode: claudecode.Config{
			Enabled: true,
			BaseURL: "https://api.anthropic.com",
			Mode:    "pool",
			Accounts: []claudecode.AccountConfig{
				{
					ID:           "acc-1",
					Name:         "Account 1",
					Token:        "old-token",
					RefreshToken: "old-refresh",
					Type:         "oauth",
					Enabled:      true,
				},
			},
			Allowlist: allowlist,
			Routing:   routing,
		},
	})

	srv := &Server{}
	now := time.Now().Add(1 * time.Hour)
	srv.syncRefreshedAccountToConfig("acc-1", "new-token", "new-refresh", &now)

	updated := config.Get().ClaudeCode
	if len(updated.Allowlist) != 1 || updated.Allowlist[0].ID != "claude-custom-1" {
		t.Fatalf("allowlist lost or modified: %+v", updated.Allowlist)
	}
	if updated.Routing.Retry429Max != 7 || updated.Routing.BackoffBaseMs != 1500 {
		t.Fatalf("routing config lost or modified: %+v", updated.Routing)
	}
}

func TestForwardToClaudeCode_401RetryFailure_FailsOver(t *testing.T) {
	reqCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		authHdr := r.Header.Get("Authorization")
		apiKeyHdr := r.Header.Get("x-api-key")
		if authHdr == "Bearer sk-ant-oat01-acc1-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"OAuth token has expired"}}`))
			return
		}
		if apiKeyHdr == "acc2-tok" || authHdr == "Bearer acc2-tok" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"msg_456","type":"message","usage":{"input_tokens":5,"output_tokens":10}}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	oauthMgr := auth.NewClaudeCodeOAuthManager()
	// OAuth refresh fails for acc1
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer tokenSrv.Close()
	oauthMgr.SetEndpoints("", tokenSrv.URL, "", nil)

	srv, err := New(Options{
		APIKey: "test-key",
		Credentials: func(ctx context.Context) (auth.Credentials, error) {
			return auth.Credentials{AccessToken: "token"}, nil
		},
		NewUpstream:        func(s string) Upstream { return nil },
		ClaudeCodeOAuthMgr: oauthMgr,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	ccPoolMu.Lock()
	ccPoolInst = nil
	ccHTTPClient = nil
	ccPoolMu.Unlock()

	cfg := claudecode.Config{
		Enabled: true,
		BaseURL: ts.URL,
		Accounts: []claudecode.AccountConfig{
			{
				ID:           "acc-1",
				Name:         "Account 1",
				Token:        "sk-ant-oat01-acc1-tok",
				RefreshToken: "bad-refresh",
				Type:         "oauth",
				Priority:     1,
				Enabled:      true,
			},
			{
				ID:       "acc-2",
				Name:     "Account 2",
				Token:    "acc2-tok",
				Type:     "api_key",
				Priority: 2,
				Enabled:  true,
			},
		},
		Allowlist: claudecode.DefaultAllowlist(),
		Routing:   claudecode.DefaultRoutingConfig(),
	}

	reqBody := []byte(`{"model":"claude-3-7-sonnet-20250219","messages":[{"role":"user","content":"hello"}]}`)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))
	w := httptest.NewRecorder()

	srv.forwardToClaudeCode(w, httpReq, cfg, reqBody, "claude-3-7-sonnet-20250219")

	if w.Code != http.StatusOK {
		t.Errorf("expected failover to acc-2 with status 200, got %d: %s", w.Code, w.Body.String())
	}
}



