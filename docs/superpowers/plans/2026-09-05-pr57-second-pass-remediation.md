# PR #57 Second-Pass Remediation Plan

## Goal
Fix 2 risks + 1 nit from second-pass review of PR #57 (gemini-3.8-flash quota + provider-segregated WebUI).

## Architecture
- Backend Go emits `/account-limits` payload (`internal/api/management.go`).
- Alpine.js WebUI consumes it (`data-store.js`, `account-manager.js`, `models.html`).
- i18n parity enforced by Go test (`internal/webui/translations_test.go`).
- No JS unit test harness; JS changes verified via `node --check` + behavior reasoning.

## Spec
Review comment: https://github.com/gustavokch/antigravity-claude-proxy-go/pull/57#issuecomment-5554310915

---

## Task 1: i18n `invalidStatus` key + parity test (TDD)

**Files:** Modify `internal/webui/translations_test.go`, `internal/webui/public/js/translations/en.js`, `internal/webui/public/js/translations/pt.js`

**Step 1 (Red):** Add `quotaStatusKeys` list + `TestTranslations_QuotaStatusKeys` covering `readyStatus, cooldownStatus, rateLimitedStatus, disabledStatus, invalidStatus`.

**Step 2:** `go test ./internal/webui/ -run TestTranslations_QuotaStatusKeys -v` — expect FAIL (`invalidStatus` missing).

**Step 3 (Green):** Add `invalidStatus: "Invalid"` (en), `invalidStatus: "Inválido"` (pt).

**Step 4:** Re-run — PASS.

**Step 5:** `git commit -m "test(webui): enforce quota status i18n key parity"`

## Task 2: status pill handles `invalid` (models.html)

**Files:** Modify `internal/webui/public/views/models.html`

- Text chain: insert `invalid` branch before final `disabled` else → `t('invalidStatus') || 'Invalid'`.
- Class map: add `bg-red-950/60 text-red-400 border border-red-800/50` for `q.status === 'invalid'`.

**Verify:** grep confirms branch present; `go build ./...` unaffected (HTML embedded).

**Step:** `git commit -m "fix(webui): label invalid google accounts correctly in status pill"`

## Task 3: modal redaction guard (account-manager.js)

**Files:** Modify `internal/webui/public/js/components/account-manager.js`

In `openQuotaModal`: names that look like emails (contain `@`) are treated as no name → modal shows only `Redact.email(email)`. Covers google rows (`name: acc.Email`) and CC rows falling back to Email.

**Verify:** `node --check account-manager.js`.

**Step:** `git commit -m "fix(webui): redact email-like account names in quota modal"`

## Task 4: provider inference default (data-store.js)

**Files:** Modify `internal/webui/public/js/data-store.js`

`acc.provider || (acc.projectId ? 'google' : 'claudecode')` → `acc.provider || 'google'`. Google is legacy default; backend now always sends `provider`.

**Verify:** `node --check data-store.js`.

**Step:** `git commit -m "fix(webui): default legacy provider inference to google"`

---

## Verification Gate
- `go test ./...` green.
- `node --check` on touched JS files.
- Push `fix/gemini-3.8-flash-quota-webui` to fork.
