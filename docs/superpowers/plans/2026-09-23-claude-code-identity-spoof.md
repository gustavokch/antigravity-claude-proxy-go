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

- [ ] `scripts/capture-claude-code-headers.sh token` — follow the URL, paste the code.
- [ ] `export CLAUDE_CODE_OAUTH_TOKEN=<token>`.
- [ ] `scripts/capture-claude-code-headers.sh oauth` — the target fingerprint.
- [ ] `export ANTHROPIC_API_KEY=<key>`; `scripts/capture-claude-code-headers.sh apikey` — the contrasting set.
- [ ] Confirm at least one `POST api.anthropic.com/v1/messages` per mode. If the
  security-monitor request does not appear, record that outcome rather than forcing it.

### Task A5: Baseline artifact and provenance

**Files:**
- Create: `.reference/claude-code-headers-<date>.jsonl` (from A4)
- Create: `.reference/claude-code-headers-<date>.txt`
- Modify: `docs/classifier-fallback-notes.md`

- [ ] Write the human baseline in the format of `.reference/agy-headers-mitm-20260903.txt`:
  per-auth-mode header table, body fingerprint, `Method:` line naming the commands run.
- [ ] Append a "Re-capturing the Claude Code baseline" section to
  `docs/classifier-fallback-notes.md` naming the script and the procedure.

---

## Phase B — `internal/ccidentity`

### Task B1: Profile shape

**Files:** Create `internal/ccidentity/profile.go`

- [ ] `Header{Name, Value string}`; `DynamicHeader{Name string; Value func(Identity) string}`.
- [ ] `Profile{Static []Header; Dynamic []DynamicHeader; Omit []string; SystemPrefix string; UserIDFormat string}`.
- [ ] `Identity{AccountUUID, OrganizationUUID, SessionKey, ClientVersion, Entrypoint string}`.

Deliberately not a struct of named header fields: the capture decides what exists, and named
fields would force a redesign on every surprise. `Omit` is the leak guard and is not optional.

### Task B2: Captured constants

**Files:** Create `internal/ccidentity/defaults.go`

- [ ] `ClientVersion = "2.1.280"` as one exported constant.
- [ ] `DefaultProfile()` returning values transcribed from the A5 artifacts, each with a
  provenance comment.
- [ ] Update the five stale `2.1.246` literals to the constant: `internal/claudecode/client.go:385`,
  `:451` (`Claude-Code/`), and `internal/auth/claudecode_oauth.go:629`, `:680`, `:733` (`claude-code/`).
  Version substring only — the case difference between the two families is left as it is
  unless the A5 capture settles which spelling the messages path uses.

### Task B3: Appliers

**Files:** Create `internal/ccidentity/apply.go`

- [ ] `ApplyHeaders(h http.Header, p Profile, id Identity)` — delete `Omit` first, then set
  `Static` in order, then `Dynamic`. Deletion before setting is the only way to guarantee a
  foreign client's value is gone.
- [ ] `ApplyBody(body []byte, p Profile, id Identity) ([]byte, error)` — set
  `metadata.user_id` via `UserIDFormat`; ensure system block 0 equals the rendered
  `SystemPrefix`; drop top-level fields the capture shows Claude Code never sends.
- [ ] Beta rule, stated because it is where normalization can break a working request:
  replace `anthropic-beta` with the profile's captured list, then append any inbound beta not
  already present, preserving inbound order. `ApplyAuthHeaders` still runs last so auth wins.

### Task B4: Tests

**Files:** Create `internal/ccidentity/*_test.go`

- [ ] Table-driven header test with rows read from the committed baseline: every recorded
  header present with the same value; every `Omit` name absent even when the input carried it.
- [ ] User id stable for one `Identity`, different across accounts, matching the captured format.
- [ ] Malformed JSON returns an error rather than a silent passthrough.
- [ ] Regression: a request built by `claudecode.SendMessage` never carries `Go-http-client`.

---

## Phase C — Wiring

### Task C1: Claude Code gateway

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

### Task C2: Custom endpoint

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

### Task C3: WebUI (deferrable)

**Files:** Modify `internal/webui/public/js/components/models.js`, `internal/webui/public/js/translations/en.js`, `pt.js`, `internal/webui/translations_test.go`

- [ ] Mirror the `appSpoof` field group, using `fetchOpenRouterConfig` / `saveOpenRouterConfig`
  (`models.js:527-618`) as the template. The feature is fully usable via `config.json` without this.

---

## Phase D — Verification

- [ ] `go test ./internal/ccidentity/... ./internal/claudecode/... ./internal/api/...`
- [ ] Integration test in the shape of `internal/api/openrouter_appspoof_test.go`: new
  `internal/api/ccidentity_proxy_test.go` with an `httptest.Server` upstream, asserting the
  upstream observes the baseline header set for a foreign-headed request; the custom-endpoint
  twin; and a gate test — extend the URL table at `internal/api/server_test.go:1126-1153`.
  Also assert a *real* Claude Code client's `anthropic-version` / `anthropic-beta` still pass
  through uncorrupted.
- [ ] `scripts/verify-claude-code-identity.sh`, modeled on `scripts/verify-ja4.sh` including
  its skip-loudly discipline (exit 0 with an explicit "did not run — this is NOT a pass").
- [ ] End to end by hand: drive `POST /v1/chat/completions` (the existing OpenAI shim,
  `internal/api/server.go:374`) at a model that previously gated, and record the before/after
  status in the PR description.

If a gateway still gates after normalization, the capture was incomplete: return to Phase A
with the new evidence rather than adding headers speculatively.

## Out of scope

TLS/JA4 mimicry and any TLS customization; OpenRouter; Zen and Kimi pass-through; the
classifier's Anthropic backend (`internal/api/classifier_rules.go:157`); the Cloud Code path
in `internal/format/builder.go`; and client detection of any kind.