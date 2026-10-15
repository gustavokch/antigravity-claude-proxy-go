package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/config"
)

// withWebUIPassword sets a management password for one test and restores the
// original config afterwards.
func withWebUIPassword(t *testing.T, srv *Server, password string) {
	t.Helper()
	original := config.Get()
	t.Cleanup(func() { config.SetForTest(original) })
	cfg := original
	cfg.WebUIPassword = password
	config.SetForTest(cfg)
}

// TestManagementRoutesRequireWebUIPassword is the guard against the drift that
// left POST /refresh-token open: the gate and the dispatch table were separate
// lists, and a route outside the /api/ prefix reached a state-changing handler
// with no credential. Every route below is one the dispatch table serves. A new
// route that is not named public in requiresWebUIPassword must appear here too.
func TestManagementRoutesRequireWebUIPassword(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	withWebUIPassword(t, srv, "s3cret")

	gated := []struct{ method, path string }{
		{http.MethodGet, "/account-limits"},
		{http.MethodPost, "/refresh-token"},
		{http.MethodPost, "/anthropic/refresh-token"},
		{http.MethodGet, "/api/accounts"},
		{http.MethodPost, "/api/accounts/test@example.com/refresh"},
		{http.MethodPost, "/api/accounts/test@example.com/toggle"},
		{http.MethodDelete, "/api/accounts/test@example.com"},
		{http.MethodPatch, "/api/accounts/test@example.com"},
		{http.MethodPost, "/api/accounts/reload"},
		{http.MethodGet, "/api/accounts/export"},
		{http.MethodPost, "/api/accounts/import"},
		{http.MethodPost, "/api/config"},
		{http.MethodPost, "/api/config/password"},
		{http.MethodGet, "/api/settings"},
		{http.MethodGet, "/api/claude/config"},
		{http.MethodPost, "/api/claude/config"},
		{http.MethodPost, "/api/claude/config/restore"},
		{http.MethodGet, "/api/claude/mode"},
		{http.MethodPost, "/api/claude/mode"},
		{http.MethodGet, "/api/claude/presets"},
		{http.MethodPost, "/api/claude/presets"},
		{http.MethodDelete, "/api/claude/presets/x"},
		{http.MethodGet, "/api/server/presets"},
		{http.MethodPost, "/api/server/presets"},
		{http.MethodPatch, "/api/server/presets/x"},
		{http.MethodDelete, "/api/server/presets/x"},
		{http.MethodPost, "/api/models/config"},
		{http.MethodGet, "/api/strategy/health"},
		{http.MethodGet, "/api/stats/history"},
		{http.MethodDelete, "/api/stats/history"},
		{http.MethodGet, "/api/headroom/stats"},
		{http.MethodGet, "/api/cache-bump"},
		{http.MethodDelete, "/api/cache-bump"},
		{http.MethodPost, "/api/cache-bump/x/stop"},
		{http.MethodGet, "/api/mitm/status"},
		{http.MethodGet, "/api/mitm/ca.pem"},
		{http.MethodGet, "/api/sessions/cloud"},
		{http.MethodGet, "/api/logs"},
		{http.MethodPost, "/api/openrouter/config"},
		{http.MethodPost, "/api/openrouter/models/fetch"},
		{http.MethodGet, "/api/openrouter/models/cached"},
		{http.MethodGet, "/api/openrouter/credits"},
		{http.MethodGet, "/api/openrouter/providers"},
		{http.MethodPost, "/api/kimi/config"},
		{http.MethodPost, "/api/kimi/models/fetch"},
		{http.MethodPost, "/api/kimi/auth/start"},
		{http.MethodGet, "/api/kimi/auth/status"},
		{http.MethodPost, "/api/kimi/auth/cancel"},
		{http.MethodPost, "/api/kimi/auth/logout"},
		{http.MethodPost, "/api/zen/config"},
		{http.MethodPost, "/api/zen/models/fetch"},
		{http.MethodPost, "/api/auth/complete"},
		{http.MethodGet, "/api/claudecode/config"},
		{http.MethodPost, "/api/claudecode/config"},
		{http.MethodGet, "/api/claudecode/accounts"},
		{http.MethodPost, "/api/claudecode/accounts"},
		{http.MethodGet, "/api/claudecode/usage"},
		{http.MethodPost, "/api/claudecode/usage/reload"},
		{http.MethodPost, "/api/claudecode/import"},
		{http.MethodGet, "/api/claudecode/auth/status"},
	}

	for _, route := range gated {
		req := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without the password = %d, want 401", route.method, route.path, rec.Code)
		}
	}
}

// TestGatedRoutesAnswerWithPassword covers the routes that answer without
// touching the network, so the gate cannot be satisfied by a blanket 401.
func TestGatedRoutesAnswerWithPassword(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	withWebUIPassword(t, srv, "s3cret")

	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/api/accounts"},
		{http.MethodGet, "/api/accounts/export"},
		{http.MethodPost, "/api/accounts/reload"},
		{http.MethodPost, "/refresh-token"},
		{http.MethodPost, "/anthropic/refresh-token"},
		{http.MethodGet, "/account-limits"},
		{http.MethodGet, "/api/claude/config"},
		{http.MethodGet, "/api/strategy/health"},
		{http.MethodGet, "/api/stats/history"},
		{http.MethodGet, "/api/logs"},
	} {
		req := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
		req.Header.Set("x-webui-password", "s3cret")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s %s with the password = 401, want it accepted", route.method, route.path)
		}
	}
}

// TestPublicManagementRoutesStayOpen pins the deliberate exceptions. GET
// /api/config must answer before login so the login form can read hasPassword.
func TestPublicManagementRoutesStayOpen(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	withWebUIPassword(t, srv, "s3cret")

	for _, route := range []struct{ method, path string }{
		{http.MethodGet, "/health"},
		{http.MethodGet, "/api/config"},
		{http.MethodGet, "/api/auth/url"},
	} {
		req := httptest.NewRequest(route.method, route.path, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s %s without the password = 401, want it public", route.method, route.path)
		}
	}
}

// TestHealthCarriesCountsNotAccounts: /health is one of the public routes, so it
// must not hand out account emails, project IDs or quota windows.
func TestHealthCarriesCountsNotAccounts(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)

	rec, body := getJSON(t, srv, "/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health = %d", rec.Code)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}
	accounts, ok := body["accounts"].(map[string]any)
	if !ok {
		t.Fatalf("accounts field = %T, want an object", body["accounts"])
	}
	for _, key := range []string{"total", "available", "rateLimited", "invalid", "summary"} {
		if _, missing := accounts[key]; !missing {
			t.Errorf("accounts.%s missing from the health probe", key)
		}
	}
	if _, present := accounts["accounts"]; present {
		t.Errorf("health probe still carries the per-account list: %v", accounts["accounts"])
	}
	if strings.Contains(rec.Body.String(), "test@example.com") {
		t.Errorf("health probe leaked an account email: %s", rec.Body.String())
	}
}

// TestRefreshTokenAliasOrigin: the alias used to sit outside the management
// branch, so a foreign origin both triggered the reload and read the result
// under Access-Control-Allow-Origin: *.
func TestRefreshTokenAliasOrigin(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)

	req := httptest.NewRequest(http.MethodPost, "/refresh-token", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin POST /refresh-token = %d, want 403", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got == "*" {
		t.Errorf("management route still advertises a wildcard origin")
	}
}

// TestManagementRoutesWithoutPasswordStillWork: an unset webuiPassword leaves
// the dashboard open. That is the shipped default and stays unchanged.
func TestManagementRoutesWithoutPasswordStillWork(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	original := config.Get()
	t.Cleanup(func() { config.SetForTest(original) })
	cfg := original
	cfg.WebUIPassword = ""
	config.SetForTest(cfg)

	req := httptest.NewRequest(http.MethodPost, "/refresh-token", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("POST /refresh-token with no password configured = %d, want 200", rec.Code)
	}
}

// TestPublicConfigStillRedactsCredentials guards the one public route that
// returns configuration: no credential may cross it.
func TestPublicConfigStillRedactsCredentials(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	original := config.Get()
	t.Cleanup(func() { config.SetForTest(original) })
	cfg := original
	cfg.WebUIPassword = "webui-secret"
	cfg.APIKey = "legacy-proxy-secret"
	cfg.OpenRouter.APIKey = "openrouter-secret"
	cfg.Zen.APIKey = "zen-secret"
	config.SetForTest(cfg)

	rec, _ := getJSON(t, srv, "/api/config")
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"webui-secret", "legacy-proxy-secret", "openrouter-secret", "zen-secret"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Errorf("GET /api/config leaked %q", secret)
		}
	}
}
