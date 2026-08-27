# PR #20 Review Remediation — OpenRouter routing panel fix

**Goal:** Resolve findings from self-review of PR #20 (`fix/openrouter-routing-ui-noop`).
**Spec:** Review comment https://github.com/gustavokch/antigravity-claude-proxy-go/pull/20#issuecomment-5428600668
**Tech stack:** Go (embed FS webui), Alpine 3, JS i18n key-value files.

## Findings

1. 🔵 `internal/webui/public/js/translations/tr.js:527` — `pinnedTo: "Sabitlendiği:"` ends with `:`; `settings.html:1087` appends `:` after the key → renders `Sabitlendiği::`. Other 4 locales have no trailing colon.
2. 🔵 PR body overclaim — "Zero missing keys remain in all 5 locales" is false: parity check vs en shows zh −32, id −8, pt −50, tr −51 keys (pre-existing debt in other feature groups); `manualReload` extra in zh/id/tr. `t()` (store.js:97) falls back to raw key names. Not a regression — rescope body, track debt separately. **No backfill in this PR** (scope creep).

## Task 1: tr.js pinnedTo colon + i18n invariant test (TDD)

- **Test:** `internal/webui/translations_test.go` (create)
- **Modify:** `internal/webui/public/js/translations/tr.js`
- **Consumes:** embedded `public/js/translations/*.js`
- **Produces:** `TestTranslations_OpenRouterKeys`

Step 1 — failing test: load 5 locale files from embed FS, regex-extract top-level keys; assert (a) the 17 OpenRouter panel keys exist in every locale, (b) no locale's `pinnedTo` value ends with `:`. tr fails (b).

Step 2 — `go test ./internal/webui/ -run TestTranslations_OpenRouterKeys -v` → red on tr.

Step 3 — minimal fix: `pinnedTo: "Sabitlendiği:"` → `pinnedTo: "Sabitlendiği"`.

Step 4 — re-run → green; then `go test ./...`.

Step 5 — `git commit -m "fix(webui): drop trailing colon in tr pinnedTo label"`

## Task 2: Rescope PR body

`gh pr edit 20 --body` — replace zero-missing claim with scoped statement + pointer to follow-up translation-debt work.

## Task 3: Verify & push

`go vet ./... && go test ./...` → 100% green → `git push origin fix/openrouter-routing-ui-noop` → summary + PR link.
