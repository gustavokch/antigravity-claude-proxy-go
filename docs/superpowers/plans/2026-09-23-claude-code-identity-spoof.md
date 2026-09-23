# Claude Code wire-identity normalization for third-party harnesses

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the proxy's outbound traffic on the Claude Code gateway and on Anthropic-shaped custom endpoints byte-indistinguishable from vanilla Claude Code at the HTTP layer, so a non-Claude-Code harness is served instead of gated.

**Architecture:** A capture harness records vanilla Claude Code's own wire traffic to `api.anthropic.com` through mitmproxy. The captured bytes become a `internal/ccidentity` profile: an ordered header list, an explicit omit list (the leak guard), a system-block prefix, and a `metadata.user_id` format. Both gateways apply that profile before sending.

**Tech Stack:** Go, mitmproxy, Podman, bash, Python 3 (`scripts/mitm_header_dump.py`).

**Spec:** this plan. Related: `docs/classifier-fallback-notes.md` (the `system[0]` billing-header finding), `internal/format/builder.go:107-132` (the inverse operation, required by Cloud Code).

## Decisions taken with the operator (2026-09-23)

| Question | Answer |
|---|---|
| Gateways in scope | `claudecode` and `custom` only. OpenRouter is already covered by `internal/openrouter/harness.go`; Zen/Kimi passthrough left alone. |
| Rewrite trigger | Always normalize, no client-fingerprint detection. On the custom endpoint, additionally gated per URL by `isAnthropicEndpoint` (`internal/api/server.go:967`). |
| TLS layer | Out of scope. Headers and JSON body only; no JA4/ClientHello mimicry. |
| Ground truth | Capture first, then implement constants from captured bytes. |
| Capture client version | Claude Code **2.1.280**. The container image was bumped from its pinned 2.1.267. |
| OAuth-account risk | Accepted. See "Risk" below. |

## Global Constraints

- Never commit or print a live credential. The addon redacts before writing; the harness scripts never echo a token.
- No constant ships without a provenance comment naming the artifact and line it was transcribed from.
- The host's own `~/.claude` is never bind-mounted into the capture container.
- `internal/format/builder.go`'s `stripBillingHeader` is not touched: the Cloud Code path needs that marker *removed*, the Anthropic paths need it *present*.
- No invented header value. If a value cannot be confirmed from the capture, it is marked `unknown` and omitted, in the style of `.reference/agy-headers-mitm-20260903.txt`.

## Risk — accepted

This changes those two gateways from "credential aggregator" to "client impersonator": the
proxy presents itself to Anthropic as the Claude Code client while carrying a harness's
traffic on the operator's own OAuth account. The operator owns the accounts in the pool and
has taken this decision deliberately. Identity is per-account-sticky, so the substitution is
internally consistent rather than random.

Do not extend this behavior to an account the operator does not own.

A technical risk is not waived: the upstream may gate on something the capture does not
show — header order, HTTP/2 vs HTTP/1.1, or a runtime-composed value. If the capture shows
the fingerprint is not reproducible from headers alone, stop and report rather than guess.

---

## Phase A — Capture the ground truth

### Task A1: Body fingerprinting and host override in the addon

**Files:**
- Modify: `scripts/mitm_header_dump.py`
- Modify: `scripts/test_mitm_header_dump.py`

- [x] `hosts_from_env()` — `$MITM_DUMP_HOSTS` comma list overrides the Cloud Code default.
- [x] `request_body_enabled()` — `$MITM_DUMP_REQUEST_BODY=1` opts in; off by default.
- [x] `redact_identifier()` — reduces `metadata.user_id` to `user_<hex>_account_<uuid>_session_<uuid>`.
- [x] `body_fingerprint()` — key tree with value types, redacted metadata, first system block; caller text never recorded.
- [x] `build_record(flow, capture_request_body=False)` — fingerprint attached only when asked.
- [x] Tests: 29 cases, `python3 -m unittest discover -s scripts -p 'test_*.py'`.

Deviation from the original sketch, deliberate: request bodies are **not** dumped even in
opt-in mode. A body *fingerprint* is recorded instead, so prompts, file contents and tool
output cannot land in a capture file. This is what the baseline needs anyway — it needs the
shape, the identity marker, and the field set, not the text.

### Task A2: Capture container on Claude Code 2.1.280

**Files:**
- Modify: `~/Git/claude-container/Containerfile` (outside this repository)

- [x] Pin bumped `@anthropic-ai/claude-code@2.1.267` → `2.1.280`.
- [x] Baked CA environment removed; the harness passes `NODE_EXTRA_CA_CERTS` / `SSL_CERT_FILE`
  at run time instead, so an ordinary container start no longer warns about a missing CA.
- [x] `podman build -t claude-box:mitm -f Containerfile .`
- [x] `podman run --rm claude-box:mitm claude --version` → `2.1.280 (Claude Code)`.
- [x] Single install confirmed: `podman run --rm --entrypoint npm claude-box:mitm ls -g --depth=0`
  lists `@anthropic-ai/claude-code@2.1.280` once, at `/usr/local/bin/claude`.

Note: `podman run --rm claude-box:mitm npm ls -g` does **not** work — `entrypoint.sh`
routes any first argument other than `claude` straight to the `claude` binary. Use
`--entrypoint npm` for anything that is not a Claude Code invocation.

### Task A3: Capture harness

**Files:**
- Create: `scripts/capture-claude-code-headers.sh`

- [x] `oauth` / `apikey` / `token` / `check` subcommands.
- [x] Fail-closed on missing podman, jq, mitmdump, Containerfile, or image.
- [x] mitmdump bound to a high port with `--allow-hosts '^api\.anthropic\.com(:\d+)?$'`,
  so non-matching hosts are refused rather than intercepted.
- [x] Waits for `mitmproxy-ca-cert.pem` before starting the container — an absent CA is the
  failure mode that produces a silently empty capture.
- [x] Two prompts: a trivial `--print` for the plain messages shape, and a Bash-tool prompt
  for the security-monitor shape.
- [x] Writes `<jsonl>` plus a `<...>.meta.txt` recording client version, hosts, record count
  and the headline requests.
- [x] Verified: `check` exits 0; unknown subcommand exits 2; missing token exits 1.

Not yet run: a live capture. It needs an interactive OAuth login (see Task A4).

### Task A4: Run the captures (operator-interactive)

- [x] `scripts/capture-claude-code-headers.sh oauth` — 7 records, 3 messages calls at 200.
- [ ] `export ANTHROPIC_API_KEY=<key>`; `scripts/capture-claude-code-headers.sh apikey`
  — blocked: no real Anthropic key available in this environment.
- [x] Security-monitor request: did NOT fire. The Bash-tool prompt was accepted rather than
  approved through a permission prompt, which is the likely cause. Recorded, not forced.

Two harness bugs were found by the first run, both fixed:

1. The addon's host filter defaults to the Cloud Code hosts, and the script never set
   `MITM_DUMP_HOSTS`, so every `api.anthropic.com` flow was dropped before a record could be
   written — indistinguishable from the container ignoring the proxy.
2. A single `nc -z` probe raced mitmdump's bind whenever the CA already existed. Now a
   bounded poll that also fails fast on a dead process.

`--quiet` was also removed from mitmdump: its flow log is the only evidence separating "the
addon filtered everything" from "the container bypassed HTTPS_PROXY", and those need
different fixes.

### Task A5: Baseline artifact and provenance

**Files:**
- Create: `.reference/claude-code-headers-20260923.jsonl` (captured)
- Create: `.reference/claude-code-headers-20260923.txt` (written)
- Create: `.reference/claude-code-headers-20260923.meta.txt` (written by the harness)
- Modify: `docs/classifier-fallback-notes.md`

- [x] Captured 7 records, all `api.anthropic.com`, three of them
  `POST /v1/messages?beta=true` returning 200.
- [x] Human baseline written, in the format of `.reference/agy-headers-mitm-20260903.txt`,
  with per-path header tables, the body fingerprint, and a `Method:` line.
- [x] Redaction confirmed: no raw token appears anywhere in the JSONL.
- [ ] Append a "Re-capturing the Claude Code baseline" section to
  `docs/classifier-fallback-notes.md` naming the script and the procedure.
- [ ] Close the gaps below, or record them as accepted.

Open gaps, from the artifact's own Gaps section:

- The API-key auth mode was never captured — no real Anthropic key was available.
- The interactive (`cc_entrypoint=cli`) session was never captured; only `--print`
  (`sdk-cli`). Re-run without `--print` to close it.
- The security-monitor request never fired, so its `x-claude-code-request-class` value is
  still `unknown`. All three captured requests were `main`.
- `cc_turn_origin` for interactive sessions, and the `cch` derivation, are `unknown`.
- `X-Stainless-OS: Linux` and `X-Stainless-Runtime-Version: v26.3.0` reflect the Linux
  capture container. A macOS host would report different values. Which to spoof is a
  decision, not a fact.

---

## Phase B — `internal/ccidentity`

### Corrected identity model (2026-09-23 capture)

The capture changed the data model this plan originally assumed. Read
`.reference/claude-code-headers-20260923.txt` before writing Phase B code; the
summary here is a pointer, not a substitute.

| Assumption | Captured reality |
|---|---|
| `metadata.user_id` = `user_<hex>_account_<uuid>_session_<uuid>` | A **string containing a JSON object**: `{"device_id":"<64 hex>","account_uuid":"","session_id":"<uuid>"}`. The flat form is wrong. |
| `system[0]` holds a constant prefix | Holds a per-request line: `cc_version=2.1.280.<3 hex>`, `cch=<5 hex, fresh each request>`, `cc_prompt_id=<uuid, per session>`, `cc_turn_origin`, and `cc_prev_req` on follow-up turns. Equality matching is impossible; the applier must **generate** it. |
| Client sent no session header | `X-Claude-Code-Session-Id` is present and stable per process. `internal/api/claudecode_proxy.go:142-146` claims otherwise and needs correcting. |
| Four headers were enough to reason about | Thirteen `x-stainless-*` / `x-app` / `x-claude-code-*` headers, `anthropic-dangerous-direct-browser-access: true`, plus the 13-entry beta list. |
| `claude-code/2.1.246` | Messages path sends `claude-cli/2.1.280 (external, sdk-cli)`. The `/api/claude_code/*` discovery path sends `claude-code/2.1.280`. Two different families. |
| Path `/v1/messages` | `/v1/messages?beta=true`. |

### Achievable target — read before implementing

The plan's original goal said "byte-indistinguishable". That is **not achievable
through `net/http`**, and both limits were verified in Go's source rather than
assumed:

1. **Header order cannot be spoofed.** `Header.writeSubset` sorts keys via
   `sortedKeyValues` (`$GOROOT/src/net/http/header.go:167`). The captured wire
   order is not sorted. Matching it needs a transport that writes the request
   itself, not `http.Transport`.
2. **`Accept-Encoding` spoofing costs transparent decompression.** Setting it
   ourselves stops `http.Transport` adding gzip, and its own comment is explicit:
   "We only attempt to uncompress the gzip stream if we were the layer that
   requested it" (`$GOROOT/src/net/http/transport.go:2995-2998`). Spoofing
   `gzip, deflate, br, zstd` therefore requires decompressing upstream responses
   in the proxy — on the SSE streaming path. zstd and br are not in the stdlib.

Header *names* can keep their exact case: assigning `req.Header["X-Stainless-OS"]`
directly preserves the key, whereas `Header.Set` canonicalizes to
`X-Stainless-Os`. The captured value is `X-Stainless-OS`, and over the captured
HTTP/1.1 transport that difference reaches the wire.

The practical target is therefore: **the same header set with the same values**,
achieving order, `Connection`, and `Accept-Encoding` only if a decision below
says to. Do not describe the result as byte-identical.

### Task B1: Profile shape — DONE

**Files:** Create `internal/ccidentity/profile.go`

- [x] `Header{Name, Value string}`; `DynamicHeader{Name string; Value func(Identity) string}`.
- [ ] `Profile{Static []Header; Dynamic []DynamicHeader; Omit []string; Path string; BillingHeader func(Identity, Turn) string; UserIDFormat func(Identity) string}`.
- [ ] `Identity{AccountUUID, DeviceID, SessionKey, ClientVersion, Entrypoint string}`.
- [ ] `Turn` carries the per-request pieces: `PrevRequestID`, and the `cch` seed.

Naming the two generated strings as functions rather than format strings is the
correction the capture forces: `metadata.user_id` is a JSON object and `system[0]`
carries three values that change per request.

### Task B2: Captured constants — DONE

**Files:** Create `internal/ccidentity/defaults.go`

- [x] `ClientVersion = "2.1.280"`, `MessagesUserAgent = "claude-cli/2.1.280 (external, sdk-cli)"`, `DiscoveryUserAgent = "claude-code/2.1.280"`.
- [ ] The 13-entry beta list, in captured order, as one ordered slice.
- [ ] `DefaultProfile()` with a provenance comment per value naming
  `.reference/claude-code-headers-20260923.txt`.
- [ ] Update the five stale `2.1.246` literals: `internal/claudecode/client.go:385`, `:451`
  are the *discovery* family (`claude-code/`, correct case, stale version);
  `internal/auth/claudecode_oauth.go:629`, `:680`, `:733` send `claude-code/2.1.246`.
  The messages-path UA is a new constant, not a correction to any existing one.
- [ ] Fix the falsified comment at `internal/api/claudecode_proxy.go:142-146`.

### Task B3: Appliers — DONE

**Files:** Create `internal/ccidentity/apply.go`

- [x] `ApplyHeaders(h http.Header, p Profile, id Identity)` — delete `Omit` first, then
  assign `Static` and `Dynamic` by **direct map assignment** to preserve the captured case
  (`h["X-Stainless-OS"] = ...`, not `h.Set`). Order within the map is Go's problem and is
  out of scope per the decision below.
- [ ] `ApplyBody(body []byte, p Profile, id Identity) ([]byte, error)` — set
  `metadata.user_id` to the stringified JSON object; replace system block 0 with the
  generated billing header; add `context_management` / `diagnostics` / `output_config` only
  if a decision says the harnesses' requests need them synthesized; drop `temperature`,
  `top_p`, `top_k`, `stop_sequences`, `tool_choice`, `service_tier`, which Claude Code does
  not send.
- [ ] Beta handling: **replace** with the captured ordered list rather than appending
  `oauth-2025-04-20`. The captured order has `claude-code-20250219` first and
  `oauth-2025-04-20` second, which append-only cannot produce. A harness beta not in the
  captured list is dropped unless a decision says to preserve it.
- [ ] Path: request `/v1/messages?beta=true`.

### Task B4: Tests — DONE (24 cases)

**Files:** Create `internal/ccidentity/*_test.go`

- [x] Table-driven header test with rows read from the committed baseline: every recorded
  header present with the same value; every `Omit` name absent even when the input carried it.
- [ ] User id stable for one `Identity`, different across accounts, matching the captured format.
- [ ] Malformed JSON returns an error rather than a silent passthrough.
- [ ] Regression: a request built by `claudecode.SendMessage` never carries `Go-http-client`.

---

## Phase C — Wiring

### Task C1: Claude Code gateway — DONE

**Files:** Modify `internal/claudecode/client.go`, `internal/claudecode/types.go`, `internal/api/claudecode_proxy.go`, `internal/api/cachebump_server.go`, `internal/config/config.go`

- [ ] `SendMessage` (`client.go:288`): replace the four-header copy block (`:296-321`) with
  `ApplyHeaders` + `ApplyBody`, then `ApplyAuthHeaders`.
- [ ] Widen the signature to carry an `Identity`. Call sites: `claudecode_proxy.go:333`, `:350`,
  `:453`, `:470`, and the CCR sender at `:311`; `cachebump_server.go:210` must thread the
  *recorded* identity through a replay, not rebuild it.
- [ ] `Identity.AccountUUID` from `AccountConfig.AccountUUID` (`types.go:20`);
  `SessionKey` from `ccExtractSessionID` (`claudecode_proxy.go:147`), already computed at `:306`.
- [ ] `claudecode.Config` gains `Identity ClaudeCodeIdentityConfig{ Disabled bool; ClientVersion string; UserAgent string }`
  — a *disable* flag, so the zero value means normalize. Default it at `config.go:426-433`.

### Task C2: Custom endpoint — DONE

**Files:** Modify `internal/api/server.go`, `internal/config/config.go`

- [ ] Non-CCR `Rewrite` (`:1073`): apply the profile after the auth block (`:1089-1104`) and
  rewrite the body already replaced at `:1086`.
- [ ] CCR sender (`:1022-1045`): same treatment; it builds a fresh five-header request.
- [ ] Gate on `isAnthropicEndpoint(endpoint.URL)` per endpoint. Packy-style
  `https://api.packyapi.com/v1/messages` qualifies. A non-Anthropic-shaped endpoint keeps its
  headers untouched — Claude Code identity would be a lie about a protocol it does not speak.
- [ ] `EndpointConfig` (`config.go:20-23`) gains the same disable-flag `Identity` field.
- [ ] Note in code that `net/http` canonicalizes header names (`X-Stainless-OS` →
  `X-Stainless-Os`); harmless over HTTP/2, as the existing comment at `:1059-1064` says.

### Task C3: WebUI — DONE

**Files:** Modify `internal/webui/public/js/components/models.js`, `internal/webui/public/views/settings.html`, `internal/webui/public/js/translations/en.js`, `pt.js`, `internal/webui/translations_test.go`

- [x] Mirror the `appSpoof` field group, using `fetchOpenRouterConfig` / `saveOpenRouterConfig`
  (`models.js:527-618`) as the template.

Three traps, each now pinned by a test rather than a comment:

1. `saveCCConfig` sends an explicit field list, not the whole object, so a section
   not named there is dropped on every save.
2. The flag is a *disable* flag. A panel binding an "enabled" checkbox would
   invert the feature for every operator who never opens it.
3. `loadCCConfig` spreads the server config over the local object, which replaces
   `identity` with `undefined` for any config saved before the panel existed.

Verified end to end: `entrypoint=cli` and `stainlessOs=Darwin` persist through
`/api/claudecode/config` and reach the wire as
`User-Agent: claude-cli/2.1.280 (external, cli)`, `X-Stainless-OS: Darwin`, and
`cc_entrypoint=cli` in the generated `system[0]`.

---

## Phase D — Verification

- [x] `go test ./internal/ccidentity/... ./internal/claudecode/... ./internal/api/...`
- [x] Integration test in the shape of `internal/api/openrouter_appspoof_test.go`: new
  `internal/api/ccidentity_proxy_test.go` with an `httptest.Server` upstream, asserting the
  upstream observes the baseline header set for a foreign-headed request; the custom-endpoint
  twin; and a gate test — extend the URL table at `internal/api/server_test.go:1126-1153`.
  Also assert a *real* Claude Code client's `anthropic-version` / `anthropic-beta` still pass
  through uncorrupted.
- [x] `scripts/verify-claude-code-identity.sh`, modeled on `scripts/verify-ja4.sh` including
  its skip-loudly discipline (exit 0 with an explicit "did not run — this is NOT a pass").
- [x] End to end by hand: drive `POST /v1/chat/completions` (the existing OpenAI shim,
  `internal/api/server.go:374`) at a model that previously gated, and record the before/after
  status in the PR description.

If a gateway still gates after normalization, the capture was incomplete: return to Phase A
with the new evidence rather than adding headers speculatively.

### D3 result — the gate uses mitmdump, not tcpdump alone

`scripts/verify-claude-code-identity.sh` plus `scripts/diff_claude_code_identity.py`.
The plan said "modeled on `scripts/verify-ja4.sh`", and the skip-loudly discipline is
taken from it unchanged. The capture mechanism could not be: JA4 reads the plaintext
ClientHello, but request headers live inside the encrypted record layer, so tcpdump
cannot see them. mitmdump terminates TLS and the existing `scripts/mitm_header_dump.py`
addon already records exactly the shape the diff needs.

The gate needs no credential — it configures a stub OAuth account, and mitmdump records
the outbound request before the upstream rejects it.

Falsified rather than assumed: changing `X-Stainless-Timeout` from `600` to `601` in
`internal/ccidentity/defaults.go` makes the gate exit 1 naming that header; a clean tree
exits 0 with `21 headers over HTTP/2.0`.

What it detects is the PROXY drifting from the capture. It cannot detect CLAUDE CODE
drifting from the capture — both sides of that diff are frozen. A new Claude Code version
changing its own fingerprint needs a fresh `scripts/capture-claude-code-headers.sh oauth`.

### D4 result — no gate was reachable, so there is no before/after delta

Run 2026-09-23 against `api.anthropic.com` on the operator's own OAuth account
(`ghlsem@gmail.com`), model `claude-haiku-4-5`, one foreign-headed
`POST /v1/chat/completions` (`User-Agent: foreign-harness/1.0`, no Anthropic headers):

| Run | Binary | Status |
|---|---|---|
| Before | `46f1a87`, the live pre-feature binary and an ancestor of this branch | 200 |
| After | `27c59b4`, this branch, isolated instance on port 8099 | 200 |

**The premise did not hold.** Nothing reachable gates a foreign harness today — the
pre-feature binary already serves 200 on the Claude Code gateway. The one endpoint
recorded as gating is packy's CC group, and it rejects genuine Claude Code from this
machine too, so it cannot separate our fingerprint from an IP or account flag.

200/200 is therefore **no regression, not "ungated"**. Do not describe D4 as proof the
gate was defeated.

Because 200/200 on its own proves nothing about what went on the wire, the wire was
observed separately, against a local recorder upstream. Normalization does apply: path
`/v1/messages?beta=true`, 19/19 baseline headers at baseline values with exact case
preserved over HTTP/1.1, `anthropic-beta` byte-identical to the captured 13-entry list,
the client's `x-app: foreign-app` and `anthropic-beta: some-harness-beta` both overwritten,
`temperature`/`top_p` stripped, `metadata.user_id` in the captured JSON-object form, and a
generated `system[0]` billing header.

This also answers open question 1 from the handoff: dropping a harness beta cost nothing
on this path. The append-inbound-extras rule is not needed on the evidence available.

### Settled question — account_uuid is always empty

`metadata.user_id.account_uuid` went out **non-empty** on the pooled Claude Code path,
carrying the real account's UUID, while the capture recorded it **empty in 3/3 requests**.
The artifact marked "whether that is specific to this token or general to the OAuth path"
as `unknown`, so the operator's decision (2026-09-23) was to settle it with a second
capture rather than change behavior against an unvaried sample.

The second capture ran the same day on an independent OAuth credential — a different
`Authorization` hash — and sent `account_uuid` empty in all three of its messages
requests. **Empty in 6 of 6 across two accounts.** The field is general to the OAuth path,
so populating it was a value no real client emits.

Fixed in `internal/ccidentity/apply.go`: `metadataUserID.AccountUUID` is now always `""`.
Per-account identity is not lost — `deviceID` still derives from `id.AccountUUID`, so two
accounts present different `device_id` values, and a test pins that.

The drift gate now compares `metadata.user_id` for equality instead of exempting the
field.

Worth recording separately: the two captures are otherwise **identical**. Running
`scripts/diff_claude_code_identity.py` with one as baseline and the other as observed
reports no drift across 23 headers, the path, `metadata.user_id` and `system[0]` — which
validates the baseline and the differ at the same time.


## Out of scope

TLS/JA4 mimicry and any TLS customization; OpenRouter; Zen and Kimi pass-through; the
classifier's Anthropic backend (`internal/api/classifier_rules.go:157`); the Cloud Code path
in `internal/format/builder.go`; and client detection of any kind.
---

## Implementation status (2026-09-23)

Phases A, B and C1/C2 are done and committed on `feat/claude-code-identity-spoof`.
The identity model was corrected against the capture before any of it was written;
see the corrected table in Phase B.

### Two canonicalisation bugs, same root cause

Both were found by tests, not by reading, and both were SILENT failures — the
request still succeeded, which is why they matter.

1. `ApplyAuthHeaders` read the beta header with `http.Header.Get`, which
   canonicalises to `Anthropic-Beta`. It therefore missed the lowercase
   `anthropic-beta` that normalization stores, then `Set` a SECOND key, putting
   two conflicting beta headers on the wire.
2. `ccidentity.ApplyHeaders` assigned the captured spelling without deleting the
   canonical key net/http had stored, so a client's `x-app` (stored `X-App`)
   survived alongside the injected `x-app`, and the upstream read the client's
   value.

The rule both fixes encode: **any header this code touches must be looked up and
deleted case-insensitively, because net/http canonicalises every name it parses.**
The captured wire names are deliberately not canonical (`X-Stainless-OS`,
lowercase `anthropic-*`), so exact-case storage is correct and canonical lookups
are the bug.

### Deliberate gaps, all recorded rather than hidden

- `Accept-Encoding` is cleared, not spoofed, per the fidelity decision. Go keeps
  adding gzip and decompressing transparently.
- Header ORDER is not reproduced: net/http sorts keys before writing.
- `context_management`, `diagnostics` and `output_config` are not synthesised.
  The capture shows Claude Code sending them and this implementation cannot
  invent their contents, so their absence is a known difference.
- `x-claude-code-prev-tool-durations` is not sent. It appears only after a tool
  has run, which a request alone cannot tell us.
- `cc_entrypoint` defaults to `sdk-cli`, the only value ever captured. `cli` is
  configurable but unverified.
- A custom endpoint must configure its `/messages` path for the gate to pass. A
  bare host does not match `isAnthropicEndpoint`.

### Still outstanding

- The `cli` entrypoint capture. Six automated attempts failed; the folder-trust
  dialog defaults its cursor to "No, exit", a second renderer prompt follows it,
  and answering that restarts the UI and re-prompts trust. `interactive` mode is
  now human-driven and documents this.
- The API-key auth mode was never captured.
- A second OAuth capture on a different account ran on 2026-09-23 and settled the
  `account_uuid` question; see "Settled question" in Phase D.

Every phase of this plan is complete. The remaining unknowns are the `cli`
entrypoint and the API-key auth mode, both recorded above.
