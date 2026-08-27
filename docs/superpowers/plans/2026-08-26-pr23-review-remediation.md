# Remediation Plan: PR #23 Review Fixes

**Date:** 2026-08-26
**Target PR:** #23 (`feat/app-spoof-webui`)
**Branch:** `feat/app-spoof-webui`

## Goal
Remediate findings from code review on PR #23:
1. Harden Alpine template bindings in `settings.html` against undefined `appSpoof` using optional chaining / guards.
2. Add template key reference assertions for `appSpoof` keys in `translations_test.go`.
3. Add explicit `appSpoof` payload and response assertions to `management_test.go`.

---

## Tasks

### Task 1: Add Safe Navigation for `openRouterConfig.appSpoof` in `settings.html`
- **Target Files:**
  - Modify: `internal/webui/public/views/settings.html`
- **Changes:**
  - Update `:open`, `:class`, and `x-text` bindings on `<details>` and `<summary>` elements to use optional chaining `openRouterConfig.appSpoof?.title || openRouterConfig.appSpoof?.categories`.
- **Verification:**
  - `go test ./internal/webui/...`

### Task 2: Add `TestTranslations_AppSpoofTemplateReferences` in `translations_test.go`
- **Target Files:**
  - Modify: `internal/webui/translations_test.go`
- **Changes:**
  - Add test asserting `settings.html` references `appSpoofTitle`, `appSpoofDefault`, `appSpoofCustom`, `appSpoofDesc`, `appSpoofFieldTitle`, `appSpoofFieldCategories`.
- **Verification:**
  - `go test -v ./internal/webui -run TestTranslations_AppSpoofTemplateReferences`

### Task 3: Add `appSpoof` Management Endpoint Round-Trip Test in `management_test.go`
- **Target Files:**
  - Modify: `internal/api/management_test.go`
- **Changes:**
  - Include `appSpoof: { "title": "Custom App", "categories": "cli-agent" }` in `POST /api/openrouter/config` payload.
  - Assert `appSpoof` is returned in `GET /api/openrouter/config` response with exact fields.
- **Verification:**
  - `go test -v ./internal/api -run TestManagement_OpenRouterConfig` (or relevant test runner)
