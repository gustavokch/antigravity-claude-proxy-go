package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestGenerateClaudeCodePKCE(t *testing.T) {
	verifier, challenge, err := GenerateClaudeCodePKCE()
	if err != nil {
		t.Fatalf("unexpected error generating PKCE: %v", err)
	}

	if len(verifier) == 0 {
		t.Fatal("empty verifier")
	}

	h := sha256.Sum256([]byte(verifier))
	expectedChallenge := base64.RawURLEncoding.EncodeToString(h[:])

	if challenge != expectedChallenge {
		t.Fatalf("expected challenge %s, got %s", expectedChallenge, challenge)
	}
}

func TestBuildAuthorizeURL(t *testing.T) {
	mgr := NewClaudeCodeOAuthManager()
	redirectURI := "http://localhost:54321/callback"
	challenge := "test-challenge-123"
	state := "test-state-456"

	authURL := mgr.BuildAuthorizeURL(redirectURI, challenge, state)

	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("failed to parse auth URL: %v", err)
	}

	if parsed.Scheme != "https" || parsed.Host != "claude.com" || parsed.Path != "/cai/oauth/authorize" {
		t.Fatalf("unexpected auth url path: %s", parsed.String())
	}

	q := parsed.Query()
	if q.Get("client_id") != ClaudeCodeClientID {
		t.Errorf("expected client_id %s, got %s", ClaudeCodeClientID, q.Get("client_id"))
	}
	if q.Get("response_type") != "code" {
		t.Errorf("expected response_type code, got %s", q.Get("response_type"))
	}
	if q.Get("code") != "true" {
		t.Errorf("expected code=true, got %s", q.Get("code"))
	}
	if q.Get("redirect_uri") != redirectURI {
		t.Errorf("expected redirect_uri %s, got %s", redirectURI, q.Get("redirect_uri"))
	}
	if q.Get("code_challenge") != challenge {
		t.Errorf("expected code_challenge %s, got %s", challenge, q.Get("code_challenge"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("expected code_challenge_method S256, got %s", q.Get("code_challenge_method"))
	}
	if q.Get("state") != state {
		t.Errorf("expected state %s, got %s", state, q.Get("state"))
	}
	if q.Get("scope") != ClaudeCodeDefaultScopes {
		t.Errorf("expected scope %s, got %s", ClaudeCodeDefaultScopes, q.Get("scope"))
	}
}

func TestStartAuthSession_Manual(t *testing.T) {
	mgr := NewClaudeCodeOAuthManager()
	session, err := mgr.StartAuthSession("manual")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if session.ID == "" || session.State == "" || session.CodeVerifier == "" {
		t.Fatal("session fields missing")
	}

	if session.RedirectURI != ClaudeCodeManualCallbackURL {
		t.Errorf("expected manual redirect URI, got %s", session.RedirectURI)
	}
	if session.Status != "pending" {
		t.Errorf("expected pending status, got %s", session.Status)
	}

	fetched, ok := mgr.GetSession(session.ID)
	if !ok || fetched.ID != session.ID {
		t.Fatal("failed to fetch session from manager")
	}

	mgr.CancelSession(session.ID)
	if _, ok := mgr.GetSession(session.ID); ok {
		t.Fatal("session should have been deleted on cancel")
	}
}

func TestStartAuthSession_Loopback(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/oauth/token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ClaudeCodeTokenResponse{
				AccessToken:  "test-access-token",
				RefreshToken: "test-refresh-token",
				ExpiresIn:    3600,
				TokenType:    "Bearer",
				Scope:        ClaudeCodeDefaultScopes,
			})
		case "/api/oauth/profile":
			w.Header().Set("Content-Type", "application/json")
			var p ClaudeCodeProfile
			p.Account.Email = "test@claude.ai"
			p.Account.UUID = "acc-12345"
			p.Organization.UUID = "org-67890"
			_ = json.NewEncoder(w).Encode(p)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockServer.Close()

	mgr := NewClaudeCodeOAuthManager()
	mgr.SetEndpoints(
		mockServer.URL+"/authorize",
		mockServer.URL+"/v1/oauth/token",
		mockServer.URL+"/api/oauth/profile",
		mockServer.Client(),
	)

	session, err := mgr.StartAuthSession("loopback")
	if err != nil {
		t.Fatalf("unexpected error starting loopback session: %v", err)
	}
	defer mgr.CancelSession(session.ID)

	if session.Port <= 0 {
		t.Fatalf("invalid port: %d", session.Port)
	}

	// Trigger callback on loopback server
	callbackURL := fmt.Sprintf("http://localhost:%d/callback?code=mock-code&state=%s", session.Port, session.State)
	resp, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("failed to call loopback callback: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected callback status 200, got %d", resp.StatusCode)
	}

	// Verify session state updated
	time.Sleep(100 * time.Millisecond)
	session.mu.Lock()
	defer session.mu.Unlock()

	if session.Status != "completed" {
		t.Fatalf("expected session status completed, got %s (err: %s)", session.Status, session.Error)
	}

	if session.Account == nil {
		t.Fatal("expected session account not nil")
	}

	if session.Account.Email != "test@claude.ai" {
		t.Errorf("expected email test@claude.ai, got %s", session.Account.Email)
	}
	if session.Account.AccessToken != "test-access-token" {
		t.Errorf("expected access token test-access-token, got %s", session.Account.AccessToken)
	}
}

func TestCompleteManualAuth(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/oauth/token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ClaudeCodeTokenResponse{
				AccessToken:  "manual-access-token",
				RefreshToken: "manual-refresh-token",
				ExpiresIn:    7200,
				TokenType:    "Bearer",
				Scope:        ClaudeCodeDefaultScopes,
			})
		case "/api/oauth/profile":
			w.Header().Set("Content-Type", "application/json")
			var p ClaudeCodeProfile
			p.Account.Email = "manual@claude.ai"
			p.Account.UUID = "manual-acc-uuid"
			_ = json.NewEncoder(w).Encode(p)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockServer.Close()

	mgr := NewClaudeCodeOAuthManager()
	mgr.SetEndpoints(
		mockServer.URL+"/authorize",
		mockServer.URL+"/v1/oauth/token",
		mockServer.URL+"/api/oauth/profile",
		mockServer.Client(),
	)

	session, err := mgr.StartAuthSession("manual")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Test with code#state format
	rawCode := "my-auth-code#my-state"
	account, err := mgr.CompleteManualAuth(session.ID, rawCode)
	if err != nil {
		t.Fatalf("unexpected error on manual auth: %v", err)
	}

	if account.Email != "manual@claude.ai" {
		t.Errorf("expected email manual@claude.ai, got %s", account.Email)
	}
	if account.AccessToken != "manual-access-token" {
		t.Errorf("expected access token manual-access-token, got %s", account.AccessToken)
	}
	if account.RefreshToken != "manual-refresh-token" {
		t.Errorf("expected refresh token manual-refresh-token, got %s", account.RefreshToken)
	}
}

func TestRefreshToken(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/oauth/token" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["grant_type"] != "refresh_token" || body["refresh_token"] != "valid-refresh-token" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ClaudeCodeTokenResponse{
				AccessToken:  "new-access-token",
				RefreshToken: "new-refresh-token",
				ExpiresIn:    3600,
				TokenType:    "Bearer",
				Scope:        ClaudeCodeDefaultScopes,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer mockServer.Close()

	mgr := NewClaudeCodeOAuthManager()
	mgr.SetEndpoints("", mockServer.URL+"/v1/oauth/token", "", mockServer.Client())

	resp, err := mgr.RefreshToken("valid-refresh-token")
	if err != nil {
		t.Fatalf("unexpected error refreshing token: %v", err)
	}

	if resp.AccessToken != "new-access-token" {
		t.Errorf("expected new-access-token, got %s", resp.AccessToken)
	}

	_, err = mgr.RefreshToken("invalid-refresh-token")
	if err == nil {
		t.Fatal("expected error on invalid refresh token")
	}
}

func TestLoopbackCallback_InvalidState(t *testing.T) {
	mgr := NewClaudeCodeOAuthManager()
	session, err := mgr.StartAuthSession("loopback")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer mgr.CancelSession(session.ID)

	callbackURL := fmt.Sprintf("http://localhost:%d/callback?code=mock-code&state=wrong-state", session.Port)
	resp, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("unexpected error calling callback: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}

	session.mu.Lock()
	defer session.mu.Unlock()
	if session.Status != "failed" {
		t.Errorf("expected status failed, got %s", session.Status)
	}
	if !strings.Contains(session.Error, "invalid state") {
		t.Errorf("expected state error, got %s", session.Error)
	}
}

func TestClaudeCodeAuthSession_Snapshot(t *testing.T) {
	session := &ClaudeCodeAuthSession{
		ID:            "test-sess-1",
		State:         "test-state",
		RedirectURI:   "http://localhost:1234/callback",
		Port:          1234,
		AuthURL:       "https://claude.com/auth",
		ManualAuthURL: "https://claude.com/manual",
		Status:        "pending",
		CreatedAt:     time.Now(),
	}

	snap := session.Snapshot()
	if snap.ID != "test-sess-1" || snap.Status != "pending" || snap.Port != 1234 {
		t.Fatalf("unexpected snapshot values: %+v", snap)
	}

	// Mutate session under lock
	session.mu.Lock()
	session.Status = "completed"
	session.Account = &ClaudeCodeAccountResult{
		Email:       "user@example.com",
		AccessToken: "tok-123",
	}
	session.mu.Unlock()

	// Previous snapshot unchanged
	if snap.Status != "pending" || snap.Account != nil {
		t.Fatalf("snapshot should be immutable snapshot of past state: %+v", snap)
	}

	snap2 := session.Snapshot()
	if snap2.Status != "completed" || snap2.Account == nil || snap2.Account.Email != "user@example.com" {
		t.Fatalf("new snapshot should reflect latest state: %+v", snap2)
	}
}

func TestPruneExpiredSessions(t *testing.T) {
	mgr := NewClaudeCodeOAuthManager()

	oldSession := &ClaudeCodeAuthSession{
		ID:        "old-sess",
		Status:    "expired",
		CreatedAt: time.Now().Add(-30 * time.Minute),
		doneChan:  make(chan struct{}),
	}
	newSession := &ClaudeCodeAuthSession{
		ID:        "new-sess",
		Status:    "pending",
		CreatedAt: time.Now(),
		doneChan:  make(chan struct{}),
	}

	mgr.mu.Lock()
	mgr.sessions[oldSession.ID] = oldSession
	mgr.sessions[newSession.ID] = newSession
	mgr.mu.Unlock()

	pruned := mgr.PruneExpiredSessions(15 * time.Minute)
	if pruned != 1 {
		t.Errorf("expected 1 session pruned, got %d", pruned)
	}

	if _, exists := mgr.GetSession("old-sess"); exists {
		t.Error("expected old-sess to be pruned")
	}

	if _, exists := mgr.GetSession("new-sess"); !exists {
		t.Error("expected new-sess to remain")
	}
}
