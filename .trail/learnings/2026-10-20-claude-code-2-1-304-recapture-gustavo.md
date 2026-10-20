---
title: Claude Code 2.1.304 recapture and identity update
author: Gustavo
date: 2026-10-20
branch: main
touches:
  - docs/adr/0004-claude-code-identity-normalization-exception.md
  - docs/classifier-fallback-notes.md
  - internal/api/ccidentity_proxy_test.go
  - internal/ccidentity/apply.go
  - internal/ccidentity/apply_test.go
  - internal/ccidentity/defaults.go
  - internal/ccidentity/defaults_test.go
  - internal/claudecode/client_test.go
  - internal/claudecode/types.go
  - internal/webui/public/views/settings.html
---

## Decided
Claude Code client updates are tracked by a fresh capture, not by the gate. `scripts/verify-claude-code-identity.sh` compares the proxy with a frozen JSONL, so it cannot see a new client version. On 2026-10-20 the stable client was 2.1.304 (`npm view @anthropic-ai/claude-code dist-tags`); the new baseline is `.reference/claude-code-headers-20261020.jsonl` plus `.txt` and the `-multiprompt.jsonl`. Procedure is in `docs/classifier-fallback-notes.md`, section "When Claude Code ships a new version".
Both `scripts/capture-claude-code-headers.sh oauth` and `scripts/verify-claude-code-identity.sh` ran from the agent shell when the user asked for them in so many words. The older learning that the user runs these is about the default, not a block.
The capture image is a thin layer on `claude-box:mitm` selected with `CLAUDE_CAPTURE_IMAGE`; `~/Git/claude-container/Containerfile` still pins 2.1.280.

## Tried and ruled out
One prompt per `--print` process: it hid that `cc_prompt_id` and `cc_turn_index` change per user prompt. Piping three prompts 12 s apart into `claude --print --input-format stream-json --output-format stream-json` showed it. Without the delay, queued prompts coalesce into one request.
Deriving `cc_turn_index` from the request `messages` count: the message shapes grow by 3 per request, not 2, so the count cannot be decoded from the fingerprint. Not implemented.

## Watch out
The proxy still sends first-prompt values (`cc_prompt_index=0`, `cc_turn_index=1`) and a session-stable `cc_prompt_id`. From a conversation's second prompt on, those differ from the real client. Fixing it needs a per-session prompt counter that no caller keeps.
An empty session key made `derivedUUID` return a new random UUID on every call, so `x-claude-code-prompt-id` and `cc_prompt_id` disagreed, and so did `X-Claude-Code-Session-Id` and `metadata.user_id.session_id`. Unit tests with a session key passed; only the live gate caught it. `claudecode.IdentityConfig.WireIdentity` now seeds a random key. Build every Identity through it.
The gate shows the proxy talks HTTP/2 to the interceptor while the real client uses HTTP/1.1. The differ exempts case across versions; ALPN parity is a separate open question.
