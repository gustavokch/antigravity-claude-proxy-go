# PR #128 Remediation — Zen wire lists vs the live catalog

**Goal:** Every live Zen catalog id that the docs endpoint table puts on a wire the proxy speaks is routed, and a test fails when the next upstream id is not.

**Architecture:** `internal/zen/client.go` keeps three static wire lists (`AnthropicWireIDs`, `ChatWireIDs`, `ResponsesWireIDs`); `WireFor` returns `WireNone` for anything else. The drift tests diff those lists against a checked-in catalog snapshot (`internal/zen/testdata/catalog-*.json`) and, under `-tags zen_live`, against `GET /zen/v1/models`.

**Tech stack:** Go 1.27rc2, `go test`; Alpine.js webUI.

**Spec reference:** PR #128 review comment (https://github.com/gustavokch/antigravity-claude-proxy-go/pull/128#issuecomment-6257689239); issue #107; Zen docs endpoint table https://opencode.ai/docs/zen/ (read 2026-10-18).

## Evidence (2026-10-18)

- Live `GET https://opencode.ai/zen/v1/models`: 85 ids. Against the PR's edited snapshot it adds `beam-free`, `claude-haiku-5-5`, `claude-opus-5-5-backup`, `deepseek-v4.1-flash-free`, `gemini-4-argon`, `glm-5.3-flash-free`, `mistral-large-4`, `nemotron-3.5-super-vl-free`, `test`, and lacks `deepseek-v4-flash-free`, `muse-spark-1.2-contributor-free`.
- Live ids on `WireNone` after the PR: `claude-haiku-5-5`, `claude-opus-5-5-backup`, `gemini-*` (8), `jev-1.13`, `jev-1.13-free`, `mistral-large-4`, `qwen3.8-max`, `test`.
- Docs table: `claude-haiku-5-5` → `/inference/anthropic/v1/messages`; `qwen3.8-max`, `mistral-large-4` → `/inference/openai/v1/chat/completions`; `gemini-*` → `/inference/google/v1beta`; `jev-*` → `/inference/systemone`; `claude-opus-5-5-backup`, `test` absent.

## Task 1 — Refresh the snapshot and add a catalog-wide drift guard

**Files:** Create `internal/zen/testdata/catalog-2026-10-18.json` (rename of the 2026-09-30 file); Modify `internal/zen/catalog_drift_test.go`, `internal/zen/catalog_live_test.go`, `internal/config/config_test.go` (comment); Delete `internal/zen/catalog_responses_drift_test.go`.

**Consumes:** `WireFor`, the three wire lists. **Produces:** `catalogDrift(catalog []string) (unrouted, stale []string)`, `unforwardableCatalogIDs map[string]string`.

- Step 1 (red): `git mv` the snapshot to `catalog-2026-10-18.json` with the live ids and `"fetched": "2026-10-18"`; point `catalogSnapshotPath` at it. Replace `wireDrift` / `responsesDrift` and their per-family tests with one `catalogDrift`: every snapshot id resolves to a wire or is a key of `unforwardableCatalogIDs` (one reason per id), and every entry of all three wire lists is in the snapshot. Keep the exemption-honesty and disjointness tests over the unified map. Exempt `gemini-*` (Google-native wire), `jev-*` (systemone wire), `claude-opus-5-5-backup` and `test` (absent from the docs table).
- Step 2: `go test -count=1 -run 'Catalog|Exemption|Disjoint' ./internal/zen` → FAIL, unrouted `[claude-haiku-5-5 mistral-large-4 qwen3.8-max]`.

## Task 2 — Route the three documented ids

**Files:** Modify `internal/zen/client.go`, `README.md`.

- Step 3: add `claude-haiku-5-5` to `AnthropicWireIDs`; `qwen3.8-max`, `mistral-large-4` to `ChatWireIDs`; list them in the README wire subsets.
- Step 4: re-run Step 2's command → PASS.
- Step 5: `git commit -m "fix(zen): route claude-haiku-5-5, qwen3.8-max, mistral-large-4; guard every catalog id"` (Tasks 1–2 land together: Task 1 alone is a red tree).

## Task 3 — Import with nothing ticked is a no-op

**Files:** Modify `internal/webui/public/js/components/models.js`.

- Step 1: in `importZenDiscovered`, when no usable row is selected, close the dialog and return before `saveZenConfig()`.
- Step 2: no JS test harness for this component; verify in a browser against the running proxy: open the discovery dialog, click Import with nothing ticked → no POST to `/api/zen/config`, no toast.
- Step 3: `git commit -m "fix(webui): skip the Zen config save when discovery imports nothing"`.

## Verification

`gofmt -l internal`, `go vet ./...`, `go test -count=1 ./...`, `go test -tags zen_live -run WireIDsLiveCatalog ./internal/zen`; then `git push fork fix/zen-free-model-wires`.
