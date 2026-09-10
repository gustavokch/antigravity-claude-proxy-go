# PR #65 review remediation

**PR:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/65
**Branch:** `fix/cachebump-scheduler-blocking`
**Review comment:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/65#issuecomment-5611327445

## Goal

Resolve the review findings on PR #65 without changing the fix it ships: the
cache-bump scheduler must keep spawning its loop internally so `cmd/proxy`
reaches `ListenAndServe`.

## Baseline verification (already done)

- `git checkout b8602d6 -- internal/api/cachebump_server.go` then
  `go test -run TestStartCacheBumpScheduler ./internal/api/` — FAILS
  ("StartCacheBumpScheduler blocked the caller").
- Same test on PR head — passes. `go vet ./internal/api/` clean,
  `go test -race` clean.

## Tech stack

Go, standard `testing`, `internal/config.SetForTest` for config pinning.

## Task 1 — scheduler loop resolves the store per tick

**Modify:** `internal/api/cachebump_server.go`
**Test:** `internal/api/cachebump_scheduler_test.go`

**Problem:** `getCacheBump()` runs once when the goroutine starts. Two
consequences: the store and scheduler are allocated at startup even when
`CacheBump.Enabled` is false, and the loop never re-reads the WebUI knobs —
`Reconfigure` only runs when a request path happens to call `getCacheBump()`.

**Consumes:** `config.Get().CacheBump`
**Produces:** a loop that allocates nothing while the feature is disabled.

1. Write a failing test: with `CacheBump.Enabled = false` pinned through
   `config.SetForTest`, start the scheduler and assert `server.cacheBumpStore`
   stays nil for ~1s (read under `server.mu`).
2. Run `go test -race -run TestStartCacheBumpScheduler ./internal/api/` and
   confirm the new case fails on the current code.
3. Move `_, sched := server.getCacheBump()` inside the `case <-ticker.C:`
   branch, after the `Enabled` check. Update the doc comment.
4. Re-run the test and confirm it passes.
5. `git commit -m "fix(api): resolve cache bump store per tick"`

## Task 2 — drop the dead config save/restore in the regression test

**Modify:** `internal/api/cachebump_scheduler_test.go`

**Problem:** `original := config.Get()` plus `defer config.SetForTest(original)`
restores a value the test never changed.

1. Replace it with an explicit `config.SetForTest` that pins the state each
   case exercises, keeping the restore so the global stays clean for other
   tests.
2. Run `go test -race ./internal/api/` and confirm green.
3. `git commit -m "test(api): pin config in cache bump scheduler test"`

## Not fixed (documented)

The regression test proves only that the call returns; it does not prove the
loop started or that it exits on `ctx` cancel. Observing a tick needs a test
hook on the fixed 10s `cacheBumpTickingInterval`, and `Scheduler.Tick` over an
empty store emits no event to observe. Out of scope for a startup-hang fix.

## Gate

`go build ./... && go vet ./... && go test -race ./...` must be green before
the branch is pushed.
