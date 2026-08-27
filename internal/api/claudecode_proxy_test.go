package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
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
