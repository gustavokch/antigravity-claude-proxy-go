# PR #63 Second-Pass Review Remediation — OpenRouter catalog-derived limits

**PR:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/63
**Branch:** `fix/openrouter-1m-context-max-tokens`
**Date:** 2026-09-09

## Goal

Second pass over the already-remediated PR. The core fix
(`matchModel` parity via `GetModelLimits`) is verified sound. Two findings
survived re-review:

1. 🟡 The Kimi discovery branch still has the unclamped
   `maxOutput = contextLen` fallback this PR fixed for OpenRouter: a 1M-context
   Kimi allowlist entry with no `MaxOutputTokens` advertises 1M max output.
   Pre-existing, same bug class.
2. 🔵 The Anthropic and Kimi branches still use the `200000` literal while the
   OpenRouter branch now uses named consts.

Accepted, no fix: the warms-test failure path can leak an async cache write
past cleanup into the global cache — only on an already-failing/slow run.

## Architecture

- `internal/api/server.go` — `models()` handler: Anthropic branch (line ~454),
  Kimi branch (line ~590), OpenRouter branch (reference implementation of the
  clamp).
- Tests: `internal/api/models_discovery_test.go`.

## Tech Stack

Go 1.x, standard `testing`, `net/http/httptest`, `config.SetForTest`.
Verification gate: `gofmt -l`, `go build ./...`, `go test ./...`.

---

## Task 1 — Cap the Kimi discovery `max_output_tokens` fallback

**Files:** modify `internal/api/server.go`; test `internal/api/models_discovery_test.go`

**Consumes:** a Kimi allowlist entry with `ContextLen` set and
`MaxOutputTokens` unset.
**Produces:** `/v1/models` Kimi entry whose `max_output_tokens` never exceeds
`defaultDiscoveryMaxOutputTokens`, while `context_window` still reports the
configured value. Manual `MaxOutputTokens` overrides stay authoritative.

**Step 1 — failing test**

`TestKimiModels_MaxOutputFallbackDoesNotEqualContextWindow`: enable Kimi with
one allowlist entry `{ID: "kimi/huge-context", ContextLen: 1048576}` and no
`MaxOutputTokens`; call `server.models`; assert `context_window == 1048576`
and `max_output_tokens == defaultDiscoveryMaxOutputTokens`.

**Step 2 — confirm failure**

`go test ./internal/api/ -run TestKimiModels_MaxOutputFallback -v`
(expected: `max_output_tokens = 1048576`).

**Step 3 — implementation**

In the Kimi branch, after `maxOutput = contextLen`, clamp with
`defaultDiscoveryMaxOutputTokens` — same shape as the OpenRouter branch.

**Step 4 — confirm pass** — same command.

**Step 5 — commit**

`fix(api): cap kimi discovery max_output_tokens fallback`

---

## Task 2 — Use the named consts in the Anthropic and Kimi branches

**Files:** modify `internal/api/server.go`

**Consumes:** `defaultDiscoveryContextWindow` (already defined for OpenRouter).
**Produces:** no behavior change — the two remaining `200000` literals in the
Anthropic and Kimi branches read from the named const.

**Step 1 — implementation**

Replace the `contextLen = 200000` literals in the Anthropic and Kimi branches
with `defaultDiscoveryContextWindow`.

**Step 2 — verify**

`go test ./internal/api/ -run 'TestClaudeCode|TestKimi|TestOpenRouter' -v`
(existing discovery tests pin the values; `gofmt -l` prints nothing).

**Step 3 — commit**

`refactor(api): use discovery context-window const in all branches`

---

## Verification gate

```
gofmt -l internal cmd    # no touched file listed
go build ./...
go test ./...
git push origin fix/openrouter-1m-context-max-tokens
```
