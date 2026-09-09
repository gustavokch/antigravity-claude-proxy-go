# PR #63 Review Remediation — OpenRouter catalog-derived limits

**PR:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/63
**Branch:** `fix/openrouter-1m-context-max-tokens`
**Date:** 2026-09-09

## Goal

PR #63 gives `deriveOpenRouterMaxOutput` and `/v1/models` discovery access to the
live OpenRouter catalog through the new `Client.GetModelLimits`. The core change
is correct. This plan closes the edges the change opened:

1. Discovery now advertises a 1M-context model's `max_output_tokens` as 1M
   whenever the catalog reports no `max_completion_tokens`, because the fallback
   equates output capacity with context window.
2. Discovery reads the cache but never repairs it, so a cold or expired cache
   silently returns the same hard-coded `200000` the PR set out to eliminate.
3. The new test mutates the package-global `openrouter.DefaultClient`, and the
   cleanup cannot restore the cache timestamp.
4. `GetModelLimits` does not document that it ignores the cache TTL, and neither
   its `contextLength` return nor its `ok=false` path is covered.

## Architecture

- `internal/api/server.go` — `models()` handler, OpenRouter allowlist branch.
- `internal/openrouter/client.go` — `Client.GetModelLimits`.
- Tests: `internal/api/models_discovery_test.go` (handler-level),
  `internal/api/server_test.go` (`deriveOpenRouterMaxOutput`),
  `internal/openrouter/client_test.go` (unit).

## Tech Stack

Go 1.x, standard `testing`, `net/http/httptest`, `config.SetForTest`.
Verification gate: `gofmt -l`, `go build ./...`, `go test ./...`.

---

## Task 1 — Cap the discovery `max_output_tokens` fallback

**Files:** modify `internal/api/server.go`; test `internal/api/models_discovery_test.go`

**Consumes:** `openrouter.DefaultClient` cache seeded with a 1M-context entry that
has no `max_completion_tokens`.
**Produces:** `/v1/models` entry whose `max_output_tokens` never exceeds the
existing `200000` default, while `context_window` still reports the true 1M.

**Step 1 — failing test**

`TestOpenRouterModels_MaxOutputFallbackDoesNotEqualContextWindow`: seed the
catalog with `{ID: "vendor/huge", ContextLength: 1048576}` (no max completion),
enable an allowlist entry for `vendor/huge` with no manual overrides, call
`server.models`, assert `context_window == 1048576` and
`max_output_tokens == 200000`.

**Step 2 — confirm failure**

`go test ./internal/api/ -run TestOpenRouterModels_MaxOutputFallback -v`
(expected: `max_output_tokens = 1048576`).

**Step 3 — implementation**

Introduce a named default for the discovery fallback and clamp the OpenRouter
branch's `maxOutput = contextLen` fallback to it. Leave the Anthropic branch
untouched — its fallback path is unchanged by PR #63.

**Step 4 — confirm pass** — same command.

**Step 5 — commit**

`fix(api): cap openrouter discovery max_output_tokens fallback`

---

## Task 2 — Self-heal the catalog cache from `/v1/models`

**Files:** modify `internal/api/server.go`; test `internal/api/models_discovery_test.go`

**Consumes:** `config.OpenRouterConfig{Enabled, APIKey, BaseURL}` and an empty
catalog cache.
**Produces:** one `WarmupCacheAsync` call per discovery request that finds the
cache empty or expired; no call when the cache is valid.

**Step 1 — failing test**

`TestOpenRouterModels_WarmsColdCatalogCache`: point `BaseURL` at an
`httptest.Server` that serves `/v1/models`, clear the cache, call
`server.models`, then wait for the async fetch and assert the catalog cache is
populated (the handler's own response may still carry the fallback — the
contract under test is that the next request will not).

**Step 2 — confirm failure** — no request ever reaches the stub server.

**Step 3 — implementation**

Call `openrouter.DefaultClient.WarmupCacheAsync(cfg.OpenRouter.APIKey,
cfg.OpenRouter.BaseURL)` once, before the allowlist loop, inside the existing
`cfg.OpenRouter.Enabled` branch. `WarmupCacheAsync` early-returns on a valid
cache, so the steady-state cost is one `IsCacheValid` read.

**Step 4 — confirm pass** — same command.

**Step 5 — commit**

`fix(api): warm openrouter catalog from model discovery`

---

## Task 3 — Isolate the global cache mutation in the derive test

**Files:** modify `internal/api/server_test.go`

**Consumes:** nothing new.
**Produces:** a `deriveOpenRouterMaxOutput` test that leaves
`openrouter.DefaultClient` in its prior state, including validity.

**Step 1 — implementation**

`SaveCache(prev)` stamps `cachedAt = time.Now()`, so a test that started with an
empty cache leaves behind a cache that reports as freshly filled. Restore by
saving `nil` when `prev` was empty, and document that the cache timestamp cannot
be restored exactly — sibling tests must not assume a specific `cachedAt`. Also
run `gofmt -w` to fix the comment alignment in the new case slice.

**Step 2 — verify** — `gofmt -l internal/api/server_test.go` prints nothing;
`go test ./internal/api/ -run TestDeriveOpenRouterMaxOutput -v` passes.

**Step 3 — commit**

`test(api): restore openrouter cache state in derive test`

---

## Task 4 — Document and cover `GetModelLimits`

**Files:** modify `internal/openrouter/client.go`; test
`internal/openrouter/client_test.go`

**Consumes:** a client with a seeded cache.
**Produces:** both return values asserted, plus the `ok=false` path.

**Step 1 — failing test**

`TestGetModelLimits`: seed one entry with a known `ContextLength` and
`TopProvider.MaxCompletionTokens`; assert prefix-tolerant and case-insensitive
lookup returns both values with `ok=true`, and that an unknown ID returns
`0, 0, false`.

**Step 2 — confirm failure** — test does not exist yet.

**Step 3 — implementation**

Extend the `GetModelLimits` doc comment: the lookup reads the cache directly and
does not enforce `cacheTTL`, so an expired entry is returned with `ok=true` and
callers own freshness (see `WarmupCacheAsync`).

**Step 4 — confirm pass** — `go test ./internal/openrouter/ -run TestGetModelLimits -v`.

**Step 5 — commit**

`test(openrouter): cover GetModelLimits limits and miss path`

---

## Verification gate

```
gofmt -l internal cmd    # no PR-touched file listed
go build ./...
go test ./...
git push origin fix/openrouter-1m-context-max-tokens
```
