# PR #52 Remediation Plan

**PR:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/52
**Branch:** `worktree-settings-models-ux` (worktree: `.claude/worktrees/settings-models-ux`)
**Date:** 2026-08-31

## Goal

Resolve 3 🔵 nit findings from the PR #52 review:

1. `formatPrice` renders `'$0'` for per-1M prices below $0.00005 (parseFloat of `toFixed(4)` rounds to 0).
2. Pricing column tooltip hardcoded English, bypasses translation system.
3. Test: `providers[1]` type assertion unchecked; negative pricing assertion silently passes when `endpoint` object missing.

## Architecture

- Frontend: plain Alpine.js components in `internal/webui/public/js/components/models.js`, views in `internal/webui/public/views/settings.html`, translations in `internal/webui/public/js/translations/{en,pt}.js`. No JS test harness — verification is `go test` + manual reasoning.
- Backend tests: `internal/api/openrouter_routing_test.go` (Go stdlib `testing`).

## Tech Stack

Go 1.x, Alpine.js, gh CLI.

## Spec reference

PR review comment: https://github.com/gustavokch/antigravity-claude-proxy-go/pull/52#issuecomment-5483369833

---

## Task 1: formatPrice sub-threshold display + tooltip i18n

**Modify:** `internal/webui/public/js/components/models.js`, `internal/webui/public/views/settings.html`, `internal/webui/public/js/translations/en.js`, `internal/webui/public/js/translations/pt.js`

**Consumes:** provider entry `{endpoint: {pricing: {prompt, completion}}}` (floats, USD per token)
**Produces:** display string `'$X.XX / $X.XX'` per 1M tokens

### Steps

1. No JS test harness exists. Behavioral contract stays locked by the Go test (`TestManagement_OpenRouterProvidersEndpoint`) for the data side. Frontend change is display-only.
2. In `models.js` `formatPrice`: replace `return '$' + parseFloat(m.toFixed(4));` with `return '$' + m.toFixed(4);` — keeps `$0.0000`? No: shows trailing zeros. Better: if `m < 0.00005` return `'<$0.0001'` (clamped, unambiguous vs FREE). Keep `parseFloat(m.toFixed(4))` path otherwise.
3. Add `priceTooltip` key: en `"USD per 1M tokens — input / output"`, pt `"USD por 1M de tokens — entrada / saída"`.
4. `settings.html` td: `:title="$store.global.t('priceTooltip') || 'USD per 1M tokens — input / output'"`.
5. Verify: `go test ./internal/webui/ ./internal/api/ -count=1`.
6. Commit: `git add -A && git commit -m "fix(webui): clamp sub-threshold price display and translate price tooltip"`

## Task 2: Harden providers test assertion

**Modify:** `internal/api/openrouter_routing_test.go`

**Consumes:** providers response JSON (2 entries, p1 with pricing, p2 without)
**Produces:** strict negative assertion for p2 pricing absence

### Steps

1. Replace lenient block:

```go
second, _ := providers[1].(map[string]any)
if ep2, ok := second["endpoint"].(map[string]any); ok {
    if _, has := ep2["pricing"]; has {
        t.Errorf("expected no pricing object on p2 endpoint (absent upstream)")
    }
}
```

with:

```go
second, ok := providers[1].(map[string]any)
if !ok {
    t.Fatalf("expected object for second provider entry")
}
ep2, ok := second["endpoint"].(map[string]any)
if !ok {
    t.Fatalf("expected endpoint object on p2 entry")
}
if _, has := ep2["pricing"]; has {
    t.Errorf("expected no pricing object on p2 endpoint (absent upstream)")
}
```

2. Run: `go test ./internal/api/ -run TestManagement_OpenRouterProvidersEndpoint -count=1` — pass expected (behavior unchanged, assertion hardened).
3. Commit: `git commit -m "test(api): require endpoint object on second provider entry"`

## Verification

`go test ./... -count=1` — 100% green gate, then `git push fork worktree-settings-models-ux`.
