# PR #105 Remediation Plan — Zen Harness / Free-Tier Gate Review Findings

**Goal:** Address every finding from the PR #105 review (14 findings, 0 critical).
**Reference:** PR #105 diff `85b0ac4..849b6cb`, review comment on the PR.
**Tech:** Go 1.27rc2, `internal/zen/*`, `internal/api/*`, `scripts/verify-zen-tls.sh`.
**Ordering:** Tasks 1–4 are independent code fixes; 5–7 are config/ops hygiene; 8 is the final gate run. Each task commits on its own.

---

## Task 1: Models fetch reaches the utls client (warn — internal/zen/client.go L190)
**Target:** `internal/zen/client.go` (`FetchModels`), `internal/zen/tls_test.go`
**Problem:** `NewClient` always sets `httpClient`, so the `TLSClient()` fallback is dead code and `/v1/models` goes out on plain Go TLS while README claims the utls disguise covers models.
**Modify:**
- In `FetchModels`, resolve the client as: `TLSClient()` when `GetTLSConfig().Enabled`, else `c.httpClient` (keeps the 15s timeout when the disguise is off).
- The shared `TLSClient()` must stay timeout-free (streaming callers own their deadlines), so the 15s bound on the utls path comes from the caller's `ctx`: keep `NewRequestWithContext(ctx, ...)` as-is and document that the enabled path relies on `ctx` for the timeout.
**Test:** `internal/zen/tls_test.go` — with TLS enabled and a custom-captured dialer, assert `FetchModels` issues its request over `TLSClient()` (spy transport via a local `httptest` server plus `zen.SetTLSConfig`).
**Steps:**
1. Write failing test.
2. Apply the client-resolution change.
3. `go test ./internal/zen/ -run Models -count=1`.
4. Commit: `fix(zen): route models fetch through the TLS disguise client`

---

## Task 2: One shared utls transport, default-transport hygiene (warn — internal/zen/passthrough.go L101, internal/zen/tls.go L99)
**Target:** `internal/zen/tls.go`, `internal/zen/passthrough.go`, `internal/zen/tls_test.go`
**Problem:** `Transport()` mints a new `http.Transport` on every call (passthrough does that per request → no keep-alive, leaked idle conns/goroutines), and the transport is zero-valued (no proxy, no dial/TLSHandshake/idle timeouts).
**Modify:**
- Cache the transport beside `tlsClient`: `var tlsTransport *http.Transport`; populate it inside `TLSClient()`; invalidate both in `SetTLSConfig`.
- Export `SharedTransport() *http.Transport` (or `Transport()` becomes the cached getter) so `passthrough.go` L101 reuses one instance.
- Build the transport from `http.DefaultTransport.(*http.Transport).Clone()`, clearing `DialTLSContext` on the clone only as needed, then override `DialTLSContext = dialOpencodeTLS`. This restores `ProxyFromEnvironment`, 30s dial timeout, 10s `TLSHandshakeTimeout`, 90s `IdleConnTimeout`.
- Never mutate the shared default transport.
**Test:** `internal/zen/tls_test.go` — assert two consecutive `Transport()` calls return the same pointer while enabled; assert `SetTLSConfig` changes it; assert `Proxy`, `IdleConnTimeout` are non-zero.
**Steps:**
1. Write failing tests (same-pointer, invalidated-on-set, timeouts populated).
2. Implement cached clone-based transport.
3. Point `passthrough.go` at the cached transport.
4. `go test ./internal/zen/ -race -count=1`.
5. Commit: `fix(zen): share one default-shaped utls transport across zen-bound clients`

---

## Task 3: Pin ALPN to http/1.1 in the replayed hello (nit+silent-failure — internal/zen/tls.go L105)
**Target:** `internal/zen/tls.go`, `internal/zen/tls_test.go`
**Problem:** The captured Bun hello offers only `http/1.1`. The fingerprint code replays whatever the file contains; a regenerated capture advertising `h2` would pass JA4 (count-only) while Go's custom-dial path has no HTTP/2 handler → every request hangs/fails.
**Modify:**
- After `fp.RawClientHello`, validate `spec.ALPNProtocols == ["http/1.1"]`; on mismatch log the same loud error as a fingerprint failure and return the error (disguise stays unavailable rather than broken).
**Test:** `internal/zen/tls_test.go` — assert the parsed spec's ALPN is exactly `["http/1.1"]`; add a unit test for the validator with a synthetic `h2` spec.
**Steps:**
1. Add validator + tests.
2. `go test ./internal/zen/ -run Spec -count=1`.
3. Commit: `fix(zen): reject a captured ClientHello whose ALPN is not http/1.1`

---

## Task 4: Injected gate tools must not surface as undeclared tool calls (warn — internal/zen/chatwire.go L152)
**Target:** `internal/zen/chatwire.go`, `internal/zen/chatwire_test.go`
**Problem:** `ensureGateTools` appends `bash`/`read` the client never declared, and the reverse-rename map only covers real renames — so a model that calls the injected tool returns a `tool_use` name the client cannot resolve (Claude Code errors on unknown tools).
**Modify:**
- Thread the set of injected names from `anthropicToChatRequest` alongside `rev` (e.g. return `(out, rev, injected map[string]bool)` or fold into one struct `toolTranslation{rename map[string]string; injected map[string]bool}`).
- In `ChatResponseToAnthropic`, `streamChatToAnthropic.handle`, and `aggregateChatStream` output, drop `tool_call`s whose upstream name is in `injected` (and the matching reverse entry stays absent).
- Keep behavior unchanged for renamed tools (`bash` → `Bash` round-trip must still work).
**Test:** `internal/zen/chatwire_test.go` — (a) non-stream response containing a `bash` tool call with no client-declared tool → no `tool_use` block emitted; (b) same in the streaming path; (c) renamed `bash`→`Bash` still round-trips.
**Steps:**
1. Write the three failing tests.
2. Implement the injected-name filter.
3. `go test ./internal/zen/ -race -count=1`.
4. Commit: `fix(zen): drop response tool calls for gate-injected tools`

---

## Task 5: Guard the Bash/bash rename collision (nit — internal/zen/chatwire.go L161)
**Target:** `internal/zen/chatwire.go`, `internal/zen/chatwire_test.go`
**Problem:** `buildToolRenames` maps case variants onto one upstream name; a client declaring both `Bash` and `bash` sends duplicate upstream tools and gets a wrong reverse lookup.
**Modify:**
- Track taken upstream names while building renames and while emitting `toolsToChat`; skip a rename when the target name already exists (tool keeps its client name; `ensureGateTools` will inject the gate name only if still absent — it is, so the gate still passes via injection).
**Test:** `internal/zen/chatwire_test.go` — `TestToolRenames_CaseCollision`: tools `Bash` + `bash` → upstream keeps both distinct, round-trip mapping correct, body still carries exactly one `bash`.
**Steps:**
1. Write failing test.
2. Apply collision guard.
3. `go test ./internal/zen/ -run Tool -count=1`.
4. Commit: `fix(zen): keep case-variant tool names distinct through the gate rename`

---

## Task 6: ObserveFreeTierGate must not swallow read errors or skip big bodies (nit — internal/zen/harness.go L250)
**Target:** `internal/zen/harness.go`, `internal/zen/harness_test.go`
**Problem:** (a) a partial read error still replaces the body with truncated bytes and the error is dropped; (b) bodies ≥1 MiB return before `WarnFreeTierGate`, so a real gate rejection is never logged.
**Modify:**
- On `err != nil && len(raw) > 0`: log the read error and hand the *original* body back (restore `resp.Body` untouched) instead of substituting bytes.
- In the `len(raw) >= limit` branch, sniff the first 1 MiB with `IsFreeTierGateError` and log the warning before returning the multi-reader.
**Test:** `internal/zen/harness_test.go` — (a) reader that yields bytes then errors → `resp.Body` is the original reader, no truncation; (b) ≥1 MiB gzip/`FreeTierError` body still logs the warning (capture via `slog` test handler or by asserting `IsFreeTierGateError` on the sniffed prefix).
**Steps:**
1. Write failing tests.
2. Apply the two branches.
3. `go test ./internal/zen/ -race -count=1`.
4. Commit: `fix(zen): preserve upstream bodies on gate-observe read errors`

---

## Task 7: Partial `zen.harness` section must not disable the disguise (nit — internal/api/server.go L250)
**Target:** `internal/api/server.go` (`applyZenHarnessConfig`), `internal/api/zen_harness_headers_test.go`
**Problem:** Only the nil-section branch applies defaults; a hand-written `{"tls":true}` section sets `Enabled=false`, sending no headers while TLS is on.
**Modify:**
- In `applyZenHarnessConfig`, treat the section as a *field-wise* overlay: `Enabled` defaults to true when the key is absent. Because `bool` cannot distinguish absent from false, decode through the raw config map (same technique as `config.Save`) or add `Enabled *bool` to `ZenHarnessConfig` with `json:"enabled"` and deref-to-true-when-nil. Preferred: `*bool`.
- Keep `config.Save`'s explicit-`false`-survives semantics (existing test `TestSave_ZenHarnessMerge` asserts it).
**Test:** `internal/api/zen_harness_headers_test.go` — `applyZenHarnessConfig(config.ZenConfig{Harness: &config.ZenHarnessConfig{TLS: true}})` → `GetHarnessConfig().Enabled == true`; explicit `enabled=false` → disabled. `internal/config/config_test.go` — JSON round-trip of absent `enabled` yields nil pointer.
**Steps:**
1. Write failing tests.
2. Switch to `*bool` + default-fill; update `DefaultConfig`, `Save` merge and public-config passthrough.
3. `go test ./internal/config/ ./internal/api/ -run Harness -count=1`.
4. Commit: `fix(config): default zen.harness.enabled when the key is absent`

---

## Task 8: Zen cache-bump replay must satisfy (or observe) the gate (nit — internal/api/cachebump_server.go L288)
**Target:** `internal/api/cachebump_server.go`, `internal/api/zen_harness_headers_test.go`
**Problem:** The replay posts the raw recorded Anthropic body to `/v1/messages` — no `stream:true`, no gate tools, no `ObserveFreeTierGate` — so free-tier replays 403/426 (the PR's documented "messages route untested" gap).
**Modify (choose A, default):**
- **A (observe):** call `zen.ObserveFreeTierGate(resp, model)` in `postBumpRequest` when the target is Zen, so gate rejections are visible instead of silent bump failures; document in README that free-tier bumps are unsupported until the messages route accepts the predicate.
- **B (normalize, only if the messages route is confirmed to share the chat predicate):** reuse the `stream:true` + `bash`/`read` normalization from `chatwire.go` for bump bodies.
- Pick A unless `verify-zen-tls.sh` shows a free-tier 200 on `/v1/messages`; note the decision in the plan file's "Outcome" section.
**Test:** `internal/api/zen_harness_headers_test.go` — bump against a 403 `FreeTierError` upstream logs the gate warning and the bump result reflects the failure.
**Steps:**
1. Write failing test (option A).
2. Wire `ObserveFreeTierGate` + model through `postBumpRequest`.
3. `go test ./internal/api/ -run Zen -count=1`.
4. Commit: `fix(api): observe the free-tier gate on zen cache-bump replays`

---

## Task 9: Verify script — always rebuild, assert ALPN fatally (warn — scripts/verify-zen-tls.sh L62, L180)
**Target:** `scripts/verify-zen-tls.sh`
**Problem:** (a) `if [ -x bin/proxy ]; then cp` can gate a stale binary; (b) ALPN mismatch is only a note while JA4 encodes ALPN count, not identity — an `h2` hello would PASS JA4 and break the dial.
**Modify:**
- Replace the `bin/proxy` reuse branch with an unconditional `go build -o "$PROXY_BIN" ./cmd/proxy`.
- Change `[ "$ALPN" = "http/1.1" ] || echo "note..."` to `fail "ALPN = $ALPN, want http/1.1"`.
**Test:** Manual: `bash -n scripts/verify-zen-tls.sh`; then a real run.
**Steps:**
1. Edit the two lines.
2. `bash -n` + `shellcheck` if available.
3. Commit: `fix(scripts): always rebuild and enforce ALPN in the zen gate`

---

## Task 10: Final gate run (definition of done)
**Verify:**
1. `gofmt -l internal/ cmd/` clean.
2. `go vet ./...` clean.
3. `go test -race ./internal/zen/ ./internal/config/ ./internal/api/` green.
4. `sudo ./scripts/verify-zen-tls.sh` → PASS (HTTP 200 + JA4 `t13d1713h1_5b57614c22b0_6a3d802a7139` + JA3 `1523504b38f0fae0d881d4b6554aac1b` + ALPN `http/1.1`), now against a freshly built binary.
5. Confirm zero `zen free-tier gate rejected request` warnings in the proxy log.
**Commit (docs):** `docs(pr105): record remediation outcomes`

---

## Outcome
All 10 tasks landed, one commit each:

| Task | Commit | Verification |
|---|---|---|
| 1 FetchModels through TLS disguise | `ed052ef` | `TestFetchModelsRoutesThroughTLSClient` green |
| 2 shared cached utls transport | `144e728` | `TestSharedTransportCached` green; `TLSClient`/`Transport()` share one cached default-shaped transport |
| 3 ALPN == http/1.1 validation | `6ebca9f` | `TestBunSpecFromCapture` + `TestValidateSpecALPN` (5 cases) green |
| 4 drop injected gate tool calls | `f7a1254` | stream + non-stream tests green; stop_reason demoted to `end_turn` when zero blocks emitted |
| 5 rename collision guard | `df8bbe2` | `TestToolRenames_CaseCollision` green (Bash/bash stay distinct, injected `read` still renamed) |
| 6 body preserve on read error | `9b4a4d2` | `TestObserveFreeTierGate_ReadErrorPreservesBody` + `TestObserveFreeTierGate_LargeBodyStillLogs` green |
| 7 partial harness default-fill | `5a402aa` | `TestZenHarnessEnabledAbsentIsNil` + `TestApplyZenHarnessConfig_PartialSectionDefaultsEnabled` green |
| 8 observe gate on bump replay | `a9d4503` | `TestSendZenBump_ObservesFreeTierGate` green; README documents free-tier bumps as unsupported |
| 9 script rebuild + ALPN fatal | `cd96554` | `bash -n` clean (shellcheck not installed) |
| 10 final gate | (this commit) | see below |

### Final gate (Task 10)
- `gofmt -l internal/ cmd/` clean; `go vet ./...` clean.
- `go test -race ./internal/zen/ ./internal/config/ ./internal/api/` all ok.
- `sudo ./scripts/verify-zen-tls.sh` → **PASS** on a freshly built binary:
  HTTP 200 (free-tier gate), ALPN `http/1.1`, JA4 `t13d1713h1_5b57614c22b0_6a3d802a7139`,
  JA3 `1523504b38f0fae0d881d4b6554aac1b`.
- Proxy log: **0** `zen free-tier gate rejected request` warnings, 0 warn/error lines.

### Deviations from the plan
- **Task 3**: utls `ClientHelloSpec` has no `ALPNProtocols` field; ALPN lives in
  `spec.Extensions` as `*utls.ALPNExtension`. Implemented `specALPN` +
  `validateSpecALPN` reading the extension instead.
- **Task 6**: instead of "leave `resp.Body` untouched" (impossible once bytes are
  consumed), `chainBody` = `io.MultiReader(bytes.NewReader(raw), original)`: the
  read bytes are preserved and the sticky read error propagates to downstream
  readers.
- **Task 7**: added a `boolPtr` helper in `internal/config` (none existed).
- **Task 8**: took option A — wire the observer through `postBumpRequest` via an
  `observe func(*http.Response)` callback and document free-tier bumps as
  unsupported in the README. Bump outcome: 403 surfaces as
  `cachebump.UpstreamError{Status: 403}` plus the gate warning, so the scheduler
  stops on the upstream 4xx.
