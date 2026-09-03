# agy 1.1.25 Fingerprint Re-verification + Model Catalog Update

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Re-verify the proxy's upstream TLS/JA4 fingerprint and hardcoded client headers against the locally installed `agy` v1.1.25, and update the Cloud Code model catalog handling for agy 1.1.25's current model list (new `gemini-3.8-flash-{high,medium,low}` family; `gemini-3.5` family and `gemini-3.6-flash-{medium,low}` no longer published).

**Architecture:** Discovery-first: capture ground truth from the local agy binary (live JA4 pcap, binary string table, raw `fetchAvailableModels` JSON) BEFORE any code change; catalog code changes branch on those findings. Zero TLS customization stays — JA4 must match or we stop and surface.

**Tech Stack:** Go 1.27, `crypto/tls` (untouched), tcpdump/tshark, `agy` CLI (at `/Users/gus/.local/bin/agy`, v1.1.25, authed), `cmd/cloudcodecheck`, jq.

**Spec:** `.reference/agy-current-baseline.txt`, `.reference/fingerprint-recheck-20260830.txt`, `AGENTS.md` fingerprint gate section, `internal/modelcatalog/catalog.go`.

**Execution first step:** copy this plan to `docs/superpowers/plans/2026-09-03-agy-fingerprint-models-reverification.md` (repo plan convention), then work from that copy.

## Global Constraints

- **JA4 contract:** `t13d131100_f57a46bbacb6_f50d94e863eb`. If agy 1.1.25's fresh live capture differs, **STOP** — surface the diff to the user. Never silently re-pin.
- **TLS:** never set `CipherSuites`, `CurvePreferences`, `NextProtos`, `MinVersion`, `MaxVersion` on the proxy transport. Empty `tls.Config{}` stays (`internal/cloudcode/client.go:113-118`).
- **User decision (dead aliases):** all dead gemini flash aliases repoint to the **gemini-3.8 family, respecting the tier** (high→3.8 High, medium→3.8 Medium, low→3.8 Low, bare family ID→3.8 High).
- **Gate per commit:** `go build ./... && go vet ./... && go test -race ./...`; live JA4 gate via `sudo ./scripts/verify-ja4.sh` at the end.
- **TDD:** catalog/header code changes land test-first in the same commit.
- **PR target:** fork `gustavokch/antigravity-claude-proxy-go`, base `main` (memory: PR Target Fork).
- **Sudo:** Tasks 1 and final gate need sudo for tcpdump (Darwin, egress iface `en0`, `ANTIGRAVITY_CAPTURE_IFACE` overrides).

---

### Task 0: Branch + pre-flight

**Files:**
- Create: `docs/superpowers/plans/2026-09-03-agy-fingerprint-models-reverification.md` (copy of this plan)

- [ ] Copy plan into repo; `git switch -c feat/agy-1.1.25-fingerprint-and-catalog`
- [ ] Preflight (all read-only): `go version`; `which tcpdump tshark`; `agy --version` (expect `1.1.25`); `agy models | tee /tmp/agy-models-20260903.txt` (expect 14 IDs: 3×3.8, 3×3.7, `gemini-3.6-flash-high`, `gemini-3.1-pro-high/low`, `claude-sonnet-4-6`, `claude-opus-4-6-thinking`, `gpt-oss-120b-medium`)
- [ ] Baseline gate: `go build ./... && go vet ./... && go test -race ./...` — expect green before any change

### Task 1: Live JA4 capture from local agy 1.1.25

**Files:**
- Create: `.reference/fingerprint-recheck-20260903.txt` (committed evidence; pcap stays in /tmp per prior-recheck precedent)

**Interfaces:** Consumes local agy binary + tcpdump/tshark. Produces: agy 1.1.25 JA4 ground truth, decision continue/stop.

- [ ] Capture (sudo needed):
```bash
sudo rm -f /tmp/agy-current-recheck-20260903.pcap
sudo tcpdump -i en0 -w /tmp/agy-current-recheck-20260903.pcap \
  "tcp port 443 and (host daily-cloudcode-pa.googleapis.com or host cloudcode-pa.googleapis.com)" >/dev/null 2>&1 &
TCPDUMP_PID=$!; sleep 1
agy --log-file=/tmp/agy-recheck-20260903.log \
  --print='Reply with exactly AGY_RECHECK_OK_20260903' --print-timeout=90s
sleep 2; sudo kill "$TCPDUMP_PID"; sleep 1
tshark -r /tmp/agy-current-recheck-20260903.pcap \
  -Y 'tls.handshake.type==1 && (tls.handshake.extensions_server_name contains "cloudcode")' \
  -T fields -e tls.handshake.extensions_server_name -e tls.handshake.extensions_alpn_str \
  -e tls.handshake.ja4 -e tls.handshake.ciphersuite -e tls.handshake.sig_hash_alg
shasum -a 256 /tmp/agy-current-recheck-20260903.pcap
```
- [ ] Assert: `agy` printed `AGY_RECHECK_OK_20260903`; JA4 == `t13d131100_f57a46bbacb6_f50d94e863eb`; ALPN absent. **Mismatch → STOP, report JA4/ciphers/sig-algs + pcap sha to user.**
- [ ] Write `.reference/fingerprint-recheck-20260903.txt` following `.reference/fingerprint-recheck-20260830.txt` structure (date, agy 1.1.25 invocation, pcap sha, tshark command, SNI/ALPN/JA4, ciphers, sig-algs, conclusion). Include the `/tmp/agy-recheck-20260903.log` head as secondary evidence.
- [ ] Commit: `docs(fingerprint): live agy 1.1.25 JA4 recheck for 2026-09-03`

### Task 2: Verify hardcoded client headers against agy 1.1.25

**Files:**
- Create: `.reference/agy-headers-20260903.txt` (committed)
- (conditional) Modify: `internal/cloudcode/client.go:30-32`, `internal/cloudcode/client_test.go`

Headers are encrypted on the wire (no keylog allowed — zero-TLS rule), so verify via binary string table (precedent: `internal/auth/agy_oauth.go` regex-over-binary). Current constants: `DefaultUserAgentVersion="2.0.3"`, `DefaultClientVersion="1.110.0"`, `GoogleAPIClient="gl-go/1.26.4 auth/0.5 google-api-go-client/0.5"`.

- [ ] Probe:
```bash
strings -a -n 4 /Users/gus/.local/bin/agy | grep -E 'antigravity|gl-go|google-api-go-client|google-byoid|auth/0\.' | head -30
grep -aoE 'gl-go/1\.[0-9]+\.[0-9]+|auth/0\.[0-9]+|google-api-go-client/0\.[0-9]+|google-byoid-sdk' /Users/gus/.local/bin/agy | sort -u
grep -aoE 'X-Client-Version' /Users/gus/.local/bin/agy | sort -u
```
- [ ] Record per-constant verdict in `.reference/agy-headers-20260903.txt`: proxy value, agy 1.1.25 evidence, confidence (`exact` / `template-only` / `unknown`). Known template in binary: `gl-go/%s auth/%s google-byoid-sdk source/%s` — exact baked literal may be unrecoverable.
- [ ] Branch: exact literal differs → do Task 3 header update; `template-only`/`unknown` → keep constants, document in evidence file. Never change the `antigravity/%s %s/%s` UA format itself.
- [ ] Commit: `docs(fingerprint): verify hardcoded client headers against agy 1.1.25`

### Task 3: (conditional) Sync header constants

Skip entirely if Task 2 found no confirmed drift.

**Files:**
- Modify: `internal/cloudcode/client.go:30-32`
- Test: `internal/cloudcode/client_test.go:46-88` (`TestFetchAvailableModelsHeadersAndDailyFallback`)

- [ ] Test-first: update expected header values in `TestFetchAvailableModelsHeadersAndDailyFallback` to the confirmed agy 1.1.25 literals. Run `go test -race ./internal/cloudcode/...` — expect FAIL.
- [ ] Update constants at `client.go:30-32`. Run again — expect PASS.
- [ ] Re-run JA4 gate `sudo ./scripts/verify-ja4.sh` — header changes must not perturb the ClientHello. Mismatch → STOP.
- [ ] Commit: `fix(fingerprint): sync client headers with agy 1.1.25 baseline`

### Task 4: Dump raw upstream catalog (ground truth for code changes)

**Files:**
- Create: `.reference/cloudcode-models-20260903.json` + `.reference/agy-models-20260903.txt` (committed)

- [ ] `go run ./cmd/cloudcodecheck -operation models -details > .reference/cloudcode-models-20260903.json` (uses `auth.Manager{}` OAuth, same as proxy). If OAuth fails: fall back to `/tmp/agy-models-20260903.txt` from Task 0 and a live `/v1/models` call (Task 6), note it in the evidence file.
- [ ] Analyze with jq:
```bash
jq -r '.models | keys[]' .reference/cloudcode-models-20260903.json | sort
jq -r '.models | to_entries[] | select(.key | test("tiered|flash-(high|medium|low)|3\\.8")) | [.key, .value.displayName, .value.maxTokens, .value.maxOutputTokens] | @tsv' \
  .reference/cloudcode-models-20260903.json
jq -r '.models | has("gemini-3.7-flash-tiered"), has("gemini-3.8-flash-tiered")' .reference/cloudcode-models-20260903.json
```
- [ ] Decision points (record in evidence file):
  - Tiered IDs present? If `gemini-3.7-flash-tiered` absent → remove `applyGemini37` synthetic path (Task 5); if present → keep it, only extend.
  - Unknown new IDs (anything beyond the 14)? → STOP, surface full ID list to user.
  - Confirm `gemini-3.5-*` absent and `displayName` values for all 14 IDs.
- [ ] Commit: `docs(catalog): dump live fetchAvailableModels JSON for 2026-09-03 (agy 1.1.25)`

### Task 5: Update catalog code for agy 1.1.25

**Files:**
- Modify: `internal/modelcatalog/catalog.go` (`routingAliases` 90-145, reasoning-effort switch in `ResolveWithRequest` ~347-397, `CleanModelIDAndName` 399-429, `applyGemini37` 481-525 + `gemini37TieredID` 147 + its call site ~215)
- Test: `internal/modelcatalog/catalog_test.go`

**Interfaces:** Consumes Task 4 findings. Produces: catalog resolving all 14 live IDs + repointed legacy aliases; `/v1/models` dedup unaffected for new family.

- [ ] **Step 1 (test-first): add `TestGemini38FlashFamily`** in `catalog_test.go` — fixture JSON with `agentModelSorts` listing `gemini-3.8-flash-{high,medium,low}` directly (no tiered ID) and `models` entries with `displayName` "Gemini 3.8 Flash (High/Medium/Low)". Assert: `Resolve("gemini-3.8-flash-high")` works; `ResolveWithRequest("gemini-3.8-flash", {reasoning_effort:"medium"})` → `gemini-3.8-flash-medium`; disabled-thinking clamp routes to `gemini-3.8-flash-low`; public `CleanModelIDAndName` collapses the family to `gemini-3.8-flash` / "Gemini 3.8 Flash". Run — expect FAIL (compile or assert).
- [ ] **Step 2: add 3.8 support** mirroring the 3.7 pattern (`applyGemini37` structure at `catalog.go:481-525`): routing aliases
```go
"gemini-3.8-flash":        "Gemini 3.8 Flash (High)",
"gemini-3.8-flash-high":   "Gemini 3.8 Flash (High)",
"gemini-3.8-flash-medium": "Gemini 3.8 Flash (Medium)",
"gemini-3.8-flash-low":    "Gemini 3.8 Flash (Low)",
```
plus reasoning-effort case for the 3.8 prefix, `CleanModelIDAndName` collapse case, and disabled-thinking clamp (mirror `isGemini37Flash` usage). If Task 4 showed a `gemini-3.8-flash-tiered` upstream ID instead of direct tiers, implement as `applyGemini38` mirroring `applyGemini37` exactly (const `gemini38TieredID`), with fallback to 3.7 tiers.
- [ ] **Step 3 (test-first): add `TestLegacyGeminiAliasRepointing`** — assert per user decision:
```go
"gemini-3.5-flash"        → Gemini 3.8 Flash (High)
"gemini-3.5-flash-high"   → Gemini 3.8 Flash (High)
"gemini-3.5-flash-medium" → Gemini 3.8 Flash (Medium)
"gemini-3.6-flash-medium" → Gemini 3.8 Flash (Medium)   // new compat alias
"gemini-3.6-flash-low"    → Gemini 3.8 Flash (Low)        // new compat alias
```
Existing `"gemini-3.6-flash": "Gemini 3.6 Flash (High)"` stays (3.6-high still live). Run — expect FAIL.
- [ ] **Step 4: repoint aliases** in `routingAliases` per the table above. Audit every remaining gemini alias target against the Task 4 `displayName` list; repoint any other dead target the same way (tier-preserving into 3.8).
- [ ] **Step 5: `applyGemini37` decision** (from Task 4): if `gemini-3.7-flash-tiered` absent upstream → delete `applyGemini37`, `gemini37TieredID`, and its call site; rewrite `TestGemini37UsesTieredUpstreamWhenPresent` (catalog_test.go:151-210) into `TestGemini37DirectTierIDs` (direct-ID fixture, no synthetic wrap). If tiered still present → keep function untouched.
- [ ] **Step 6: update stale fixtures** — `TestParseUsesAgyAgentModelOrderAndResolvesRoutingAlias` (catalog_test.go:8-51) fixture should reflect the 1.1.25 lineup; `TestClaudeRoutingAliases` unchanged (Claude targets still live).
- [ ] **Step 7: run** `go test -race ./internal/modelcatalog/...` then `go test -race ./...` — expect green.
- [ ] **Step 8: commit(s):**
  - `feat(catalog): add Gemini 3.8 Flash family routing for agy 1.1.25`
  - `feat(catalog): repoint dead gemini-3.5/3.6 tier aliases to gemini-3.8`
  - `refactor(catalog): drop gemini-3.7 synthetic tiering` (only if Step 5 removed it)

### Task 6: Live proxy smoke + refresh model evidence

**Files:**
- Modify: `.reference/agy-current-models.txt` (refresh with 1.1.25 lineup)

- [ ] Build + boot: `go build -o bin/antigravity-proxy ./cmd/proxy`; `ANTIGRAVITY_PROXY_LISTEN=127.0.0.1:8099 ANTIGRAVITY_PROXY_API_KEY=test-verification-key ./bin/antigravity-proxy &`
- [ ] `curl -sS -H "x-api-key: test-verification-key" http://127.0.0.1:8099/v1/models | jq '.data | map(.id)'` — expect the collapsed Cloud Code set including `gemini-3.8-flash` and NOT `gemini-3.5-flash-*`.
- [ ] Smoke: `POST /v1/messages` with `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"ping"}],"max_tokens":32}` — expect 200 with text (quota permitting; a quota/429 body still proves resolution succeeded — record which).
- [ ] Smoke legacy: same with `model:"gemini-3.5-flash-high"` — expect resolution to 3.8 (200 or quota error, not SelectionError).
- [ ] Kill proxy. Rewrite `.reference/agy-current-models.txt` with `Captured: 2026-09-03`, agy 1.1.25, the 14-ID list from Task 0, and the `/v1/models` public IDs.
- [ ] Commit: `docs(catalog): refresh agy-current-models.txt for agy 1.1.25`

### Task 7: Final gate + PR

- [ ] `go build ./... && go vet ./... && go test -race ./...` — green.
- [ ] `sudo ./scripts/verify-ja4.sh` — expect `Fingerprint verification SUCCESS! JA4 matches agy baseline exact.` (loud SKIP is NOT a pass; rerun with sudo).
- [ ] `./scripts/verify-fingerprint.sh` — full wrapper green.
- [ ] `git push -u origin feat/agy-1.1.25-fingerprint-and-catalog`; open PR against **gustavokch/antigravity-claude-proxy-go** `main`, title `feat: agy 1.1.25 fingerprint recheck + model catalog update`, body listing evidence files and the alias repoint decision.

## Verification Summary

| Layer | Command | Pass condition |
|---|---|---|
| Build/static/tests | `go build ./... && go vet ./... && go test -race ./...` | exit 0, no race warnings |
| agy JA4 ground truth | Task 1 tshark | JA4 == `t13d131100_f57a46bbacb6_f50d94e863eb`, ALPN absent |
| Proxy JA4 gate | `sudo ./scripts/verify-ja4.sh` | `Fingerprint verification SUCCESS!` |
| Headers | `go test -race ./internal/cloudcode/...` + Task 2 evidence | constants match agy 1.1.25 or documented unknown |
| Catalog | `go test -race ./internal/modelcatalog/...` | 3.8 family + repointed aliases + (rewritten or kept) 3.7 tests green |
| Live | `/v1/models` + `/v1/messages` smokes | 3.8 resolves end-to-end; legacy 3.5 ID resolves to 3.8 |

## Risks

1. **JA4 changed in agy 1.1.25** → Task 1 stops; user decides. Never re-pin silently.
2. **Header literals unrecoverable from stripped binary** → keep constants, document `unknown`/`template-only` in evidence. Conservative over guessing.
3. **cloudcodecheck OAuth failure** → fall back to `agy models` output + live `/v1/models`.
4. **`gemini-3.7-flash-tiered` may still exist upstream** → Task 5 Step 5 is conditional on the Task 4 jq check.
5. **Live smoke depends on account quota** — a 429/quota body still proves routing; record honestly, don't fake a pass.
6. **Scope excludes** OpenRouter/Kimi/Claude-Code allowlists (user-configured, not upstream-driven) and any `tls.Config` change.
