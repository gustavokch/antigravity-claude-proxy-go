# PR #140 Remediation — agy refresh reads the login client from the token it refreshes

**Goal:** `Manager.Get` orders the OAuth client candidates by the `azp` claim of the token file it is refreshing, from the bytes it already holds under its exclusive lock, and never opens a token file a second time while it holds that lock.

**Architecture:** `Manager.Get` (`internal/auth/token.go`) locks `m.Path` (default: `DefaultTokenPath()`) with `flock(LOCK_EX)`, reads it into `raw`, and calls `m.refresh` when the token is stale. `refresh` → `oauthCredentialCandidates` → `agyOAuthCredentialCandidates` (`internal/auth/agy_oauth.go`), which puts the login client (`agyLoginClientID`, the `id_token` `azp` of the default token file) first. PR #140 replaced the shared-lock read in `agyLoginClientID` with `os.ReadFile` to stop the self-deadlock. The read still targets `DefaultTokenPath()`, not `m.Path`; `CredentialResolver` sets `m.Path = account.AgyTokenPath` (`internal/accounts/credentials.go:55`).

**Tech stack:** Go 1.27rc2, `go test`.

**Spec reference:** PR #140 review comment (https://github.com/gustavokch/antigravity-claude-proxy-go/pull/140#issuecomment-6524708916).

## Evidence (2026-10-30)

- With the two PR source files reverted, `TestManagerGetRefreshesTheDefaultAgyTokenWithoutDeadlock` fails after 5 s (`Manager.Get did not return`). At PR head `go test -race ./internal/auth/` passes.
- `refresh` falls back to the next candidate only on `401` with `invalid_client` in the body (`refreshWithCredentials`), so a wrong first candidate is not guaranteed to recover.

## Findings

| # | Site | Severity | Decision |
|---|------|----------|----------|
| 1 | `internal/auth/agy_oauth.go:L131` | 🟡 risk | Fix: Task 1 |
| 2 | `internal/auth/agy_oauth_test.go:L208` | 🔵 nit | Fix: Task 2 |

## Task 1 — Take the login client from the bytes `Get` holds

**Files:** Modify `internal/auth/agy_oauth.go`, `internal/auth/token.go`, `internal/auth/oauth_server.go`; Test `internal/auth/agy_oauth_test.go`.

**Consumes:** `Manager.Get`'s `raw`, `agyLoginClientID`. **Produces:** `loginClientIDFromToken(raw []byte) string`; `agyOAuthCredentialCandidates(loginClientID string)`; `Manager.oauthCredentialCandidates(loginClientID func() string)`; `Manager.refresh(ctx, refreshToken string, loginClientID func() string)`.

- Step 1 (red): add `TestManagerGetOrdersByTheLoginOfTheTokenItRefreshes`. The agy fixture binary holds `loginID otherID secret`, so the default order puts `otherID` first. `AGY_TOKEN_PATH` points at a login token for `otherID`; `Manager.Path` points at a login token for `loginID`. The fake token endpoint records each `client_id` and answers `401 invalid_client` for any client except `loginID`. Assert one attempt, with `loginID`.

  ```go
  func TestManagerGetOrdersByTheLoginOfTheTokenItRefreshes(t *testing.T) {
  	binary := filepath.Join(t.TempDir(), "agy")
  	loginID, otherID := fixtureClientID("123", "login"), fixtureClientID("456", "other")
  	if err := os.WriteFile(binary, []byte(loginID+" "+otherID+" "+fixtureClientSecret("a")), 0o700); err != nil {
  		t.Fatal(err)
  	}
  	t.Setenv(agyBinaryPathEnv, binary)
  	t.Setenv(oauthClientIDEnv, "")
  	t.Setenv(oauthClientSecretEnv, "")
  	t.Setenv("AGY_TOKEN_PATH", fixtureLoginToken(t, otherID))
  	var attempts []string
  	server := httptest.NewServer(... records client_id; 401 {"error":"invalid_client"} unless loginID ...)
  	defer server.Close()
  	manager := Manager{Path: fixtureLoginToken(t, loginID), HTTPClient: server.Client(), TokenURL: ..., UserInfoURL: ...}
  	if _, err := manager.Get(context.Background()); err != nil {
  		t.Fatal(err)
  	}
  	if len(attempts) != 1 || attempts[0] != loginID {
  		t.Fatalf("client_id attempts = %v, want only the login client of the refreshed token %q", attempts, loginID)
  	}
  }
  ```

- Step 2: `go test -count=1 -run TestManagerGetOrdersByTheLoginOfTheTokenItRefreshes ./internal/auth/` → FAIL, attempts `[otherID loginID]`.
- Step 3 (green):
  - Split `agyLoginClientID` into the default-file read and `loginClientIDFromToken(raw []byte) string` (the `id_token` → `azp` parse). Keep the default-file read unlocked; its callers (sign-in, `FromRefreshToken`) hold no lock, and a lock would make them wait on any agy refresh. Rewrite its comment.
  - `agyOAuthCredentialCandidates(loginClientID string)` orders by the given client.
  - `Manager.oauthCredentialCandidates(loginClientID func() string)` calls `loginClientID` only when no explicit client is configured, so environment credentials still read no token file.
  - `Manager.refresh(ctx, refreshToken, loginClientID func() string)`. `Get` passes `func() string { return loginClientIDFromToken(raw) }`; `FromRefreshToken`, `oauthCredentials` and both sign-in calls in `oauth_server.go` pass `agyLoginClientID`.
  - Update `TestAgyOAuthCredentialCandidatesPutLoginClientFirst` to pass `agyLoginClientID()`.
- Step 4: `go test -race -count=1 ./internal/auth/` → PASS, including the deadlock test.
- Step 5: `git commit -m "fix(auth): order an agy refresh by the login of the token it refreshes"`.

## Task 2 — Isolate the refresh tests from exported client credentials

**Files:** Test `internal/auth/agy_oauth_test.go`.

- Step 1: in `TestManagerGetRefreshesTheDefaultAgyTokenWithoutDeadlock`, add `t.Setenv(oauthClientIDEnv, "")` and `t.Setenv(oauthClientSecretEnv, "")` (Task 1's test already has them).
- Step 2: `AGY_OAUTH_CLIENT_ID=x AGY_OAUTH_CLIENT_SECRET=y go test -count=1 -run 'TestManagerGet' ./internal/auth/` → PASS (before the change the deadlock test fails on `client_id`).
- Step 3: `git commit -m "test(auth): clear exported OAuth client credentials in agy refresh tests"`.

## Verification

- `gofmt -l internal/` empty; `go build ./...`; `go vet ./...`; `go test ./...`; `go test -race ./internal/auth/ ./internal/accounts/`.
- `trail check` before the push; `git push fork fix/agy-token-refresh-deadlock`.
- Handover (operator, live token): `go run ./cmd/authcheck` against an expired agy token returns `refreshed=true`.
