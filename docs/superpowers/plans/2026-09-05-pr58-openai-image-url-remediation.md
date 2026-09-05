# PR #58 Remediation Plan: OpenAI Image Translation Hardening

## Goal
Resolve 2 risks and 2 nits from code review on PR #58 (robust OpenAI image_url, input_image, native image part translation, and data URI normalization).

## Architecture
- `internal/api/openai_request.go`: `openAIContentToBlocks` and `openAIImageURLToImageBlock` convert OpenAI/Anthropic content parts into standard Anthropic Messages image blocks.
- `internal/api/openai_proxy_test.go`: table-driven tests asserting deterministic translation into Anthropic Messages format.

## Spec Reference
Review comment: https://github.com/gustavokch/antigravity-claude-proxy-go/pull/58#issuecomment-5554896971

---

## Task 1: Add Comprehensive Image Translation Tests (TDD - Red)

**Files:** `internal/api/openai_proxy_test.go`

**Step 1 (Red):** Add test cases in `TestTranslateOpenAIRequest`:
- `image_url with flat url property`
- `image_url with direct string url`
- `input_image part variant`
- `native Anthropic image block with base64 source`
- `data URI with MIME parameters and whitespace in base64`
- `invalid data URI without base64 skipped`

**Step 2:** Run `go test ./internal/api/ -run TestTranslateOpenAIRequest -v` — confirm failures on unhandled variants.

---

## Task 2: Implement Robust Image Parsing & Normalization (TDD - Green)

**Files:** `internal/api/openai_request.go`

**Step 1:** In `openAIContentToBlocks`:
- Support `partType == "image"` (pass through existing valid source or convert).
- Ensure `partType == "image_url"` and `partType == "input_image"` pass `part["image_url"]` with fallback to `part`.

**Step 2:** In `openAIImageURLToImageBlock`:
- Extract URL from string, `url` field, `image_url` string/map field, or `source` map field.
- For data URIs:
  - Extract MIME type and strip parameters after `;` (e.g., `image/png;charset=utf-8` → `image/png`).
  - Trim whitespace/newlines from base64 data.
  - If base64 data is empty, return nil.

**Step 3:** Run `go test ./internal/api/ -run TestTranslateOpenAIRequest -v` — verify all pass.

**Step 4:** Git commit: `fix(openai): robust image part extraction and data URI normalization`

---

## Verification Gate
- `go test ./...` 100% green.
- `git push origin fix/openai-image-url-translation`.
