# PR #72 Review Remediation Plan

**Goal:** Close the residual prompt-cache prefix instability found in review of PR #72, and harden the tests and the verification script so the invariant can actually fail when it is violated.

**Spec reference:** `docs/superpowers/specs/2026-09-11-gemini-cache-prefix-stability.md`

**Invariant under test:** the Google `contents` array is a pure function of the client's message history. No process-local state (cache warmth, TTL, uptime) may change the conversion of an identical history.

**Branch:** `feat/gemini-cache-prefix-stability` (PR #72 head), worked in worktree `/tmp/pr72`.

---

## Task 1 — Thinking blocks must not depend on cache warmth

**Modify:** `internal/format/content.go`
**Test:** `internal/format/cacheprefix_test.go`

**Problem:** `convertContentToParts` drops a `thinking` block unless `cache.ThinkingFamily(signature)` returns the target family. After the 2h TTL expires or the proxy restarts, the lookup returns `FamilyUnknown` and the block is dropped, so the same client history converts to a shorter `contents` array and the cache prefix diverges.

**Decision:** drop only on an explicit family mismatch. `FamilyUnknown` keeps the block. This trades a possible upstream 400 on a cross-model handoff with an expired cache entry for a guaranteed cache hit on the common same-family path.

- Step 1: Write a failing test `TestThinkingBlockConversionSurvivesCacheExpiry` — convert the same history twice, once with a warm cache and once with a cache whose clock is advanced past `signatureCacheTTL`, and assert the two `contents` arrays are byte-identical.
- Step 2: `go test ./internal/format/ -run TestThinkingBlockConversionSurvivesCacheExpiry` — confirm failure.
- Step 3: Change the two family guards from `!=` to an explicit opposite-family `==` check, and comment why `FamilyUnknown` is kept.
- Step 4: Re-run the test — confirm pass. Run the whole package to confirm no regression in the Claude-family thinking tests.
- Step 5: `git commit -m "fix(format): keep thinking blocks when signature family is unknown"`

## Task 2 — The prefix invariant test must be able to fail

**Modify:** `internal/format/cacheprefix_test.go`

**Problem:** `convertPiTurn` receives one shared `SignatureCache` for turn N and turn N+1, so any divergence caused by cache state is invisible.

- Step 1: Change `TestGeminiToolLoopPrefixIsStableAcrossTurns` to convert turn N with one cache and turn N+1 with a fresh `NewSignatureCache()`.
- Step 2: Run the test — it should pass once Task 1 lands, and would have failed before it.
- Step 3: `git commit -m "test(format): convert turn N+1 with a fresh signature cache"`

## Task 3 — Test nits

**Modify:** `internal/format/format_test.go`, `internal/format/cacheprefix_test.go`

- `TestSignatureCacheExpires`: replace the hardcoded `3 * time.Hour` with `signatureCacheTTL + time.Millisecond` so the test pins the actual boundary.
- `buildPiStyleHistory`: replace `string(rune('a'+index))` with `strconv.Itoa(index)` so tool ids stay well-formed past 26 turns.
- `git commit -m "test(format): pin TTL boundary and fix tool id generation"`

## Task 4 — The verification script must assert

**Modify:** `scripts/verify-gemini-cache-prefix.sh`

**Problem:** the script prints the usage block and exits 0 whenever both turns return HTTP 200, including when `cache_read_input_tokens` is 0. It cannot gate anything.

- Step 1: Capture `cache_read_input_tokens` from the turn-2 response and exit non-zero when it is absent or 0, with a message explaining the ~32k implicit-cache floor as the likely benign cause.
- Step 2: `bash -n` the script.
- Step 3: `git commit -m "test(scripts): fail verification when turn 2 reports no cache read"`

---

## Gate

`go build ./... && go test ./...` must be fully green before the branch is pushed.
