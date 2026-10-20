---
title: "PR 130 review fixes: httpwire safety, proxies, gateway bytes, conversation scope"
author: Gustavo
date: 2026-10-20
branch: fix/claude-code-2.1.304-identity
touches:
  - internal/httpwire/transport.go
  - internal/proxydial/proxydial.go
  - internal/tlsreplay/tlsreplay.go
  - internal/transport/claudecode.go
  - internal/api/dispatch.go
  - internal/api/claudecode_proxy.go
  - internal/ccidentity/turn.go
  - internal/ccidentity/replies.go
  - internal/claudecode/client.go
  - scripts/check_identity_policy.py
---

## Decided
httpwire must behave like net/http where it matters for a shared pool: skip 1xx (cap 5, a 101 is final and never pooled), never send a caller's Expect, resend a request only on a reused connection and only when the write failed or the method is GET/HEAD/OPTIONS, dial a fresh connection for that retry, and probe an idle connection before reuse (5 ms read deadline through TLS; an already-expired deadline returns a timeout without touching the socket, so it cannot be zero). Close before EOF closes the connection first, then drains; the decoder is guarded by a mutex so a concurrent Close does not race gzip/zstd. zstd window capped at 8 MiB (RFC 9659).
internal/proxydial owns how a connection reaches a proxy (http, https over crypto/tls, socks5/socks5h). tlsreplay tunnels https targets through it; httpwire sends plain-http requests to an http/https proxy in absolute form and pools them under a key that includes the proxy URL. Hello.Transport keeps base.Proxy for non-https targets.
The pooled Claude Code gateway writes only model and max_tokens into the client's raw bytes (ccidentity.SetObjectMembers); a mutated body is encoded with ccidentity.MarshalJS (no HTML escaping).
cc_prompt_id hashes a canonical prompt: string content as one text block, cache_control removed at every depth, <system-reminder>/empty text blocks dropped, sorted keys, numbers kept literal.
ReplyScope(upstream, session, turn) folds in the canonical digest of the conversation's first prompt, so a session's main thread, subagents and side queries cannot name each other's request in cc_prev_req. MessageRequest.Normalizes() is the one normalization rule; callers compute TurnOf once per client request and set ReplyScope only when Normalizes().
The 14-entry anthropic-beta list is the --print text mode; thinking-display-updates-2026-08-18 belongs to the connector_text display mode (thinking.display "updates"), seen only in the stream-json run.

## Tried and ruled out
Hashing the conversation prefix for the scope (an earlier learning ruled it out: cache_control moves and microcompaction change it). The first prompt's canonical digest is stable across those; a client that rewrites its first prompt gets a miss, not another conversation's id.
Retrying any stale-looking error: a POST whose bytes were written was processed, so a replay double-charges.

## Watch out
ProxyFromEnvironment never proxies localhost, and caches the environment per process: tests that exercise env proxies re-exec the test binary. A smoke run needs a non-loopback upstream name (e.g. upstream.test) and a relaying proxy.
The digest change moves cc_prompt_id once for conversations in flight at deploy.
