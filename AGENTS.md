# Project: antigravity-go-proxy

Go Cloud Code proxy whose network fingerprint is **identical to the official
`agy` CLI**.

## The one rule that matters most
This build is a disguise: a *normal Go HTTPS client is the disguise*, because
that is how current `agy` reaches Cloud Code. **Do NOT touch TLS internals.**
Use an empty `tls.Config{}` through the standard HTTP transport. No `utls`, no
custom `CipherSuites`, no custom `CurvePreferences`, no custom `NextProtos`.
Go 1.27rc2 + default crypto/tls reproduces `agy`'s JA3/JA4 for free.
Overriding anything *breaks* the match.

The rule covers the Cloud Code path. The Zen and Claude Code upstreams are Bun
(BoringSSL) clients, which Go cannot imitate, so they replay recorded
ClientHellos through utls in `internal/tlsreplay` (ADR 0004). Never route Cloud
Code through it, and never hand-build a hello: recapture the real client's.

## Ground truth (already gathered — don't re-derive)
- `.reference/grpc-methods.txt` — real `CloudCode/*` gRPC methods from the binary.
- `.reference/proto-messages.txt` — real `v1internal.*` protobuf message names.
- `.reference/agy-current-capture.pcap` — current Cloud Code ClientHello.
- `.reference/agy-current-baseline.txt` — current `agy` fingerprint baseline.
- `.reference/go-current-baseline.txt` — matching Go proxy baseline.

## Reference code (read, port faithfully — do not redesign)
- `/root/antigravity-claude-proxy/src/` — historical business-logic reference:
  `format/` (Anthropic↔Google conversion + schema sanitizer), `cloudcode/`
  (message-handler backoff/rotation), `constants.js` (request identity), and
  `auth/agy-token.js` (token reader).
- `/root/hermes-claude-auth/anthropic_billing_bypass.py` — philosophy reference
  for byte-exact client masking (not reused directly).
- The `agy` binary: `/root/.local/bin/agy`. Cross-check every constant against it
  with `strings`.

## Key facts
- Go 1.27rc2 installed. `protoc`/`protodump` are NOT required for normal proxy
  development.
- agy OAuth token: `~/.gemini/antigravity-cli/antigravity-oauth-token`.
  Never commit OAuth client credentials; obtain refresh values from the
  installed `agy` executable only when a refresh is needed.
- Target host: every Cloud Code call (generation, metadata, provisioning) goes
  only to `daily-cloudcode-pa.googleapis.com:443` (agy 1.4.10 parity,
  `.reference/agy-1.4.10-wire-20261030.txt`; thought signatures are tied to the
  issuing host, so there is no cross-host fallback).
- Local port: the binary defaults to **8080** (`127.0.0.1:8080`). `make install`
  restarts a running `~/.local/bin/antigravity-proxy` with the flags it was
  started with, so its port does not change. The Linux systemd unit
  `antigravity-go-proxy.service` sets **8091** via `ANTIGRAVITY_PROXY_LISTEN`.
  Before a live check, find the live port:
  `for p in 8080 8091; do curl -s -o /dev/null -w "$p %{http_code}\n" 127.0.0.1:$p/health; done`.
- Fingerprint gate command:
  `tshark -r <cap.pcap> -Y 'tls.handshake.type==1' -T fields -e tls.handshake.ja4`
  must match `.reference/agy-current-baseline.txt` for the Go proxy.

## Commands
- Build: `go build -o bin/proxy ./cmd/proxy`
- Git hooks (opt-in): `make install-hooks` points `core.hooksPath` at `scripts/git-hooks`, which refuses a commit whose staged Go files are not gofmt-clean. Bypass once with `git commit --no-verify` or `SKIP_GOFMT_HOOK=1`.
- Secret scanning (trufflehog; `brew install trufflehog`): `scripts/secret-scan.sh staged|push|history` (`make secret-scan` = history). The omp extension `.omp/extensions/trufflehog.ts` runs it before any `git commit` / `git push` issued through the bash tool and blocks on a finding (disable: `SECRET_SCAN_DISABLED=1`). CI: `.github/workflows/secret-scan.yml`. Findings are verified or unknown secrets; rotate a real one, never just delete it from the diff.
- Client drift (agy, opencode, Claude Code wire identity): the omp extension `.omp/extensions/client-drift.ts` runs `scripts/client_drift.py status` when a session starts. When an installed client binary has changed, it starts `client_drift.py run` in the background. The run captures each real client against its real upstream through mitmdump, with credentials redacted. It also captures this build's Cloud Code requests through `tools/clientprobe` and checks the WebUI sign-in client with Google. It then updates the pins in `PINS`/`HELLOS` in a private worktree of `fork/main`, runs the tests, and pushes `chore/client-identity-drift` with a PR. The PR is a draft when something needs a human. Needs `mitmdump`, `gh`, trufflehog and `~/.claude-oat` (`claude setup-token`; override with `CLIENT_DRIFT_CLAUDE_TOKEN_FILE`). By hand: `make client-drift` (capture and report only). State and logs: `~/.cache/antigravity-proxy/client-drift`. Disable: `CLIENT_DRIFT_DISABLED=1`. When you change a pinned value's spelling or location, update its `Site` in `scripts/client_drift.py`; `scripts/test_client_drift.py` fails until the registry matches the tree.
- Proto: `protodump -output ./proto /root/.local/bin/agy` then `protoc --go_out=gen --go-grpc_out=gen ...`
- Fingerprint test: capture with `tcpdump -i any -w /tmp/go.pcap host daily-cloudcode-pa.googleapis.com -c 30` while hitting the proxy (generation only goes to daily; on macOS use `tcpdump -i pktap,all -P` as in `.reference/fingerprint-recheck-20260924.txt`), then run the tshark gate above.
- Git remote: ALWAYS push to `fork` (`git push fork <branch>`). NEVER push to `origin`.

## Definition of done

The non-negotiable requirement is **packet-verified JA4 match**. Never
fabricate the proto schema — if it can't be recovered from the binary, stop and
report rather than inventing one (a wrong schema fails silently, looks like a
ban).

## Agent skills

### Issue tracker

GitHub issues via `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Default five-role triage labels. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context layout (`CONTEXT.md` + `docs/adr/`). See `docs/agents/domain.md`.

<!-- trail:start -->
## Trail — repo context graph

This repo is indexed in `.trail/cache/`: small linked markdown nodes that explain each
system and carry exact file:line spans, kept in sync with the code through git.

For ANY task here — understanding how something works, finding where code lives,
or scoping a change — get context from the graph before grepping or opening
source files. Re-ask freely (it's cheap) and reuse literal identifiers you
already have (symbol, error string, file name) as the query. New to this repo?
Run `trail map` first — a token-budgeted orientation (dir clusters, hubs,
hotspots), no LLM, no key.

- Run `trail ask "<your question>" --source` → ranked nodes with the relevant
  code spans inlined (each hit's ≤8-line crux by default; `--full` for whole
  definitions when the crux isn't enough). Match the tool to the task shape:
  for understanding or editing, the top node IS the answer — cite its
  `covers:` file:line spans and edit straight from `--source`. For
  exhaustive tasks ("every occurrence / every caller of this pattern"), ranked
  results are top-N, not complete — run `trail grep "<literal>"` instead
  (exhaustive over indexed files, grouped by enclosing symbol), falling back
  to raw `grep -rn` only for unindexed files.
- `trail skeleton <file>` → every definition's signature + span, ~10× cheaper
  than reading the file; use it to skim an API surface.
- `trail callers <symbol>` gives precomputed, exact edges — who calls this.
  Add `--direction out` for what it calls, or `--depth N` to walk
  transitively for the full blast radius. For structural questions, skip
  ranking and use this directly.
- Or browse: `.trail/cache/INDEX.md` lists every node; follow the links.
- Monorepos and folders of multiple repos rank fairly across sub-projects —
  hits carry `[scope/]` labels naming which one they're from. Narrow with
  `trail ask "<task>" --in <scope>/` once you know where you're working.

If a returned span is truncated ("+N more lines"), open the file at that exact
range before finalizing. Only open source files when a node genuinely lacks a
needed detail, and then at the exact file:line the node points to — never
re-read whole files.

After big code changes, refresh the graph with `trail build` (deterministic,
no API key, $0).
<!-- trail:end -->
