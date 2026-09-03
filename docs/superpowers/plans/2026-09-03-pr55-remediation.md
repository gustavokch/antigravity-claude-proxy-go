# PR #55 Remediation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Remediate review findings on PR #55 (`feat/agy-1.1.25-fingerprint-and-catalog`) by sanitizing leaked user paths, adding reference caveats, formatting reference JSON, merging missing catalog changes/tests onto the PR branch, and ensuring full test suite compliance.

**Architecture:** 
- Sanitize `.reference/` documentation to remove personal local filesystem paths.
- Add superseded caveat to static string extraction doc (`.reference/agy-headers-20260903.txt`).
- Format `.reference/cloudcode-models-20260903.json`.
- Add `gemini-3.8-flash` family routing, reasoning-effort handling, alias repointing, and comprehensive tests to `internal/modelcatalog/`.
- Update `.reference/agy-current-models.txt` with agy 1.1.25 lineup.

**Tech Stack:** Go 1.27, standard library.

---

### Task 1: Add Plan Documents
- Create `docs/superpowers/plans/2026-09-03-pr55-remediation.md`
- Include `docs/superpowers/plans/2026-09-03-agy-fingerprint-models-reverification.md`

### Task 2: Sanitize Reference Documentation and Format JSON
- Sanitize `/Users/gus/.local/bin/agy` -> `$HOME/.local/bin/agy` or `agy`.
- Add superseded banner to `.reference/agy-headers-20260903.txt`.
- Format `.reference/cloudcode-models-20260903.json` using jq.
- Sanitize interface/temp paths in `.reference/fingerprint-recheck-20260903.txt`.

### Task 3: Implement Model Catalog Updates and Unit Tests (TDD)
- Target: `internal/modelcatalog/catalog.go` and `internal/modelcatalog/catalog_test.go`.
- Write failing unit tests for Gemini 3.8 Flash family (`gemini-3.8-flash`, `gemini-3.8-flash-high`, `gemini-3.8-flash-medium`, `gemini-3.8-flash-low`), legacy 3.5 alias repointing, and tiering preservation.
- Implement `applyGemini38`, routing aliases, reasoning effort mapping, and collapse rules.
- Verify tests pass.

### Task 4: Update agy-current-models.txt
- Target: `.reference/agy-current-models.txt`.
- Record the 14 models from agy 1.1.25.

### Task 5: Full Test Suite Verification
- Run `go test -race ./...`.

### Task 6: Push to Fork and Verify PR #55
- Push `feat/agy-1.1.25-fingerprint-and-catalog` to `fork`.
