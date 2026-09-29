# Zen Harness Spoofing — Implementation Plan

## Context

Since ~2026-09-17/19 the Zen gateway rejects free-tier requests from
non-OpenCode clients with `403 FreeTierError`:

```
{"type":"error","error":{"type":"FreeTierError","message":"Error from provider (Console): OpenCode's free tier can only be used from within OpenCode"}}
```

A variant exists: `"OpenCode 1.17.0 or newer is required to use the free tier"`.
The proxy surfaces this through `chatErrorToAnthropic` (`internal/zen/chatwire.go:442`),
which prefixes the upstream message with `"Zen: "`.

Ground truth for the genuine client header set, from two sources:

1. `packages/core/src/plugin/provider/opencode.ts` (official source, commit
   f632c6c): the plugin fetches the provider catalog from the remote Console
   (`GET https://opencode.ai/console/api/config`, bearer-authenticated) and
   merges `item.options?.headers` into `provider.request.headers`. The headers
   are therefore server-configurable, but the defaults below are what the
   genuine client sends.
2. `kode-ai/providers/opencode/headers.go` (public reimplementation, MIT):
   the exact header names, defaults, and ID-generation algorithm.

Genuine per-request header set:

| Header | Value | Source |
|---|---|---|
| `User-Agent` | `opencode/<version>` — AI SDK appends ` ai-sdk/provider-utils/<v> runtime/bun/<v>` | `OPENCODE_VERSION` env, default `local` |
| `x-opencode-client` | `cli` (or `desktop`) | `OPENCODE_CLIENT` env, default `cli` |
| `x-opencode-project` | `global` (or git-root hash when inside a repo) | git resolution, default `global` |
| `x-opencode-session` | `ses_<26-char base62>` | generated per-request |
| `x-opencode-request` | `msg_<26-char base62>` | generated per-request |

Session/request ID format (from `createID` in headers.go): 12 hex chars from
the millisecond timestamp (bit-flipped for sessions so IDs sort descending),
followed by 14 random base62 chars (`0-9A-Za-z`). Total 26 chars after the
`ses_`/`msg_` prefix.

The proxy today sends none of these headers:

- `internal/zen/passthrough.go:56-80` (ReverseProxy Director, Anthropic wire,
  CCR-disabled) forwards the incoming client's UA (e.g. `claude-cli/...`).
- `internal/zen/chatwire.go:34-43` (`SendChat`, Chat wire) and
  `internal/api/server.go:1775-1795` (CCR sender closure) send Go's default
  `Go-http-client/1.1`.
- `internal/zen/client.go:178-209` (`FetchModels`) sends only Bearer.
- `internal/api/cachebump_server.go:279-291` (`sendZenBump`) replays only
  allowlisted headers (`anthropic-version`, `anthropic-beta`).

## TLS Fingerprint Ground Truth

Captured from opencode 1.18.30 (`/opt/homebrew/bin/opencode`) on 2026-09-29:

| Field | Value |
|---|---|
| JA4 | `t13d1713h1_5b57614c22b0_6a3d802a7139` |
| JA3 | `1523504b38f0fae0d881d4b6554aac1b` |
| TLS version | 1.3 |
| SNI | `models.opencode.ai` |
| Ciphers | 17 |
| Extensions | 13 |
| ALPN | `h1` |
| Raw ClientHello | 1505 bytes (saved at `/tmp/opencode-ch-full.bin`) |

The ClientHello is Bun's TLS stack (BoringSSL-based). Go's default `crypto/tls`
produces a different JA3/JA4. utls (`github.com/refraction-networking/utls`
v1.8.2) can replicate any ClientHello via `FingerprintClientHello` +
`ApplyPreset`.

## Caveat (read first)

Public evidence (anomalyco/opencode#49621 elimination matrix): byte-faithful
HTTP replay with the genuine UA + all `x-opencode-*` headers, over HTTP/1.1 and
HTTP/2, still receives 403 — even from the exact Bun 1.3.14 runtime.

**No request secret exists in OpenCode source** (verified by subagent exploration
of github.com/anomalyco/opencode, 2026-09-29). No `x-opencode-signature`
header, no HMAC, no per-request secret generation. The only signatures in the
codebase are AWS SigV4 (Bedrock auth) and AI SDK tool-approval HMAC
(anti-forgery, unrelated to Zen). The `x-opencode-*` headers are routing
metadata, not authentication.

Gate evolution (3 phases):

1. Pre-Sep 2026: UA-only check (`opencode/<version>`).
2. Sep 17, 2026: version-checked UA (release builds only, no git suffix).
3. Sep 19/20, 2026 (current): TLS fingerprint — all third-party stacks fail.

The discriminator is TLS-level (Bun's BoringSSL ClientHello). This plan adds
both layers:

- **HTTP headers** (T1–T5): cheap, correct, matches the repo's `ApplySpoofHeaders` pattern.
- **TLS fingerprint** (T8–T10): utls → Bun ClientHelloSpec from the captured bytes above.
  Conflicts with the repo's "normal Go TLS client" philosophy. The AGENTS.md TLS
  rule is scoped to the CloudCode/agy disguise, but the philosophy is repo-wide.
  Gated behind a config flag (`zen.harness.tlsEnabled`, default `false`) so the
  operator can opt in after testing.
- **Paid Zen key.** Keyed access may bypass the free-tier gate entirely. Test
  in parallel with this plan.
- **Alternative:** drive local `opencode serve` session API
  (`POST /session/{id}/message`) instead of `/zen/v1` directly. This is the
  only currently-known working workaround for third-party clients.

## Technical Specification

### 1. New `internal/zen/harness.go`

Mirror `internal/openrouter/harness.go`.

- Constants (same names as kode-ai):
  ```go
  const (
      HeaderProject = "x-opencode-project"
      HeaderSession = "x-opencode-session"
      HeaderRequest = "x-opencode-request"
      HeaderClient  = "x-opencode-client"
      HeaderUA      = "User-Agent"

      DefaultVersion = "1.18.31"
      DefaultClient  = "cli"
      DefaultProject = "global"
  )
  ```
- `type HarnessConfig struct { Enabled bool; Version, Client, Project string }`.
- Package-level `var harnessCfg HarnessConfig` + `SetHarnessConfig(cfg HarnessConfig)`
  — no signature changes at call sites; mirrors `openrouter.DefaultRouter.SetConfig`.
- `ApplyHarnessHeaderMap(hdr http.Header)`:
  - No-op when `harnessCfg.Enabled == false`.
  - Trims inputs; applies defaults when empty.
  - Sets `User-Agent: opencode/<version>`, `x-opencode-client`, `x-opencode-project`.
  - Generates per-request `x-opencode-session` (`ses_<id>`) and
    `x-opencode-request` (`msg_<id>`) via `NewSessionID()`/`NewRequestID()`.
- `ApplyHarnessHeaders(req *http.Request)` — nil-guard, delegates to
  `ApplyHarnessHeaderMap`.
- ID generation (`NewSessionID`, `NewRequestID`): port `createID` from
  headers.go — 12 hex chars from `time.Now().UnixMilli()` (bit-flipped for
  sessions), + 14 random base62 chars. Prefix `ses_` / `msg_`.
- `IsFreeTierGateError(body []byte) bool` — case-insensitive match on
  `"free tier can only be used from within opencode"`, `"freetiererror"`,
  `"1.17.0 or newer is required"`.
- Unit tests mirroring `internal/openrouter/harness_test.go`: defaults,
  overrides, whitespace, nil req, disabled no-op, gate-error variants, ID
  format (prefix + 26-char base62 body).

### 2. Config (`internal/config/config.go`)

- New struct:
  ```go
  type ZenHarnessConfig struct {
      Version  string `json:"version,omitempty"`
      Client   string `json:"client,omitempty"`
      Project  string `json:"project,omitempty"`
      Enabled  bool   `json:"enabled"`
  }
  ```
- `ZenConfig.Harness *ZenHarnessConfig` (`json:"harness,omitempty"`).
- `DefaultConfig()`: `Harness: &ZenHarnessConfig{Enabled: true, Version: "1.18.31", Client: "cli", Project: "global"}`
  — disguise-by-default, consistent with the repo's purpose.
- `Save()`: merge per the unmentioned-sections rule (commit f632d01). When the
  incoming `harness` section is present, merge individual fields (empty string
  = keep existing); when absent, keep the existing section. No secrets in this
  section.
- `GetPublicConfig()`: pass through, no redaction needed.

### 3. Config hook

Call `zen.SetHarnessConfig(...)` from `applyRouterConfig`
(`internal/api/server.go:211`), which is called at server init and on config
save (`internal/api/management.go:1945`). One hook covers both paths. Convert
`config.ZenHarnessConfig` → `zen.HarnessConfig` there.

### 4. Apply at all 5 zen-bound call sites

1. `internal/zen/passthrough.go` Director (~L68) — after auth/version/beta,
   call `ApplyHarnessHeaders(req)` (same package, direct call).
2. `internal/zen/chatwire.go` `SendChat` (~L38) — after Content-Type/Authorization,
   call `ApplyHarnessHeaders(httpReq)`.
3. `internal/api/server.go` CCR sender closure (~L1781) — after auth/version/beta,
   call `zen.ApplyHarnessHeaders(httpReq)`.
4. `internal/zen/client.go` `FetchModels` (~L185) — after Bearer, call
   `ApplyHarnessHeaders(req)` (consistency; endpoint is unauthenticated today).
5. `internal/api/cachebump_server.go` `sendZenBump` (~L279) — extend the
   `applyAuth` callback to call `zen.ApplyHarnessHeaderMap(hdr)` after setting
   Authorization. `postBumpRequest` stays shared with other routes.

### 5. Observability

In `chatErrorToAnthropic` (`internal/zen/chatwire.go:412`): when
`IsFreeTierGateError(raw)`, emit a distinct `slog.Warn` with model + hint
("free-tier gate hit; see zen harness spoofing plan"). The error body passes
through to the client unchanged.

### 6. WebUI

Deferred. Defaults are the disguise; overrides are editable via `config.json`.
Add panel controls later if the operator wants UI.

### 7. TLS fingerprint spoofing (`internal/zen/tls.go`)

**Dependency:** `github.com/refraction-networking/utls` v1.8.2 (Jan 2026).
No known CVEs at this version (CVE-2026-26994 affects <= 1.6.7).

**Approach:** utls replaces only the ClientHello bytes; the handshake itself
still uses `crypto/tls`. We generate a `ClientHelloSpec` from the captured
opencode ClientHello bytes, then use `utls.UClient` with `HelloCustom` +
`ApplyPreset(spec)` in a custom `DialTLSContext`.

**New file `internal/zen/tls.go`:**

- Embed the captured 1505-byte ClientHello as a `[]byte` constant (or load from
  a file embedded via `embed.FS` — prefer embed for cleanliness).
- `func init() ` — call `tls.FingerprintClientHello(capturedBytes)` to generate
  the `utls.ClientHelloSpec` at startup. Store in package-level `var bunSpec`.
- `type ZenTLSConfig struct { Enabled bool }`.
- `func (c ZenTLSConfig) Transport() *http.Transport` — returns an
  `http.Transport` with a custom `DialTLSContext` that:
  1. Dials TCP normally.
  2. Wraps with `utls.UClient(conn, &utls.Config{...}, utls.HelloCustom)`.
  3. Calls `uconn.ApplyPreset(bunSpec)`.
  4. Performs `uconn.Handshake()`.
  5. Returns the connection.
- When `Enabled == false`, return `nil` (callers use `http.DefaultTransport`).
- The `utls.Config` should mirror what the genuine client uses: TLS 1.2+,
  SNI from the request, ALPN `h1` (the capture shows ALPN `h1` only).

**Config:** `ZenHarnessConfig.TLS bool` (`json:"tls,omitempty"`).
Default `false` — operator opts in after testing.

**Apply at call sites:** replace `http.DefaultClient` with a shared
`*http.Client` whose `Transport` is `zenTLSConfig.Transport()` for all
zen-bound requests:
- `internal/zen/chatwire.go` `SendChat` — uses `http.DefaultClient` → replace.
- `internal/zen/passthrough.go` — ReverseProxy uses its own transport; set
  `proxy.Transport = zenTLSConfig.Transport()`.
- `internal/api/server.go` CCR sender — uses `http.DefaultClient.Do` → replace.
- `internal/zen/client.go` `FetchModels` — uses `c.httpClient` → replace.
- `internal/api/cachebump_server.go` `postBumpRequest` — uses
  `http.DefaultClient.Do` → replace.

**Unit test:** `internal/zen/tls_test.go` — start a local TLS server, dial
via the custom transport, assert the server sees a ClientHello matching the
expected JA3 (`1523504b38f0fae0d881d4b6554aac1b`).

### 8. Docs

- README Zen section: spoof defaults, config overrides, the TLS caveat, the
  paid-key alternative.
- `antigravity-go-proxy.env.example`: note that `OPENCODE_VERSION` /
  `OPENCODE_CLIENT` can override the spoof identity (mirroring the genuine
  client's env behavior).

## Task List

| # | Task | Files | Acceptance |
|---|------|-------|------------|
| T1 | `internal/zen/harness.go`: constants, `HarnessConfig`, `SetHarnessConfig`, `ApplyHarnessHeaderMap`, `ApplyHarnessHeaders`, `NewSessionID`/`NewRequestID`, `IsFreeTierGateError` + unit tests | `internal/zen/harness.go`, `internal/zen/harness_test.go` | `go test ./internal/zen/...` green: defaults, overrides, whitespace, nil req, disabled no-op, gate-error variants, ID format |
| T2 | Config schema: `ZenHarnessConfig`, `ZenConfig.Harness`, defaults, `Save()` merge, `GetPublicConfig()` passthrough | `internal/config/config.go` | Round-trip test: defaults present; save with partial harness section merges; public view passes through |
| T3 | Wire `zen.SetHarnessConfig` into `applyRouterConfig` | `internal/api/server.go` | Config reload pushes harness config into zen package |
| T4 | Apply spoof headers at all 5 call sites | `internal/zen/passthrough.go`, `internal/zen/chatwire.go`, `internal/zen/client.go`, `internal/api/server.go`, `internal/api/cachebump_server.go` | `httptest` upstream sees UA `opencode/1.18.31` + all 4 `x-opencode-*` headers on every path (passthrough, chatwire, CCR sender, bump replay) |
| T5 | `IsFreeTierGateError` detection + distinct warn in `chatErrorToAnthropic` | `internal/zen/chatwire.go` | Free-tier 403 logs a distinct warning; error body unchanged |
| T6 | Docs: README Zen section, env example note | `README.md`, `antigravity-go-proxy.env.example` | Spoof defaults, config overrides, TLS caveat, paid-key alternative documented |
| T7 | Full verify (headers only) | — | `go build`, `go vet ./...`, `go test ./internal/zen/... ./internal/api/... ./internal/config/... ./internal/cachebump/...` green; live capture shows spoofed headers on the wire; free-tier model re-tested (expect 403 per caveat) and paid key tested if available |
| T8 | Add utls dependency + `internal/zen/tls.go`: embed captured ClientHello, `FingerprintClientHello` → `bunSpec`, `ZenTLSConfig.Transport()` with custom `DialTLSContext` | `go.mod`, `internal/zen/tls.go` | `go mod tidy` clean; `bunSpec` generated without error |
| T9 | Apply custom TLS transport at all 5 zen call sites | Same as T4 | Each zen-bound client uses the utls transport when enabled |
| T10 | TLS fingerprint test + live verify | `internal/zen/tls_test.go` | Local TLS server sees JA3 `1523504b38f0fae0d881d4b6554aac1b`; live capture shows matching JA4 `t13d1713h1_5b57614c22b0_6a3d802a7139` |

## Risks / Open Items

- **Header spoofing may not be sufficient.** Public evidence shows byte-faithful
  HTTP replay still fails. The gate may be TLS-fingerprint-based (Bun runtime)
  or use an embedded secret. Mitigation: implement both layers, test live.
- **TLS fingerprint may not be sufficient either.** The gate may use an
  embedded request secret (not replicable at any layer). If both header + TLS
  spoofing fail, the only remaining option is a paid key.
- **utls ClientHelloSpec drift.** The captured spec is from opencode 1.18.30.
  If OpenCode updates their Bun version or TLS settings, the spec becomes
  stale. The proxy will still connect (utls sends the old spec) but the
  fingerprint will not match the current genuine client. Re-capture after
  OpenCode updates.
- **utls maintenance burden.** utls forks crypto/tls and may lag behind Go
  updates. The repo uses Go 1.27rc2; utls v1.8.2 supports Go 1.21+. Monitor
  for compatibility issues.
- **Config merge complexity.** The `Save()` merge for the new `harness` section
  must follow the unmentioned-sections rule (commit f632d01). Partial sections
  merge field-by-field; absent sections are preserved.
- **WebUI not updated.** Operators editing `config.json` directly get the
  overrides; the WebUI panel does not expose them yet. Documented as deferred.
- **Philosophy conflict.** TLS spoofing via utls contradicts the repo's
  "normal Go TLS client" principle. Gated behind `zen.harness.tlsEnabled`
  (default `false`) so the operator can opt in consciously.
