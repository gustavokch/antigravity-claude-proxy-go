# Handoff: Headroom Native Integration — Phase 2 (CCR)

## Context & Current State
- **Branch**: `feat/headroom-native-integration`
- **Plan File**: `docs/superpowers/plans/2026-08-27-headroom-native-integration.md`
- **Spec File**: `docs/superpowers/specs/2026-08-27-headroom-integration-design.md`
- **Status**: Phase 1 (Tasks 1–10) complete, fully tested (`go test ./... -race` passing), and committed.

### Phase 1 Commits on Branch
- `0f7ff74`: `feat(headroom): define core types, pipeline interface, and configuration`
- `f2ce7ce`: `feat(headroom): add shared tool_result text walker`
- `d51bf94`: `feat(headroom): implement SmartCrusher JSON compaction`
- `d903366`: `feat(headroom): implement CodeCompressor whitespace and repetition pruning`
- `51b30e3`: `feat(headroom): implement OutputShaper verbosity steering and effort routing`
- `d0173e7`: `feat(headroom): assemble Engine with cache-stability regression tests`
- `620e377`: `feat(api): run Headroom pipeline before every provider dispatch`
- `cba3560`: `feat(stats): track Headroom compression ratio and clamped thinking budget`
- `39a671d`: `feat(webui): add Headroom settings, bypass controls, and analytics card`
- `8ae8476`: `docs(headroom): reconcile spec with implementation and document configuration`

---

## Next Session Objectives: Phase 2 — Content-Conditioned Retrieval (CCR)

Follow `docs/superpowers/plans/2026-08-27-headroom-native-integration.md` §Phase 2:

### Task 11: CCR Chunk Store
- Files: `internal/headroom/ccr_store.go`, `internal/headroom/ccr_store_test.go`
- In-memory thread-safe LRU store (`sync.Mutex`) with capacity `maxStoreMB` (default 64MB).
- Stable content-addressed chunk IDs: `chunk_<sha256[:12]>`.
- Reject payloads larger than capacity up front (`size > maxBytes`).
- Cover LRU eviction, byte accounting, and `-race` concurrent access.

### Task 12: CCR Demotion Stage
- Files: `internal/headroom/ccr_stage.go`, `internal/headroom/ccr_test.go`
- Demote `tool_result` payloads at message index `<= FrozenPrefixIndex` and size `>= minChunkBytes` to:
  `[HEADROOM_CHUNK id="chunk_..." lines=... preview="..."]`.
- Run `CCRStage` **first** in the pipeline before `SmartCrusher`/`CodeCompressor` modify content.
- Inject `headroom_retrieve` tool definition when CCR enabled AND client provided a `tools` array (Invariant I4).
- Leave the newest `liveTurns` (default 2) messages inline.

### Task 13: Transparent Hydration Loop
- Files: `internal/api/server.go`, `internal/api/headroom_ccr_test.go`
- **Important**: Feasible only on Cloud Code and OpenRouter paths (where proxy owns the request loop). Kimi and custom reverse-proxy endpoints bypass CCR hydration.
- Intercept streamed `tool_use` with name `headroom_retrieve`, suppress terminal SSE events, fetch payload from CCR store, build synthetic `tool_result`, and re-issue continuation upstream.
- Handle cache miss on evicted chunk by returning an `is_error` tool result to model.
- Record `CCRRetrievals` count in `stats.Tracker`.

### Task 14: CCR Configuration Exposure & Final Verification
- Verify WebUI toggles and dashboard display retrieval stats.
- Update documentation in `README.md` and spec if needed.
- Run full test suite: `go test ./... -race`.

---

## Critical Invariants to Keep
- **I1 (Determinism)**: Keep non-CCR transforms position-independent and idempotent.
- **I2 (CCR prefix shift)**: Live messages (`liveTurns`) stay inline to avoid instant retrieval roundtrips.
- **I3 (Targeting)**: Only rewrite `tool_result` content; never touch top-level user prompts or thinking signatures.
- **I4 (Schema integrity)**: Only inject `headroom_retrieve` when client sent a `tools` array.
- **I5 (Opt-in)**: CCR defaults to `enabled: false`.

---

## Suggested Skills for Next Agent
1. `superpowers:executing-plans`: Execute Tasks 11–14 from `docs/superpowers/plans/2026-08-27-headroom-native-integration.md`.
2. `superpowers:test-driven-development`: Write failing unit and integration tests before implementing each stage.
3. `superpowers:verification-before-completion`: Run `-race` test suite across all packages before finishing.
