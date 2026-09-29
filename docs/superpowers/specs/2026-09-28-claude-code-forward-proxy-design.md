# Claude Code Forward Proxy (Observe-Only) — Design

Status: implemented in PR #104 (`internal/mitm`). Approved section by section in conversation on 2026-09-28; where the code differs from the text below, see "As built" at the end.
Related: `2026-09-28-claude-code-cloud-feasibility.md` (§6 option (a), §7 risk 2, §9 Q1).

## 1. Goal and scope

Give the WebUI visibility into Claude Code **cloud sessions** (`claude --cloud`), whose control traffic goes straight to `api.anthropic.com` and ignores `ANTHROPIC_BASE_URL` (feasibility §9 Q1, captured 2026-09-28). The only way to see it is a forward proxy that terminates TLS with a locally installed CA.

Decisions taken:
- **Observe-only.** No request or response is rewritten. The create-time model alias and any keepalive rewriting were considered and dropped.
- **Loopback only.** The listener binds `127.0.0.1`; no LAN or container access.
- **Approach A:** a native package, `internal/mitm`, on the standard library. No `goproxy`, no `mitmproxy` sidecar. The one added dependency, `github.com/andybalholm/brotli`, only inflates `br` response bodies for the observer.
- **Accepted trade-off:** upstream calls carry a Go TLS fingerprint, not the CLI's (a native Bun binary). HTTP/1.1, header casing and header order are matched; the TLS handshake cannot be.

Out of scope: emulating cloud execution (it runs in Anthropic's container), rewriting, cancel/pin/kill, unifying with the local session registry (`/api/sessions`, feasibility §5), inference traffic (it keeps using `ANTHROPIC_BASE_URL`).

## 2. Components (`internal/mitm`)

- **`ca.go`**
  - Load or create an ECDSA P-256 CA under `<configdir>/mitm/`; the key file has mode 0600.
  - The CA is name-constrained (permitted DNS): `anthropic.com`, `claude.ai`, `claude.com`.
  - Leaf certs are minted per host, valid 24 h, cached in memory. Minting for a host outside the constraint fails.
  - Only the certificate is ever exported. The CA is never installed in a system trust store; clients trust it per process (`NODE_EXTRA_CA_CERTS`).
- **`server.go`**
  - Second listener, default `127.0.0.1:8092`, started only when `mitm.enabled` (`runtime.go` validates the address and starts it for `cmd/proxy/main.go`).
  - `CONNECT host:443` to an allowlisted host: reply 200, hijack, terminate TLS (server-side `tls.Config`, ALPN `http/1.1` only).
  - `CONNECT` to any other host: blind byte tunnel; the proxy sees the SNI only.
- **Upstream relay (`server.go`, `httpwire.go`)**
  - Requests are relayed as raw HTTP/1.1 messages (`httpwire.go`) over a connection opened with an empty `tls.Config{}`; `net/http` is not used for the wire because it sorts and canonicalizes headers. HTTP/1.1 only, no total timeout, so streams are not cut (the existing 5 min total timeout is a known defect).
  - Header names are forwarded exactly as the client sent them, by parsing the raw request head; Go's canonicalized `http.Header` is not used for the wire. Bodies pass through unmodified in both directions.
- **Observation (`observe.go`, `registry.go`)**
  - Receives a parsed summary only: method, masked route, status, and the extracted fields below. It never receives headers or bodies, so credentials cannot be logged by construction.
  - Capped registry (default 1000 entries, TTL 24 h), keyed by a 12-character hash of the session id.

## 3. Data flow

1. Per CLI process: `HTTPS_PROXY=http://127.0.0.1:8092` and `NODE_EXTRA_CA_CERTS=<ca.pem>` (adds a root; do not use `SSL_CERT_FILE`, which replaces the pool). Inference continues to use `ANTHROPIC_BASE_URL` (`:8091`).
2. `CONNECT` → 200 → hijack → TLS with a minted leaf → HTTP/1.1 keep-alive loop.
3. Per request: read the raw head, stream the body upstream, stream the response back with a flush per chunk (SSE-safe).
4. After the response headers, the observer gets a summary. For matched routes with JSON responses ≤ 1 MB the body is tee'd to extract `id`, `session_status`, `status_bucket`, `environment_kind`, `configured_model`, `connection_status`. `title`, message text and any prompt-derived field are never read or stored. SSE and other bodies are not parsed. Tee'd bodies are inflated first when `Content-Encoding` is `gzip` or `br` (the CLI accepts brotli; unknown encodings yield nothing).

Matched routes (from the 2026-09-28 capture): `POST /v1/sessions`, `GET /v1/environment_providers`, `/v1/code/sessions/{id}` and its `/events`. Everything else on the host is forwarded and counted, not parsed. Interception cannot be narrower than the host, because paths are visible only after TLS termination.

## 4. Errors and limits

- CA missing or unreadable: mitm disables itself and logs the reason; the main proxy still starts.
- Client TLS handshake failure (usually trust): log once per host with a `NODE_EXTRA_CA_CERTS` hint, then close.
- Upstream dial or TLS error: 502 JSON to the client. No fallback host.
- Upgrade / WebSocket: forward, then splice bytes both ways, recording the path only. Best-effort; the capture never observed one. `Expect: 100-continue` returns 417. `101` upgrades are spliced.
- Observer panic or parse error: recovered, record dropped, forwarding unaffected.
- Limits: 64 KB header cap, 2 min keep-alive idle timeout, no total stream timeout.

## 5. Config, API, WebUI

Config (default off, so existing configs need no migration; there is no config version field):
```json
"mitm": { "enabled": false, "listen": "127.0.0.1:8092", "registryMax": 1000, "registryTtlMinutes": 1440 }
```
- `handleConfigSave` rejects a non-loopback `listen`.
- All `mitm` fields are scalars, so the generic config merge is correct; a partial block keeps the defaults.
- `cmd/proxy/main.go` starts the listener only when enabled and shuts it down with the existing signal handler. A start failure never stops the main proxy.

API (read-only, behind `checkWebUIPassword`):
- `GET /api/mitm/status`: enabled, listen, CA fingerprint and expiry, counters (terminated, blind-tunnelled, handshake failures, upstream errors).
- `GET /api/mitm/ca.pem`: certificate only; 404 when disabled.
- `GET /api/sessions/cloud`: `id` (12-char hash), `environmentKind`, `model`, `sessionStatus`, `statusBucket`, `connectionStatus`, `createdAt`, `lastSeenAt`, `requests`, `lastRoute`.
- `GET /api/sessions/cloud/{id}`: one entry.
- No SSE; the WebUI polls (pattern: `js/components/models.js`).

WebUI: a "cloud sessions" sub-tab in settings with a status card (CA fingerprint, certificate download, copy-ready `HTTPS_PROXY=… NODE_EXTRA_CA_CERTS=…` snippet) and a sessions table polling every 5 s. The tab has an enable toggle (applies on restart).

Prerequisite, not part of this work: the WebUI password comparison is not constant-time and also accepts `?password=` (`internal/api/management.go:29-40`, feasibility §7 risk 3).

## 6. Risks

1. **Token exposure.** All `api.anthropic.com` traffic is decrypted. Mitigations: loopback bind, off by default, observer never sees headers or bodies, no body persistence, a leak test.
2. **CA trust.** Key at 0600, name-constrained, trusted per process only, never in a system store. Residual risk: a stolen key can impersonate the three in-scope domains to clients that trust it.
3. **Fingerprint and detection.** Go TLS differs from the CLI's stack. HTTP version, header case and order are matched. A pcap of proxy → `api.anthropic.com` (ALPN, JA4) is recorded beside the real CLI's and documented; it is not gated on equality because it cannot match. The AGENTS.md JA4 gate concerns agy and Cloud Code and is unaffected: this package never touches `internal/cloudcode`.
4. **Route drift.** The routes are internal. Unknown routes are forwarded untouched; the observer tolerates schema changes.
5. **Availability.** With `HTTPS_PROXY` set and the proxy down, the CLI's `api.anthropic.com` calls fail visibly — the same failure mode as the base-URL path today.

## 7. Tests

- A leaf for a host outside the name constraint fails; a leaf for `api.anthropic.com` verifies against the CA.
- Header case is preserved upstream (`X-Stainless-OS` stays as sent).
- SSE chunks arrive incrementally.
- A blind tunnel carries a non-allowlisted host.
- A leak test: captured logs contain no `Authorization` value.
- All against an `httptest` upstream via an injectable dialer and root pool.

## 8. Build order and definition of done

Each step ends with `go build -o bin/proxy ./cmd/proxy` and `go test ./...`:
1. CA generation and constraint tests.
2. `CONNECT` with a blind tunnel.
3. Terminate and pass through to an `httptest` upstream, with header-case and SSE tests.
4. Observer, registry and API.
5. Real-client check. In the cloud: `curl --proxy` with `--cacert` and a dummy key returns 401 through the MITM. On macOS (user): a real `claude --cloud` session appears in `/api/sessions/cloud`.
6. Fingerprint pcap record (proxy → Anthropic vs real CLI).
7. WebUI sub-tab.

Done when: all tests pass and the binary builds; with `mitm.enabled: false` the main proxy behaves exactly as before and the Cloud Code transport tests are untouched; a real cloud session appears in the API; logs contain no `Authorization` value.

## 9. As built

Differences between the approved design and PR #104:
- The relay, framing and observation code live in `server.go`, `httpwire.go`, `observe.go` and `registry.go`; there is no `upstream.go` or `observer.go`.
- The upstream connection is `tls.Client` with an empty `tls.Config{}` and no ALPN (`NextProtos` unset), so its ClientHello is `t13d131100_f57a46bbacb6_f50d94e863eb`, not the CLI's `t13d1713h1_...`. This is recorded in `.reference/mitm-upstream-fingerprint-20260928.txt`.
- A routed exchange is observed once: as soon as the response head is forwarded when the body is not parsed (so an open SSE stream is registered), or after the body when it is parsed (or with the route and status alone if the copy fails).
- Route names are drawn from a closed vocabulary; a non-standard request method is reported as `other`.
- The Cloud tab fetches nothing until it is opened and polls only while it is the visible tab.
