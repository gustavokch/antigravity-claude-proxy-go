# Claude Code Cloud Interface — Feasibility Report

Status: feasibility spike (read-only investigation, no code changed). Date: 2026-09-28.
Scope: session management, keepalive settings, independent model aliasing, via a MiTM-style interface native to the proxy and WebUI.

**Bottom line:** all three features are feasible as proxy-owned extensions. The literal "Cloud" part is not: no captured traffic shows any Anthropic cloud-session API, so emulating it now would mean inventing a schema (the AGENTS.md stop condition).

## 0. Definitions

**MiTM model evaluated: (b), an in-process shim in front of the existing ingress.** Claude Code CLI already reaches the proxy through `ANTHROPIC_BASE_URL` (`README.md:651`), so the proxy already sees the traffic without a CA. Model (a), a forward proxy with an installed CA, is compared in §6. Model (c), an upstream response rewriter, is the egress-alias part of (b).

**What "Claude Code Cloud interface" can mean today.**
- The only evidenced surface is `POST /v1/messages?beta=true`, `GET /api/claude_code/settings` (404) and `GET /api/claude_code/policy_limits` (`.reference/claude-code-headers-20260923.meta.txt:14-16`).
- Headers: `internal/ccidentity/defaults.go:11-178`.
- Auth: OAuth with PKCE (`internal/auth/claudecode_oauth.go:26-44`).
- The OAuth scope includes `user:sessions:claude_code` (`claudecode_oauth.go:44`), which suggests a remote-sessions API exists. But the capture was restricted to `api.anthropic.com` (`meta.txt:8`), so its endpoints are unknown.

**Session: what exists vs what is requested.**
- Existing: a routing-affinity key.
  - It comes from `ccExtractSessionID` (`internal/api/claudecode_proxy.go:193-218`). It reads the headers `x-session-id`, `session-id`, `anthropic-session-id` and `x-conversation-id`, then `metadata.session_id`, then `metadata.user_id`, then top-level `session_id`/`user_id`.
  - It is held in the sticky map (`internal/claudecode/pool.go:37-38`): capped at 10000 entries (`:22`), oldest 10% evicted (`:765-788`), no TTL, memory only.
  - `ccusage.Session()` (`internal/claudecode/ccusage/reports.go:171`) is an after-the-fact report, not a live session.
  - On Antigravity, `sessionId` is per account, not per conversation (`internal/format/builder.go:77`).
- Requested: sessions you can manage — list, inspect, pin, kill, and expire on a timeout.

**Keepalive has five separate layers.**

| Layer | Where | Tunable today? |
|---|---|---|
| TCP/TLS keepalive (dial) | Cloud Code has no dialer (`internal/cloudcode/client.go:170-175`). Claude Code uses DefaultTransport (`internal/claudecode/client.go:307-311`) | No |
| HTTP idle / connection pool | Cloud Code `IdleConnTimeout 90s` (`internal/cloudcode/client.go:174`). Inbound `IdleTimeout 2m` (`cmd/proxy/main.go:265`) | No |
| Stream bounds | 5m **total** `Client.Timeout`, which also cuts streams (`internal/cloudcode/client.go:190`, `internal/claudecode/client.go:310`) | `-upstream-timeout` flag only (`cmd/proxy/main.go:92`) |
| SSE heartbeat to client | None anywhere. The Cloud Code parser also drops upstream comments (`internal/cloudcode/client.go:447-448`) | No |
| OAuth refresh window | 5m per request (`internal/claudecode/pool.go:325`), 15m background (`internal/api/server.go:247`), 1m skew for agy (`internal/auth/token.go:26`) | No |
| Prompt-cache keepalive | Cache Bump (`internal/config/config.go:554-566`, `internal/cachebump/scheduler.go:128-222`) | Yes |

**Aliasing.**
- Ingress aliases today:
  - Global `modelMapping`. It chains and runs first (`internal/api/server.go:1022-1048`, called at `:703`).
  - Per-backend allowlist aliases for Claude Code (`internal/claudecode/types.go:112-127`) and for Kimi, Zen and OpenRouter (`internal/config/config.go:36,88,120`).
  - Cloud Code routing aliases (`internal/modelcatalog/catalog.go:241-273`).
- Egress:
  - Pass-through gateways return the upstream name unchanged (`internal/api/claudecode_proxy.go:344-356`).
  - Antigravity returns the name after mapping (`internal/api/server.go:3276,3388`).

## 1. Verdict

| Feature | Verdict | Confidence | Key evidence |
|---|---|---|---|
| Sessions: local, owned by the proxy | Feasible with constraints | High | The sticky map is in memory with no API (`internal/claudecode/pool.go:37-38,697-788`). The key is already extracted (`internal/api/claudecode_proxy.go:193`). Kill needs a per-request cancel registry; `Release` is deferred at `claudecode_proxy.go:622`. |
| Sessions: emulate Anthropic's cloud-session API | **Not feasible now** | High | No endpoint was captured (`meta.txt:8,14-16`). agy has no session or heartbeat RPC: 0 hits for `session\|heartbeat\|keepalive\|ping` in `.reference/grpc-methods.txt` and `.reference/proto-messages.txt`. This hits the stop condition. |
| Keepalive: client-side SSE ping, stream timeouts, and the Claude Code / pass-through transports | Feasible | High | No ping exists. The 5m total timeout is a real defect (`internal/claudecode/client.go:310`). The planned fix was never built (`docs/superpowers/plans/2026-09-13-sse-stream-timeouts.md`). |
| Keepalive: Cloud Code transport knobs | Feasible only if frozen or read-only | Medium | Transport fields are not TLS internals, but they change the connection-reuse pattern compared with agy. Tests pin the transport (`internal/cloudcode/client_test.go:27-31,43-47`). |
| Ingress alias per backend | Feasible | High | Per-backend matchers already exist (`internal/api/dispatch.go:65-160`). The Claude Code rewrite point is `dispatch.go:113`. |
| Egress (display) alias | Feasible with constraints | Medium | Needs a rewriter for `message_start` and the unary `"model"` field, placed before `ccCopyStream` and the CCR path (`internal/api/claudecode_proxy.go:505-507`). It must not disturb the usage interceptor (`internal/claudecode/usagecapture.go:268`). |

## 2. Current request path (Claude Code branch)

| # | Hop | Location |
|---|---|---|
| 1 | `http.Server` (default listen 127.0.0.1:8080; 8091 is set via `ANTIGRAVITY_PROXY_LISTEN` in `antigravity-go-proxy.service:12`) | `cmd/proxy/main.go:86,259-265` |
| 2 | `Handler()` then `serveHTTP` (strips `/anthropic`, sets CORS) | `internal/api/server.go:354,358` |
| 3 | `handleManagement` first; falls through to `routeClaudeCodeManagement` | `internal/api/management.go:42,250`; `internal/api/claudecode_management.go:392` |
| 4 | `messages()`, then `resolveModelMapping` | `internal/api/server.go:676,703` |
| 5 | `dispatchAlternateBackend` walks `GatewayOrder` (Kimi, Zen, ClaudeCode, OpenRouter, Custom; Cloud Code last) | `internal/api/dispatch.go:49`; `internal/config/gateway_order.go:28,81` |
| 6 | `tryClaudeCodeGateway`, then `matchClaudeCodeModel` | `internal/api/dispatch.go:105`; `internal/api/claudecode_proxy.go:162` |
| 7 | `forwardToClaudeCode` → `ccExtractSessionID` | `internal/api/claudecode_proxy.go:365,373` |
| 8 | `SelectAccount` → `Acquire` → `WireIdentity` → `SendMessage` | `internal/claudecode/pool.go:697,807`; `internal/claudecode/types.go:238`; `internal/claudecode/client.go:349` |
| 9 | 401 refresh/retry, 429 `RecordRateLimit`, failover | `internal/api/claudecode_proxy.go:556-601` |
| 10 | `ccInstrumentResponse` → `UsageInterceptor`, then `ccCopyStream` | `internal/api/claudecode_proxy.go:320,344`; `internal/claudecode/usagecapture.go:268` |
| 11 | `recordClaudeCodeMetrics` → `ccusage.Engine.Record` | `internal/api/claudecode_proxy.go:261`; `internal/claudecode/ccusage/engine.go:458` |

`proxyStreamResponse`/`parseSSEStream` (`internal/api/server.go:3013,3058`) are used only by OpenRouter. The Claude Code path does not use them.

## 3. Proposed architecture (option b)

```
Claude Code CLI --ANTHROPIC_BASE_URL=http://127.0.0.1:8091/anthropic--> serveHTTP (server.go:358)
  |- handleManagement (management.go:42) --> [NEW] /api/sessions*, /api/aliases
  `- messages (server.go:676)
       resolveModelMapping (703)                       <- unchanged, global
       dispatchAlternateBackend (dispatch.go:49)
        |- tryClaudeCode -[NEW alias resolve]- matchClaudeCodeModel (claudecode_proxy.go:162)
        |    forwardToClaudeCode (365)
        |    ccExtractSessionID (373) --> [NEW] SessionRegistry.Touch/IsKilled
        |    SelectAccount (519) - Acquire (534) --> [NEW] register cancel func
        |    SendMessage (tuned client, first-byte bound) --> api.anthropic.com
        |    ccInstrumentResponse (605) --> [NEW] egress model rewrite + ping ticker
        |    ccCopyStream (344) - Release (622) - recordClaudeCodeMetrics (261) --> registry usage
        |- tryKimi / tryZen / tryOpenRouter / tryCustom -[NEW per-backend alias]
        `- Cloud Code fallthrough -[NEW alias before catalog.Resolve]- Dispatcher
             defaultTransport: TLS config UNTOUCHED
```

Request lifecycle:
1. Ingress.
2. Global model mapping.
3. Per-backend alias.
4. The session key resolves to a registry entry: create, touch, or reject with 409 if the session was killed.
5. Sticky account selection.
6. Upstream call bounded by a first-byte deadline, not a total timeout.
7. Stream out, with an optional `event: ping` and model-name rewrite.
8. Usage goes to the ledger and to the registry.
9. The background worker (`internal/api/server.go:236`) sweeps idle sessions and unpins them from the pool.

## 4. Touch list

| Group | Location | Change |
|---|---|---|
| Server | `internal/api/claudecode_proxy.go:193,373,519,534,605,622,344,261` | Registry hooks, cancel registration, egress rewrite, ping |
| | `internal/api/claudecode_proxy.go:79,104` | Pass a tuned `*http.Client` instead of nil |
| | `internal/api/dispatch.go:65-160` | Per-backend alias resolve |
| | `internal/api/discovery.go:22-114` | Advertise aliases; keep `gatewayOwnedModelIDs` consistent with dispatch |
| | `internal/api/server.go:3347,3013` | Ping in `streamMessage` and `proxyStreamResponse` |
| | `internal/api/server.go:1196,1249,1551,1708,1759` | Replace `http.DefaultClient` with a shared tuned client (non-Cloud-Code only) |
| | `internal/api/server.go:236-264` | TTL sweep in the background worker |
| | `internal/api/management.go:67-247` | New routes |
| | `internal/api/management.go:29-40` | Constant-time password compare |
| | `internal/api/management.go:1256` | Validate the new config blocks in `handleConfigSave` |
| | `cmd/proxy/main.go:259-265` | Configurable `IdleTimeout`; keep `WriteTimeout` at 0 |
| Pool / client | `internal/claudecode/pool.go:37-38,697,765` | `ListSticky`, `Unpin`, `Pin`, TTL-aware eviction |
| | `internal/claudecode/client.go:307-311` | Drop the 5m total timeout |
| | `internal/claudecode/observability.go:13-53` | Reuse per-session stats for the list view |
| | `internal/cloudcode/client.go:170-190` | **Transport frozen.** Only the Client-level total `Timeout` becomes a first-byte bound |
| Config | `internal/config/config.go:143-180`, defaults `:605-697` | New `sessions` and `keepalive` blocks |
| | `internal/claudecode/types.go:255-270`, `internal/config/config.go:36,88,120` | `modelAliases` per backend |
| | `internal/config/config.go:1020-1033`, `:871-891`, `:939-959` | Save special cases (see §6) |
| WebUI | `internal/webui/public/views/settings.html:16-56` | New sub-tabs "sessions" and "keepalive" |
| | `internal/webui/public/views/settings.html:1858` | Extend the alias editor |
| | `internal/webui/public/app.js:11-16`, `index.html:545-577` | Register the new components |
| | `internal/webui/public/js/utils.js:7-32` | Use this request helper |
| | `js/components/models.js:747-796` or `internal/api/sse.go:11` | Live refresh: polling pattern or SSE |
| New store | `internal/claudecode/sessions.go` | In-memory registry: hashed ID, backend, account, model, timestamps, counters, cancel funcs. Optional JSON persistence modelled on `internal/claudecode/storage.go:32` |

## 5. API draft

| Method | Path | Body / response | Status |
|---|---|---|---|
| GET | `/api/sessions?backend=&state=` | `[{id (sha256, 12 chars), backend, accountId, model, createdAt, lastSeenAt, requests, inFlight, tokens, costUSD, pinned, state}]` | New |
| GET | `/api/sessions/{id}` | One entry, plus its cache-bump state | New |
| POST | `/api/sessions` | `{accountId, ttlMinutes}` returns `{sessionKey}`, which the client sends as `x-session-id` (read at `internal/api/claudecode_proxy.go:195`) | New ("create" means pre-pin) |
| POST | `/api/sessions/{id}/pin` | `{accountId}` | New |
| POST | `/api/sessions/{id}/cancel` | Cancels in-flight requests | New |
| DELETE | `/api/sessions/{id}` | Unpin, forget, and stop the cache bump (reuses `/api/cache-bump/{id}/stop`, `internal/api/management.go:182`) | New |
| GET | `/api/sessions/stream` | Server-sent events via `internal/api/sse.go` | New |
| GET/POST | `/api/config` | `{keepalive:{...}, sessions:{...}}` | Reuse |
| GET/DELETE | `/api/cache-bump` | — | Reuse |
| GET/POST | `/api/claudecode/config` | `{modelAliases:[...]}` | Reuse |
| GET/POST | `/api/{kimi,zen,openrouter}/config` | `{modelAliases:[...]}` | Reuse |
| GET | `/api/aliases` | Merged read-only view with `collisions[]` | New |
| POST | `/api/aliases/validate` | Dry-run collision check | New |

All new routes sit behind `checkWebUIPassword` (`internal/api/management.go:29-40`).

## 6. Config draft

```json
"sessions":  { "idleTimeoutMinutes": 0, "maxEntries": 10000, "persist": false },
"keepalive": {
  "clientPingSeconds": 0,
  "streamFirstByteSeconds": 300,
  "streamIdleSeconds": 0,
  "inboundIdleTimeoutSeconds": 120,
  "claudeCode":  { "dialKeepAliveSeconds": 30, "idleConnTimeoutSeconds": 90, "maxIdleConnsPerHost": 2 },
  "passthrough": { "dialKeepAliveSeconds": 30, "idleConnTimeoutSeconds": 90 }
},
"claudecode": { "modelAliases": [ { "alias": "my-opus", "target": "<allowlist id>", "display": "My Opus", "egress": false } ] }
```

Every default equals today's behavior, so existing configs need no migration. There is no version field (`internal/config/config.go:771-797`).

Migration notes:
- There is deliberately no Cloud Code transport block.
- `openrouter` and `zen` are replaced wholesale on save (`internal/config/config.go:871-891,939-959`). The UI must echo `modelAliases` back, or they are wiped.
- The generic merge cannot delete keys (`internal/config/config.go:1024-1033`). `sessions` and `keepalive` need the same special case that `modelMapping` gets (`:1020-1022`).

### MiTM options compared

| | (b) In-process shim (recommended) | (a) Forward proxy + installed CA |
|---|---|---|
| TLS / fingerprint | No change. The Cloud Code transport is untouched | No change upstream. The Node-side TLS toward the proxy becomes Go-terminated, which is new surface |
| Cert trust | None needed | Linux: `NODE_EXTRA_CA_CERTS` + `HTTPS_PROXY` (`scripts/capture-claude-code-headers.sh:440-476`). macOS: a combined `SSL_CERT_FILE` bundle; `SSL_CERT_FILE` replaces the root pool (`.reference/agy-headers-mitm-20260903.txt`) |
| Auth token handling | The existing pool holds the tokens (`internal/claudecode/storage.go:32-33`) | Sees every Anthropic token in cleartext, including the OAuth exchange (`internal/auth/claudecode_oauth.go:35`) |
| Streaming integrity | Existing flush-per-chunk copy (`internal/api/claudecode_proxy.go:344-362`) | Needs new CONNECT/Hijack code; none exists in the repo |
| Failure modes | Proxy down means the CLI errors (same as today) | A CA key leak can impersonate any host unless the CA is name-constrained. A bypassed proxy fails silently |

## 7. Risks (ranked)

1. **Critical: fingerprint break.**
   - Any change to Cloud Code dialing or idle behavior changes the connection pattern compared with agy.
   - Mitigation: freeze `defaultTransport`; keep `internal/cloudcode/client_test.go:27-31` green; run the JA4 gate on every POC step.
2. **High: token leak if option (a) is built.**
   - A forward-proxy CA terminates all `api.anthropic.com` TLS, including the OAuth token exchange.
   - Mitigation: don't build (a). It adds nothing, because the base URL already routes traffic to the proxy.
3. **High: PII in session IDs.**
   - The raw key is `metadata.user_id` JSON, which contains `device_id` (`internal/ccidentity/apply.go:135-165`).
   - The password check is a plain string comparison and also accepts `?password=` (`internal/api/management.go:29-40`).
   - Mitigation: expose only a hashed ID; compare in constant time.
4. **High: upstream ToS / detection.**
   - Cancel and pin change request cadence.
   - An egress alias must never reach upstream.
   - Pings go to the client only.
5. **Medium: streams cut at 5m.** This happens today; fixing it is the main value of the keepalive feature.
6. **Medium: alias collision.**
   - Claude Code prefix matching can swallow new names (`internal/claudecode/router.go:227-231`).
   - Global mapping runs first (`internal/api/server.go:703`).
   - `/v1/models` could name a different owner than the dispatcher.
   - Classifier rules match the raw client name (`internal/api/server.go:795`).
7. **Medium: session store growth.** Use a cap plus a TTL, mirroring the existing 10000 cap (`internal/claudecode/pool.go:22`, `internal/claudecode/observability.go:13`).
8. **Low: config migration.** The save quirks listed in §6.
9. **Low: connection churn.** Low idle timeouts mean more handshakes.

## 8. POC plan

Each step ends with `go build -o bin/proxy ./cmd/proxy` plus the check listed.

1. **Read-only session list.**
   - Build: `GET /api/sessions` from the pool's sticky map and the session tracker.
   - Check: send 2 requests with `-H 'x-session-id: poc1'`. `curl -H 'x-webui-password: …' :8091/api/sessions` shows the hashed ID, the account, and `requests=2`.
2. **Unpin.**
   - Build: `DELETE /api/sessions/{id}`.
   - Check: the next request logs a fresh `SelectAccount`, and the cache-bump entry is gone from `GET /api/cache-bump`.
3. **Idle expiry.**
   - Build: TTL sweep with `idleTimeoutMinutes: 1`.
   - Check: the entry disappears within 1–6 minutes. The background worker ticks every 5m (`internal/api/server.go:267-283`).
4. **Cancel in flight.**
   - Build: `POST /api/sessions/{id}/cancel`.
   - Check: a long `curl -N` stream ends, and `/api/claudecode/accounts` shows `InFlight` back at 0.
5. **Stream bounds and ping.**
   - Build: a first-byte bound replaces the total timeout; set `clientPingSeconds: 15`.
   - Check: `curl -N` shows `event: ping` lines, and a stream longer than 5m against an `httptest` upstream survives.
6. **Ingress alias for Claude Code.**
   - Check: `GET /v1/models` lists `my-opus`, and a request with `model:"my-opus"` routes to Claude Code (visible in the log).
   - Check: the Kimi, Zen and OpenRouter tests still pass.
7. **Egress alias.**
   - Check: in the `curl -N` output, the first `message_start` has `"model":"My Opus"`.
   - Check: the ccusage ledger still records the canonical ID.
8. **Fingerprint gate.**
   - Run `go test ./internal/cloudcode/`.
   - Then run the tshark JA4 command against `.reference/agy-current-baseline.txt`. It must match, and the number of new connections per minute must match agy's.

WebUI checks for steps 1, 5 and 6 happen in the new sub-tabs.

## 9. Open questions

1. **Cloud session API.** Recapture `claude --remote` / teleport traffic with the capture's allowed hosts widened beyond `api.anthropic.com`. Until then, no emulation.
2. **Claude Code session identity.** Does 2.1.280 send a stable per-conversation `metadata.user_id.session_id`? The capture never dumps bodies (`.reference/claude-code-headers-20260923.meta.txt:18-20`), so a capture of body keys only is needed.
3. **Forcing a session from the CLI.** Can the CLI send `x-session-id`, for example via `ANTHROPIC_CUSTOM_HEADERS`? Needs a docs lookup.
4. **HTTP/1.1 vs HTTP/2 to Anthropic.** Real Claude Code uses HTTP/1.1 to `api.anthropic.com` (`.reference/claude-code-headers-20260923.txt:15`). The proxy's Claude Code client uses DefaultTransport, which will likely negotiate HTTP/2. This is an existing mismatch that has not been confirmed; a pcap of proxy→Anthropic would settle it. A tuned client must not change the protocol silently.
5. **agy connection-reuse profile.** Idle close timing and TCP keepalive probes need a pcap before any Cloud Code knob is exposed.
6. **agy `sessionId` semantics.** Per process or per conversation? Check with `strings` on the agy binary. The current per-account value (`internal/format/builder.go:77`) stays unchanged until there is evidence.

## Found in passing (not part of this scope)

- `internal/webui/public/app.js:229` (Claude Code OAuth poll) uses raw `fetch` with no `x-webui-password` header. Adding an account therefore fails with 401 whenever a WebUI password is set.
- Unverified: `handleModelsConfigPost` may write the shared config map without a lock (`internal/api/management.go:1700-1722`).
