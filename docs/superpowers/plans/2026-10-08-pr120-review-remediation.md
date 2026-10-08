# PR #120 Review Remediation Plan

**Goal:** Resolve the review findings on PR #120 (`feat/zen-harness-empty-stop`).

**Architecture:** `internal/zen` only. No TLS changes. Guard lives in `ChatResponseToAnthropic` / stream `finish()`; header preservation in `harness.go`.

**Tech stack:** Go 1.27rc2, `go test ./internal/zen/`.

**Spec reference:** PR #120 body items 1-3; review comment https://github.com/gustavokch/antigravity-claude-proxy-go/pull/120#issuecomment-6052160546

## Task 1 — no-choices chat response still gets the fallback

- Modify: `internal/zen/chatwire.go` (`ChatResponseToAnthropic`)
- Test: `internal/zen/chatwire_test.go` (`TestEmptyStop_ChatNonStream`, case `no choices`)
- Consumes: `emptyStopFallbackText`. Produces: non-empty `content` on every `end_turn`.

1. Flip the `no choices` case: `wantFallback: true`, drop `wantEmpty`.
2. `go test ./internal/zen/ -run TestEmptyStop_ChatNonStream` → FAIL (content empty).
3. Move the `!hasText && stop == "end_turn"` guard out of the `if len(choices) > 0` block.
4. Re-run → PASS.
5. `git commit -m "fix(zen): fallback text for chat responses with no choices"`

## Task 2 — restore strict assertions in responses tests

- Modify: `internal/zen/responseswire_test.go` (`ReasoningMultiPartSeparator`, `AggregateResponsesStream_ReasoningSummaryIndexSeparator`)

1. Replace `len(content) < 1` with `len(content) != 2` and assert `content[1]` is the text fallback.
2. Run both tests → PASS (they exercise already-green code; mutation check: temporarily drop the guard → FAIL).
3. `git commit -m "test(zen): assert exact blocks in reasoning-only responses tests"`

## Task 3 — harness.go doc comment + single config snapshot

- Modify: `internal/zen/harness.go`
- Test: `internal/zen/harness_test.go`

1. Restore the `ApplyHarnessHeaderMap` doc comment; give `resolveHarnessIdentity` its own.
2. Change `resolveHarnessIdentity(cfg HarnessConfig)`; callers snapshot `GetHarnessConfig()` once.
3. `go test ./internal/zen/` → PASS.
4. `git commit -m "refactor(zen): snapshot harness config once per header stamp"`

## Task 4 — OMP config doc + history note

- Create: `docs/zen-omp-client.md`

1. Document `compat.replayUnsignedThinking: false` for OMP `models.yml`, header preservation rules, and the synthetic `No response text.` block living in history.
2. `git commit -m "docs(zen): OMP models.yml compat and empty-stop fallback note"`

## Final gate

`go build ./... && go vet ./internal/zen/ && go test ./...`, then `git push fork feat/zen-harness-empty-stop`.
