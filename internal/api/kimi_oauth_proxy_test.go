package api

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-go-proxy/internal/cachebump"
	"antigravity-go-proxy/internal/config"
)

// kimiOAuthUpstream records the request the upstream saw and answers with a
// minimal SSE message_start stream.
type kimiOAuthUpstream struct {
	srv       *httptest.Server
	path      atomic.Value // string
	auth      atomic.Value // string
	platform  atomic.Value // string
	userAgent atomic.Value // string
	called    atomic.Int64
}

func newKimiOAuthUpstream(t *testing.T) *kimiOAuthUpstream {
	t.Helper()
	u := &kimiOAuthUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.called.Add(1)
		u.path.Store(r.URL.Path)
		u.auth.Store(r.Header.Get("Authorization"))
		u.platform.Store(r.Header.Get("X-Msh-Platform"))
		u.userAgent.Store(r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *kimiOAuthUpstream) load(v atomic.Value) string {
	s, _ := v.Load().(string)
	return s
}

// seedKimiOAuthConfig saves a kimi block with an OAuth credential pointing at
// the given hosts.
func seedKimiOAuthConfig(t *testing.T, oauth map[string]any, extra map[string]any) {
	t.Helper()
	kimiBlock := map[string]any{
		"enabled": true,
		"allowlist": []map[string]any{
			{"id": "kimi-for-coding", "enabled": true},
		},
		"oauth": oauth,
	}
	maps.Copy(kimiBlock, extra)
	if _, err := config.Save(map[string]any{"kimi": kimiBlock}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
}

func postKimiMessage(t *testing.T, server *Server) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"kimi-for-coding","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func TestServer_ForwardToKimi_OAuthTokenUsed(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	upstream := newKimiOAuthUpstream(t)
	seedKimiOAuthConfig(t, map[string]any{
		"token":     "oauth-tok-1",
		"oauthHost": "https://auth.kimi.ai",
		"baseUrl":   upstream.srv.URL + "/coding/v1",
	}, nil)

	server := newKimiTestServer(t)
	rec := postKimiMessage(t, server)

	if rec.Code != 200 {
		t.Fatalf("client status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := upstream.load(upstream.path); got != "/coding/v1/messages" {
		t.Errorf("upstream path = %q, want /coding/v1/messages", got)
	}
	if got := upstream.load(upstream.auth); got != "Bearer oauth-tok-1" {
		t.Errorf("Authorization = %q, want Bearer oauth-tok-1", got)
	}
	if got := upstream.load(upstream.platform); got != "kimi_cli" {
		t.Errorf("X-Msh-Platform = %q, want kimi_cli", got)
	}
	if got := upstream.load(upstream.userAgent); !strings.HasPrefix(got, "KimiCLI/") {
		t.Errorf("User-Agent = %q, want KimiCLI/ prefix", got)
	}
}

func TestServer_ForwardToKimi_OAuthRefreshOnExpiry(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	upstream := newKimiOAuthUpstream(t)

	var refreshCalls atomic.Int64
	var refreshGrant atomic.Value
	fakeAuth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/token" {
			http.NotFound(w, r)
			return
		}
		refreshCalls.Add(1)
		r.ParseForm()
		refreshGrant.Store(r.PostForm.Get("grant_type"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"oauth-tok-2","refresh_token":"rt-2","expires_in":3600}`))
	}))
	defer fakeAuth.Close()

	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	seedKimiOAuthConfig(t, map[string]any{
		"token":        "oauth-tok-1",
		"refreshToken": "rt-1",
		"expiresAt":    past,
		"oauthHost":    fakeAuth.URL,
		"baseUrl":      upstream.srv.URL + "/coding/v1",
	}, nil)

	server := newKimiTestServer(t)
	rec := postKimiMessage(t, server)

	if rec.Code != 200 {
		t.Fatalf("client status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := upstream.load(upstream.auth); got != "Bearer oauth-tok-2" {
		t.Errorf("Authorization = %q, want Bearer oauth-tok-2", got)
	}
	if refreshCalls.Load() != 1 {
		t.Errorf("refresh POSTs = %d, want 1", refreshCalls.Load())
	}
	if got := refreshGrant.Load(); got != "refresh_token" {
		t.Errorf("grant_type = %v, want refresh_token", got)
	}
	if tok := config.Get().Kimi.OAuth; tok == nil || tok.Token != "oauth-tok-2" || tok.RefreshToken != "rt-2" {
		t.Errorf("persisted OAuth = %+v, want token oauth-tok-2 / rt-2", tok)
	}
}

func TestServer_ForwardToKimi_OAuthInvalidGrant(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	upstream := newKimiOAuthUpstream(t)

	var refreshCalls atomic.Int64
	fakeAuth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer fakeAuth.Close()

	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	oauth := map[string]any{
		"token":        "oauth-tok-1",
		"refreshToken": "rt-1",
		"expiresAt":    past,
		"oauthHost":    fakeAuth.URL,
		"baseUrl":      upstream.srv.URL + "/coding/v1",
	}
	seedKimiOAuthConfig(t, oauth, nil)

	server := newKimiTestServer(t)
	for i := range 3 {
		rec := postKimiMessage(t, server)
		if rec.Code != 401 {
			t.Fatalf("request %d: client status = %d, want 401; body = %s", i, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "authentication_error") {
			t.Errorf("request %d: body = %s, want authentication_error", i, rec.Body.String())
		}
	}
	if upstream.called.Load() != 0 {
		t.Errorf("upstream called %d times, want 0", upstream.called.Load())
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Errorf("refresh POSTs after 3 requests = %d, want 1 (a rejected refresh token must not be re-sent)", got)
	}

	// A new login carries a new refresh token, which is tried again.
	oauth["token"], oauth["refreshToken"] = "oauth-tok-new", "rt-new"
	seedKimiOAuthConfig(t, oauth, nil)
	postKimiMessage(t, server)
	if got := refreshCalls.Load(); got != 2 {
		t.Errorf("refresh POSTs after a new login = %d, want 2", got)
	}
}

func TestServer_ForwardToKimi_OAuthWinsOverAPIKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	upstream := newKimiOAuthUpstream(t)
	seedKimiOAuthConfig(t, map[string]any{
		"token":     "oauth-tok-1",
		"oauthHost": "https://auth.kimi.ai",
		"baseUrl":   upstream.srv.URL + "/coding/v1",
	}, map[string]any{
		"apiKey":  "sk-kimi-test",
		"baseUrl": "https://api.moonshot.ai/anthropic",
	})

	server := newKimiTestServer(t)
	rec := postKimiMessage(t, server)

	if rec.Code != 200 {
		t.Fatalf("client status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := upstream.load(upstream.auth); got != "Bearer oauth-tok-1" {
		t.Errorf("Authorization = %q, want OAuth token to win", got)
	}
}

func TestServer_ForwardToKimi_APIKeyOnly(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	upstream := newKimiOAuthUpstream(t)
	seedKimiOAuthConfig(t, nil, map[string]any{
		"apiKey":  "sk-kimi-test",
		"baseUrl": upstream.srv.URL,
	})

	server := newKimiTestServer(t)
	rec := postKimiMessage(t, server)

	if rec.Code != 200 {
		t.Fatalf("client status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := upstream.load(upstream.auth); got != "Bearer sk-kimi-test" {
		t.Errorf("Authorization = %q, want Bearer sk-kimi-test", got)
	}
	if got := upstream.load(upstream.platform); got != "" {
		t.Errorf("X-Msh-Platform = %q, want absent", got)
	}
}

func TestServer_ForwardToKimi_NoCredential(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	seedKimiOAuthConfig(t, nil, nil)

	server := newKimiTestServer(t)
	rec := postKimiMessage(t, server)

	if rec.Code != 400 {
		t.Fatalf("client status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no credential configured") {
		t.Errorf("body = %s, want no-credential message", rec.Body.String())
	}
}

func TestServer_SendKimiBump_OAuthCredential(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	upstream := newKimiOAuthUpstream(t)
	seedKimiOAuthConfig(t, map[string]any{
		"token":     "oauth-tok-1",
		"oauthHost": "https://auth.kimi.ai",
		"baseUrl":   upstream.srv.URL + "/coding/v1",
	}, nil)

	server := newKimiTestServer(t)
	rec := cachebump.Record{
		Body: []byte(`{"model":"kimi-for-coding","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`),
	}
	if _, err := server.sendKimiBump(context.Background(), rec); err != nil {
		t.Fatalf("sendKimiBump: %v", err)
	}

	if got := upstream.load(upstream.path); got != "/coding/v1/messages" {
		t.Errorf("upstream path = %q, want /coding/v1/messages", got)
	}
	if got := upstream.load(upstream.auth); got != "Bearer oauth-tok-1" {
		t.Errorf("Authorization = %q, want Bearer oauth-tok-1", got)
	}
	if got := upstream.load(upstream.platform); got != "kimi_cli" {
		t.Errorf("X-Msh-Platform = %q, want kimi_cli", got)
	}
}

// blockingKimiAuth is a fake auth host whose refresh answers only after
// release runs, so a test can act while a refresh is in flight.
func blockingKimiAuth(t *testing.T) (host string, arrived <-chan struct{}, release func()) {
	t.Helper()
	arrivedCh := make(chan struct{})
	releaseCh := make(chan struct{})
	var arriveOnce, releaseOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arriveOnce.Do(func() { close(arrivedCh) })
		<-releaseCh
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"oauth-tok-2","refresh_token":"rt-2","expires_in":3600}`))
	}))
	t.Cleanup(srv.Close)
	release = func() { releaseOnce.Do(func() { close(releaseCh) }) }
	t.Cleanup(release) // LIFO: runs before srv.Close, so a failed test never hangs
	return srv.URL, arrivedCh, release
}

func TestServer_KimiLogoutDuringRefreshStaysLoggedOut(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	upstream := newKimiOAuthUpstream(t)
	authHost, refreshArrived, releaseRefresh := blockingKimiAuth(t)
	seedKimiOAuthConfig(t, map[string]any{
		"token":        "oauth-tok-1",
		"refreshToken": "rt-1",
		"expiresAt":    time.Now().Add(-time.Hour).Format(time.RFC3339),
		"oauthHost":    authHost,
		"baseUrl":      upstream.srv.URL + "/coding/v1",
	}, nil)
	server := newKimiTestServer(t)

	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		postKimiMessage(t, server)
	}()
	<-refreshArrived

	var logoutCode atomic.Int64
	logoutDone := make(chan struct{})
	go func() {
		defer close(logoutDone)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/kimi/auth/logout", nil))
		logoutCode.Store(int64(rec.Code))
	}()
	// Unfixed, logout returns at once and the refresh then re-saves the
	// credential. Fixed, logout waits for the refresh to finish.
	select {
	case <-logoutDone:
	case <-time.After(200 * time.Millisecond):
	}
	releaseRefresh()
	<-requestDone
	<-logoutDone

	if code := logoutCode.Load(); code != http.StatusOK {
		t.Fatalf("logout status = %d, want 200", code)
	}
	if tok := config.Get().Kimi.OAuth; tok != nil {
		t.Errorf("OAuth after logout = %+v, want nil (refresh must not resurrect it)", tok)
	}
}

func TestServer_KimiRefreshKeepsCredentialReplacedMidFlight(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	upstream := newKimiOAuthUpstream(t)
	authHost, refreshArrived, releaseRefresh := blockingKimiAuth(t)
	seedKimiOAuthConfig(t, map[string]any{
		"token":        "oauth-tok-1",
		"refreshToken": "rt-1",
		"expiresAt":    time.Now().Add(-time.Hour).Format(time.RFC3339),
		"oauthHost":    authHost,
		"baseUrl":      upstream.srv.URL + "/coding/v1",
	}, nil)
	server := newKimiTestServer(t)

	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		postKimiMessage(t, server)
	}()
	<-refreshArrived

	// A /api/config save does not take kimiRefreshMu.
	if _, err := config.Save(map[string]any{"kimi": map[string]any{"oauth": map[string]any{
		"token":        "manual-tok",
		"refreshToken": "rt-manual",
		"expiresAt":    time.Now().Add(time.Hour).Format(time.RFC3339),
		"oauthHost":    authHost,
		"baseUrl":      upstream.srv.URL + "/coding/v1",
	}}}); err != nil {
		t.Fatalf("replace Save: %v", err)
	}
	releaseRefresh()
	<-requestDone

	if tok := config.Get().Kimi.OAuth; tok == nil || tok.Token != "manual-tok" || tok.RefreshToken != "rt-manual" {
		t.Errorf("stored OAuth = %+v, want the mid-flight replacement kept", tok)
	}
	if got := upstream.load(upstream.auth); got != "Bearer manual-tok" {
		t.Errorf("Authorization = %q, want Bearer manual-tok", got)
	}
}
