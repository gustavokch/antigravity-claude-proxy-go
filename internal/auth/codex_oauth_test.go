package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// codexTestJWT builds an unsigned JWT carrying claims; the manager reads
// claims without verifying signatures.
func codexTestJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(payload) + ".sig"
}

func codexTestIDToken(t *testing.T) string {
	return codexTestJWT(t, map[string]any{
		"email": "user@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-123",
			"chatgpt_plan_type":  "plus",
		},
	})
}

// newCodexTokenServer serves the token endpoint: each POSTed form goes to the
// returned channel and the reply is the given token response.
func newCodexTokenServer(t *testing.T, reply map[string]any) (*httptest.Server, chan url.Values) {
	t.Helper()
	forms := make(chan url.Values, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("token request %s with Content-Type %q, want a form POST", r.Method, r.Header.Get("Content-Type"))
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token form: %v", err)
		}
		forms <- r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	return srv, forms
}

func assertForm(t *testing.T, form url.Values, want map[string]string) {
	t.Helper()
	for key, value := range want {
		if got := form.Get(key); got != value {
			t.Errorf("form %s = %q, want %q", key, got, value)
		}
	}
	if len(form) != len(want) {
		t.Errorf("form has %d fields, want %d: %v", len(form), len(want), form)
	}
}

func TestCodexAuthorizeURLCarriesPKCEAndCodexParams(t *testing.T) {
	m := NewCodexOAuthManager()
	m.SetEndpoints("https://auth.example.test/oauth/authorize", "")
	session, err := m.StartAuthSession("manual")
	if err != nil {
		t.Fatalf("StartAuthSession: %v", err)
	}
	t.Cleanup(func() { _ = m.CancelSession(session.ID) })

	u, err := url.Parse(session.AuthURL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != "https://auth.example.test/oauth/authorize" {
		t.Errorf("authorize endpoint = %s", got)
	}

	q := u.Query()
	verifier := session.codeVerifier
	sum := sha256.Sum256([]byte(verifier))
	assertForm(t, q, map[string]string{
		"client_id":                  CodexClientID,
		"response_type":              "code",
		"redirect_uri":               CodexRedirectURI,
		"scope":                      CodexScopes,
		"state":                      session.state,
		"code_challenge":             base64.RawURLEncoding.EncodeToString(sum[:]),
		"code_challenge_method":      "S256",
		"prompt":                     "login",
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
	})

	if len(session.state) != 32 || strings.Trim(session.state, "0123456789abcdef") != "" {
		t.Errorf("state %q is not 16 random bytes in hex", session.state)
	}
	raw, err := base64.RawURLEncoding.DecodeString(verifier)
	if err != nil || len(raw) != 96 {
		t.Errorf("verifier %q is not 96 bytes of base64url (decoded %d bytes, err %v)", verifier, len(raw), err)
	}

	other, err := m.StartAuthSession("manual")
	if err != nil {
		t.Fatalf("StartAuthSession: %v", err)
	}
	t.Cleanup(func() { _ = m.CancelSession(other.ID) })
	if other.state == session.state || other.codeVerifier == session.codeVerifier {
		t.Error("two sessions share a state or a verifier")
	}
}

func TestCodexLoopbackCallbackCompletesSession(t *testing.T) {
	exp := time.Now().Add(10 * 24 * time.Hour).Truncate(time.Second)
	srv, forms := newCodexTokenServer(t, map[string]any{
		"access_token":  codexTestJWT(t, map[string]any{"exp": exp.Unix()}),
		"refresh_token": "refresh-1",
		"id_token":      codexTestIDToken(t),
		"expires_in":    3600,
	})

	m := NewCodexOAuthManager()
	m.SetEndpoints("", srv.URL)
	hooked := make(chan *CodexAccountResult, 2)
	m.OnComplete(func(acc *CodexAccountResult) { hooked <- acc })

	session, err := m.StartAuthSession("loopback")
	if err != nil {
		t.Fatalf("StartAuthSession: %v", err)
	}
	t.Cleanup(func() { _ = m.CancelSession(session.ID) })
	if session.Mode != "loopback" {
		t.Skipf("%s is unavailable to the test process", codexCallbackAddr)
	}

	resp, err := http.Get("http://" + codexCallbackAddr + codexCallbackPath + "?code=auth-code&state=" + session.state)
	if err != nil {
		t.Fatalf("callback request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status = %d, want 200", resp.StatusCode)
	}

	assertForm(t, <-forms, map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     CodexClientID,
		"code":          "auth-code",
		"redirect_uri":  CodexRedirectURI,
		"code_verifier": session.codeVerifier,
	})

	var acc *CodexAccountResult
	select {
	case acc = <-hooked:
	default:
		t.Fatal("OnComplete hook did not fire before the callback answered")
	}
	if acc.Email != "user@example.com" || acc.AccountID != "acct-123" || acc.PlanType != "plus" || acc.RefreshToken != "refresh-1" {
		t.Errorf("hook account = %+v", acc)
	}
	if !acc.ExpiresAt.Equal(exp) {
		t.Errorf("ExpiresAt = %v, want the access token exp %v", acc.ExpiresAt, exp)
	}
	if len(hooked) != 0 {
		t.Error("OnComplete hook fired more than once")
	}

	snap, ok := m.GetSession(session.ID)
	if !ok || snap.Status != "completed" || snap.Email != "user@example.com" || snap.Mode != "loopback" {
		t.Errorf("snapshot = %+v (found %v)", snap, ok)
	}

	// The fixed port must be free for the next login once this one is done.
	ln, err := net.Listen("tcp", codexCallbackAddr)
	if err != nil {
		t.Fatalf("callback port still held after completion: %v", err)
	}
	ln.Close()
}

func TestCodexLoopbackCallbackRejectsStateMismatch(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	t.Cleanup(srv.Close)

	m := NewCodexOAuthManager()
	m.SetEndpoints("", srv.URL)
	m.OnComplete(func(*CodexAccountResult) { t.Error("hook fired for a rejected callback") })
	session := &CodexAuthSession{ID: "s1", Mode: "loopback", state: "expected", status: "pending", done: make(chan struct{})}

	rec := httptest.NewRecorder()
	m.handleLoopbackCallback(session, rec, httptest.NewRequest(http.MethodGet, codexCallbackPath+"?code=c&state=forged", nil))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	snap := session.Snapshot()
	if snap.Status != "failed" || !strings.Contains(snap.Error, "state") {
		t.Errorf("snapshot = %+v, want failed with a state error", snap)
	}
	if calls.Load() != 0 {
		t.Error("token endpoint called for a forged state")
	}
}

func TestCodexCallbackEscapesErrorParam(t *testing.T) {
	m := NewCodexOAuthManager()
	session := &CodexAuthSession{ID: "s1", state: "state-1", status: "pending", done: make(chan struct{})}

	rec := httptest.NewRecorder()
	m.handleLoopbackCallback(session, rec, httptest.NewRequest(http.MethodGet, codexCallbackPath+"?error=%3Cimg+src%3Dx+onerror%3Dalert(1)%3E", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "&lt;img") || strings.Contains(body, "<img src=x onerror=alert(1)>") {
		t.Errorf("error parameter not escaped: %s", body)
	}
	if session.Snapshot().Status != "failed" {
		t.Errorf("status = %q, want failed", session.Snapshot().Status)
	}
}

func TestCodexStartAuthSessionFallsBackToManualWhenPortBusy(t *testing.T) {
	ln, err := net.Listen("tcp", codexCallbackAddr)
	if err != nil {
		t.Skipf("cannot occupy %s for the test: %v", codexCallbackAddr, err)
	}
	defer ln.Close()

	m := NewCodexOAuthManager()
	session, err := m.StartAuthSession("loopback")
	if err != nil {
		t.Fatalf("StartAuthSession: %v", err)
	}
	t.Cleanup(func() { _ = m.CancelSession(session.ID) })

	if session.Mode != "manual" {
		t.Errorf("Mode = %q, want manual", session.Mode)
	}
	snap, _ := m.GetSession(session.ID)
	if snap.Mode != "manual" || snap.Status != "pending" {
		t.Errorf("snapshot = %+v, want a pending manual session", snap)
	}
	u, err := url.Parse(session.AuthURL)
	if err != nil || u.Query().Get("redirect_uri") != CodexRedirectURI {
		t.Errorf("auth URL %q does not redirect to %s", session.AuthURL, CodexRedirectURI)
	}
}

func TestCodexLoopbackSessionSupersedesEarlierOne(t *testing.T) {
	m := NewCodexOAuthManager()
	first, err := m.StartAuthSession("loopback")
	if err != nil {
		t.Fatalf("StartAuthSession: %v", err)
	}
	t.Cleanup(func() { _ = m.CancelSession(first.ID) })
	if first.Mode != "loopback" {
		t.Skipf("%s is unavailable to the test process", codexCallbackAddr)
	}

	second, err := m.StartAuthSession("loopback")
	if err != nil {
		t.Fatalf("StartAuthSession: %v", err)
	}
	t.Cleanup(func() { _ = m.CancelSession(second.ID) })

	if second.Mode != "loopback" {
		t.Errorf("second session Mode = %q, want loopback", second.Mode)
	}
	if snap, _ := m.GetSession(first.ID); snap.Status != "cancelled" {
		t.Errorf("first session status = %q, want cancelled", snap.Status)
	}
}

func TestCodexCompleteManualAuth(t *testing.T) {
	srv, forms := newCodexTokenServer(t, map[string]any{
		"access_token":  "opaque-access",
		"refresh_token": "refresh-1",
		"id_token":      codexTestIDToken(t),
		"expires_in":    3600,
	})
	m := NewCodexOAuthManager()
	m.SetEndpoints("", srv.URL)
	m.OnComplete(func(*CodexAccountResult) { t.Error("hook fired for a manual completion") })

	start := func(t *testing.T) *CodexAuthSession {
		t.Helper()
		session, err := m.StartAuthSession("manual")
		if err != nil {
			t.Fatalf("StartAuthSession: %v", err)
		}
		t.Cleanup(func() { _ = m.CancelSession(session.ID) })
		return session
	}

	for _, tc := range []struct {
		name     string
		input    func(s *CodexAuthSession) string
		wantCode string
	}{
		{
			name: "full redirect URL",
			input: func(s *CodexAuthSession) string {
				return "  http://localhost:1455/auth/callback?code=url-code&scope=openid&state=" + s.state + "\n"
			},
			wantCode: "url-code",
		},
		{
			name:     "bare code",
			input:    func(*CodexAuthSession) string { return " bare-code " },
			wantCode: "bare-code",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session := start(t)
			acc, err := m.CompleteManualAuth(session.ID, tc.input(session))
			if err != nil {
				t.Fatalf("CompleteManualAuth: %v", err)
			}
			assertForm(t, <-forms, map[string]string{
				"grant_type":    "authorization_code",
				"client_id":     CodexClientID,
				"code":          tc.wantCode,
				"redirect_uri":  CodexRedirectURI,
				"code_verifier": session.codeVerifier,
			})
			if acc.AccessToken != "opaque-access" || acc.Email != "user@example.com" || acc.AccountID != "acct-123" {
				t.Errorf("account = %+v", acc)
			}
			if snap, _ := m.GetSession(session.ID); snap.Status != "completed" || snap.Email != "user@example.com" {
				t.Errorf("snapshot = %+v", snap)
			}
		})
	}

	t.Run("state mismatch", func(t *testing.T) {
		session := start(t)
		_, err := m.CompleteManualAuth(session.ID, "http://localhost:1455/auth/callback?code=c&state=other")
		if err == nil || !strings.Contains(err.Error(), "state") {
			t.Fatalf("err = %v, want a state mismatch", err)
		}
		select {
		case form := <-forms:
			t.Errorf("token endpoint called despite the mismatch: %v", form)
		default:
		}
		if snap, _ := m.GetSession(session.ID); snap.Status != "pending" {
			t.Errorf("status = %q, want the session still pending for a corrected paste", snap.Status)
		}
	})

	t.Run("URL without state", func(t *testing.T) {
		session := start(t)
		if _, err := m.CompleteManualAuth(session.ID, "http://localhost:1455/auth/callback?code=c"); err == nil {
			t.Fatal("a redirect URL without state was accepted")
		}
	})

	t.Run("empty input", func(t *testing.T) {
		session := start(t)
		if _, err := m.CompleteManualAuth(session.ID, "   "); err == nil {
			t.Fatal("empty input was accepted")
		}
	})

	t.Run("cancelled session", func(t *testing.T) {
		session := start(t)
		if err := m.CancelSession(session.ID); err != nil {
			t.Fatalf("CancelSession: %v", err)
		}
		if _, err := m.CompleteManualAuth(session.ID, "code"); err == nil {
			t.Fatal("a cancelled session was completed")
		}
		if snap, _ := m.GetSession(session.ID); snap.Status != "cancelled" {
			t.Errorf("status = %q, want cancelled", snap.Status)
		}
	})
}

func TestCodexRefreshToken(t *testing.T) {
	exp := time.Now().Add(10 * 24 * time.Hour).Truncate(time.Second)
	srv, forms := newCodexTokenServer(t, map[string]any{
		"access_token":  codexTestJWT(t, map[string]any{"exp": exp.Unix()}),
		"refresh_token": "rotated-refresh",
		"id_token":      codexTestIDToken(t),
		"expires_in":    3600,
	})
	m := NewCodexOAuthManager()
	m.SetEndpoints("", srv.URL)

	acc, err := m.RefreshToken(context.Background(), "old-refresh")
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	assertForm(t, <-forms, map[string]string{
		"client_id":     CodexClientID,
		"grant_type":    "refresh_token",
		"refresh_token": "old-refresh",
		"scope":         "openid profile email",
	})
	if acc.RefreshToken != "rotated-refresh" || acc.Email != "user@example.com" || acc.AccountID != "acct-123" || acc.PlanType != "plus" {
		t.Errorf("account = %+v", acc)
	}
	if !acc.ExpiresAt.Equal(exp) {
		t.Errorf("ExpiresAt = %v, want the access token exp %v", acc.ExpiresAt, exp)
	}
}

func TestCodexRefreshTokenExpiryFallsBackToExpiresIn(t *testing.T) {
	srv, _ := newCodexTokenServer(t, map[string]any{
		"access_token":  "opaque-access",
		"refresh_token": "rotated-refresh",
		"expires_in":    3600,
	})
	m := NewCodexOAuthManager()
	m.SetEndpoints("", srv.URL)

	before := time.Now()
	acc, err := m.RefreshToken(context.Background(), "old-refresh")
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if acc.ExpiresAt.Before(before.Add(time.Hour)) || acc.ExpiresAt.After(time.Now().Add(time.Hour)) {
		t.Errorf("ExpiresAt = %v, want now + expires_in", acc.ExpiresAt)
	}
	if acc.Email != "" || acc.AccountID != "" || acc.PlanType != "" {
		t.Errorf("claims without an id_token = %q %q %q, want empty so callers keep theirs", acc.Email, acc.AccountID, acc.PlanType)
	}
}

func TestCodexRefreshTokenClassifiesErrors(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		terminal bool
	}{
		{"invalid_grant", http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Invalid refresh token"}`, true},
		{"refresh_token_reused", http.StatusBadRequest, `{"error":{"message":"Your refresh token has already been used","type":"invalid_request_error","code":"refresh_token_reused"}}`, true},
		{"refresh_token_expired", http.StatusBadRequest, `{"error":{"code":"refresh_token_expired"}}`, true},
		{"refresh_token_invalidated", http.StatusBadRequest, `{"error":{"code":"refresh_token_invalidated"}}`, true},
		{"token_revoked", http.StatusForbidden, `{"error":{"code":"token_revoked"}}`, true},
		{"401 without a code", http.StatusUnauthorized, `{}`, true},
		{"other 4xx", http.StatusBadRequest, `{"error":"invalid_request"}`, false},
		{"rate limited", http.StatusTooManyRequests, `{"error":"rate_limited"}`, false},
		{"5xx naming a grant error", http.StatusBadGateway, `upstream said invalid_grant`, false},
		{"200 without expires_in", http.StatusOK, `{"access_token":"a","refresh_token":"r"}`, false},
		{"200 without refresh_token", http.StatusOK, `{"access_token":"a","expires_in":3600}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			m := NewCodexOAuthManager()
			m.SetEndpoints("", srv.URL)

			_, err := m.RefreshToken(context.Background(), "refresh")
			if err == nil {
				t.Fatal("RefreshToken succeeded")
			}
			if got := errors.Is(err, ErrCodexGrantInvalid); got != tc.terminal {
				t.Errorf("errors.Is(err, ErrCodexGrantInvalid) = %v, want %v (err: %v)", got, tc.terminal, err)
			}
			if tc.status != http.StatusOK && !strings.Contains(err.Error(), fmt.Sprintf("(status %d)", tc.status)) {
				t.Errorf("error %q does not name the status", err)
			}
		})
	}

	if _, err := NewCodexOAuthManager().RefreshToken(context.Background(), ""); !errors.Is(err, ErrCodexGrantInvalid) {
		t.Errorf("empty refresh token: err = %v, want ErrCodexGrantInvalid", err)
	}
}

func TestCodexRefreshTokenDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, r *http.Request) {
		followed.Store(true)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "refresh_token": "r", "expires_in": 3600})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m := NewCodexOAuthManager()
	m.SetEndpoints("", srv.URL+"/oauth/token")
	if _, err := m.RefreshToken(context.Background(), "refresh"); err == nil {
		t.Fatal("a redirected refresh succeeded")
	}
	if followed.Load() {
		t.Error("the refresh token was re-sent to the redirect target")
	}
}

func TestParseCodexIDToken(t *testing.T) {
	cases := []struct {
		name                       string
		token                      string
		email, accountID, planType string
	}{
		{"all claims", codexTestIDToken(t), "user@example.com", "acct-123", "plus"},
		{
			"plan missing defaults to free",
			codexTestJWT(t, map[string]any{"email": "a@b.c", "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-9"}}),
			"a@b.c", "acct-9", "free",
		},
		{"padded payload", strings.Replace(codexTestIDToken(t), ".sig", "", 1) + "=.sig", "user@example.com", "acct-123", "plus"},
		{"not a JWT", "opaque-token", "", "", ""},
		{"payload not base64", "a.!!!.c", "", "", ""},
		{"payload not JSON", "a." + base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".c", "", "", ""},
		{"empty", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			email, accountID, planType := ParseCodexIDToken(tc.token)
			if email != tc.email || accountID != tc.accountID || planType != tc.planType {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)", email, accountID, planType, tc.email, tc.accountID, tc.planType)
			}
		})
	}
}
