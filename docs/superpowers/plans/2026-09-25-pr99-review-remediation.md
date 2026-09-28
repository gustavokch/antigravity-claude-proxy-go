# PR #99 Review Remediation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task by task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix every actionable finding from the PR #99 review (Kimi Code OAuth device-flow login) on branch `feat/kimi-code-oauth`. Each fix goes in as an atomic TDD commit. Then push the branch and reply to the review.

**Architecture:**
- Every write to `kimi.oauth` goes through one helper, `saveKimiLocked`, and every caller holds `Server.kimiRefreshMu`. The callers are the refresh, login completion and logout.
- The refresh moves out of `resolveKimiCredential` into `refreshKimiOAuth`. It persists only if the stored credential did not change during the network call (compare-and-swap).
- The refresh remembers a refresh token that Kimi rejected and fails fast on it without calling Kimi again.
- If persisting a rotated token fails, the refresh keeps the new credential in memory and retries the persist on the next request.
- `KimiOAuthManager` allows one login at a time.
- A session is dropped once its result is persisted or its terminal status has been reported.
- One function maps credential errors to HTTP statuses.
- Verification URIs must be `https`.

**Tech Stack:** Go 1.27rc2, `net/http/httptest`, `testing`, git, `gh` CLI.

**Spec:** review comment https://github.com/gustavokch/antigravity-claude-proxy-go/pull/99#issuecomment-5837139961. It sits on PR #99, head `1d7847e`.

## Global Constraints

- AGENTS.md: **do NOT touch TLS internals.** No task touches `internal/cloudcode`, any transport, or `tls.Config`. Kimi traffic uses the existing `http.Client`s unchanged. The JA4 gate does not apply because no Cloud Code transport changes.
- Branch `feat/kimi-code-oauth`. The only remote is `fork`, and there is no `origin`. Local `HEAD` equals the PR head `1d7847e`. Push fast-forward only and never force-push.
- The working tree has unrelated untracked files (`.gemini/`, `pr.sh`, `tools/quotaprobe/`, other plan files, …). Stage explicit paths only. Never run `git add -A` or `git add .`.
- `core.hooksPath=scripts/git-hooks` runs gofmt on staged Go files. Before each commit, `gofmt -l internal/api internal/auth` must print nothing. Do not use `gofmt -l .`, because `gen/` has known drift.
- Scope is only the findings below. Do not change `internal/kimi/*`, `internal/config/*`, or `internal/webui/*`. The UI already stops polling on `completed` or a terminal status, so the change that drops sessions after they resolve needs no UI change.
- Run everything from the repo root. If the Go build cache is not writable, prefix `go` with `GOCACHE=$PWD/.gocache`.

## Baseline and pre-validation

- At `1d7847e`, `go test ./internal/api ./internal/auth ./internal/config ./internal/kimi -count=1` passes.
- Throwaway probes at `1d7847e` reproduced findings 1–4. They were deleted afterwards:
  - Logout followed by the end of an in-flight refresh brought back `oauth={Token:t2 RefreshToken:rt-2}`.
  - 3 requests with a revoked refresh token made 3 refresh POSTs.
  - After a failed persist, the refresh tokens sent were `[rt-1 rt-1]`.
  - 5 `/start` calls left 5 sessions, and the claimed session still held its token.
- Scratch-worktree dry run at `1d7847e`:
  - With every test in this plan applied, each new or changed test failed with the "Expected failure" text quoted in its task.
  - With the **combined** code from Tasks 1–7 applied, `go test` on `internal/{api,auth,config,kimi}` passed, and `go test -race -run Kimi ./internal/api ./internal/auth` passed.
  - `gofmt -l` and `go vet` were clean.
  - Each per-task intermediate state is derived from that validated final code but was not run on its own. Steps 2 and 4 of each task are the real check.

## Finding → Task map

| # | Location | Severity | Finding | Task |
|---|---|---|---|---|
| 1 | `internal/api/kimi_oauth_handlers.go:146`, `server.go:1351` | 🔴 bug | Logout and login write `kimi.oauth` without taking `kimiRefreshMu`, so an in-flight refresh brings the credential back. The refresh also spends a refresh token snapshotted before it took the lock. | 1 |
| 2 | `internal/api/server.go:1330` | 🟡 risk | A revoked refresh token is sent to `auth.kimi.ai` again on every request. | 2 |
| 3 | `internal/api/server.go:1353` | 🟡 risk | When the persist fails, the rotated token is lost, so the next request spends the old refresh token again. | 3 |
| 4 | `internal/auth/kimi_oauth.go:364` | 🟡 risk | Sessions and their pollers pile up, and completed sessions keep holding tokens. | 4 |
| 5 | `internal/api/kimi_oauth_handlers.go:79` | 🔵 nit | The claim is used up even when the save fails. Later polls then report `completed` although nothing was persisted. | 5 |
| 6 | `internal/api/management.go:2027` | 🔵 nit | Model fetch maps credential errors to 502. The forward path returns 400/401 for the same errors. | 6 |
| 7 | `internal/auth/kimi_oauth.go:331` | 🔵 nit | A verification URI from upstream reaches `:href` without a scheme check. | 7 |
| 8 | `docs/superpowers/2026-09-25-kimi-code-oauth-2.md` | 🔵 nit | Byte-identical copy of the plan file, at a path outside the plans convention. | 8 |

Not addressed, as stated in the review:
- The committed `KimiCodeClientID` is a public device-flow client with no secret; `ClaudeCodeClientID` sets the precedent.
- HTTP ≥500 during polling stays terminal, matching kimi-code.
- `refresh_token` stays required on refresh, matching kimi-code.

---

### Task 1: Serialize `kimi.oauth` writes; compare-and-swap the refresh (Finding 1)

**Files:**
- Modify: `internal/api/server.go:1310-1379`. Replace `resolveKimiCredential` and add `kimiOAuthExpiring`, `refreshKimiOAuth`, `kimiOAuthMap` and `saveKimiLocked`.
- Modify: `internal/api/kimi_oauth_handlers.go:144-196`. Replace `handleKimiAuthLogoutPost` and `registerAuthenticatedKimiOAuth`.
- Test: `internal/api/kimi_oauth_proxy_test.go`. Add the `"sync"` import, the `blockingKimiAuth` helper and 2 tests.

**Interfaces:**
- Consumes: `config.Get`, `config.Save`, `(*auth.KimiOAuthManager).RefreshToken`, `ConfigUpdater`.
- Produces (unexported, `package api`):
  - `func kimiOAuthExpiring(*config.KimiOAuthConfig) bool`
  - `func (*Server) refreshKimiOAuth(context.Context) (*config.KimiOAuthConfig, error)`
  - `func kimiOAuthMap(*config.KimiOAuthConfig) map[string]any`
  - `func (*Server) saveKimiLocked(map[string]any) error`, which requires `kimiRefreshMu` to be held.
  - Test helper `blockingKimiAuth(t) (host string, arrived <-chan struct{}, release func())`.

- [ ] **Step 1: Write the failing tests.** Add `"sync"` to the import block of `internal/api/kimi_oauth_proxy_test.go`, then append:

```go
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
```

```go
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
```

```go
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
```

- [ ] **Step 2: Run them and confirm they fail**

```bash
go test ./internal/api -count=1 -run 'TestServer_KimiLogoutDuringRefreshStaysLoggedOut|TestServer_KimiRefreshKeepsCredentialReplacedMidFlight'
```

Expected failure:
```
TestServer_KimiLogoutDuringRefreshStaysLoggedOut: OAuth after logout = &{Token:oauth-tok-2 RefreshToken:rt-2 ...}, want nil (refresh must not resurrect it)
TestServer_KimiRefreshKeepsCredentialReplacedMidFlight: stored OAuth = &{Token:oauth-tok-2 RefreshToken:rt-2 ...}, want the mid-flight replacement kept
TestServer_KimiRefreshKeepsCredentialReplacedMidFlight: Authorization = "Bearer oauth-tok-2", want Bearer manual-tok
```

- [ ] **Step 3: Implement the minimal fix**

In `internal/api/server.go`, replace `resolveKimiCredential` (L1310-1379, from its doc comment to its closing brace) with:

```go
// resolveKimiCredential picks the credential for a Kimi upstream call. The
// OAuth credential wins over apiKey when present; a stale OAuth token is
// refreshed and persisted.
func (server *Server) resolveKimiCredential(ctx context.Context, cfg config.KimiConfig) (kimiCredential, error) {
	if cfg.OAuth != nil && cfg.OAuth.Token != "" {
		o := cfg.OAuth
		if kimiOAuthExpiring(o) {
			var err error
			if o, err = server.refreshKimiOAuth(ctx); err != nil {
				return kimiCredential{}, err
			}
		}
		baseURL := o.BaseURL
		if baseURL == "" {
			baseURL = auth.KimiCodeBaseURL
		}
		return kimiCredential{token: o.Token, baseURL: baseURL, oauth: true}, nil
	}
	if cfg.APIKey != "" {
		return kimiCredential{token: cfg.APIKey, baseURL: cfg.BaseURL}, nil
	}
	return kimiCredential{}, errKimiNoCredential
}

// kimiOAuthExpiring reports whether o must be refreshed before use.
func kimiOAuthExpiring(o *config.KimiOAuthConfig) bool {
	return o.ExpiresAt != nil && time.Until(*o.ExpiresAt) <= 60*time.Second
}

// refreshKimiOAuth returns the stored OAuth credential, refreshing and
// persisting it first when it is about to expire. It holds kimiRefreshMu,
// which login and logout also take, so a refresh never interleaves with them.
func (server *Server) refreshKimiOAuth(ctx context.Context) (*config.KimiOAuthConfig, error) {
	server.kimiRefreshMu.Lock()
	defer server.kimiRefreshMu.Unlock()

	stored := config.Get().Kimi.OAuth
	if stored == nil || stored.Token == "" {
		return nil, errKimiLoginExpired
	}
	if !kimiOAuthExpiring(stored) {
		return stored, nil
	}
	// WithoutCancel: a client disconnect must not strand a rotated refresh
	// token half-persisted.
	tok, err := server.kimiOAuthMgr.RefreshToken(context.WithoutCancel(ctx), stored.RefreshToken, stored.OAuthHost)
	if err != nil {
		if errors.Is(err, auth.ErrKimiOAuthUnauthorized) {
			return nil, errKimiLoginExpired
		}
		return nil, fmt.Errorf("Kimi OAuth token refresh failed: %w", err)
	}
	refreshed := &config.KimiOAuthConfig{
		Token:        tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    &tok.ExpiresAt,
		Email:        stored.Email,
		UserID:       stored.UserID,
		Nickname:     stored.Nickname,
		OAuthHost:    stored.OAuthHost,
		BaseURL:      stored.BaseURL,
	}
	// Compare-and-swap: /api/config saves do not take kimiRefreshMu, so the
	// stored credential may have been replaced during the network call.
	latest := config.Get().Kimi.OAuth
	if latest == nil || latest.Token == "" {
		return nil, errKimiLoginExpired
	}
	if latest.Token != stored.Token || latest.RefreshToken != stored.RefreshToken {
		return latest, nil
	}
	if err := server.saveKimiLocked(map[string]any{"oauth": kimiOAuthMap(refreshed)}); err != nil {
		server.logger.Warn("kimi oauth refresh persist failed", "error", err)
	}
	return refreshed, nil
}

// kimiOAuthMap renders o in the config.Save update shape for kimi.oauth.
func kimiOAuthMap(o *config.KimiOAuthConfig) map[string]any {
	m := map[string]any{
		"token":        o.Token,
		"refreshToken": o.RefreshToken,
		"oauthHost":    o.OAuthHost,
		"baseUrl":      o.BaseURL,
	}
	if o.ExpiresAt != nil {
		m["expiresAt"] = o.ExpiresAt.Format(time.RFC3339)
	}
	if o.Email != "" {
		m["email"] = o.Email
	}
	if o.UserID != "" {
		m["userId"] = o.UserID
	}
	if o.Nickname != "" {
		m["nickname"] = o.Nickname
	}
	return m
}

// saveKimiLocked persists a kimi section update and pushes the saved config
// to the backend. Every kimi.oauth writer calls it with kimiRefreshMu held.
func (server *Server) saveKimiLocked(update map[string]any) error {
	saved, err := config.Save(map[string]any{"kimi": update})
	if err != nil {
		return err
	}
	if updater, ok := server.backend.(ConfigUpdater); ok {
		updater.UpdateConfig(saved)
	}
	return nil
}
```

In `internal/api/kimi_oauth_handlers.go`, replace L144-196 (`handleKimiAuthLogoutPost` and `registerAuthenticatedKimiOAuth`) with:

```go
// handleKimiAuthLogoutPost clears the stored Kimi Code credential.
func (server *Server) handleKimiAuthLogoutPost(writer http.ResponseWriter, request *http.Request) {
	server.kimiRefreshMu.Lock()
	err := server.saveKimiLocked(map[string]any{"oauth": nil})
	server.kimiRefreshMu.Unlock()
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{
			"status": "error",
			"error":  "Failed to save config: " + err.Error(),
		})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"config": config.GetPublicConfig()["kimi"],
	})
}

// registerAuthenticatedKimiOAuth persists a completed device-flow login.
func (server *Server) registerAuthenticatedKimiOAuth(snap auth.KimiAuthSessionSnapshot) error {
	if snap.Token == nil {
		return nil
	}
	o := &config.KimiOAuthConfig{
		Token:        snap.Token.AccessToken,
		RefreshToken: snap.Token.RefreshToken,
		ExpiresAt:    &snap.Token.ExpiresAt,
		OAuthHost:    snap.OAuthHost,
		BaseURL:      snap.BaseURL,
	}
	if snap.User != nil {
		o.Email, o.UserID, o.Nickname = snap.User.Email, snap.User.UserID, snap.User.Nickname
	}
	server.kimiRefreshMu.Lock()
	defer server.kimiRefreshMu.Unlock()
	return server.saveKimiLocked(map[string]any{"enabled": true, "oauth": kimiOAuthMap(o)})
}
```

- [ ] **Step 4: Run the tests and confirm they pass, along with the existing Kimi suites**

```bash
go test ./internal/api -count=1 -run 'Kimi' && go test ./internal/auth -count=1 -run 'Kimi'
```

Expected: `ok` for both packages. This includes the existing `TestServer_ForwardToKimi_OAuthRefreshOnExpiry`, `TestKimiOAuthHandlers_Logout` and `TestKimiOAuthHandlers_FullFlow`.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/api internal/auth
git add internal/api/server.go internal/api/kimi_oauth_handlers.go internal/api/kimi_oauth_proxy_test.go
git commit -m "fix(kimi): serialize oauth credential writes under the refresh lock"
```

---

### Task 2: Fail fast on a rejected refresh token (Finding 2)

**Files:**
- Modify: `internal/api/server.go`. Add the struct field after `classifierCorpus` (L125) and 2 edits in `refreshKimiOAuth`.
- Test: `internal/api/kimi_oauth_proxy_test.go:161-196`. Replace `TestServer_ForwardToKimi_OAuthInvalidGrant`.

**Interfaces:**
- Consumes: `refreshKimiOAuth` (Task 1).
- Produces: the field `Server.kimiDeadRefreshToken string`, guarded by `kimiRefreshMu`.

- [ ] **Step 1: Write the failing test.** Replace `TestServer_ForwardToKimi_OAuthInvalidGrant` with:

```go
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
```

- [ ] **Step 2: Run it and confirm it fails**

```bash
go test ./internal/api -count=1 -run 'TestServer_ForwardToKimi_OAuthInvalidGrant'
```

Expected failure:
```
refresh POSTs after 3 requests = 3, want 1 (a rejected refresh token must not be re-sent)
refresh POSTs after a new login = 4, want 2
```

- [ ] **Step 3: Implement the minimal fix**

In the `Server` struct, turn the lines after `classifierCorpus` into:

```go
	classifierCorpus   atomic.Pointer[corpus.Recorder]

	// Kimi OAuth refresh state, guarded by kimiRefreshMu.
	kimiDeadRefreshToken string // refresh token Kimi rejected

	mu                sync.Mutex
```

In `refreshKimiOAuth`, replace the `if !kimiOAuthExpiring(stored) { … }` block with:

```go
	if !kimiOAuthExpiring(stored) {
		return stored, nil
	}
	// A refresh token Kimi already rejected fails fast; re-sending it on every
	// request only hammers auth.kimi.ai. An empty one matches the zero value
	// and could never refresh anyway.
	if stored.RefreshToken == server.kimiDeadRefreshToken {
		return nil, errKimiLoginExpired
	}
```

In its `RefreshToken` error branch, replace the `errors.Is(err, auth.ErrKimiOAuthUnauthorized)` block with:

```go
		if errors.Is(err, auth.ErrKimiOAuthUnauthorized) {
			server.kimiDeadRefreshToken = stored.RefreshToken
			return nil, errKimiLoginExpired
		}
```

- [ ] **Step 4: Run the tests and confirm they pass**

```bash
go test ./internal/api -count=1 -run 'Kimi'
```

Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/api
git add internal/api/server.go internal/api/kimi_oauth_proxy_test.go
git commit -m "fix(kimi): stop re-sending a rejected refresh token"
```

---

### Task 3: Keep a rotated token in memory when persisting it fails (Finding 3)

**Files:**
- Modify: `internal/api/server.go`. Add 2 struct fields, replace `refreshKimiOAuth`, and change `handleKimiAuthLogoutPost` in `kimi_oauth_handlers.go`.
- Test: `internal/api/kimi_oauth_proxy_test.go`. Add the `"os"` and `"slices"` imports, the `breakConfigWrites` helper and 1 test.

**Interfaces:**
- Consumes: `kimiDeadRefreshToken` (Task 2) and `saveKimiLocked` (Task 1).
- Produces:
  - The fields `Server.kimiUnsaved *config.KimiOAuthConfig` and `Server.kimiUnsavedFrom string`, both guarded by `kimiRefreshMu`.
  - The test helper `breakConfigWrites(t) (restore func())`, which Task 5 reuses.
    It makes `config.Save` fail by creating `config.json.tmp` as a directory, which fails even when running as root.

- [ ] **Step 1: Write the failing test.** Add `"os"` and `"slices"` to the imports, then append:

```go
// breakConfigWrites makes config.Save fail until restore runs: its temp file
// path becomes a directory, which fails even as root.
func breakConfigWrites(t *testing.T) (restore func()) {
	t.Helper()
	path, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("ConfigFilePath: %v", err)
	}
	blocker := path + ".tmp"
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatalf("create write blocker: %v", err)
	}
	return func() {
		if err := os.Remove(blocker); err != nil {
			t.Fatalf("remove write blocker: %v", err)
		}
	}
}
```

```go
func TestServer_KimiRefreshPersistFailureKeepsRotatedToken(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)

	upstream := newKimiOAuthUpstream(t)
	var mu sync.Mutex
	var spent []string
	fakeAuth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		spent = append(spent, r.PostForm.Get("refresh_token"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"oauth-tok-2","refresh_token":"rt-2","expires_in":3600}`))
	}))
	defer fakeAuth.Close()
	seedKimiOAuthConfig(t, map[string]any{
		"token":        "oauth-tok-1",
		"refreshToken": "rt-1",
		"expiresAt":    time.Now().Add(-time.Hour).Format(time.RFC3339),
		"oauthHost":    fakeAuth.URL,
		"baseUrl":      upstream.srv.URL + "/coding/v1",
	}, nil)
	server := newKimiTestServer(t)

	restore := breakConfigWrites(t)
	for i := range 2 {
		if rec := postKimiMessage(t, server); rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, body = %s", i, rec.Code, rec.Body.String())
		}
		if got := upstream.load(upstream.auth); got != "Bearer oauth-tok-2" {
			t.Errorf("request %d: Authorization = %q, want Bearer oauth-tok-2", i, got)
		}
	}
	mu.Lock()
	sent := slices.Clone(spent)
	mu.Unlock()
	if !slices.Equal(sent, []string{"rt-1"}) {
		t.Errorf("refresh tokens sent = %v, want [rt-1] (the rotated token must be reused)", sent)
	}

	restore()
	postKimiMessage(t, server)
	if tok := config.Get().Kimi.OAuth; tok == nil || tok.Token != "oauth-tok-2" || tok.RefreshToken != "rt-2" {
		t.Errorf("stored OAuth after writes recover = %+v, want oauth-tok-2 / rt-2", tok)
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

```bash
go test ./internal/api -count=1 -run 'TestServer_KimiRefreshPersistFailureKeepsRotatedToken'
```

Expected failure:
```
refresh tokens sent = [rt-1 rt-1], want [rt-1] (the rotated token must be reused)
```

- [ ] **Step 3: Implement the minimal fix**

Extend the struct block from Task 2 to:

```go
	// Kimi OAuth refresh state, guarded by kimiRefreshMu.
	kimiDeadRefreshToken string                  // refresh token Kimi rejected
	kimiUnsaved          *config.KimiOAuthConfig // refreshed credential whose persist failed
	kimiUnsavedFrom      string                  // stored refresh token kimiUnsaved replaced
```

Replace `refreshKimiOAuth` completely with:

```go
// refreshKimiOAuth returns the stored OAuth credential, refreshing and
// persisting it first when it is about to expire. It holds kimiRefreshMu,
// which login and logout also take, so a refresh never interleaves with them.
func (server *Server) refreshKimiOAuth(ctx context.Context) (*config.KimiOAuthConfig, error) {
	server.kimiRefreshMu.Lock()
	defer server.kimiRefreshMu.Unlock()

	stored := config.Get().Kimi.OAuth
	if stored == nil || stored.Token == "" {
		return nil, errKimiLoginExpired
	}
	current := stored
	if server.kimiUnsaved != nil {
		if server.kimiUnsavedFrom == stored.RefreshToken {
			// An earlier refresh rotated the stored refresh token upstream but
			// could not persist the result: use it and retry the persist.
			current = server.kimiUnsaved
			if err := server.saveKimiLocked(map[string]any{"oauth": kimiOAuthMap(current)}); err == nil {
				stored = current
				server.kimiUnsaved, server.kimiUnsavedFrom = nil, ""
			}
		} else {
			server.kimiUnsaved, server.kimiUnsavedFrom = nil, ""
		}
	}
	if !kimiOAuthExpiring(current) {
		return current, nil
	}
	// A refresh token Kimi already rejected fails fast; re-sending it on every
	// request only hammers auth.kimi.ai. An empty one matches the zero value
	// and could never refresh anyway.
	if current.RefreshToken == server.kimiDeadRefreshToken {
		return nil, errKimiLoginExpired
	}
	// WithoutCancel: a client disconnect must not strand a rotated refresh
	// token half-persisted.
	tok, err := server.kimiOAuthMgr.RefreshToken(context.WithoutCancel(ctx), current.RefreshToken, current.OAuthHost)
	if err != nil {
		if errors.Is(err, auth.ErrKimiOAuthUnauthorized) {
			server.kimiDeadRefreshToken = current.RefreshToken
			return nil, errKimiLoginExpired
		}
		return nil, fmt.Errorf("Kimi OAuth token refresh failed: %w", err)
	}
	refreshed := &config.KimiOAuthConfig{
		Token:        tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    &tok.ExpiresAt,
		Email:        current.Email,
		UserID:       current.UserID,
		Nickname:     current.Nickname,
		OAuthHost:    current.OAuthHost,
		BaseURL:      current.BaseURL,
	}
	// Compare-and-swap: /api/config saves do not take kimiRefreshMu, so the
	// stored credential may have been replaced during the network call.
	latest := config.Get().Kimi.OAuth
	if latest == nil || latest.Token == "" {
		return nil, errKimiLoginExpired
	}
	if latest.Token != stored.Token || latest.RefreshToken != stored.RefreshToken {
		return latest, nil
	}
	if err := server.saveKimiLocked(map[string]any{"oauth": kimiOAuthMap(refreshed)}); err != nil {
		server.logger.Warn("kimi oauth refresh persist failed; keeping the rotated token in memory", "error", err)
		server.kimiUnsaved, server.kimiUnsavedFrom = refreshed, stored.RefreshToken
	} else {
		server.kimiUnsaved, server.kimiUnsavedFrom = nil, ""
	}
	return refreshed, nil
}
```

In `handleKimiAuthLogoutPost`, clear the unsaved credential while the lock is held. The finished function:

```go
// handleKimiAuthLogoutPost clears the stored Kimi Code credential.
func (server *Server) handleKimiAuthLogoutPost(writer http.ResponseWriter, request *http.Request) {
	server.kimiRefreshMu.Lock()
	err := server.saveKimiLocked(map[string]any{"oauth": nil})
	server.kimiUnsaved, server.kimiUnsavedFrom = nil, ""
	server.kimiRefreshMu.Unlock()
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{
			"status": "error",
			"error":  "Failed to save config: " + err.Error(),
		})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"config": config.GetPublicConfig()["kimi"],
	})
}
```

- [ ] **Step 4: Run the tests and confirm they pass**

```bash
go test ./internal/api -count=1 -run 'Kimi'
```

Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/api
git add internal/api/server.go internal/api/kimi_oauth_handlers.go internal/api/kimi_oauth_proxy_test.go
git commit -m "fix(kimi): keep a rotated oauth token in memory when persisting fails"
```

---

### Task 4: One login at a time; drop sessions once they resolve (Finding 4)

**Files:**
- Modify: `internal/auth/kimi_oauth.go:364-371`. Assign `cancel` before `registerSession` publishes the session.
- Modify: `internal/auth/kimi_oauth.go:374-394`. Replace `registerSession`.
- Modify: `internal/api/kimi_oauth_handlers.go:79-87`. Drop the session after its result is persisted.
- Modify: `internal/api/kimi_oauth_handlers.go:108-115`. Drop the session after reporting a terminal status.
- Test: `internal/auth/kimi_oauth_test.go`. Add 1 test.
- Test: `internal/api/kimi_oauth_handlers_test.go`. Add a `denied` switch to the fake, change 1 test and add 1 test.

**Interfaces:**
- Consumes: `(*KimiOAuthManager).CancelSession`.
- Produces: this changes behaviour on the status endpoint. Once a poll has returned a persisted `completed` or a terminal status, later polls for that session return 404. The web UI stops polling on both outcomes, so it never makes such a poll.

- [ ] **Step 1: Write the failing tests.**

Append to `internal/auth/kimi_oauth_test.go`:

```go
func TestKimiStartDeviceAuthSupersedesEarlierSession(t *testing.T) {
	var sleeps []time.Duration
	srv, _ := newKimiFake(t, kimiFakeDeviceJSON,
		func() (int, string) { return 400, `{"error":"authorization_pending"}` },
	)
	mgr := newKimiTestManager(t, srv, &sleeps)

	first, err := mgr.StartDeviceAuth(context.Background())
	if err != nil {
		t.Fatalf("first StartDeviceAuth: %v", err)
	}
	second, err := mgr.StartDeviceAuth(context.Background())
	if err != nil {
		t.Fatalf("second StartDeviceAuth: %v", err)
	}
	defer mgr.CancelSession(second.ID)

	if snap := first.Snapshot(); snap.Status != "cancelled" {
		t.Errorf("first session status = %q, want cancelled", snap.Status)
	}
	if _, ok := mgr.GetSession(first.ID); ok {
		t.Error("first session still registered after a newer login started")
	}
	if _, ok := mgr.GetSession(second.ID); !ok {
		t.Error("second session not registered")
	}
}
```

In `internal/api/kimi_oauth_handlers_test.go`, replace the `kimiAuthFake` struct (L19-22) with:

```go
type kimiAuthFake struct {
	srv      *httptest.Server
	approved atomic.Bool
	denied   atomic.Bool
}
```

In the same file, replace the head of the `/api/oauth/token` case (L39-40) with:

```go
		case "/api/oauth/token":
			if f.denied.Load() {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"access_denied","error_description":"user rejected"}`))
				return
			}
			if !f.approved.Load() {
```

In `TestKimiOAuthHandlers_ClaimOnceKeepsRefreshedToken`, replace the second-poll check (L192-195) with the version below. The `tok.Token != "rotated-tok"` assertion that follows stays as it is.

```go
	code, status := doKimiRequest(t, server, http.MethodGet, "/api/kimi/auth/status?session_id="+sessionID, "")
	if code != http.StatusNotFound {
		t.Fatalf("second poll: code=%d body=%v, want 404 (session dropped once persisted)", code, status)
	}
```

Append:

```go
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
```

- [ ] **Step 2: Run them and confirm they fail**

```bash
go test ./internal/auth -count=1 -run 'TestKimiStartDeviceAuthSupersedesEarlierSession' ; go test ./internal/api -count=1 -run 'TestKimiOAuthHandlers_ClaimOnceKeepsRefreshedToken|TestKimiOAuthHandlers_TerminalStatusDropsSession'
```

Expected failure:
```
first session status = "pending", want cancelled
first session still registered after a newer login started
second poll: code=200 body=map[account:... status:completed], want 404 (session dropped once persisted)
poll after a terminal status: code=200 body=map[error:user rejected status:denied], want 404
```

- [ ] **Step 3: Implement the minimal fix**

In `StartDeviceAuth`, replace L364-371, from `m.registerSession(session)` to the end of the function, with:

```go
	pollCtx, cancel := context.WithCancel(context.Background())
	session.cancel = cancel // before registerSession publishes the session
	m.registerSession(session)
	go m.pollDeviceGrant(pollCtx, session)
	return session, nil
}
```

Replace `registerSession` (L374-394) with:

```go
// registerSession stores session as the only login in flight. A new device
// flow supersedes every earlier one: its poller is cancelled and its state,
// including any unclaimed token, is dropped.
func (m *KimiOAuthManager) registerSession(session *KimiAuthSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		s.mu.Lock()
		if s.cancel != nil {
			s.cancel()
		}
		if s.Status == "pending" {
			s.Status = "cancelled"
			s.Error = "superseded by a newer login"
		}
		s.mu.Unlock()
		delete(m.sessions, id)
	}
	m.sessions[session.ID] = session
}
```

In `handleKimiAuthStatusGet`, replace the claim block (L79-87) with:

```go
		if session.ClaimCompletion() {
			if err := server.registerAuthenticatedKimiOAuth(snap); err != nil {
				writeJSON(writer, http.StatusInternalServerError, map[string]any{
					"status": "error",
					"error":  "Failed to save Kimi login: " + err.Error(),
				})
				return
			}
			// Persisted: drop the session so its token copy leaves memory.
			server.kimiOAuthMgr.CancelSession(sessionID)
		}
```

In the same function, replace the first two lines of the terminal case (L109-110) with:

```go
	case "expired", "denied", "cancelled", "error":
		// Terminal: report it once, then drop the session.
		server.kimiOAuthMgr.CancelSession(sessionID)
		writeJSON(writer, http.StatusOK, map[string]any{
```

- [ ] **Step 4: Run the tests and confirm they pass**

```bash
go test ./internal/api -count=1 -run 'Kimi' && go test ./internal/auth -count=1 -run 'Kimi' && go test -race -count=1 -run 'Kimi' ./internal/auth ./internal/api
```

Expected: `ok` for every package, with no `DATA RACE`.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/api internal/auth
git add internal/auth/kimi_oauth.go internal/auth/kimi_oauth_test.go internal/api/kimi_oauth_handlers.go internal/api/kimi_oauth_handlers_test.go
git commit -m "fix(kimi): allow one device login at a time and drop resolved sessions"
```

---

### Task 5: Release the completion claim when the save fails (Finding 5)

**Files:**
- Modify: `internal/auth/kimi_oauth.go:119-132`. Update the `ClaimCompletion` doc comment and add `ReleaseCompletion` before `finish`.
- Modify: `internal/api/kimi_oauth_handlers.go`. Edit the claim block written in Task 4.
- Test: `internal/api/kimi_oauth_handlers_test.go`. Add 1 test that uses `breakConfigWrites` from Task 3.

**Interfaces:**
- Produces: `func (s *KimiAuthSession) ReleaseCompletion()`, exported from `package auth`.

- [ ] **Step 1: Write the failing test.** Append:

```go
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
```

- [ ] **Step 2: Run it and confirm it fails**

```bash
go test ./internal/api -count=1 -run 'TestKimiOAuthHandlers_SaveFailureAllowsRetry'
```

Expected failure:
```
OAuth after retry = <nil>, want at-123456789012 persisted
```

- [ ] **Step 3: Implement the minimal fix**

In `internal/auth/kimi_oauth.go`, change the `ClaimCompletion` doc comment to:

```go
// ClaimCompletion marks the session's completed token as consumed. It returns
// true once per completion (again only after ReleaseCompletion), so later
// status polls cannot re-save a stale token over one refreshed since.
```

Add this directly above `// finish sets the terminal status …`:

```go
// ReleaseCompletion undoes a ClaimCompletion whose save failed, so a later
// status poll can retry persisting the token.
func (s *KimiAuthSession) ReleaseCompletion() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registered = false
}
```

In `handleKimiAuthStatusGet`, inside the `registerAuthenticatedKimiOAuth` error branch and before `writeJSON`, add:

```go
				// Let the next poll retry the save.
				session.ReleaseCompletion()
```

- [ ] **Step 4: Run the tests and confirm they pass**

```bash
go test ./internal/api -count=1 -run 'Kimi' && go test ./internal/auth -count=1 -run 'Kimi'
```

Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/api internal/auth
git add internal/auth/kimi_oauth.go internal/api/kimi_oauth_handlers.go internal/api/kimi_oauth_handlers_test.go
git commit -m "fix(kimi): let a failed login save be retried"
```

---

### Task 6: Share one credential-error → status mapping (Finding 6)

**Files:**
- Modify: `internal/api/server.go`. Add `kimiCredentialErrorStatus` after `saveKimiLocked`, and replace the error `switch` in `forwardToKimi`, which sits right after its `resolveKimiCredential` call.
- Modify: `internal/api/management.go:2025-2027`, in `handleKimiModelsFetch`.
- Test: `internal/api/kimi_oauth_handlers_test.go`. Add 1 table test.

**Interfaces:**
- Produces: `func kimiCredentialErrorStatus(error) (status int, errType string)`, unexported.

- [ ] **Step 1: Write the failing test.** Append:

```go
func TestKimiModelsFetch_CredentialErrorStatus(t *testing.T) {
	deadAuth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer deadAuth.Close()

	tests := []struct {
		name string
		kimi map[string]any
		want int
	}{
		{"no credential", map[string]any{"enabled": true}, http.StatusBadRequest},
		{"login expired", map[string]any{"enabled": true, "oauth": map[string]any{
			"token":        "at-1",
			"refreshToken": "rt-1",
			"expiresAt":    time.Now().Add(-time.Hour).Format(time.RFC3339),
			"oauthHost":    deadAuth.URL,
		}}, http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, _, _ := newTestServerWithManager(t)
			if _, err := config.Save(map[string]any{"kimi": tt.kimi}); err != nil {
				t.Fatalf("seed Save: %v", err)
			}
			code, body := doKimiRequest(t, server, http.MethodPost, "/api/kimi/models/fetch", "{}")
			if code != tt.want {
				t.Errorf("code = %d, want %d; body=%v", code, tt.want, body)
			}
		})
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

```bash
go test ./internal/api -count=1 -run 'TestKimiModelsFetch_CredentialErrorStatus'
```

Expected failure:
```
no_credential: code = 502, want 400
login_expired: code = 502, want 401
```

- [ ] **Step 3: Implement the minimal fix**

Add to `internal/api/server.go`, after `saveKimiLocked`:

```go
// kimiCredentialErrorStatus maps a resolveKimiCredential error to the HTTP
// status and Anthropic error type returned to the client.
func kimiCredentialErrorStatus(err error) (int, string) {
	switch {
	case errors.Is(err, errKimiNoCredential):
		return http.StatusBadRequest, "invalid_request_error"
	case errors.Is(err, errKimiLoginExpired):
		return http.StatusUnauthorized, "authentication_error"
	default:
		return http.StatusBadGateway, "api_error"
	}
}
```

In `forwardToKimi`, replace the `resolveKimiCredential` call and its error `switch` with:

```go
	cred, err := server.resolveKimiCredential(request.Context(), kimiCfg)
	if err != nil {
		status, kind := kimiCredentialErrorStatus(err)
		writeAPIError(writer, status, kind, err.Error())
		return
	}
```

In `handleKimiModelsFetch` (`management.go:2025-2027`), replace the call and the head of its error branch with:

```go
		cred, err := server.resolveKimiCredential(request.Context(), cfg.Kimi)
		if err != nil {
			status, _ := kimiCredentialErrorStatus(err)
			writeJSON(writer, status, map[string]any{
```

- [ ] **Step 4: Run the tests and confirm they pass**

```bash
go test ./internal/api -count=1 -run 'Kimi'
```

Expected: `ok`. This includes `TestServer_ForwardToKimi_NoCredential` (400) and `TestServer_ForwardToKimi_OAuthInvalidGrant` (401).

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/api
git add internal/api/server.go internal/api/management.go internal/api/kimi_oauth_handlers_test.go
git commit -m "fix(kimi): map credential errors to the same status on model fetch"
```

---

### Task 7: Reject verification URIs that are not https (Finding 7)

**Files:**
- Modify: `internal/auth/kimi_oauth.go:332-334`. Add the check right after the "missing verification_uri_complete" guard in `StartDeviceAuth`.
- Test: `internal/auth/kimi_oauth_test.go`. Add 1 test.

**Interfaces:** no new symbols. `StartDeviceAuth` now returns an error, and registers no session, when either URI is set but is not `https`.

- [ ] **Step 1: Write the failing test.** Append:

```go
func TestKimiStartDeviceAuthRejectsNonHTTPSVerificationURI(t *testing.T) {
	var sleeps []time.Duration
	srv, _ := newKimiFake(t, `{
		"device_code": "dc-1",
		"user_code": "ABCD-1234",
		"verification_uri": "https://www.kimi.ai/code/authorize_device",
		"verification_uri_complete": "javascript:alert(1)",
		"expires_in": 1800,
		"interval": 5
	}`, func() (int, string) { return 400, `{"error":"authorization_pending"}` })
	mgr := newKimiTestManager(t, srv, &sleeps)

	session, err := mgr.StartDeviceAuth(context.Background())
	if session != nil {
		mgr.CancelSession(session.ID)
	}
	if err == nil {
		t.Fatal("StartDeviceAuth accepted a javascript: verification URI")
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

```bash
go test ./internal/auth -count=1 -run 'TestKimiStartDeviceAuthRejectsNonHTTPSVerificationURI'
```

Expected failure:
```
StartDeviceAuth accepted a javascript: verification URI
```

- [ ] **Step 3: Implement the minimal fix.** Directly after the guard `if verificationURIComplete == "" && verificationURI == "" { … }`, add:

```go
	// The UI binds these to an href; only https may reach it.
	for _, uri := range []string{verificationURI, verificationURIComplete} {
		if uri == "" {
			continue
		}
		if u, err := url.Parse(uri); err != nil || u.Scheme != "https" {
			return nil, fmt.Errorf("Device authorization response has a non-https verification URI: %q", uri)
		}
	}
```

- [ ] **Step 4: Run the tests and confirm they pass**

```bash
go test ./internal/auth -count=1 -run 'Kimi' && go test ./internal/api -count=1 -run 'Kimi'
```

Expected: `ok`. The existing fakes all use `https://www.kimi.ai/...`.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/auth
git add internal/auth/kimi_oauth.go internal/auth/kimi_oauth_test.go
git commit -m "fix(kimi): reject non-https device verification URIs"
```

---

### Task 8: Delete the duplicate plan document (Finding 8)

**Files:** Delete `docs/superpowers/2026-09-25-kimi-code-oauth-2.md`. It is byte-identical to `docs/superpowers/plans/2026-09-25-kimi-code-auth.md` (`cmp` says so), and nothing references it.

No test, since this changes only docs.

- [ ] **Step 1: Confirm it is a duplicate and delete it**

```bash
cmp docs/superpowers/2026-09-25-kimi-code-oauth-2.md docs/superpowers/plans/2026-09-25-kimi-code-auth.md && git rm docs/superpowers/2026-09-25-kimi-code-oauth-2.md
```

- [ ] **Step 2: Commit**

```bash
git commit -m "docs(kimi): drop duplicate plan copy"
```

---

### Task 9: Full gate, push, and reply to the review

- [ ] **Step 1: Run the full gate.** Every command must succeed.

```bash
gofmt -l internal/api internal/auth internal/config internal/kimi
go vet ./internal/api ./internal/auth ./internal/config ./internal/kimi
go build ./... && GOOS=windows GOARCH=amd64 go build ./... && GOOS=linux GOARCH=amd64 go build ./...
go test -race -count=1 -run 'Kimi' ./internal/api ./internal/auth
go test ./...
```

Gate: `gofmt` prints nothing, and every build and test command exits 0.

- [ ] **Step 2: Live smoke check against the global host. No Kimi account is needed.**

```bash
go build -o bin/proxy ./cmd/proxy
mkdir -p /tmp/kimi-e2e && ANTIGRAVITY_CONFIG_DIR=/tmp/kimi-e2e ./bin/proxy -port 18091 -api-key test-local   # background service
curl -sS -X POST 127.0.0.1:18091/api/kimi/auth/start   # 200, user_code, https verification_uri_complete
curl -sS -X POST 127.0.0.1:18091/api/kimi/auth/start   # second login supersedes the first
curl -sS '127.0.0.1:18091/api/kimi/auth/status?session_id=<first id>'   # 404
curl -sS '127.0.0.1:18091/api/kimi/auth/status?session_id=<second id>'  # {"status":"pending"}
```

Then stop the service and run `rm -rf /tmp/kimi-e2e`.

- [ ] **Step 3: Push fast-forward**

```bash
git log --oneline fork/feat/kimi-code-oauth..HEAD   # expect the 8 commits from Tasks 1-8
git push fork feat/kimi-code-oauth
```

- [ ] **Step 4: Reply on the PR.** The reply maps each finding to its commit SHA, states the full-suite result, and lists the three items intentionally left alone.

```bash
gh pr comment 99 --body-file <reply.md>
```

- [ ] **Step 5: Refresh the local graph.** `graft/` is not tracked, so this is local only.

```bash
graft build
```

