# PR #38 Remediation — OpenRouter Response Caching

**PR:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/38
**Branch:** `feat/openrouter-response-caching`
**Goal:** Resolve the review findings on the OpenRouter server-side response caching integration without changing its intended behavior.

## Architecture

- `internal/openrouter/cache.go` — header contract, config merge, TTL clamping.
- `internal/openrouter/observability.go` — cost and token accounting for cache HIT/MISS.
- `internal/config/config.go` — persisted `responseCache` config (global and per-model).
- `internal/webui/public/js/components/models.js` — WebUI OpenRouter settings panel.

## Tech Stack

Go 1.x standard library, `net/http/httptest` for tests. Alpine.js for the WebUI panel.
Gate: `go test ./...`.

---

## Task 1 — WebUI must not wipe `responseCache` on save

**Finding:** 🔴 `models.js:L478`. `config.Save` replaces the whole `openrouter` object, and the
save payload omits `responseCache`, so any WebUI save discards the global cache config.

- **Modify:** `internal/webui/public/js/components/models.js`
- **Consumes:** `GET /api/openrouter/config` → `config.responseCache`
- **Produces:** `POST /api/openrouter/config` payload including `responseCache`

Steps:
1. Add `responseCache: null` to the `openRouterConfig` initial state.
2. Populate it in `fetchOpenRouterConfig` from `data.config.responseCache`.
3. Ride it through the payload in `saveOpenRouterConfig`, matching the existing `routing` guard.
4. Verify by inspection plus the Go-side round-trip test in Task 2.
5. Commit: `fix(webui): preserve openrouter responseCache across config saves`

---

## Task 2 — Round-trip test for `responseCache` persistence

**Finding:** the data-loss path in Task 1 had no test.

- **Modify:** `internal/api/management_test.go`
- **Test:** save an OpenRouter config with `responseCache`, then save again with a payload
  that omits it (the WebUI shape), and assert the field survives only when it is sent —
  pinning the whole-object-replace contract so the WebUI fix is provably required.

Steps:
1. Write `TestManagement_OpenRouterResponseCachePersistence` (Red).
2. `go test ./internal/api/ -run ResponseCachePersistence -v` — confirm behavior.
3. Commit: `test(api): pin openrouter config whole-object replace semantics`

---

## Task 3 — Clamp and normalize client cache override headers

**Findings:** 🟡 `cache.go:L94`, `cache.go:L98`.

- **Modify:** `internal/openrouter/cache.go`
- **Test:** `internal/openrouter/cache_test.go`

Behavior after the fix:
- Client `X-OpenRouter-Cache` is parsed case-insensitively (`true`/`false`), and forwarded
  normalized. An unrecognized value is ignored and falls through to proxy config.
- Client `X-OpenRouter-Cache-TTL` is parsed and clamped locally with `ClampCacheTTL`.
  An unparseable TTL is dropped rather than forwarded.
- A client TTL sent without `X-OpenRouter-Cache` is honored when override is allowed and
  caching is enabled by proxy config, instead of being silently discarded.

Steps:
1. Add failing cases to `TestApplyResponseCacheHeaders` (Red).
2. `go test ./internal/openrouter/ -run ApplyResponseCacheHeaders -v` — confirm failure.
3. Implement normalization and clamping.
4. Re-run — confirm pass.
5. Commit: `fix(openrouter): clamp and normalize client cache override headers`

---

## Task 4 — Pin the cache-HIT accounting contract

**Finding:** 🟡 `observability.go:L75`. Cost is zeroed on HIT while tokens are still recorded,
and every HIT test mock reports zero usage, so the real contract is untested.

Contract to pin: on HIT the call is free (`CallCost == 0`) but reported token usage is still
recorded in the session tracker, because it reflects real context consumption.

- **Modify:** `internal/openrouter/observability.go` (comment only)
- **Test:** `internal/openrouter/observability_test.go`

Steps:
1. Write `TestRequestMetrics_ComputeFinalMetrics_CacheHitWithNonZeroUsage` (Red).
2. `go test ./internal/openrouter/ -run CacheHitWithNonZeroUsage -v`.
3. Clarify the comment at the HIT branch.
4. Re-run — confirm pass.
5. Commit: `test(openrouter): pin cache HIT cost and token accounting`

---

## Task 5 — Dead branch and misleading comments

**Findings:** 🔵 `cache.go:L45`, `cache.go:L103`.

- **Modify:** `internal/openrouter/cache.go`

Steps:
1. Remove the unreachable `ttl < MinCacheTTLSeconds` branch.
2. Reword the "client headers stripped" comment to describe the real mechanism: the upstream
   request is constructed fresh, so client cache headers never propagate unless set here.
3. `go test ./internal/openrouter/ -v`.
4. Commit: `refactor(openrouter): drop dead TTL branch, correct cache header comments`

---

## Task 6 — Document the config keys

**Finding:** 🔵 `README.md`.

- **Modify:** `README.md`

Steps:
1. Document global and per-model `responseCache` keys, defaults, and the override headers.
2. Commit: `docs(readme): document openrouter responseCache configuration`

---

## Verification

- `go build ./...`
- `go vet ./internal/...`
- `go test ./...` — must be fully green before push.
- `git push fork feat/openrouter-response-caching`
