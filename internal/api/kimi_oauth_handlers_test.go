package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/config"
)

// kimiAuthFake is a fake global Kimi host: device authorization, token
// polling (pending until approved flips), and /me.
type kimiAuthFake struct {
	srv      *httptest.Server
	approved atomic.Bool
	denied   atomic.Bool
}

func newKimiAuthFake(t *testing.T) *kimiAuthFake {
	t.Helper()
	f := &kimiAuthFake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/oauth/device_authorization":
			_, _ = w.Write([]byte(`{
				"device_code": "dc",
				"user_code": "ABCD-1234",
				"verification_uri": "https://www.kimi.ai/code/authorize_device",
				"verification_uri_complete": "https://www.kimi.ai/code/authorize_device?user_code=ABCD-1234",
				"expires_in": 1800,
				"interval": 5
			}`))
		case "/api/oauth/token":
			if f.denied.Load() {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"access_denied","error_description":"user rejected"}`))
				return
			}
			if !f.approved.Load() {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"at-123456789012","refresh_token":"rt-1","expires_in":3600}`))
		case "/coding/v1/me":
			_, _ = w.Write([]byte(`{"user_id":"u1","email":"dev@example.com","nickname":"dev"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *kimiAuthFake) attach(t *testing.T, server *Server) {
	t.Helper()
	mgr := auth.NewKimiOAuthManager(nil)
	mgr.SetEndpoints(f.srv.URL, f.srv.URL+"/coding/v1", f.srv.Client())
	mgr.SetSleep(func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(10 * time.Millisecond)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	})
	server.kimiOAuthMgr = mgr
}

func doKimiRequest(t *testing.T, server *Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	var reader *strings.Reader = strings.NewReader(body)
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	var parsed map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	}
	return rec.Code, parsed
}

func TestKimiOAuthHandlers_FullFlow(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	fake := newKimiAuthFake(t)
	fake.attach(t, server)

	code, start := doKimiRequest(t, server, http.MethodPost, "/api/kimi/auth/start", "{}")
	if code != 200 || start["status"] != "ok" {
		t.Fatalf("start: code=%d body=%v", code, start)
	}
	if start["user_code"] != "ABCD-1234" {
		t.Errorf("user_code = %v", start["user_code"])
	}
	sessionID, _ := start["session_id"].(string)
	if sessionID == "" {
		t.Fatal("no session_id")
	}

	code, status := doKimiRequest(t, server, http.MethodGet, "/api/kimi/auth/status?session_id="+sessionID, "")
	if code != 200 || status["status"] != "pending" {
		t.Fatalf("status before approval: code=%d body=%v", code, status)
	}

	fake.approved.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for {
		code, status = doKimiRequest(t, server, http.MethodGet, "/api/kimi/auth/status?session_id="+sessionID, "")
		if status["status"] == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for completion; last=%v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	account, _ := status["account"].(map[string]any)
	if account["email"] != "dev@example.com" {
		t.Errorf("account.email = %v", account["email"])
	}
	if _, ok := account["expires_at"].(string); !ok {
		t.Errorf("account.expires_at missing: %v", account)
	}

	cfg := config.Get().Kimi
	if !cfg.Enabled {
		t.Error("kimi not enabled after login")
	}
	if cfg.OAuth == nil || cfg.OAuth.Token != "at-123456789012" || cfg.OAuth.RefreshToken != "rt-1" {
		t.Fatalf("OAuth = %+v", cfg.OAuth)
	}
	if cfg.OAuth.OAuthHost != fake.srv.URL {
		t.Errorf("OAuthHost = %q, want %q", cfg.OAuth.OAuthHost, fake.srv.URL)
	}
	if cfg.OAuth.BaseURL != fake.srv.URL+"/coding/v1" {
		t.Errorf("BaseURL = %q", cfg.OAuth.BaseURL)
	}
	if cfg.OAuth.Email != "dev@example.com" {
		t.Errorf("Email = %q", cfg.OAuth.Email)
	}
}

func TestKimiOAuthHandlers_ClaimOnceKeepsRefreshedToken(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	fake := newKimiAuthFake(t)
	fake.attach(t, server)

	if _, err := config.Save(map[string]any{"kimi": map[string]any{
		"enabled": true,
		"apiKey":  "sk-keep-me",
		"baseUrl": "https://api.moonshot.ai/anthropic",
	}}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	_, start := doKimiRequest(t, server, http.MethodPost, "/api/kimi/auth/start", "{}")
	sessionID, _ := start["session_id"].(string)

	fake.approved.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, status := doKimiRequest(t, server, http.MethodGet, "/api/kimi/auth/status?session_id="+sessionID, "")
		if status["status"] == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out; last=%v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cfg := config.Get().Kimi
	if cfg.APIKey != "sk-keep-me" {
		t.Errorf("APIKey = %q, want preserved", cfg.APIKey)
	}
	if cfg.BaseURL != "https://api.moonshot.ai/anthropic" {
		t.Errorf("BaseURL = %q, want preserved", cfg.BaseURL)
	}

	// Simulate a refresh that rotated the stored token after the session
	// completed: a second status poll must not re-save the stale session token.
	if _, err := config.Save(map[string]any{"kimi": map[string]any{"oauth": map[string]any{
		"token":        "rotated-tok",
		"refreshToken": "rt-rotated",
		"expiresAt":    time.Now().Add(time.Hour).Format(time.RFC3339),
	}}}); err != nil {
		t.Fatalf("rotate Save: %v", err)
	}
	code, status := doKimiRequest(t, server, http.MethodGet, "/api/kimi/auth/status?session_id="+sessionID, "")
	if code != http.StatusNotFound {
		t.Fatalf("second poll: code=%d body=%v, want 404 (session dropped once persisted)", code, status)
	}
	if tok := config.Get().Kimi.OAuth; tok == nil || tok.Token != "rotated-tok" {
		t.Errorf("token after second poll = %+v, want rotated-tok kept", tok)
	}
}

func TestKimiOAuthHandlers_UnknownSessionReturns404(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	fake := newKimiAuthFake(t)
	fake.attach(t, server)

	code, body := doKimiRequest(t, server, http.MethodGet, "/api/kimi/auth/status?session_id=nope", "")
	if code != 404 || body["status"] != "expired" {
		t.Errorf("unknown session: code=%d body=%v", code, body)
	}

	_, start := doKimiRequest(t, server, http.MethodPost, "/api/kimi/auth/start", "{}")
	sessionID, _ := start["session_id"].(string)
	doKimiRequest(t, server, http.MethodPost, "/api/kimi/auth/cancel", `{"session_id":"`+sessionID+`"}`)
	code, body = doKimiRequest(t, server, http.MethodGet, "/api/kimi/auth/status?session_id="+sessionID, "")
	if code != 404 {
		t.Errorf("after cancel: code=%d body=%v", code, body)
	}
}

func TestKimiOAuthHandlers_Logout(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	fake := newKimiAuthFake(t)
	fake.attach(t, server)

	if _, err := config.Save(map[string]any{"kimi": map[string]any{
		"enabled": true,
		"apiKey":  "sk-keep-me",
		"oauth": map[string]any{
			"token":        "at-1",
			"refreshToken": "rt-1",
			"email":        "dev@example.com",
		},
	}}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	code, body := doKimiRequest(t, server, http.MethodPost, "/api/kimi/auth/logout", "")
	if code != 200 || body["status"] != "ok" {
		t.Fatalf("logout: code=%d body=%v", code, body)
	}
	cfg := config.Get().Kimi
	if cfg.OAuth != nil {
		t.Errorf("OAuth = %+v, want nil", cfg.OAuth)
	}
	if cfg.APIKey != "sk-keep-me" {
		t.Errorf("APIKey = %q, want kept", cfg.APIKey)
	}
	respCfg, _ := body["config"].(map[string]any)
	if _, has := respCfg["oauth"]; has {
		t.Errorf("response config should not contain oauth: %v", respCfg["oauth"])
	}
}

func TestKimiOAuthHandlers_TerminalStatusDropsSession(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	fake := newKimiAuthFake(t)
	fake.attach(t, server)
	fake.denied.Store(true)

	_, start := doKimiRequest(t, server, http.MethodPost, "/api/kimi/auth/start", "{}")
	sessionID, _ := start["session_id"].(string)

	deadline := time.Now().Add(2 * time.Second)
	for {
		_, status := doKimiRequest(t, server, http.MethodGet, "/api/kimi/auth/status?session_id="+sessionID, "")
		if status["status"] == "denied" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for denied; last=%v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, body := doKimiRequest(t, server, http.MethodGet, "/api/kimi/auth/status?session_id="+sessionID, ""); code != http.StatusNotFound {
		t.Errorf("poll after a terminal status: code=%d body=%v, want 404", code, body)
	}
}

func TestKimiOAuthHandlers_SaveFailureAllowsRetry(t *testing.T) {
	server, _, _ := newTestServerWithManager(t)
	fake := newKimiAuthFake(t)
	fake.attach(t, server)

	_, start := doKimiRequest(t, server, http.MethodPost, "/api/kimi/auth/start", "{}")
	sessionID, _ := start["session_id"].(string)

	restore := breakConfigWrites(t)
	fake.approved.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for {
		code, status := doKimiRequest(t, server, http.MethodGet, "/api/kimi/auth/status?session_id="+sessionID, "")
		if code == http.StatusInternalServerError {
			break
		}
		if status["status"] == "completed" {
			t.Fatalf("reported completed while config writes fail: %v", status)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the save failure; last=%v", status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	restore()

	code, status := doKimiRequest(t, server, http.MethodGet, "/api/kimi/auth/status?session_id="+sessionID, "")
	if code != http.StatusOK || status["status"] != "completed" {
		t.Fatalf("retry poll: code=%d body=%v, want 200 completed", code, status)
	}
	if tok := config.Get().Kimi.OAuth; tok == nil || tok.Token != "at-123456789012" {
		t.Fatalf("OAuth after retry = %+v, want at-123456789012 persisted", tok)
	}
}
