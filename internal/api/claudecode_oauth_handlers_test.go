package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"antigravity-go-proxy/internal/auth"
)

func TestClaudeCodeOAuthHandlers_FullFlow(t *testing.T) {
	// Mock Anthropic endpoints
	mockAnthropicServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/oauth/token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(auth.ClaudeCodeTokenResponse{
				AccessToken:  "mock-access-token",
				RefreshToken: "mock-refresh-token",
				ExpiresIn:    3600,
				TokenType:    "Bearer",
				Scope:        auth.ClaudeCodeDefaultScopes,
			})
		case "/api/oauth/profile":
			w.Header().Set("Content-Type", "application/json")
			var p auth.ClaudeCodeProfile
			p.Account.Email = "developer@example.com"
			p.Account.UUID = "dev-uuid-123"
			p.Organization.UUID = "org-uuid-456"
			_ = json.NewEncoder(w).Encode(p)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockAnthropicServer.Close()

	server, _, _ := newTestServerWithManager(t)

	oauthMgr := auth.NewClaudeCodeOAuthManager()
	oauthMgr.SetEndpoints(
		mockAnthropicServer.URL+"/cai/oauth/authorize",
		mockAnthropicServer.URL+"/v1/oauth/token",
		mockAnthropicServer.URL+"/api/oauth/profile",
		mockAnthropicServer.Client(),
	)
	server.claudeCodeOAuthMgr = oauthMgr

	// 1. POST /api/claudecode/auth/start (manual mode)
	startReq := httptest.NewRequest(http.MethodPost, "/api/claudecode/auth/start", bytes.NewBufferString(`{"mode":"manual"}`))
	startReq.Header.Set("Authorization", "Bearer test-api-key")
	startReq.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.serveHTTP(w, startReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200 on auth start, got %d: %s", w.Code, w.Body.String())
	}

	var startResp struct {
		Status        string `json:"status"`
		SessionID     string `json:"session_id"`
		AuthURL       string `json:"auth_url"`
		ManualAuthURL string `json:"manual_auth_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &startResp); err != nil {
		t.Fatalf("unmarshal start response: %v", err)
	}

	if startResp.Status != "ok" || startResp.SessionID == "" {
		t.Fatalf("invalid start response: %+v", startResp)
	}

	// 2. GET /api/claudecode/auth/status (should be pending)
	statusReq := httptest.NewRequest(http.MethodGet, "/api/claudecode/auth/status?session_id="+startResp.SessionID, nil)
	statusReq.Header.Set("Authorization", "Bearer test-api-key")
	w = httptest.NewRecorder()
	server.serveHTTP(w, statusReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200 on auth status, got %d: %s", w.Code, w.Body.String())
	}

	var statusResp struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &statusResp)
	if statusResp.Status != "pending" {
		t.Fatalf("expected status pending, got %s", statusResp.Status)
	}

	// 3. POST /api/claudecode/auth/complete (complete manual code exchange)
	completePayload, _ := json.Marshal(map[string]string{
		"session_id": startResp.SessionID,
		"code":       "auth-code-test#mock-state",
	})
	completeReq := httptest.NewRequest(http.MethodPost, "/api/claudecode/auth/complete", bytes.NewReader(completePayload))
	completeReq.Header.Set("Authorization", "Bearer test-api-key")
	completeReq.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	server.serveHTTP(w, completeReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200 on auth complete, got %d: %s", w.Code, w.Body.String())
	}

	var completeResp struct {
		Status  string `json:"status"`
		Account struct {
			Email string `json:"email"`
		} `json:"account"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &completeResp)
	if completeResp.Status != "ok" || completeResp.Account.Email != "developer@example.com" {
		t.Fatalf("unexpected complete response: %+v", completeResp)
	}

	// 4. Verify account appears in GET /api/claudecode/accounts
	listReq := httptest.NewRequest(http.MethodGet, "/api/claudecode/accounts", nil)
	listReq.Header.Set("Authorization", "Bearer test-api-key")
	w = httptest.NewRecorder()
	server.serveHTTP(w, listReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200 on list accounts, got %d", w.Code)
	}

	var listResp struct {
		Status   string `json:"status"`
		Accounts []struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"accounts"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &listResp)

	found := false
	for _, a := range listResp.Accounts {
		if a.Email == "developer@example.com" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("authenticated account developer@example.com not found in accounts list: %+v", listResp.Accounts)
	}
}

func TestClaudeCodeOAuthHandlers_Cancel(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)

	// Start session
	startReq := httptest.NewRequest(http.MethodPost, "/api/claudecode/auth/start", bytes.NewBufferString(`{"mode":"loopback"}`))
	startReq.Header.Set("Authorization", "Bearer test-api-key")
	w := httptest.NewRecorder()
	server.serveHTTP(w, startReq)

	var startResp struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &startResp)

	// Cancel session
	cancelReq := httptest.NewRequest(http.MethodPost, "/api/claudecode/auth/cancel", bytes.NewBufferString(`{"session_id":"`+startResp.SessionID+`"}`))
	cancelReq.Header.Set("Authorization", "Bearer test-api-key")
	w = httptest.NewRecorder()
	server.serveHTTP(w, cancelReq)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200 on cancel, got %d", w.Code)
	}

	// Verify session is deleted/expired
	statusReq := httptest.NewRequest(http.MethodGet, "/api/claudecode/auth/status?session_id="+startResp.SessionID, nil)
	statusReq.Header.Set("Authorization", "Bearer test-api-key")
	w = httptest.NewRecorder()
	server.serveHTTP(w, statusReq)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected status 404 after cancel, got %d", w.Code)
	}
}
