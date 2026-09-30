# Handoff: finish the Zen free-tier spoofer (find the real gate)

Date: 2026-09-29. Follows `2026-09-29-zen-harness-spoofing-plan.md`
(T1–T10 done, completion log at its end). Deliverable: a spoofer that gets
HTTP 200 from Zen free tier — or conclusive proof that only a paid key /
`opencode serve` will.

## Mission in one paragraph

PR #105 (`feat/zen-harness-spoofing`) spoofs the OpenCode harness headers
and replays a byte-faithful genuine ClientHello through utls. The live gate
was verified packet-by-packet: JA4 `t13d1713h1_5b57614c22b0_6a3d802a7139`
and JA3 `1523504b38f0fae0d881d4b6554aac1b` on the wire to `opencode.ai`
are exact matches to the genuine client. The response is **still 403**
(`OpenCode's free tier can only be used from within OpenCode`). So the
discriminator is not (only) the ClientHello. Two evidence tracks remain:
read the real opencode source, and capture long interactive sessions of the
genuine client to diff everything below the TLS layer.

## Where we stand

Done and verified (commit `20b4213`, PR #105):

- Headers spoofed at all 5 zen-bound sites, env overrides
  `OPENCODE_VERSION`/`OPENCODE_CLIENT`, config `zen.harness.*` with
  field-preserving `Save()` merge (`internal/zen/harness.go`).
- Gate warning on all 3 response paths; gzip-aware detection;
  403 bodies preserved byte-for-byte (`ObserveFreeTierGate`).
- TLS disguise opt-in `zen.harness.tls` (default false):
  `internal/zen/tls.go` + committed capture
  `internal/zen/opencode-clienthello.bin` + unit JA3 equality test
  (`internal/zen/tls_test.go`).
- Live gate script `scripts/verify-zen-tls.sh` (run with sudo) — asserts
  JA4/JA3/SNI/ALPN on wire; currently PASS on fingerprint, HTTP status 403.

Unknowns recorded in the plan completion log: request-level secret,
certificate/time correlation, session binding, header order/completeness,
auth scheme.

## Track A — source investigation

### What exists locally

- Genuine client: `opencode 1.18.30`, Homebrew, Bun-compiled Mach-O at
  `/opt/homebrew/bin/opencode` → `Cellar/opencode/1.18.30_2/bin/opencode`.
  JS is embedded — `strings` works, but naive search for `x-opencode`
  header names found **nothing** (names are constructed or minified), and
  the 403 string is **not** in the client (the gate is server-side).
  Endpoints visible: `opencode.ai/zen`, `opencode.ai` (docs/assets).
- Upstream repo: start from `anomalyco/opencode` (plan cites issue
  #49621: byte-faithful replay still got 403); confirm the real formula
  source with `brew info opencode`. The original project is `sst/opencode`
  — check which one Homebrew tracks.
- Runtime data: `~/.config/opencode/opencode.json` (providers `local`,
  `google` — no zen provider configured explicitly, zen is the built-in
  `opencode/*` free models), `~/.local/share/opencode/auth.json` (only
  `llama` and `anthropic` keys — **zen looks keyless or token-based from
  elsewhere**), `~/.local/share/opencode/opencode.db`, log
  `~/.local/share/opencode/log/opencode.log`, package cache
  `~/.cache/opencode/packages`.
- How the capture was made: `/tmp/capture-opencode-tls.sh` (ephemeral —
  re-create or commit it; commands are in the plan/handoff chain).

### Questions to answer (priority order)

1. **Zen auth scheme.** How does the genuine client authenticate to
   `opencode.ai/zen`? The proxy sends `Authorization: Bearer <zen.apiKey>`
   from `config.json`. If the real client uses a device-flow token, a
   signed session token, or no key at all (cookie / bound session), the
   server can reject us with that exact 403 regardless of headers/TLS.
   Look at the zen provider auth code and any OAuth/device endpoints.
2. **The exact gate condition, if the server code is in the repo.**
   Search for `"can only be used from within OpenCode"`,
   `FreeTierError`, `1.17.0 or newer`. Enumerate every input the check
   reads (headers, UA parse, TLS metadata, token, IP, account).
3. **Complete request header set and order.** Our spoof sends exactly 5
   headers + UA. Diff against the client source for a real
   `/v1/messages` call: any `x-app`, `x-stainless-*`, announce headers,
   and **wire order** (HTTP/1.1 header order — Go canonicalizes, Bun does
   not; order alone is fingerprintable). Where do we apply headers:
   `internal/zen/harness.go` (`ApplyHarnessHeaderMap`).
4. **UA version.** Genuine build is `1.18.30`; our default is `1.18.31`
   (`DefaultVersion`). The server parses the version ("1.17.0 or newer"
   message). Quick experiment: set `OPENCODE_VERSION=1.18.30` (or config
   `version`) and re-hit the gate — cheap, do it first.
5. **Connection behavior** the server can see: session resumption, TLS
   ticket use, keep-alive reuse, second-request differences. Compare with
   what Go/utls does (see Track B).

### Method notes

- `strings <binary> | rg -i '<literal>'` first, then clone the repo and
  grep the same literals in JS/TS — the repo search is the productive one.
- Cross-check every spoofed constant against the binary/repo: header
  names, UA format, ID prefixes (`ses_`/`msg_`), version string.
- Never commit OAuth material; values from the binary are for refresh
  use only (AGENTS.md).

## Track B — longer interactive captures

### Why

The existing capture (`/tmp/opencode-zen.pcap`, ephemeral) is a 20-second
`opencode run "ping"`: one model fetch (SNI `models.opencode.ai`) and one
chat call (SNI `opencode.ai`). It shows the first ClientHello only.
Interactive sessions add: more endpoints, connection reuse, TLS resumption
(a resumed ClientHello has a PSK extension — utls never resumes, so if the
gate checks that, short captures miss it), header variation per call type,
and any challenge/response handshake in HTTP.

### Capture procedure (root required)

```bash
sudo tcpdump -i pktap,all -P -w /tmp/opencode-interactive.pcap 'tcp port 443' &
# then, in a terminal, start a REAL interactive session and keep it open:
opencode            # TUI: several prompts, tool use, ≥2 minutes
# close TUI, stop tcpdump (kill -INT), analyze with tshark
```

Use `pktap,all` on macOS (per `.reference/fingerprint-recheck-20260924.txt`);
`en0` is fine when traffic is known to be Wi-Fi. Capture **all** 443 hosts,
not just `opencode.ai` — models, auth and any CDN endpoints matter.

Analyze:

```bash
tshark -r /tmp/opencode-interactive.pcap \
  -Y 'tls.handshake.type==1' -T fields \
  -e frame.number -e ip.dst -e tls.handshake.extensions_server_name \
  -e tls.handshake.ja4 -e tls.handshake.ja3
```

Compare every row against the committed baseline hello
(`internal/zen/opencode-clienthello.bin`; expected JA4
`t13d1713h1_5b57614c22b0_6a3d802a7139` for the first full handshake).
Rows 2+ reveal resumption/rekeying behavior — that diff is a deliverable
by itself.

### Decrypting the HTTP layer (the hard part)

Header order and full header sets need plaintext. Options, in order:

1. **Read the source** (Track A) — often enough, no decryption.
2. `SSLKEYLOGFILE=/tmp/sslkeys.log opencode` then load keys in Wireshark
   (`Edit → Preferences → Protocols → TLS → (Pre)-Master-Secret log
   filename`). Bun may or may not honor it — verify by opening the
   capture; if no keys, this path is dead.
3. MITM with a local CA and whatever proxy/CA-trust knob opencode
   documents (check repo for `HTTPS_PROXY`, `NODE_EXTRA_CA_CERTS`,
   `--insecure` handling). Only with a sandboxed account.

For comparison, run the proxy under the same capture with
`sudo scripts/verify-zen-tls.sh` (writes to `/tmp/zen-tls-verify/`).

### Diff checklist (genuine vs proxy)

- TLS: JA3/JA4 per connection, resumption vs full handshake, SNI/ALPN,
  TLS version, ticket behavior, ClientHello byte offsets (diff hexdumps).
- HTTP: header names + **order** + exact values (UA version, ID formats),
  `Accept-Encoding`, connection reuse, request framing.
- Endpoints: full list of URLs hit during a session, auth header per
  endpoint.
- Response: full 403 body + response headers of a gated call — any
  challenge/nonce the client is expected to answer.

## Definition of done

1. Genuine client gets 200 on a free-tier model in a live capture
   (baseline proof).
2. Proxy gets 200 for the same model, with
   `sudo scripts/verify-zen-tls.sh` still PASSing JA4/JA3.
3. Unit + race tests green (`go test -race ./internal/zen/...
   ./internal/api/... ./internal/config/... ./internal/cachebump/...`),
   `gofmt`/`go vet ./...` clean.
4. Findings appended to the plan completion log; gate script extended to
   assert whatever new discriminator was found.
5. PR (amend #105 or new branch from `fork/main`).

If Track A shows the gate binds to a server-issued secret we cannot
obtain, stop: document it, recommend paid Zen key or `opencode serve` as
upstream, and close the effort. Do not fabricate constants.

## Constraints

- AGENTS.md rule stands: never touch TLS internals of the CloudCode/agy
  path; the utls work is zen-only and opt-in (`zen.harness.tls`, default
  false) until the gate passes.
- Packet-verified evidence only; no invented header names, IDs, or proto
  schemas.
- Local junk in the worktree (`.gemini/`, `opencode.json`, `pr.sh`,
  `tools/quotaprobe/`, …) is untracked on purpose — stage explicitly.

## Asset map

| Asset | Path | Durability |
|---|---|---|
| Handoff input (this session) | `docs/plans/2026-09-29-zen-harness-spoofing-handoff.md` | committed |
| Executed plan + completion log | `docs/plans/2026-09-29-zen-harness-spoofing-plan.md` | committed |
| Feature branch / PR | `feat/zen-harness-spoofing`, PR #105 | pushed |
| Genuine ClientHello | `internal/zen/opencode-clienthello.bin` | committed |
| Fingerprint gate script | `scripts/verify-zen-tls.sh` | committed |
| Short genuine capture | `/tmp/opencode-zen.pcap` (+ `/tmp/capture-opencode-tls.sh`) | ephemeral — re-capture |
| Live proxy verify artifacts | `/tmp/zen-tls-verify/` | ephemeral |
| agy baselines (CloudCode, not zen) | `.reference/*` | committed |
