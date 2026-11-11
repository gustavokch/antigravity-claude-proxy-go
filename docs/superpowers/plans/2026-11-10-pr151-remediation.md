# PR #151 Remediation Implementation Plan

> Execute inline, without subagents, as requested. Each task has a RED → GREEN regression gate and an atomic commit.

**Goal:** Preserve working OAuth flows during slow or failed registration probes, and advise account replacement only when Cloud Code explicitly rejects the OAuth client.

**Architecture:** Prepare an OAuth replacement before taking the existing flow-registry mutex; retain atomic replacement of successful flows. Keep entitlement diagnosis in `accounts.UnentitledClientError`, shared by both project resolvers.

**Tech Stack:** Go 1.27rc2, net/http, httptest, Go race detector.

**Spec:** [PR #151](https://github.com/gustavokch/antigravity-claude-proxy-go/pull/151), its published Code Review, and `docs/adr/0006-cloud-code-sign-in-client.md`.

## Global Constraints

- Do not change TLS configuration, transports, Cloud Code request identity, or protobuf schemas.
- Keep the chosen client's credential pairs bound to its flow.
- Preserve the user's shared checkout; implement in `.worktrees/pr151-remediation`.
- Push `HEAD:fix/agy-oauth-signin-client` to `fork`, never `origin`.
- Test observable HTTP behavior and diagnosis; do not assert mutex layout or wording.

## Review Focus

- A pending registration probe must not delay callback rejection.
- A failed replacement must leave the previous callback URL usable.
- A successful replacement must still invalidate its predecessor (existing coverage).
- Missing, null, empty-string, and empty-object project fields must be treated as no project.
- A valid project or a non-client eligibility reason must not prompt account deletion.

## Task 1: Isolate registration I/O from the active flow

**Files:** Modify `internal/auth/oauth_server.go`, `internal/auth/oauth_server_test.go`, and `docs/adr/0006-cloud-code-sign-in-client.md`.

**Interfaces:** Consumes `prepareSignIn(string)` and `OAuthManager.ServeHTTP`; produces unchanged `StartFlow() (string, string, error)` with responsive callbacks and failure-atomic replacement. No dependency on Task 2.

- [ ] Step 1: Add the following regression tests (imports: errors and time). The existing `stubProbe` provides a deterministic external registration response.

```go
func TestOAuthManagerProbeDoesNotBlockCallbacks(t *testing.T) {
    t.Setenv(oauthClientIDEnv, "client")
    t.Setenv(oauthClientSecretEnv, "secret")
    started, release := make(chan struct{}), make(chan struct{})
    stubProbe(t, func(string) (bool, string, error) {
        close(started)
        <-release
        return false, "", errors.New("registration unavailable")
    })
    om := NewOAuthManager(nil)
    flowDone := make(chan error, 1)
    go func() { _, _, err := om.StartFlow(); flowDone <- err }()
    <-started
    callbackDone := make(chan *httptest.ResponseRecorder, 1)
    go func() {
        rec := httptest.NewRecorder()
        req := httptest.NewRequest(http.MethodPost, "/api/auth/complete", strings.NewReader(`{"callbackInput":"code","state":"unknown"}`))
        om.ServeHTTP(rec, req)
        callbackDone <- rec
    }()
    select {
    case rec := <-callbackDone:
        if rec.Code != http.StatusBadRequest { t.Errorf("callback status = %d, want 400", rec.Code) }
    case <-time.After(2 * time.Second):
        t.Error("callback blocked behind registration probe")
    }
    close(release)
    if err := <-flowDone; err == nil { t.Fatal("expected registration failure") }
}

func TestOAuthManagerFailedProbePreservesActiveCallback(t *testing.T) {
    t.Setenv(oauthClientIDEnv, "client")
    t.Setenv(oauthClientSecretEnv, "secret")
    stubProbe(t, func(string) (bool, string, error) { return true, "", nil })
    om := NewOAuthManager(nil)
    authURL, state, err := om.StartFlow()
    if err != nil { t.Fatal(err) }
    parsed, err := url.Parse(authURL)
    if err != nil { t.Fatal(err) }
    stubProbe(t, func(string) (bool, string, error) { return false, "", errors.New("registration unavailable") })
    if _, _, err := om.StartFlow(); err == nil { t.Fatal("expected registration failure") }
    client := &http.Client{Timeout: 2 * time.Second}
    response, err := client.Get(parsed.Query().Get("redirect_uri") + "?state=" + url.QueryEscape(state))
    if err != nil { t.Fatalf("active callback closed by failed replacement: %v", err) }
    defer response.Body.Close()
    if response.StatusCode != http.StatusBadRequest { t.Errorf("callback status = %d, want 400 for missing code", response.StatusCode) }
}
```

- [ ] Step 2: Run `go test ./internal/auth -run 'TestOAuthManager(ProbeDoesNotBlockCallbacks|FailedProbePreservesActiveCallback)$' -count=1 -v`. Expected: blocked callback and closed listener failures.
- [ ] Step 3: Move flow locking and old-flow cleanup after successful preparation, before publishing the new flow. Do not alter replacement semantics for successful starts. Delete the existing bare-error `completeFlow with custom redirect URI` subtest, which makes an unbounded real Google request; local redemption coverage already exists.
- [ ] Step 4: Run `go test -race ./internal/auth -count=1`. Expected: all pass. Document failure-atomic flow replacement in ADR 0006 after the smoke proof.
- [ ] Step 5: `git add internal/auth/oauth_server.go internal/auth/oauth_server_test.go docs/adr/0006-cloud-code-sign-in-client.md docs/superpowers/plans/2026-11-10-pr151-remediation.md && git commit -m "fix(auth): prepare sign-in before replacing the active flow"`.

## Task 2: Ground account migration advice in explicit client rejection

**Files:** Modify `internal/accounts/manager.go`, `internal/accounts/manager_test.go`, and `docs/adr/0006-cloud-code-sign-in-client.md`.

**Interfaces:** Consumes Cloud Code JSON; produces unchanged `UnentitledClientError([]byte) error`. Both project resolver callers remain unchanged. No dependency on Task 1.

- [ ] Step 1: Add a table-driven regression test with the exact cases below, preserving the captured-response test already present.

```go
func TestUnentitledClientErrorRequiresClientRejection(t *testing.T) {
    t.Parallel()
    const tiers = `"allowedTiers":[{"id":"standard-tier","userDefinedCloudaicompanionProject":true}]`
    const rejection = `"ineligibleTiers":[{"reasonCode":"GOOGLE_TOS_NOT_SUPPORTED_BY_CLIENT","tierId":"free-tier"}]`
    for _, tc := range []struct { name, body string; wantHint bool }{
        {"no rejection evidence", `{` + tiers + `}`, false},
        {"other eligibility reason", `{` + tiers + `,"ineligibleTiers":[{"reasonCode":"OTHER_REASON"}]}`, false},
        {"missing project", `{` + tiers + `,` + rejection + `}`, true},
        {"null project", `{` + tiers + `,` + rejection + `,"cloudaicompanionProject":null}`, true},
        {"empty string project", `{` + tiers + `,` + rejection + `,"cloudaicompanionProject":""}`, true},
        {"empty object project", `{` + tiers + `,` + rejection + `,"cloudaicompanionProject":{}}`, true},
        {"empty project id", `{` + tiers + `,` + rejection + `,"cloudaicompanionProject":{"id":""}}`, true},
        {"string project", `{` + tiers + `,` + rejection + `,"cloudaicompanionProject":"managed"}`, false},
        {"object project", `{` + tiers + `,` + rejection + `,"cloudaicompanionProject":{"id":"managed"}}`, false},
    } {
        t.Run(tc.name, func(t *testing.T) {
            if err := UnentitledClientError([]byte(tc.body)); (err != nil) != tc.wantHint {
                t.Errorf("migration hint = %v, want present %v", err, tc.wantHint)
            }
        })
    }
}
```

- [ ] Step 2: Run `go test ./internal/accounts -run '^TestUnentitledClientError' -count=1 -v`. Expected: false-positive diagnosis and empty-project false-negative failures.
- [ ] Step 3: Decode `ineligibleTiers.reasonCode`, require `GOOGLE_TOS_NOT_SUPPORTED_BY_CLIENT`, and recognize usable string/object project IDs using the existing project representation. Keep the allowed-tier guard and migration message; remove the unused allowed-tier ID field.
- [ ] Step 4: Run `go test -race ./internal/accounts ./internal/api -count=1`. Expected: all pass. Document the explicit evidence requirement in ADR 0006 after the smoke proof.
- [ ] Step 5: `git add internal/accounts/manager.go internal/accounts/manager_test.go docs/adr/0006-cloud-code-sign-in-client.md && git commit -m "fix(accounts): require explicit client rejection for migration advice"`.

## Final verification and delivery

- Run `go test -race -json ./...` once; record test/subtest counts, skips, failures, and package results.
- Run the project's live JA4 gate; a tooling or capture limitation must be reported as unverified, not a match.
- Run `trail check` before commits/push where supported.
- Push `git push fork HEAD:fix/agy-oauth-signin-client` only after a green full suite.
- Post the resolved findings, verification evidence, and commit IDs to PR #151; report its link.

## Execution record

- Baseline: `go test ./internal/auth ./internal/accounts` passed, 354 tests/subtests in two packages.
- Ruling: use a private remediation branch/worktree because workspace policy blocks switching the shared checkout; push only to the existing PR head.
- Pre-flight: the two tasks share no code interfaces; both append narrowly scoped ADR consequences.
- Task 1: RED reproduced both a callback blocked behind the probe and an old listener closed on probe failure. GREEN: `go test -race ./internal/auth -count=1`, 92 tests/subtests passed. Removed the real-Google bare-error test; retained local redemption coverage.
- Ruling: `trail check --base fork/main` flags the pre-existing `agyCLIClientID` comment as reintroducing claim-based token reads. This remediation changes only flow-registry lock placement, not the unlocked login hint or the refresh file lock; no decision is overridden. Record the warning in the commit and PR rather than altering unrelated refresh behavior.
- Task 2: RED reproduced false hints without client rejection and missing hints for empty string/object project fields. GREEN: `go test -race ./internal/accounts ./internal/api -count=1`, 1,264 tests/subtests passed across both project resolver packages.
- Final review: self-review, as the user prohibited subagents. Exported signatures and both project resolver callers remain unchanged; no TLS or transport code was changed. The lifecycle regressions exercise HTTP behavior rather than mutex implementation; the diagnosis table covers the supported string/object project forms.
- Full suite: `go test -race -json ./...` exited 0: 3,281 passing tests/subtests (2,161 top-level), no failures, 3 existing skips, 41 passing packages. The skips are two subprocess helpers and a network-dependent userinfo test; 29 additional packages have no tests.
- JA4 remains unverified: `scripts/verify-ja4.sh` could not remove an existing unwritable `/tmp/antigravity-go.pcap`; retrying tcpdump with a private capture path failed with `/dev/bpf0: Permission denied`. No TLS or transport changes were made. A packet capture with BPF privileges is still required before claiming fingerprint parity.
- Decision gate: the final `trail check --base fork/main` reports the same single false positive already documented above; no lock decision was changed.
