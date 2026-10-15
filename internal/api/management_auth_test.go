package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
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

// TestManagementPreflightAllowsThePasswordHeader: the gate reads
// x-webui-password, so the preflight must name it, or a cross-origin browser
// cannot reach a gated route at all.
func TestManagementPreflightAllowsThePasswordHeader(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	req := httptest.NewRequest(http.MethodOptions, "/api/accounts", nil)
	req.Header.Set("Origin", "http://localhost:8091")
	req.Host = "127.0.0.1:8091"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Headers"), "x-webui-password") {
		t.Errorf("x-webui-password not allowed: %q", rec.Header().Get("Access-Control-Allow-Headers"))
	}
}

// publicByDesign names the dispatcher paths outside /api/ that are served by
// handleManagement and deliberately kept on the public CORS path.
var publicByDesign = map[string]string{
	"/health": "monitoring probe; counts only",
}

// fragments are not routes. The dispatcher matches them as the second argument
// of a HasSuffix/TrimSuffix call against an /api/ path, so the source scan below
// sees them. They are excused only while every occurrence stays in that slot: any
// other use makes the literal a route, and the scan checks it. A real non-/api/
// route named /refresh therefore cannot hide behind this list.
var fragments = map[string]string{
	"/refresh": "suffix of /api/accounts/{email}/refresh",
	"/toggle":  "suffix of /api/accounts/{email}/toggle",
	"/stop":    "suffix of /api/cache-bump/{id}/stop",
}

// isFragmentOnly reports whether every occurrence of path in body sits in the
// second-argument slot of a HasSuffix/TrimSuffix call. Any other occurrence makes
// it a route: a whole-path comparison, or a later slot of a multi-value case such
// as `case "/zzz", "/stop":`. Checking every occurrence, not only the literal
// that follows `case` or `path ==`, is what keeps a later slot from hiding.
func isFragmentOnly(body, path string) bool {
	quoted := `"` + regexp.QuoteMeta(path) + `"`
	// The suffix match ends on the literal, so its end index identifies the
	// occurrence it justifies. An occurrence with no such match is a route.
	justified := map[int]bool{}
	for _, m := range regexp.MustCompile(`Suffix\([^\n]*,\s*`+quoted).FindAllStringIndex(body, -1) {
		justified[m[1]] = true
	}
	hits := regexp.MustCompile(quoted).FindAllStringIndex(body, -1)
	if len(hits) == 0 {
		return false
	}
	for _, h := range hits {
		if !justified[h[1]] {
			return false
		}
	}
	return true
}

// TestEveryNonAPIDispatchRouteIsOriginChecked reads the dispatcher itself, so a
// route added to handleManagement outside /api/ fails here until it is named in
// isManagementRoute (or listed above with a reason). A hand-written route list
// in the test would guard nothing.
func TestEveryNonAPIDispatchRouteIsOriginChecked(t *testing.T) {
	src, err := os.ReadFile("management.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	start := strings.Index(text, "func (server *Server) handleManagement(")
	if start < 0 {
		t.Fatal("handleManagement not found in management.go")
	}
	body := text[start:]
	if end := strings.Index(body[1:], "\nfunc "); end >= 0 {
		body = body[:end+1]
	}
	lit := regexp.MustCompile(`"(/[A-Za-z0-9_\-./]+)"`) // "+" skips the bare "/" used by strings.SplitN
	seen := 0
	for _, m := range lit.FindAllStringSubmatch(body, -1) {
		p := m[1]
		if strings.HasPrefix(p, "/api/") {
			continue
		}
		seen++
		if _, ok := fragments[p]; ok && isFragmentOnly(body, p) {
			continue
		}
		if _, ok := publicByDesign[p]; ok {
			if isManagementRoute(p) {
				t.Errorf("%s is listed public-by-design but isManagementRoute claims it", p)
			}
			continue
		}
		if !isManagementRoute(p) {
			t.Errorf("handleManagement serves %q outside /api/ but isManagementRoute does not name it: "+
				"it would be answered with Access-Control-Allow-Origin: * and no Origin check", p)
		}
	}
	if seen == 0 {
		t.Fatal("scan found no non-/api/ routes: the regexp or the function boundary is wrong")
	}
}

// TestAccountLimitsOrigin covers the GET route TestRefreshTokenAliasOrigin does not.
func TestAccountLimitsOrigin(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	req := httptest.NewRequest(http.MethodGet, "/account-limits", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-origin GET /account-limits = %d, want 403", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") == "*" {
		t.Error("/account-limits advertises a wildcard origin")
	}
}
