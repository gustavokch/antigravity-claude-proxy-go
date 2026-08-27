# PR #27 Remediation Plan: Headroom Hardening & WebUI Polish

**Goal:** Resolve code review findings from PR #27: harden streaming usage map initialization, surface `tabularArrays` in WebUI with multi-locale translations, and expand edge-case tests.

**Architecture:**
- `internal/api/server.go`: Safely ensure `usage` map exists on `message_delta` events before patching token counts in OpenRouter streaming CCR hydration.
- `internal/webui/`: Expose `tabularArrays` toggle under `SmartCrusher` in `views/settings.html`, `js/components/server-config.js`, and provide translations across `en`, `zh`, `pt`, `tr`, `id` + verify in `translations_test.go`.
- `internal/headroom/tabular_test.go`: Add tests for empty strings, null values, and complex character escapes in tabular conversion.

**Tech Stack:** Go 1.27, Alpine.js, Vanilla HTML/CSS.

---

## Tasks

### Task 1: Harden `message_delta` usage map in OpenRouter streaming CCR
- **Files:** `internal/api/server.go`, `internal/api/headroom_ccr_test.go`
- **Step 1:** Write test asserting `message_delta` without initial `usage` map properly gets usage totals.
- **Step 2:** Run test to verify behavior.
- **Step 3:** Implement defensive `usage` map initialization in `forwardToOpenRouter`.
- **Step 4:** Run test to verify pass.
- **Step 5:** Git commit.

### Task 2: Surface `tabularArrays` in WebUI and update all translations
- **Files:**
  - `internal/webui/public/views/settings.html`
  - `internal/webui/public/js/components/server-config.js`
  - `internal/webui/public/js/translations/en.js`
  - `internal/webui/public/js/translations/zh.js`
  - `internal/webui/public/js/translations/pt.js`
  - `internal/webui/public/js/translations/tr.js`
  - `internal/webui/public/js/translations/id.js`
  - `internal/webui/translations_test.go`
- **Step 1:** Add `headroomTabularArrays` and `headroomTabularArraysDesc` to `translations_test.go`.
- **Step 2:** Run test to verify failure on missing keys.
- **Step 3:** Add translation keys in all 5 locale files and wire toggle in `settings.html` / `server-config.js`.
- **Step 4:** Run tests to verify all locales pass.
- **Step 5:** Git commit.

### Task 3: Expand tabular edge-case test coverage
- **Files:** `internal/headroom/tabular_test.go`
- **Step 1:** Add test cases for null values, empty string cells, escaped backslashes, and multiline values.
- **Step 2:** Run tests to confirm pass and verify coverage.
- **Step 3:** Git commit.
