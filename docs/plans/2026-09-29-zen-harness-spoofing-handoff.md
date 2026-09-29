# Handoff: Zen Harness Spoofing Implementation

> Revised 2026-09-29 after a verification pass (see the plan's "Review log").
> The first draft of this handoff stated several inferences as verified facts
> and mis-described the config hook and call sites; both are corrected here.

## Execute this plan

`docs/plans/2026-09-29-zen-harness-spoofing-plan.md` — read it first. **Do T0
before anything else**: it is a go/no-go experiment that decides whether T1–T10
are worth building. Then implement T1–T10 in order.

## Key context (not in the plan)

**Problem:** Zen gateway returns `403 FreeTierError "OpenCode's free tier can only be used from within OpenCode"` for all third-party clients since ~2026-09-19/20. The proxy's zen route hits this on free-tier models.

**Observed (from the previous session, not re-verified):**
- No request secret exists in the OpenCode *source*. No `x-opencode-signature`, no HMAC. The `x-opencode-*` headers look like routing metadata.
  - Caveat: the provider plugin merges `options.headers` from the remote `GET /console/api/config` catalog, so a server-issued value would not appear in source. T0a inspects it.
- Genuine ClientHello captured from opencode 1.18.30: JA3 `1523504b38f0fae0d881d4b6554aac1b`, JA4 `t13d1713h1_5b57614c22b0_6a3d802a7139` (SNI `models.opencode.ai`, ALPN `h1`, 1505 bytes).
  - **The raw bytes (`/tmp/opencode-ch-full.bin`) and the capture script are gone** (confirmed missing). They must be re-captured on the Mac that has `/opt/homebrew/bin/opencode` (T0c) and **committed** (`.reference/`), not left in `/tmp`.
- Genuine headers: `User-Agent: opencode/<version>` (AI-SDK suffix unverified), `x-opencode-client: cli`, `x-opencode-project: global`, `x-opencode-session: ses_<26-char base62>`, `x-opencode-request: msg_<26-char base62>`.
- ID algorithm: port from `kode-ai/providers/opencode/headers.go` (`createID`) but check it against OpenCode's own `packages/opencode/src/id/id.ts` first; third-party reimplementation.

**Hypothesis, not fact — "the gate is TLS-level".** The cited elimination matrix (anomalyco/opencode#49621) reports 403 even for byte-faithful replays *from the exact Bun runtime*. A Bun replay has the genuine ClientHello, so that result argues *against* a pure TLS gate. Header/TLS work may not be sufficient; T0 settles it cheaply.

**Verified this session:**
- utls v1.8.2 builds under Go 1.27rc2. `(&utls.Fingerprinter{}).FingerprintClientHello(rawRecord)` → `UClient(..., utls.HelloCustom)` → `ApplyPreset(spec)` inside `http.Transport.DialTLSContext` reproduces the source hello's JA3 and completes an HTTP/1.1 round trip. (The package-level `tls.FingerprintClientHello` named in the first draft does not exist.)
- `net/http` bypasses `DialTLSContext` when an HTTP proxy is configured (`ProxyFromEnvironment` default), falling back to stdlib TLS. The utls transport must set `Proxy: nil`.

**Repo rules (AGENTS.md):**
- Do NOT touch TLS internals for the CloudCode/agy path (empty `tls.Config{}`, standard transport). This rule is scoped to that path — zen is separate, but the philosophy is repo-wide, hence the opt-in flag.
- Build: `go build -o bin/proxy ./cmd/proxy`
- Tests (the Makefile runs with `-race`; the new atomic config state needs it): `go test -race ./internal/zen/... ./internal/api/... ./internal/config/... ./internal/cachebump/...`
- Git hooks enforce gofmt (bypass with `--no-verify` or `SKIP_GOFMT_HOOK=1`).
- After big code changes, refresh graft: `graft build` (not installed in the cloud container).

**Existing pattern to mirror:** `internal/openrouter/harness.go` (`ApplySpoofHeaders` + default constants + config-driven). Note OpenRouter's spoof is *reactive* (`appSpoofActivated` flips on the first gate 403); the zen plan applies headers on every request — see the plan's Decisions section.

**Config hook — corrected:** `applyRouterConfig` (`internal/api/server.go:219`, called at `:211`) takes only `config.OpenRouterConfig` and is otherwise called from `handleOpenRouterConfigSave` (`management.go:1945`). It does **not** run on Zen saves. Add `applyZenConfig` and call it at init (`server.go:211`), in `handleZenConfigSave` (`management.go:2283`), and in `handleConfigSave` (`management.go:~1488`).

**`Save()` trap:** the `zen` branch in `config.Save` (`config.go:941-958`) replaces the section wholesale, so a save without `harness` erases it. T2 must fix this and test it.

**Zen-bound sites — corrected:** headers at 5 sites, HTTP client at 6.
1. `internal/zen/passthrough.go` Director (~L68) — also strip Claude-client headers and `X-Forwarded-For`.
2. `internal/zen/chatwire.go` `SendChat` (~L38) — covers both Chat-wire callers for headers.
3. `internal/api/server.go` CCR Anthropic-wire sender closure (~L1781).
4. `internal/zen/client.go` `FetchModels` (~L185); the client has its own 15s-timeout `http.Client`.
5. `internal/api/cachebump_server.go` `sendZenBump` (~L279).

Client-only extra: `SendChat` receives `http.DefaultClient` from `chatwire.go:57` (`ForwardChat`) and `server.go:1739` (CCR Chat wire). `postBumpRequest` (`cachebump_server.go:325`) is shared with Kimi/custom endpoints — give it a client parameter instead of replacing `http.DefaultClient`.

**Fallback if spoofing fails:** paid Zen key, or drive local `opencode serve` session API instead of `/zen/v1`. Both avoid circumventing the vendor's free-tier restriction; the spoof path carries terms-of-service / account-block risk that the plan asks the operator to acknowledge.

## Execution order

0. **T0: decisive experiments** (plaintext capture of a genuine request + `/console/api/config`; header-only replay; re-capture and commit ClientHello; utls spike). Stop if the spike returns 403.
1. T1: `internal/zen/harness.go` + tests (atomic snapshot, stable session ID)
2. T2: config schema + `Save()` merge fix
3. T3: `applyZenConfig` hook (3 call points)
4. T4: apply headers at 5 sites
5. T5: FreeTierError detection on all 3 response paths
6. T6: docs
7. T7: verify headers-only build
8. T8: utls dependency + `internal/zen/tls.go` (`Proxy: nil`, `HTTPClient()`)
9. T9: apply TLS transport at 6 client sites
10. T10: TLS fingerprint test (tapped listener, JA4 primary) + live verify

Run `go build` and `go test -race` after each task. Commit between tasks if desired.
