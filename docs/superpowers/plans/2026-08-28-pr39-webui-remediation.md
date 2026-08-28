# PR #39 Review Remediation Plan

## Goal
Remediate code review findings on PR #39 (`refactor(webui): quiet UI design and trim unused locales`):
1. Fix i18n translation fallback in `store.js` when `app_lang` contains deleted/unknown locales (`zh`, `tr`, `id`) to prevent `TypeError` crashes.
2. Fix malformed duplicate table markup inside OpenRouter provider routing modal and reconcile models table in `settings.html`.
3. Add test coverage in `translations_test.go` verifying locale fallback and validity.

## Architecture & Tech Stack
- Frontend: Vanilla JavaScript + Alpine.js, Tailwind CSS / DaisyUI, HTML5 templates.
- Backend / Tests: Go test suite (`internal/webui/translations_test.go`).

---

### Task 1: Safe i18n Locale Fallback in Global Store

- **Target Files:**
  - Modify: `internal/webui/public/js/store.js`
  - Modify: `internal/webui/translations_test.go`
- **Interfaces:**
  - `t(key, params)` returns translated string or falls back to English, then raw key.
  - Initial `lang` falls back to `'en'` if stored `app_lang` is not available in `translations`.

#### Step 1: Write Test
Add `TestTranslations_StoreFallback` in `internal/webui/translations_test.go` verifying `store.js` implements defensive fallback.

#### Step 2: Run Test
Confirm test failure before implementation.

#### Step 3: Implementation
In `internal/webui/public/js/store.js`:
- Guard `this.translations[this.lang]`: use `(this.translations && this.translations[this.lang]) || (this.translations && this.translations.en) || {}`.
- Validate stored `lang` during initialization: check if `translations[savedLang]` exists, else default to `'en'`.

#### Step 4: Run Test
Verify test passes.

#### Step 5: Git Commit
`fix(webui): add safe fallback for missing or legacy i18n locales in store`

---

### Task 2: Fix Malformed Table Markup & Clean Merge Residue in `settings.html`

- **Target Files:**
  - Modify: `internal/webui/public/views/settings.html`
- **Interfaces:**
  - Valid HTML5 DOM hierarchy across all sections and modals.
  - Accessible `aria-label` attributes on all buttons and inputs.

#### Step 1: Write Test
Run automated HTML parser check on all templates.

#### Step 2: Implementation
- Remove duplicate unclosed table rows and orphaned `</table>` tags in OpenRouter provider routing section (around line 1189).
- Clean up duplicate `<th>` tags / merge conflict residue in models list table (around line 1747-1755).
- Ensure all inputs and buttons have valid labels and styling.

#### Step 3: Run HTML Validation
Verify zero syntax or tag mismatch errors.

#### Step 4: Git Commit
`fix(webui): remove duplicate table markup and fix html structure in settings view`

---

### Task 3: Full Suite Verification & PR Push

- Run `go test ./...`
- Verify all tests pass 100%.
- Push branch `pr38` to `fork/pr38`.
