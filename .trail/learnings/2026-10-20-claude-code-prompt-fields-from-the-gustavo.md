---
title: "Claude Code prompt fields from the body, and HTTP/1.1 on the wire"
author: Gustavo
date: 2026-10-20
branch: fix/claude-code-2.1.304-identity
touches:
  - docs/adr/0004-claude-code-identity-normalization-exception.md
  - docs/classifier-fallback-notes.md
  - internal/api/server.go
  - internal/ccidentity/apply.go
  - internal/ccidentity/defaults.go
  - internal/ccidentity/profile.go
  - internal/claudecode/client.go
  - internal/claudecode/wireidentity_test.go
  - internal/transport/transport.go
  - scripts/check_identity_policy.py
---

## Decided
Prompt fields come from the request body, with no stored state. `ccidentity.TurnOf(body)` counts user messages that carry a prompt (not tool_result-only, not empty or `<system-reminder>`-only text) and hashes the latest one. `cc_turn_index` is the count; `cc_prompt_id` and `x-claude-code-prompt-id` derive from session key + count + digest. A tool follow-up repeats its prompt's values, the next prompt gets new ones, and a retry or cache-bump replay of the same body sends the same values.
Callers compute one Turn per request and pass it to both ApplyBody and ApplyHeaders (`SendMessage`, `forwardToCustomEndpoint`). `MessageRequest.Turn` is now `PrevRequestID`; the prompt fields are not a caller input.
HTTP/1.1: `transport.UpstreamHTTP1` (Protocols = HTTP1 only, ALPN `http/1.1`) backs `claudecode.ccTransport` and `customEndpointIdentityTransport`, which the custom-endpoint path uses only when it normalizes. The real client is HTTP/1.1 in all four committed captures. Unnormalized custom-endpoint traffic keeps the shared h2 transport.
The gate now fails on an h2 request (`diff_claude_code_identity.py`, `check_identity_policy.py`), and its keyless custom-endpoint phase sends two prompts and asserts `cc_turn_index=2`.

## Tried and ruled out
A per-session prompt counter, as the 2026-10-20 recapture learning proposed: it needs a store and an eviction rule, and a retry or a cache-bump replay would advance it. Counting prompts in the body is idempotent.
Prompt id from the count alone: a client that compacts its history sends a shorter conversation, the count drops, and an old id is reused. The digest of the latest prompt prevents it.
Replaying a Bun ClientHello through utls for Claude Code, as internal/zen does for OpenCode: no Claude Code ClientHello capture exists, and only ALPN was asked for.

## Watch out
The prompt rule is calibrated on the capture's message SHAPES, which are type-erased, so it was never run against a real Claude Code transcript. If a client's tool results carry text that is not a `<system-reminder>`, every tool round trip reads as a new prompt.
Still different from the real client: `cc_prev_req` (needs the previous response's request id kept per session), `cc_prompt_index` (0 in every capture), and the TLS ClientHello beyond ALPN.
Tests that need a TLS server must trust its certificate on a CLONE of the shipped transport. The shared transports cache their pools, so mutating them leaks across tests.
