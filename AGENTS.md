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
- Target host: generation goes only to `daily-cloudcode-pa.googleapis.com:443`
  (agy 1.2.10 parity; thought signatures are tied to the issuing host, so there
  is no cross-host fallback); metadata and provisioning go to
  `cloudcode-pa.googleapis.com` and then daily.
- Local port **8091**.
- Fingerprint gate command:
  `tshark -r <cap.pcap> -Y 'tls.handshake.type==1' -T fields -e tls.handshake.ja4`
  must match `.reference/agy-current-baseline.txt` for the Go proxy.

## Commands
- Build: `go build -o bin/proxy ./cmd/proxy`
- Git hooks (opt-in): `make install-hooks` points `core.hooksPath` at `scripts/git-hooks`, which refuses a commit whose staged Go files are not gofmt-clean. Bypass once with `git commit --no-verify` or `SKIP_GOFMT_HOOK=1`.
- Proto: `protodump -output ./proto /root/.local/bin/agy` then `protoc --go_out=gen --go-grpc_out=gen ...`
- Fingerprint test: capture with `tcpdump -i any -w /tmp/go.pcap host cloudcode-pa.googleapis.com -c 30` while hitting the proxy, then run the tshark gate above.

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

<!-- graft:start -->
## Graft — repo context graph

This repo is indexed in `graft/`: small linked markdown nodes that explain each
system and carry exact file:line spans, kept in sync with the code through git.

For ANY task here — understanding how something works, finding where code lives,
or scoping a change — get context from the graph before grepping or opening
source files. Re-ask freely (it's cheap) and reuse literal identifiers you
already have (symbol, error string, file name) as the query. New to this repo?
Run `graft map` first — a token-budgeted orientation (dir clusters, hubs,
hotspots), no LLM, no key.

- Run `graft ask "<your question>" --source` → ranked nodes with the relevant
  code spans inlined (each hit's ≤8-line crux by default; `--full` for whole
  definitions when the crux isn't enough). Match the tool to the task shape:
  for understanding or editing, the top node IS the answer — cite its
  `covers:` file:line spans and edit straight from `--source`. For
  exhaustive tasks ("every occurrence / every caller of this pattern"), ranked
  results are top-N, not complete — run `graft grep "<literal>"` instead
  (exhaustive over indexed files, grouped by enclosing symbol), falling back
  to raw `grep -rn` only for unindexed files.
- `graft skeleton <file>` → every definition's signature + span, ~10× cheaper
  than reading the file; use it to skim an API surface.
- `graft callers <symbol>` gives precomputed, exact edges — who calls this.
  Add `--direction out` for what it calls, or `--depth N` to walk
  transitively for the full blast radius. For structural questions, skip
  ranking and use this directly.
- Or browse: `graft/INDEX.md` lists every node; follow the links.
- Monorepos and folders of multiple repos rank fairly across sub-projects —
  hits carry `[scope/]` labels naming which one they're from. Narrow with
  `graft ask "<task>" --in <scope>/` once you know where you're working.

If a returned span is truncated ("+N more lines"), open the file at that exact
range before finalizing. Only open source files when a node genuinely lacks a
needed detail, and then at the exact file:line the node points to — never
re-read whole files.

After big code changes, refresh the graph with `graft build` (deterministic,
no API key, $0).
<!-- graft:end -->
