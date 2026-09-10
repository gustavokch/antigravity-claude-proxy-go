# PR #64 Review Remediation — Second Pass

**Date:** 2026-09-09
**PR:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/64
**Branch:** `cache-bump`
**Review comment:** posted on the PR (second pass; first-pass fixes verified holding)

## Goal

Resolve the four risks and four nits from the second-pass review while keeping
`go build ./...` clean and `go test -race ./...` fully green.

## Architecture touched

- `internal/cachebump` — store lifecycle (oversized upsert, stop-time memory
  release), scheduler config lifetime (`Reconfigure`) and re-arm race guard.
- `internal/api` — cache-bump store/scheduler wiring, management handler.
- `internal/webui` — settings panel fallback config object.

## Tech stack

Go, stdlib `testing` with `-race`. Gate:
`go test -race ./internal/cachebump/... ./internal/api/... ./internal/config/...`

---

## Task 1 — running scheduler must track config edits

**Modify:** `internal/cachebump/scheduler.go`, `internal/api/cachebump_server.go`
**Test:** `internal/cachebump/scheduler_test.go`, `internal/api/cachebump_integration_test.go`

- **Consumes:** `SchedulerConfig`, `config.Get().CacheBump`.
- **Produces:** `Scheduler.Reconfigure(cfg SchedulerConfig)`; `bump` reads config
  under `s.mu`; `getCacheBump` re-applies current config when the store exists.

Step 1 — failing test: a 1h-TTL record bumps with `LeadSeconds: 60`
(NextBump = bump+59m); after `Reconfigure(LeadSeconds: 600)` the next bump
reschedules at bump+50m. Plus a `-race` test running `Tick` concurrently with
`Reconfigure`.
Step 2 — `go test -race ./internal/cachebump/ -run Reconfigure` (red).
Step 3 — add `Reconfigure` (stores cfg under `s.mu`); snapshot cfg under lock at
the top of `bump`; call `sched.Reconfigure` from `getCacheBump` when the store
already exists.
Step 4 — re-run (green), including the api-level idle-stop test that flips
`maxIdleMinutes` between `getCacheBump` calls.
Step 5 — `git commit -m "fix(cachebump): running scheduler tracks config edits"`

## Task 2 — oversized upsert keeps the session's existing record

**Modify:** `internal/cachebump/store.go`
**Test:** `internal/cachebump/store_test.go`

Step 1 — failing test: record a small body, then upsert the same key with a
body larger than the whole budget; the original record must survive with its
original body and `Bytes()` unchanged.
Step 2 — run (red).
Step 3 — drop the `dropLocked` call in the oversized branch; just return.
Step 4 — re-run (green).
Step 5 — `git commit -m "fix(cachebump): oversized upsert keeps existing record"`

## Task 3 — Stop releases the replay body

**Modify:** `internal/cachebump/store.go`
**Test:** `internal/cachebump/store_test.go`

Step 1 — failing test: `Bytes()` returns to 0 after `Stop` on the only record;
`Get` still reports the record with `Stopped` set and a nil body; a later
`Upsert` re-arm restores normal byte accounting.
Step 2 — run (red).
Step 3 — in `Stop`, subtract `len(rec.Body)` from `s.bytes` and nil
`rec.Body`/`rec.Headers` (management JSON already omits them; re-arm supplies
a fresh body).
Step 4 — re-run (green).
Step 5 — `git commit -m "fix(cachebump): stopped records release body memory"`

## Task 4 — stale in-flight bump must not clobber a re-armed record

**Modify:** `internal/cachebump/scheduler.go`
**Test:** `internal/cachebump/scheduler_test.go`

Step 1 — failing test: the sender re-arms the record via `Upsert` (newer
`LastSeen`, 1h TTL) and then returns a `paid_write` result; the record must
stay active with its fresh `NextBump`, `Bumps` untouched.
Step 2 — run (red).
Step 3 — after the sender returns, fetch the stored record; skip all
accounting (MarkBumped/stop/retry) when it is missing or its `LastSeen` is
newer than the fired copy's.
Step 4 — re-run (green).
Step 5 — `git commit -m "fix(cachebump): ignore stale bump results after re-arm"`

## Task 5 — nits

**Modify:** `internal/api/cachebump_management.go`,
`internal/cachebump/scheduler.go`, `internal/cachebump/scheduler_test.go`,
`internal/webui/public/js/components/server-config.js`

- `handleCacheBumpGet` uses the `Snapshot()` slice directly.
- Remove the unused exported `Scheduler.Run`.
- Drop `newTestRecord`'s unused `lead` parameter.
- WebUI fallback `cacheBump` object gains `maxBodyMB: 64`.

Step 5 — `git commit -m "chore(cachebump): second-pass nits"`

## Verification gate

```
go build ./...
go vet ./...
gofmt -l ./cmd ./internal   # no cachebump files listed
go test -race ./...          # 100% green before push
```
