# Handoff: Brainstorm How to Resolve the Claude Code Cloud Open Questions

> **Superseded (2026-09-28).** The questions were resolved by running probes instead of through this brainstorm: see §9 of `docs/superpowers/specs/2026-09-28-claude-code-cloud-feasibility.md` (Q1-Q5 answered, Q6 probe written). The decision file named at the end, `2026-09-28-claude-code-cloud-open-questions.md`, was never created. This hand-off is kept as the record of how the questions were framed.

## Goal

Work with the user to pick a **resolution method** for each of the six open questions in §9 of the feasibility report:

- A **resolution method** is a concrete probe that will produce evidence.
- For each question, agree on:
  - the probe;
  - who runs it (you in the cloud container, or the user on their machine);
  - the artifact that proves the answer;
  - what each possible answer unblocks.

This session is for choosing probes. Running them is later work, each piece approved separately.

## Start here

1. Load the `superpowers:brainstorming` skill. Classify this as a **spike**, since the output is decisions, not code.
2. Check out the branch and read the report:
   - `git fetch origin feat/claude-code-cloud && git checkout feat/claude-code-cloud`
   - Read `docs/superpowers/specs/2026-09-28-claude-code-cloud-feasibility.md` in full. §9 is the question list. §1 and §7 explain why each question matters.
3. Read `AGENTS.md` for the hard rules:
   - The TLS config is untouchable.
   - Never fabricate a proto schema.
   - Never commit credentials.
4. Take the questions **one at a time**, in the order given in "Suggested order" below, unless the user reorders them. For each one:
   - present 2–3 candidate probes;
   - recommend one;
   - let the user decide.

## Environment facts you cannot discover by looking

- **The cloud container lacks the ground-truth tools.**
  - `/root/.local/bin/agy` and `/root/antigravity-claude-proxy` do not exist in this container.
  - There is no `mitmdump`, no Podman, and no live OAuth tokens.
  - So every capture and every `strings` run on the agy binary is a **user-side step** on their machine (macOS, per `.reference/fingerprint-recheck-20260924.txt`).
  - Design each such probe as a command or script the user can run and paste back.
  - The `wizard` skill fits multi-step user-side procedures.
- **The capture harness already exists.** Reuse it; don't design a new one.
  - `scripts/capture-claude-code-headers.sh`: Podman plus mitmdump, `oauth` or `apikey` mode, `--allow-hosts` restricted to `api.anthropic.com`.
  - `scripts/capture-agy-headers.sh`
  - `scripts/mitm_header_dump.py`: redacts `Authorization`, never dumps request bodies. Its tests are in `scripts/test_mitm_header_dump.py`.
- **`.reference/` files follow a provenance contract.** Every value is a verbatim quote from observed traffic; anything not byte-confirmed is marked `unknown`. Any evidence file a probe produces must follow the same rule. See the header of `.reference/claude-code-headers-20260923.txt`.
- **macOS trust gotcha.** `SSL_CERT_FILE` replaces the whole root pool on darwin, so mitm trust needs a combined bundle (`.reference/agy-headers-mitm-20260903.txt`). Node uses `NODE_EXTRA_CA_CERTS` instead.

## The questions, with the evidence already in hand

Treat the "seed" probes as starting points for discussion, not decisions.

| # | Question | Known evidence | What the answer unblocks | Seed probes |
|---|---|---|---|---|
| Q1 | What are the Claude Code "cloud session" endpoints? | The OAuth scope includes `user:sessions:claude_code` (`internal/auth/claudecode_oauth.go:44`). The capture saw only 3 routes, because allowed hosts were limited to `api.anthropic.com` (`.reference/claude-code-headers-20260923.meta.txt:8,14-16`). | Whether cloud-session emulation is possible at all; it is blocked until then. | Rerun the harness with wider allowed hosts while running remote/teleport commands; Claude Code docs lookup. |
| Q2 | Does Claude Code 2.1.280 send a stable per-conversation `metadata.user_id.session_id`? | Bodies are never dumped (`meta.txt:18-20`). The proxy uses `metadata.user_id` as the sticky key (`internal/api/claudecode_proxy.go:202-208`). | The session identity model for POC steps 1–4. | Add a body-keys-only mode to `mitm_header_dump.py` (key paths plus a hash of each value, no raw values); compare two turns of one conversation with a new one. |
| Q3 | Can the CLI send `x-session-id` (for example via `ANTHROPIC_CUSTOM_HEADERS`)? | The proxy already reads the header (`claudecode_proxy.go:195`). | The "create session = pre-pin" API design. | Claude Code docs lookup (the `claude-code-guide` agent); one local request through the proxy with debug logging. |
| Q4 | Does the proxy talk HTTP/2 to `api.anthropic.com` while real Claude Code uses HTTP/1.1? | The capture shows HTTP/1.1 (`.reference/claude-code-headers-20260923.txt:15`). The proxy's Claude Code client uses DefaultTransport (`internal/claudecode/client.go:307-311`). | Whether a tuned Claude Code transport must pin the protocol. This is also an existing detection risk. | A pcap of proxy→Anthropic plus `tshark -e tls.handshake.extensions_alpn_str`; alternatively a Go `httptrace` probe. |
| Q5 | How does agy reuse connections (idle close timing, TCP keepalive probes)? | The Cloud Code transport sets `IdleConnTimeout 90s` and has no dialer (`internal/cloudcode/client.go:170-175`). No agy timing data exists. | Whether any Cloud Code transport setting may ever be exposed; it stays frozen by default. | One long `tcpdump` of agy idle and active periods, counting SYN, FIN and keepalive ACKs over time, then the same for the proxy. |
| Q6 | Is agy's `sessionId` per process or per conversation? | The proxy uses one per account per process (`internal/format/builder.go:14-45,77`). A grep for `session\|heartbeat\|keepalive\|ping` over `.reference/grpc-methods.txt` and `proto-messages.txt` returns 0 hits. | Antigravity session semantics; no change until there is evidence. | `strings agy \| grep -i sessionid`; the existing agy mitm harness logging the value of `sessionId` across two conversations. |

**Suggested order:**
1. Q3, then Q2. They are cheap and unblock the session POC.
2. Q4 and Q5 together, since they can share one pcap session.
3. Q6.
4. Q1 last. It is the biggest probe and only gates the cloud-emulation branch.

## Done when

- Every question Q1–Q6 has a recorded decision:
  - the chosen probe;
  - who runs it;
  - the exact command or script outline;
  - the expected evidence file name under `.reference/`;
  - the decision each possible outcome leads to.
- A question may also end as "deferred" or "dropped", but only with the user's stated reason.
- The decisions are written to `docs/superpowers/specs/2026-09-28-claude-code-cloud-open-questions.md` on `feat/claude-code-cloud`.
- The user has reviewed that file.
- Commit and push only when the user asks.
