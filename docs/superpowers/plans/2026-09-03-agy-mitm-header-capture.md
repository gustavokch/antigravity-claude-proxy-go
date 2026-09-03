# MITM-Based Header Capture for agy Fingerprint Verification

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the low-confidence binary-strings header check (`docs/superpowers/plans/2026-09-03-agy-fingerprint-models-reverification.md` Task 2, verdicts `template-only`/`unknown`) with a byte-level ground truth: run local `agy` 1.1.25 through a localhost mitmproxy, capture the decrypted requests to `cloudcode-pa.googleapis.com` / `daily-cloudcode-pa.googleapis.com`, and diff the observed header set (names, values, order, casing) against the proxy's hardcoded headers.

**Why MITM is the only path:** upstream headers ride inside TLS; the zero-TLS-customization rule forbids keylog/decryption of the proxy's own transport, and pcap only yields JA4. Binary string tables recover templates, not baked literals. A localhost MITM terminates agy's TLS with a locally trusted CA and records the exact header list.

**Architecture:** `mitmdump` on `127.0.0.1:18080` with a small Python dump addon (ordered JSONL per request, Authorization redacted at write time). `agy` launched with `HTTPS_PROXY` pointing at it and the mitmproxy CA trusted in the login keychain. Capture scoped to cloudcode hosts only via `allow_hosts`. Evidence committed redacted to `.reference/`. Constants sync (if drift) lands test-first.

**Tech Stack:** Go 1.27, mitmproxy 10+ (`brew install mitmproxy`), Python 3 addon, `agy` CLI v1.1.25 (`/Users/gus/.local/bin/agy`, authed), macOS `security` CLI, jq.

**Spec:** `internal/cloudcode/client.go:30-32` (constants), `client.go:139-147` (defaultHeader), `client.go:271-276` (per-request headers), `.reference/agy-headers-20260903.txt` (prior low-confidence verdicts), AGENTS.md fingerprint gate.

**Execution first step:** copy this plan to `docs/superpowers/plans/2026-09-03-agy-mitm-header-capture.md`, then work from that copy.

## Global Constraints

- **Secret hygiene (hard rule):** the captured `Authorization: Bearer` value is a live OAuth token. The dump addon redacts it before writing (keep scheme + sha256 of token + length). No committed file ever contains the raw token. Same treatment for any `x-goog-*` auth-ish header carrying credentials.
- **Scope:** intercept ONLY `cloudcode-pa.googleapis.com` and `daily-cloudcode-pa.googleapis.com` (`--set allow_hosts=...`). All other traffic passes through un-intercepted. Kill mitmdump and revoke CA trust when done.
- **Zero-TLS rule unchanged:** this capture observes agy, not the proxy. `internal/cloudcode/client.go:113-118` `tls.Config{}` stays empty. Never run `scripts/verify-ja4.sh` against a MITM session — the upstream leg from mitmdump has mitmproxy's fingerprint, not agy's. JA4 ground truth still comes from the pcap method.
- **Byte-exactness contract:** mitmproxy's `flow.request.headers` preserves header name casing, value, and order as received. It does NOT preserve raw wire octets (whitespace around the colon, line endings). Evidence states "header-list exact (names, values, order, case)", not "raw-octet exact".
- **HTTP version:** agy 1.1.25 ClientHello has ALPN absent (2026-09-03 pcap) → HTTP/1.1. If the capture shows h2, header order is still recorded; note it in evidence.
- **Cert pinning stop condition:** if agy fails TLS through the MITM after correct CA trust (pinning), **STOP** — do not patch agy or bypass pins; report to the user.
- **Gate per commit:** `go build ./... && go vet ./... && go test -race ./...`.
- **TDD:** constant changes land test-first in the same commit.
- **PR target:** fork `gustavokch/antigravity-claude-proxy-go`, base `main` (memory: PR Target Fork). If `feat/agy-1.1.25-fingerprint-and-catalog` is still unmerged, stack this branch on it instead.
- **Sudo:** needed only for `security add-trusted-cert` (admin trust domain) and its removal.

---

### Task 0: Branch + pre-flight

**Files:**
- Create: `docs/superpowers/plans/2026-09-03-agy-mitm-header-capture.md` (copy of this plan)

- [ ] Copy plan into repo; `git switch -c feat/agy-mitm-header-capture` (off `main`, or stacked per Global Constraints)
- [ ] Preflight (read-only): `agy --version` (expect `1.1.25`); `which mitmdump || brew install mitmproxy`; `mitmdump --version` (expect 10+); `python3 --version`
- [ ] Baseline gate: `go build ./... && go vet ./... && go test -race ./...` — green before any change

### Task 1: mitmdump header-capture addon + harness script

**Files:**
- Create: `scripts/mitm_header_dump.py` (committed)
- Create: `scripts/capture-agy-headers.sh` (committed wrapper, mirrors `scripts/verify-ja4.sh` conventions)

**Interfaces:** addon consumes mitmproxy flows; produces JSONL at `$OUT` (one line per request: `ts, http_version, method, host, path, headers` as an ordered list of `[name, value]` pairs). Redaction happens inside the addon before write.

- [ ] Write `scripts/mitm_header_dump.py`:
  - `requestheaders` / `response` hook on `http.HTTPFlow`; filter `flow.request.pretty_host` in the two cloudcode hosts.
  - Serialize `list(flow.request.headers.items())` (order + case preserved) to JSONL; replace the value of `authorization` (and any header whose name matches `authorization|proxy-authorization|x-goog-iam-authorization`) with `{"redacted": true, "scheme": <first token>, "sha256": <hex>, "len": <int>}`.
  - Append to path from `MITM_DUMP_OUT` env (default `/tmp/agy-headers-mitm.jsonl`), flush per line.
- [ ] Write `scripts/capture-agy-headers.sh` (executable): starts `mitmdump --listen-host 127.0.0.1 --listen-port 18080 -s scripts/mitm_header_dump.py --set allow_hosts='cloudcode-pa\.googleapis\.com' --set confdir="$HOME/.mitmproxy"` in background, waits for the port, runs the given command, then kills mitmdump. Honors `ANTIGRAVITY_MITM_PORT` override. Loud usage line when called with no command.
- [ ] Smoke the harness against a harmless HTTPS endpoint through the proxy (e.g. `HTTPS_PROXY=http://127.0.0.1:18080 curl -sS https://cloudcode-pa.googleapis.com/ -o /dev/null -w '%{http_code}'` — any response incl. 4xx proves interception works); assert a JSONL line exists with the curl headers.
- [ ] Commit: `feat(scripts): mitmproxy header-capture addon and harness`

### Task 2: Trust mitmproxy CA for Go/agy

**Files:** none (machine state only; reversal in Task 5)

- [ ] Ensure CA exists: run `mitmdump --set confdir="$HOME/.mitmproxy" &` once, kill after `~/.mitmproxy/mitmproxy-ca-cert.pem` appears.
- [ ] **Probe A (no keychain):** `SSL_CERT_FILE="$HOME/.mitmproxy/mitmproxy-ca-cert.pem"` — verify whether Go 1.27 on Darwin honors it for a test request through the MITM (small `go run` probe or the curl smoke with a Go-built client). Record result.
- [ ] **Probe B (keychain, expected path):** `sudo security add-trusted-cert -d -r trustRoot -k "$HOME/Library/Keychains/login.keychain-db" "$HOME/.mitmproxy/mitmproxy-ca-cert.pem"`. Record the cert SHA-256 (`shasum -a 256` on the pem) for later removal.
- [ ] Verification: Task 1 curl smoke through MITM succeeds WITHOUT `-k`.
- [ ] Note in evidence which trust path worked; if both fail and agy then breaks TLS → **STOP** (pinning), report to user.

### Task 3: Capture agy 1.1.25 headers through the MITM

**Files:**
- Create: `.reference/agy-headers-mitm-20260903.jsonl` (redacted, committed)
- Create: `.reference/agy-headers-mitm-20260903.txt` (human-readable evidence + analysis, committed)

- [ ] Run:
```bash
scripts/capture-agy-headers.sh env \
  HTTPS_PROXY=http://127.0.0.1:18080 HTTP_PROXY=http://127.0.0.1:18080 \
  agy --log-file=/tmp/agy-mitm-20260903.log \
  --print='Reply with exactly AGY_MITM_OK_20260903' --print-timeout=90s
```
- [ ] Assert: agy printed `AGY_MITM_OK_20260903` (proves requests succeeded through MITM). TLS failure → **STOP** (pinning), report.
- [ ] Assert capture non-empty and covers the expected call sequence: `loadCodeAssist`, `fetchAvailableModels`, `generateContent`/`streamGenerateContent` on the two cloudcode hosts.
- [ ] Copy the JSONL to `.reference/agy-headers-mitm-20260903.jsonl`; `grep -c 'redacted": false' ` must be 0 for auth headers; eyeball one line to confirm redaction shape.
- [ ] Extract per-endpoint header tables (name, value, order) with jq into the analysis file. Include: HTTP version, per-path ordered header list, UA value, `X-Client-Version` value, `x-goog-api-client` literal, presence/absence of `Accept-Encoding`, `X-Machine-Session-Id`, `Accept`, plus any header the proxy does NOT send.
- [ ] Commit: `docs(fingerprint): capture agy 1.1.25 headers via local MITM for 2026-09-03`

### Task 4: Diff agy ground truth vs proxy headers

**Files:**
- Modify: `.reference/agy-headers-mitm-20260903.txt` (append verdict table)
- (conditional) Modify: `internal/cloudcode/client.go:30-32` + `client.go:139-147`/`271-276`
- (conditional) Test: `internal/cloudcode/client_test.go`

Proxy's current outbound set (`client.go:141-147`, `271-276`): `Authorization`, `Content-Type: application/json`, `Accept-Encoding: identity`, `User-Agent: antigravity/<DefaultUserAgentVersion> <goos>/<goarch>`, `X-Client-Name: antigravity`, `X-Client-Version: <DefaultClientVersion>`, `x-goog-api-client: <GoogleAPIClient>`, plus per-request `Accept` and `X-Machine-Session-Id`. Constants: `DefaultUserAgentVersion="2.0.3"`, `DefaultClientVersion="1.110.0"`, `GoogleAPIClient="gl-go/1.26.4 auth/0.5 google-api-go-client/0.5"`.

- [ ] Build the comparison table in the evidence file, one row per header: agy 1.1.25 observed value/order vs proxy value; verdict `exact` / `value-drift` / `missing-in-proxy` / `extra-in-proxy` / `order-drift`.
- [ ] Pay special attention to: `x-goog-api-client` exact literal (previously `template-only`), `X-Client-Version` (previously unknown), UA version segment, header ORDER (Go's `http.Header` map randomizes wire order — if agy shows a stable deliberate order the proxy cannot reproduce with a map, record as `order-drift` and decide whether it matters; do NOT restructure the transport without user sign-off).
- [ ] Branch:
  - Any `value-drift` in the three constants → Task 5 sync (test-first).
  - Only `unknown`-clearing with no drift → keep constants; evidence supersedes `.reference/agy-headers-20260903.txt` verdicts (note supersession in the old file's header line).
  - `missing-in-proxy`/`extra-in-proxy` beyond noise → STOP, surface to user before touching code.
- [ ] Commit: `docs(fingerprint): diff MITM ground truth against proxy header constants`

### Task 5: (conditional) Sync header constants + cleanup machine state

Skip the constant sync if Task 4 found no drift; cleanup is NOT optional.

**Files:**
- (conditional) Modify: `internal/cloudcode/client.go:30-32`
- (conditional) Test: `internal/cloudcode/client_test.go` (`TestFetchAvailableModelsHeadersAndDailyFallback`)

- [ ] Test-first: update expected header values in the test to the confirmed agy 1.1.25 literals; `go test -race ./internal/cloudcode/...` — expect FAIL.
- [ ] Update constants; rerun — expect PASS. Then full gate `go build ./... && go vet ./... && go test -race ./...`.
- [ ] `sudo ./scripts/verify-ja4.sh` — header changes must not perturb the ClientHello; expect `Fingerprint verification SUCCESS!`. Mismatch → STOP.
- [ ] Commit: `fix(fingerprint): sync client headers with agy 1.1.25 MITM baseline`
- [ ] **Cleanup (always):** revoke CA trust: `sudo security delete-certificate -Z <SHA256-from-Task-2> "$HOME/Library/Keychains/login.keychain-db"` (or remove the trust setting via Keychain Access); verify a fresh curl through a restarted mitmdump now FAILS TLS; remove any `SSL_CERT_FILE` exports. Confirm no mitmdump process left running.
- [ ] Append cleanup confirmation (commands + results) to the evidence file. Commit: `docs(fingerprint): record MITM CA trust revocation for 2026-09-03`

### Task 6: Final gate + PR

- [ ] `go build ./... && go vet ./... && go test -race ./...` — green.
- [ ] `sudo ./scripts/verify-ja4.sh` — green (proves capture work did not touch the transport).
- [ ] `./scripts/verify-fingerprint.sh` — full wrapper green.
- [ ] `git push -u origin feat/agy-mitm-header-capture`; open PR against **gustavokch/antigravity-claude-proxy-go** `main`, title `feat: MITM-based agy header ground-truth capture`, body linking the evidence files and stating the supersession of the binary-strings verdicts.

## Verification Summary

| Layer | Command | Pass condition |
|---|---|---|
| Build/static/tests | `go build ./... && go vet ./... && go test -race ./...` | exit 0 |
| Harness | Task 1 curl smoke through MITM | JSONL line with curl headers, ordered pairs |
| Interception | Task 3 agy run | agy prints `AGY_MITM_OK_20260903`; JSONL covers loadCodeAssist/fetchAvailableModels/generateContent |
| Redaction | `grep -c 'redacted": false'` on auth headers | 0; no raw bearer token in any committed file |
| Header parity | Task 4 table | every header verdict recorded; drift resolved per branch rules |
| TLS untouched | `sudo ./scripts/verify-ja4.sh` | `Fingerprint verification SUCCESS!` |
| Cleanup | Task 5 post-revocation curl | TLS failure without `-k` proves trust removed |

## Risks

1. **agy pins the cloudcode certificate** → capture impossible; STOP in Task 3, report. Never bypass pinning.
2. **Token leakage** → mitigated by in-addon redaction before write; double-checked with grep before commit. Evidence files are committed — treat every line as public.
3. **Go header-order limitation** → `http.Header` map cannot guarantee wire order; if agy proves order-sensitive upstream, that is a separate design decision — surface, don't hack.
4. **mitmproxy version skew** (`allow_hosts` renamed across versions; older used `--ignore-hosts` with inverse regex) → pin expectation to 10+ in pre-flight; adapt flag if `mitmdump --version` differs, note in evidence.
5. **CA trust residue** → Task 5 revocation is mandatory and verified by a failing curl; forgetting it leaves a trusted local MITM CA on the machine.
6. **Scope excludes:** the proxy's own outbound leg (its `defaultTransport` sets no `Proxy`, so it ignores `HTTPS_PROXY`; capturing the proxy through the same MITM would require a temporary transport patch — explicitly out of scope unless the user asks), keylog-based decryption, and any `tls.Config` change.
