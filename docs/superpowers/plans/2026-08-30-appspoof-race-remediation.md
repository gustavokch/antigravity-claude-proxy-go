# PR #46 remediation: appSpoofActivated data race + gate portability

Goal: fix unsynchronized access to `Server.appSpoofActivated`, make the JA4
live-capture gate opt-in, drop the dead `EXPECTED_SNI` var.

Spec: review comment https://github.com/gustavokch/antigravity-claude-proxy-go/pull/46#issuecomment-5470975261

## Task 1: guard appSpoofActivated with server.mu

Target: `internal/api/server.go` (modify), `internal/api/openrouter_appspoof_test.go` (add test)

1. Write failing test: `TestOpenRouterForwarding_AppSpoofActivatedConcurrentAccess` —
   fire N goroutines through `forwardToOpenRouter` (via `makeReq`) concurrently
   against a backend that always returns the harness-gate 403 on the
   unattributed first attempt, run with `-race`, assert no race and that the
   flag ends up `true`.
2. Run: `go test -race -run TestOpenRouterForwarding_AppSpoofActivatedConcurrentAccess ./internal/api/...`
   — confirm `WARNING: DATA RACE` before the fix.
3. Minimal fix: wrap the read at line ~1211 and the read-then-write at
   ~1542-1543 with `server.mu.Lock()/Unlock()`.
4. Re-run same command — confirm clean, no race.
5. Commit: `fix(openrouter): guard appSpoofActivated with server.mu`

## Task 2: make verify-ja4.sh opt-in

Target: `scripts/verify-fingerprint.sh` (modify)

1. No unit test (shell gate script) — manual verification only.
2. Guard the `./scripts/verify-ja4.sh` call behind
   `ANTIGRAVITY_RUN_JA4_GATE=1` env check, printing a skip notice otherwise.
3. Commit: `fix(scripts): make live JA4 capture gate opt-in`

## Task 3: drop dead EXPECTED_SNI

Target: `scripts/verify-ja4.sh` (modify)

1. Remove unused `EXPECTED_SNI` assignment (or wire it into the assertion —
   chose removal since assertion already gates on JA4 alone by design).
2. Commit: `fix(scripts): drop unused EXPECTED_SNI in verify-ja4.sh`

## Verification & push

`go build ./... && go vet ./... && go test -race ./...` must be clean, then
push `fix/openrouter-harness-gate-sticky-spoof`.
