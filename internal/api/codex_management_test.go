package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/codex"
	"antigravity-go-proxy/internal/config"
)

// codexManagementCall sends one management request through the server's
// handler and decodes the JSON reply.
func codexManagementCall(t *testing.T, srv *Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: reply is not JSON (%d): %s", method, path, rec.Code, rec.Body.String())
	}
	return rec.Code, out
}

// requireCodexError checks the management error shape.
func requireCodexError(t *testing.T, code int, out map[string]any, wantCode int, wantText string) {
	t.Helper()
	if code != wantCode || out["status"] != "error" {
		t.Fatalf("reply = %d %v, want %d with status error", code, out, wantCode)
	}
	if msg, _ := out["error"].(string); !strings.Contains(strings.ToLower(msg), strings.ToLower(wantText)) {
		t.Errorf("error = %q, want it to mention %q", msg, wantText)
	}
}

// codexModelsBackend serves the catalog on /models with usage headers, and
// Codex replies on /responses.
func codexModelsBackend(t *testing.T) (*fakeCodexBackend, *httptest.Server) {
	return newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.URL.Path == "/models" {
			for k, v := range codexQuotaHeaderMap() {
				w.Header().Set(k, v)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"models":[`+
				`{"slug":"gpt-5.5","display_name":"GPT-5.5","context_window":500000,"visibility":"list","supported_in_api":true,"default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"}]},`+
				`{"slug":"gpt-6-sol","display_name":"GPT-6 Sol","context_window":272000,"visibility":"list","supported_in_api":true},`+
				`{"slug":"hidden","visibility":"hide"}]}`)
			return
		}
		writeCodexSSE(w, nil)
	})
}

func TestCodexConfigGetRedactsAccounts(t *testing.T) {
	_, upstream := codexModelsBackend(t)
	srv := newCodexTestServer(t, upstream, codex.AccountConfig{ID: "codex-a", Token: "access-token-secret", RefreshToken: "refresh-secret", IDToken: "id-secret", Enabled: true})

	code, out := codexManagementCall(t, srv, http.MethodGet, "/api/codex/config", "")
	if code != http.StatusOK || out["status"] != "ok" {
		t.Fatalf("reply = %d %v", code, out)
	}
	cfg, _ := out["config"].(map[string]any)
	if cfg["enabled"] != true || cfg["baseUrl"] != upstream.URL {
		t.Errorf("config = %v, want the saved section", cfg)
	}
	accounts, _ := cfg["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %v", cfg["accounts"])
	}
	acc := accounts[0].(map[string]any)
	for _, secret := range []string{"token", "refreshToken", "idToken"} {
		if _, present := acc[secret]; present {
			t.Errorf("public account carries %s: %v", secret, acc)
		}
	}
	if acc["hasToken"] != true || acc["hasRefreshToken"] != true || acc["maskedToken"] != "access...cret" {
		t.Errorf("public account = %v, want the redaction markers", acc)
	}
}

func TestCodexConfigPostValidates(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"identity control character", `{"identity":{"userAgent":"codex\u0007"}}`, "control character"},
		{"identity not an object", `{"identity":"codex"}`, "identity"},
		{"enabled not a boolean", `{"enabled":"yes"}`, "enabled"},
		{"baseUrl not http", `{"baseUrl":"ftp://example.test"}`, "baseUrl"},
		{"baseUrl relative", `{"baseUrl":"/backend-api/codex"}`, "baseUrl"},
		{"allowlist entry without id", `{"allowlist":[{"id":" ","enabled":true}]}`, "no id"},
		{"duplicate id", `{"allowlist":[{"id":"gpt-5.5"},{"id":"GPT-5.5"}]}`, "gpt-5.5"},
		{"alias claims another id", `{"allowlist":[{"id":"gpt-5.5"},{"id":"gpt-6-sol","alias":"Gpt-5.5"}]}`, "gpt-5.5"},
		{"alias repeated across entries", `{"allowlist":[{"id":"a","aliases":["fast"]},{"id":"b","alias":"x, FAST"}]}`, "fast"},
		{"allowlist wrong type", `{"allowlist":{"id":"a"}}`, "allowlist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := newTestServerWithManager(t)
			code, out := codexManagementCall(t, srv, http.MethodPost, "/api/codex/config", tc.body)
			requireCodexError(t, code, out, http.StatusBadRequest, tc.want)
		})
	}
}

// A settings save stores the posted settings and leaves the accounts alone,
// even when the client echoes a stale, redacted copy of them.
func TestCodexConfigPostSavesSettingsNotAccounts(t *testing.T) {
	_, upstream := codexModelsBackend(t)
	srv := newCodexTestServer(t, upstream, codex.AccountConfig{ID: "codex-a", Token: "tok-a", RefreshToken: "r-a", Enabled: true})

	body := `{"enabled":true,"baseUrl":"https://example.test/codex/","accounts":[],` +
		`"allowlist":[{"id":"gpt-5.5","alias":"fast","displayName":"GPT-5.5","contextLength":500000,"maxOutputTokens":1000,"efforts":["low","high"],"defaultEffort":"low","enabled":true}],` +
		`"identity":{"userAgent":"","originator":"custom"}}`
	code, out := codexManagementCall(t, srv, http.MethodPost, "/api/codex/config", body)
	if code != http.StatusOK || out["status"] != "ok" {
		t.Fatalf("reply = %d %v", code, out)
	}
	got := config.Get().Codex
	if got.BaseURL != "https://example.test/codex/" || got.Identity.Originator != "custom" || got.Identity.UserAgent != "" {
		t.Errorf("saved settings = %+v", got)
	}
	if len(got.Allowlist) != 1 || got.Allowlist[0].MaxOutputTokens != 1000 || got.Allowlist[0].DefaultEffort != "low" {
		t.Errorf("saved allowlist = %+v", got.Allowlist)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].RefreshToken != "r-a" {
		t.Errorf("accounts after a settings save = %+v, want them untouched", got.Accounts)
	}
}

// /api/config applies the same checks to a codex section.
func TestConfigSaveValidatesCodexSection(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	for _, body := range []string{
		`{"codex":{"identity":{"originator":"a\nb"}}}`,
		`{"codex":{"allowlist":[{"id":"a"},{"id":"A"}]}}`,
	} {
		code, out := codexManagementCall(t, srv, http.MethodPost, "/api/config", body)
		if code != http.StatusBadRequest || out["status"] != "error" {
			t.Errorf("POST /api/config %s = %d %v, want a 400 error", body, code, out)
		}
	}
}

func TestCodexAccountRoutes(t *testing.T) {
	_, upstream := codexModelsBackend(t)
	srv := newCodexTestServer(t, upstream,
		codex.AccountConfig{ID: "codex-a", Token: "tok-a", RefreshToken: "r-a", Email: "a@b.test", Priority: 1, Enabled: true, Source: "oauth"},
		codex.AccountConfig{ID: "codex-b", Token: "tok-b", RefreshToken: "r-b", Priority: 2, Enabled: true, Source: "oauth"})

	code, out := codexManagementCall(t, srv, http.MethodGet, "/api/codex/accounts", "")
	if code != http.StatusOK || out["status"] != "ok" {
		t.Fatalf("list = %d %v", code, out)
	}
	list, _ := out["accounts"].([]any)
	if len(list) != 2 {
		t.Fatalf("accounts = %v, want two", out["accounts"])
	}
	first := list[0].(map[string]any)
	if first["id"] != "codex-a" || first["status"] != "healthy" || first["email"] != "a@b.test" {
		t.Errorf("first snapshot = %v", first)
	}
	if _, leaked := first["token"]; leaked {
		t.Errorf("snapshot carries the token: %v", first)
	}

	code, out = codexManagementCall(t, srv, http.MethodPost, "/api/codex/accounts",
		`{"id":"codex-a","enabled":false,"priority":5,"name":"Work","token":"ignored"}`)
	if code != http.StatusOK || out["status"] != "ok" {
		t.Fatalf("update = %d %v", code, out)
	}
	saved := config.Get().Codex.Accounts[0]
	if saved.Enabled || saved.Priority != 5 || saved.Name != "Work" || saved.Token != "tok-a" {
		t.Errorf("updated account = %+v, want disabled, priority 5, named Work, token unchanged", saved)
	}
	if snap := codexSnapshot(t, srv, "codex-a"); snap.Status != "disabled" {
		t.Errorf("pool status after the update = %s, want disabled", snap.Status)
	}

	code, out = codexManagementCall(t, srv, http.MethodPost, "/api/codex/accounts", `{"id":"nope","enabled":true}`)
	requireCodexError(t, code, out, http.StatusNotFound, "not found")
	code, out = codexManagementCall(t, srv, http.MethodPost, "/api/codex/accounts", `{"enabled":true}`)
	requireCodexError(t, code, out, http.StatusBadRequest, "id")
	code, out = codexManagementCall(t, srv, http.MethodPost, "/api/codex/accounts", `{"id":"codex-a","priority":-1}`)
	requireCodexError(t, code, out, http.StatusBadRequest, "priority")
	code, out = codexManagementCall(t, srv, http.MethodPost, "/api/codex/accounts", `{"id":"codex-a","priority":"high"}`)
	requireCodexError(t, code, out, http.StatusBadRequest, "invalid json")

	code, out = codexManagementCall(t, srv, http.MethodDelete, "/api/codex/accounts/codex-b", "")
	if code != http.StatusOK || out["status"] != "ok" {
		t.Fatalf("delete = %d %v", code, out)
	}
	if accounts := config.Get().Codex.Accounts; len(accounts) != 1 || accounts[0].ID != "codex-a" {
		t.Errorf("accounts after delete = %+v", accounts)
	}
	code, out = codexManagementCall(t, srv, http.MethodDelete, "/api/codex/accounts/codex-b", "")
	requireCodexError(t, code, out, http.StatusNotFound, "not found")
}

func TestCodexAccountRefreshRoute(t *testing.T) {
	_, upstream := codexModelsBackend(t)
	tokenEndpoint := fakeCodexTokenEndpoint(t, func(w http.ResponseWriter, form url.Values) {
		if form.Get("refresh_token") != "r-a" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"tok-a2","refresh_token":"r-a2","expires_in":864000}`)
	})
	srv := newCodexTestServer(t, upstream,
		codex.AccountConfig{ID: "codex-a", Token: "tok-a", RefreshToken: "r-a", Enabled: true},
		codex.AccountConfig{ID: "codex-b", Token: "tok-b", RefreshToken: "r-gone", Enabled: true})
	srv.codexOAuthMgr.SetEndpoints("", tokenEndpoint.URL)

	code, out := codexManagementCall(t, srv, http.MethodPost, "/api/codex/accounts/codex-a/refresh", "")
	if code != http.StatusOK || out["status"] != "ok" {
		t.Fatalf("refresh = %d %v", code, out)
	}
	if expires, _ := out["expiresAt"].(string); expires == "" {
		t.Errorf("expiresAt = %v, want the new expiry", out["expiresAt"])
	}
	if saved := config.Get().Codex.Accounts[0]; saved.Token != "tok-a2" || saved.RefreshToken != "r-a2" {
		t.Errorf("saved account = %+v, want the rotated tokens", saved)
	}

	code, out = codexManagementCall(t, srv, http.MethodPost, "/api/codex/accounts/codex-b/refresh", "")
	requireCodexError(t, code, out, http.StatusBadGateway, "invalid_grant")
	if snap := codexSnapshot(t, srv, "codex-b"); snap.Status != "needs_login" {
		t.Errorf("codex-b status = %s, want needs_login", snap.Status)
	}

	code, out = codexManagementCall(t, srv, http.MethodPost, "/api/codex/accounts/nope/refresh", "")
	requireCodexError(t, code, out, http.StatusNotFound, "not found")
}

func TestCodexAccountTestRoute(t *testing.T) {
	backend, upstream := codexModelsBackend(t)
	srv := newCodexTestServer(t, upstream, codex.AccountConfig{ID: "codex-a", Token: "tok-a", AccountID: "acct-a", Enabled: true})

	code, out := codexManagementCall(t, srv, http.MethodPost, "/api/codex/accounts/codex-a/test", "")
	if code != http.StatusOK || out["status"] != "ok" || out["models"] != float64(2) {
		t.Fatalf("test = %d %v, want ok with 2 models", code, out)
	}
	call := backend.last(t)
	if call.path != "/models" || call.header.Get("Authorization") != "Bearer tok-a" || call.header.Get("Chatgpt-Account-Id") != "acct-a" {
		t.Errorf("catalog request = %s %v", call.path, call.header)
	}
	if snap := codexSnapshot(t, srv, "codex-a"); snap.Quota == nil || snap.Quota.Primary == nil {
		t.Errorf("quota = %+v, want the catalog reply's windows recorded", snap.Quota)
	}

	code, out = codexManagementCall(t, srv, http.MethodPost, "/api/codex/accounts/nope/test", "")
	requireCodexError(t, code, out, http.StatusNotFound, "not found")
}

func TestCodexAccountTestRouteReportsUpstreamError(t *testing.T) {
	_, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"detail":"workspace deactivated"}`)
	})
	srv := newCodexTestServer(t, upstream, codex.AccountConfig{ID: "codex-a", Token: "tok-a", Enabled: true})
	code, out := codexManagementCall(t, srv, http.MethodPost, "/api/codex/accounts/codex-a/test", "")
	requireCodexError(t, code, out, http.StatusBadGateway, "workspace deactivated")
}

// Discovery reads the catalog of the first enabled account.
func TestCodexModelsFetchRoute(t *testing.T) {
	backend, upstream := codexModelsBackend(t)
	srv := newCodexTestServer(t, upstream,
		codex.AccountConfig{ID: "codex-off", Token: "tok-off", Enabled: false},
		codex.AccountConfig{ID: "codex-on", Token: "tok-on", Enabled: true})

	code, out := codexManagementCall(t, srv, http.MethodPost, "/api/codex/models/fetch", "{}")
	if code != http.StatusOK || out["status"] != "ok" {
		t.Fatalf("fetch = %d %v", code, out)
	}
	if got := backend.last(t).header.Get("Authorization"); got != "Bearer tok-on" {
		t.Errorf("catalog fetched with %q, want the first enabled account", got)
	}
	models, _ := out["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("models = %v, want the two listed entries", out["models"])
	}
	first := models[0].(map[string]any)
	if first["id"] != "gpt-5.5" || first["displayName"] != "GPT-5.5" || first["contextLength"] != float64(500000) || first["defaultEffort"] != "medium" {
		t.Errorf("first model = %v", first)
	}
	if efforts, _ := first["efforts"].([]any); len(efforts) != 2 {
		t.Errorf("efforts = %v", first["efforts"])
	}
}

func TestCodexModelsFetchRouteNeedsAnAccount(t *testing.T) {
	_, upstream := codexModelsBackend(t)
	srv := newCodexTestServer(t, upstream, codex.AccountConfig{ID: "codex-off", Token: "t", Enabled: false})
	code, out := codexManagementCall(t, srv, http.MethodPost, "/api/codex/models/fetch", "{}")
	requireCodexError(t, code, out, http.StatusBadRequest, "no enabled codex account")
}

func TestCodexAuthStartStatusCancel(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)

	code, out := codexManagementCall(t, srv, http.MethodPost, "/api/codex/auth/start", `{"mode":"manual"}`)
	if code != http.StatusOK || out["status"] != "ok" || out["mode"] != "manual" {
		t.Fatalf("start = %d %v", code, out)
	}
	sessionID, _ := out["session_id"].(string)
	authURL, _ := out["auth_url"].(string)
	if sessionID == "" || !strings.HasPrefix(authURL, auth.CodexAuthorizeURL+"?") {
		t.Errorf("start reply = %v, want a session id and the authorize URL", out)
	}

	code, out = codexManagementCall(t, srv, http.MethodGet, "/api/codex/auth/status?session_id="+sessionID, "")
	if code != http.StatusOK || out["status"] != "ok" {
		t.Fatalf("status = %d %v", code, out)
	}
	session, _ := out["session"].(map[string]any)
	if session["id"] != sessionID || session["status"] != "pending" || session["mode"] != "manual" || session["auth_url"] != authURL {
		t.Errorf("session = %v", session)
	}

	code, out = codexManagementCall(t, srv, http.MethodPost, "/api/codex/auth/cancel", `{"session_id":"`+sessionID+`"}`)
	if code != http.StatusOK || out["status"] != "ok" {
		t.Fatalf("cancel = %d %v", code, out)
	}
	_, out = codexManagementCall(t, srv, http.MethodGet, "/api/codex/auth/status?session_id="+sessionID, "")
	if session, _ := out["session"].(map[string]any); session["status"] != "cancelled" {
		t.Errorf("session after cancel = %v, want cancelled", session)
	}

	code, out = codexManagementCall(t, srv, http.MethodGet, "/api/codex/auth/status?session_id=nope", "")
	requireCodexError(t, code, out, http.StatusNotFound, "not found")
	code, out = codexManagementCall(t, srv, http.MethodGet, "/api/codex/auth/status", "")
	requireCodexError(t, code, out, http.StatusBadRequest, "session_id")
	code, out = codexManagementCall(t, srv, http.MethodPost, "/api/codex/auth/cancel", `{"session_id":"nope"}`)
	requireCodexError(t, code, out, http.StatusNotFound, "not found")
}

// Completing a login with the pasted redirect URL registers the account,
// enabled at priority 1, and turns the gateway on.
func TestCodexAuthCompleteRegistersAccount(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	idToken := fakeCodexIDToken("user@example.com", "acct-1234567890", "plus")
	tokenEndpoint := fakeCodexTokenEndpoint(t, func(w http.ResponseWriter, form url.Values) {
		if form.Get("grant_type") != "authorization_code" || form.Get("code") != "the-code" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok-1", "refresh_token": "r-1", "id_token": idToken, "expires_in": 864000,
		})
	})
	srv.codexOAuthMgr.SetEndpoints("", tokenEndpoint.URL)

	_, start := codexManagementCall(t, srv, http.MethodPost, "/api/codex/auth/start", `{"mode":"manual"}`)
	sessionID, _ := start["session_id"].(string)
	authURL, _ := url.Parse(start["auth_url"].(string))
	redirect := auth.CodexRedirectURI + "?code=the-code&state=" + authURL.Query().Get("state")

	code, out := codexManagementCall(t, srv, http.MethodPost, "/api/codex/auth/complete",
		`{"session_id":"`+sessionID+`","redirect_url":"`+redirect+`"}`)
	if code != http.StatusOK || out["status"] != "ok" {
		t.Fatalf("complete = %d %v", code, out)
	}
	account, _ := out["account"].(map[string]any)
	if account["id"] != "codex-user@example.com" || account["email"] != "user@example.com" || account["planType"] != "plus" {
		t.Errorf("account = %v", account)
	}
	cfg := config.Get().Codex
	if !cfg.Enabled {
		t.Error("login did not turn the gateway on")
	}
	if len(cfg.Accounts) != 1 {
		t.Fatalf("accounts = %+v", cfg.Accounts)
	}
	got := cfg.Accounts[0]
	if got.Token != "tok-1" || got.RefreshToken != "r-1" || got.IDToken != idToken || got.AccountID != "acct-1234567890" ||
		!got.Enabled || got.Priority != 1 || got.Source != "oauth" || got.ExpiresAt == "" {
		t.Errorf("registered account = %+v", got)
	}

	code, out = codexManagementCall(t, srv, http.MethodPost, "/api/codex/auth/complete", `{"session_id":"`+sessionID+`"}`)
	requireCodexError(t, code, out, http.StatusBadRequest, "redirect_url")
	code, out = codexManagementCall(t, srv, http.MethodPost, "/api/codex/auth/complete", `{"session_id":"nope","redirect_url":"x"}`)
	requireCodexError(t, code, out, http.StatusBadRequest, "not found")
}

func TestRegisterCodexAccountMatching(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	seedCodexConfig(t, codex.Config{Accounts: []codex.AccountConfig{
		{ID: "codex-user@example.com", Name: "Mine", Email: "user@example.com", AccountID: "acct-1", Token: "old", RefreshToken: "r-old", Priority: 4, Enabled: false},
	}})
	login := func(email, accountID string) codex.AccountConfig {
		t.Helper()
		acc, err := srv.registerCodexAccount(&auth.CodexAccountResult{
			AccessToken: "tok-" + email + accountID, RefreshToken: "r-" + email + accountID,
			ExpiresAt: time.Now().Add(time.Hour), Email: email, AccountID: accountID, PlanType: "team",
		})
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		return acc
	}

	// The same ChatGPT account logs in again: its entry is updated in place.
	again := login("user@example.com", "acct-1")
	accounts := config.Get().Codex.Accounts
	if again.ID != "codex-user@example.com" || len(accounts) != 1 {
		t.Fatalf("re-login = %+v, accounts = %+v, want the existing entry", again, accounts)
	}
	if a := accounts[0]; a.Token != "tok-user@example.comacct-1" || a.Name != "Mine" || a.Priority != 4 || !a.Enabled || a.PlanType != "team" {
		t.Errorf("re-logged account = %+v, want new tokens, kept name and priority, enabled", a)
	}

	// Another member of the same workspace is another account.
	other := login("colleague@example.com", "acct-1")
	if other.ID != "codex-colleague@example.com" || other.Priority != 1 {
		t.Errorf("workspace member = %+v, want its own account at priority 1", other)
	}

	// The same email in another workspace collides on the id.
	elsewhere := login("user@example.com", "acct-9876543210")
	if elsewhere.ID != "codex-user@example.com-acct-987" {
		t.Errorf("collision id = %q, want codex-user@example.com-acct-987", elsewhere.ID)
	}

	// Without an email the account id names it.
	anonymous := login("", "acct-anon")
	if anonymous.ID != "codex-acct-anon" {
		t.Errorf("id without email = %q, want codex-acct-anon", anonymous.ID)
	}
	if n := len(config.Get().Codex.Accounts); n != 4 {
		t.Errorf("accounts = %d, want 4", n)
	}
	if !config.Get().Codex.Enabled {
		t.Error("registration did not turn the gateway on")
	}
}

// The loopback hook registers the account; nothing waits on its result.
func TestRegisterCodexLoginHook(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	srv.registerCodexLogin(&auth.CodexAccountResult{AccessToken: "tok", RefreshToken: "r", Email: "loop@example.com", AccountID: "acct-l"})
	if accounts := config.Get().Codex.Accounts; len(accounts) != 1 || accounts[0].ID != "codex-loop@example.com" {
		t.Errorf("accounts = %+v, want the loopback login registered", accounts)
	}
}

// /v1/usage lists a 5h and a weekly window for each account whose last reply
// carried them.
func TestCodexUsageWindows(t *testing.T) {
	_, upstream := newFakeCodexBackend(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		writeCodexSSE(w, codexQuotaHeaderMap())
	})
	srv := newCodexTestServer(t, upstream, codex.AccountConfig{ID: "codex-a", Token: "tok-a", Email: "a@b.test", Enabled: true})
	if rec := postCodexMessages(t, srv, `{"model":"gpt-5.5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("messages status = %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	req.Header.Set("x-api-key", "test-api-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("usage status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Windows []map[string]any `json:"windows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	byLabel := map[string]map[string]any{}
	for _, w := range body.Windows {
		if w["provider"] == "codex" {
			byLabel[w["label"].(string)] = w
		}
	}
	fiveHour, weekly := byLabel["Codex 5h (a@b.test)"], byLabel["Codex weekly (a@b.test)"]
	if fiveHour == nil || weekly == nil {
		t.Fatalf("codex windows = %v, want 5h and weekly", byLabel)
	}
	if fiveHour["used_percent"] != float64(25) || fiveHour["remaining_fraction"] != 0.75 || fiveHour["account_id"] != "codex-a" {
		t.Errorf("5h window = %v", fiveHour)
	}
	if weekly["used_percent"] != float64(60) || weekly["reset_at"] == "" {
		t.Errorf("weekly window = %v", weekly)
	}
	ids, _ := fiveHour["model_ids"].([]any)
	if len(ids) != len(codex.DefaultAllowlist()) {
		t.Errorf("model_ids = %v, want every allowlisted model", ids)
	}
	if got := codexWindowLabel(45); got != "45m" {
		t.Errorf("codexWindowLabel(45) = %q, want 45m", got)
	}
}

// /v1/models advertises the enabled Codex allowlist, aliases included, and the
// Cloud Code catalog cedes those ids to the gateway.
func TestCodexDiscovery(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Codex.Enabled = true
	cfg.Codex.Allowlist = []codex.ModelConfig{
		{ID: "gpt-5.5", Alias: "fast", DisplayName: "GPT-5.5", ContextLength: 500000, Enabled: true},
		{ID: "gpt-6-sol", Enabled: false},
	}
	srv, _, _ := newTestServerWithManager(t)
	var models []any
	appendCodexDiscovery(srv, cfg, &models, map[string]bool{})
	if len(models) != 2 {
		t.Fatalf("models = %v, want the entry and its alias", models)
	}
	entry := models[0].(map[string]any)
	if entry["id"] != "gpt-5.5" || entry["owned_by"] != "openai" || entry["context_window"] != 500000 || entry["max_output_tokens"] != 128000 {
		t.Errorf("entry = %v, want the built-in output limit filled in", entry)
	}
	if alias := models[1].(map[string]any); alias["id"] != "fast" || alias["description"] != "GPT-5.5 (Alias)" {
		t.Errorf("alias entry = %v", alias)
	}

	owned := gatewayOwnedModelIDs(cfg)
	for _, id := range []string{"gpt-5.5", "GPT-5.5", "fast"} {
		if !owned[id] && !owned[strings.ToLower(id)] {
			t.Errorf("%q not owned by the Codex gateway", id)
		}
	}
	if owned["gpt-6-sol"] {
		t.Error("a disabled entry is owned")
	}

	cfg.Codex.Enabled = false
	models = nil
	appendCodexDiscovery(srv, cfg, &models, map[string]bool{})
	if len(models) != 0 {
		t.Errorf("disabled gateway advertised %v", models)
	}
}
