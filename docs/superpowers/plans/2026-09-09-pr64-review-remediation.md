# PR #64 Review Remediation — Cache Bump

**Date:** 2026-09-09
**PR:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/64
**Branch:** `cache-bump`
**Review comment:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/64#issuecomment-5610305897

## Goal

Resolve the two bugs, six risks and four nits raised in the PR #64 review while keeping
`go build ./...` clean and `go test -race ./...` fully green.

## Architecture touched

- `internal/cachebump` — TTL detection, store capacity, scheduler rescheduling.
- `internal/api` — Claude Code bump sender (rate-limit accounting, token refresh),
  recorder hot path, custom-endpoint header forwarding, management stop path.
- `internal/config` — cache-bump enablement precedence, byte budget knob.

## Tech stack

Go 1.x, stdlib `encoding/json`, `testing` with `-race`. Test command:
`go test -race ./internal/cachebump/... ./internal/api/... ./internal/config/...`

## Findings dropped after verification

- "Unrelated re-indent churn in `internal/config/config.go` and
  `internal/api/claudecode_management.go`" — `fork/main` versions of both files are
  **not** gofmt-clean, so the reformat in this PR is a legitimate `gofmt` fix, not churn.
  No action.

---

## Task 1 — `X-Cache-Bump: on` must not bypass the global kill switch

**Modify:** `internal/config/config.go`
**Test:** `internal/config/cachebump_test.go`

- **Consumes:** `CacheBumpConfig{Enabled, AllowHeaderOverride, Routes}`, header value.
- **Produces:** `EnabledFor(route, header) bool` where `off` always disarms, `on` only
  overrides the *per-route* flag, never the global `Enabled` switch.

Step 1 — failing test: `EnabledFor("claudecode", "on")` with `Enabled: false,
AllowHeaderOverride: true` must return `false`; with `Enabled: true, Routes.ClaudeCode:
false` it must return `true`; `"off"` must return `false` in both.
Step 2 — `go test -race ./internal/config/ -run CacheBump` (red).
Step 3 — reorder `EnabledFor`: handle `off` first, then `!c.Enabled` short-circuit, then `on`.
Step 4 — re-run (green).
Step 5 — `git commit -m "fix(config): keep cache-bump global switch authoritative"`

## Task 2 — scheduler must not reschedule a bump at cache expiry

**Modify:** `internal/cachebump/scheduler.go`
**Test:** `internal/cachebump/scheduler_test.go`

- **Consumes:** `SchedulerConfig.LeadSeconds`, `Record.TTL`.
- **Produces:** next bump always `NextBumpTime(now, ttl, lead)`, which clamps a
  non-positive lead to `ttl/5`, matching the recorder.

Step 1 — failing test: with `LeadSeconds: 0` and a 5m TTL, after a successful bump the
record's `NextBump` must be `now + 4m`, not `now + 5m`.
Step 2 — `go test -race ./internal/cachebump/ -run Lead` (red).
Step 3 — drop the `if s.cfg.LeadSeconds > 0` branch; always call `NextBumpTime`.
Step 4 — re-run (green).
Step 5 — `git commit -m "fix(cachebump): always schedule bumps a lead before expiry"`

## Task 3 — `Upsert` must not evict a live session when re-arming an existing one

**Modify:** `internal/cachebump/store.go`
**Test:** `internal/cachebump/store_test.go`

Step 1 — failing test: fill a store to `maxEntries`, re-`Upsert` an existing key, assert
every original key is still present and `Len() == maxEntries`.
Step 2 — run (red).
Step 3 — guard `evictOldestLocked` on the key not already being present.
Step 4 — re-run (green).
Step 5 — `git commit -m "fix(cachebump): only evict when upsert adds a new record"`

## Task 4 — cap the store by bytes, not only by record count

**Modify:** `internal/cachebump/store.go`, `internal/config/config.go`,
`internal/api/cachebump_server.go`, `internal/api/cachebump_management.go`
**Test:** `internal/cachebump/store_test.go`, `internal/config/cachebump_test.go`

- **Produces:** `NewStoreWithLimits(ttl, maxEntries, maxBytes)`; `NewStore` delegates with
  a 64 MiB default. `Store.Bytes()` reports the current budget use and is surfaced on
  `GET /api/cache-bump`. New config field `cacheBump.maxBodyBytesMB` (default 64).

Step 1 — failing tests: oldest records are evicted until the budget fits; a single body
larger than the whole budget is not stored at all; `Bytes()` tracks replacement correctly.
Step 2 — run (red).
Step 3 — track `totalBytes` incrementally in `Upsert`/`Stop`-free paths, `pruneLocked`,
`evictOldestLocked` and `Clear`; wire the config knob through `getCacheBump`.
Step 4 — re-run (green).
Step 5 — `git commit -m "feat(cachebump): bound recorded bodies by a byte budget"`

## Task 5 — Claude Code bumps must feed rate-limit accounting

**Modify:** `internal/api/cachebump_server.go`
**Test:** `internal/api/cachebump_integration_test.go`

- **Consumes:** `claudecode.ExtractRateLimits(resp.Header)`.
- **Produces:** 429 → `pool.RecordRateLimit(id, rl, 10s)`; 5xx → `pool.RecordFailure(id,
  true, 30s)`; other 4xx → `pool.RecordFailure(id, false, 0)`; success →
  `pool.UpdateAccountRateLimits(id, rl)`.

Step 1 — failing test: a 429 bump response leaves the account with a cooldown in the
future; a successful bump propagates the `anthropic-ratelimit-*` headers onto the account.
Step 2 — run (red).
Step 3 — extract rate limits and record the outcomes in `sendClaudeCodeBump`.
Step 4 — re-run (green).
Step 5 — `git commit -m "fix(api): record rate limits and failures for cache bumps"`

## Task 6 — one allocation-light scan instead of two deep JSON decodes

**Modify:** `internal/cachebump/ttl.go`, `internal/api/cachebump_server.go`
**Test:** `internal/cachebump/ttl_test.go`

- **Produces:** `InspectCacheControl(body) (found bool, extended bool)` implemented as a
  single `json.Decoder` token walk (no `map[string]any` tree). `HasCacheControl` and
  `DetectTTL` become wrappers; the recorder calls `InspectCacheControl` once.

Step 1 — failing test: `InspectCacheControl` reports `found` for a nested marker and
`extended` only for `{"ttl":"1h"}`; it must not treat a *value* string `"cache_control"`
as a member name.
Step 2 — run (red).
Step 3 — token-stream scan; delete `findCacheControl` / `scanCacheControl`.
Step 4 — re-run existing `ttl_test.go` as the regression net (green).
Step 5 — `git commit -m "perf(cachebump): scan cache_control in one token pass"`

## Task 7 — a failed token refresh must retry, not permanently stop the session

**Modify:** `internal/api/cachebump_server.go`
**Test:** `internal/api/cachebump_integration_test.go`

Step 1 — failing test: a refresher that always errors makes `sendClaudeCodeBump` return
`cachebump.ErrAccountUnavailable` instead of sending a stale token.
Step 2 — run (red).
Step 3 — return `ErrAccountUnavailable` when `RefreshTokenIfNeeded` errors.
Step 4 — re-run (green).
Step 5 — `git commit -m "fix(api): stop cache bump on token refresh failure"`

## Task 8 — custom-endpoint TTL must match the beta actually sent

**Modify:** `internal/api/server.go`, `internal/cachebump/ttl.go`,
`internal/api/cachebump_server.go`
**Test:** `internal/cachebump/ttl_test.go`, `internal/api/cachebump_integration_test.go`

- **Produces:** the custom-endpoint `Director` forwards `anthropic-version` and
  `anthropic-beta` (matching its own CCR sender). `DetectTTL(body, betaHeader)` returns 1h
  only when the body opts in **and** `anthropic-beta` carries `extended-cache-ttl`.

Step 1 — failing tests: `DetectTTL(oneHourBody, "")` is 5m; with
`"extended-cache-ttl-2025-04-11"` it is 1h. Custom-endpoint forward carries the two
Anthropic headers upstream.
Step 2 — run (red).
Step 3 — thread the beta header through the recorder; forward it in the Director.
Step 4 — re-run (green).
Step 5 — `git commit -m "fix(api): gate 1h cache TTL on the extended-cache-ttl beta"`

## Task 9 — nits

**Modify:** `internal/cachebump/scheduler.go`, `internal/api/cachebump_management.go`,
`internal/cachebump/ttl.go`, `internal/cachebump/record.go`,
`internal/api/cachebump_management_test.go`

- Manual stop goes through the scheduler so `stops_by_reason` counts it: add
  `Scheduler.StopSession(key, reason)` and call it from `handleCacheBumpStop`.
- `NextBumpTime` doc: the clamp is `TTL/5` (12m for a 1h TTL), not 5m.
- Rename the `max` local in `NextBumpTime` so it stops shadowing the builtin.
- Move `bytesEqual` from `record.go` into a `_test.go` file.
- `gofmt -w internal/api/cachebump_management_test.go`.

Step 5 — `git commit -m "chore(cachebump): review nits — stats, docs, test helpers"`

## Verification gate

```
go build ./...
gofmt -l ./cmd ./internal   # no cachebump files listed
go vet ./...
go test -race ./...          # 100% green before push
```
