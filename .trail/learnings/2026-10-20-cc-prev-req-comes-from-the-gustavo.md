---
title: "cc_prev_req comes from the latest assistant reply, not the session"
author: Gustavo
date: 2026-10-20
branch: fix/claude-code-2.1.304-identity
touches:
  - ".reference/claude-code-headers-20261020.txt"
  - docs/adr/0004-claude-code-identity-normalization-exception.md
  - docs/classifier-fallback-notes.md
  - internal/api/cachebump_server.go
  - internal/api/ccidentity_wire_test.go
  - internal/api/claudecode_proxy.go
  - internal/api/server.go
  - internal/ccidentity/apply.go
  - internal/ccidentity/apply_test.go
  - internal/ccidentity/profile.go
---

## Decided
cc_prev_req is per conversation, not per session. In 2.1.304 the billing builder gets `previousRequestId` from a function that walks the outgoing messages back from the end and returns the `requestId` of the latest assistant message that has one (and is not an API-error message). The builder writes the field only when it matches `^req_[A-Za-z0-9_-]{1,36}$`. I found this by extracting the binary from `localhost/claude-box:mitm-2.1.304` and searching for `cc_prev_req` and `previousRequestId`. All 4 follow-ups in the captures name the `request-id` response header of the request before them.
The proxy's clients send no request ids, so `ccidentity.ReplyLog` records each normalized reply under its `request-id`. The keys are the reply's tool call ids, thinking signatures, redacted-thinking data and text blocks. SSE goes through `Watch` and unary bodies through `RecordMessage`. A later request looks up its latest assistant message (`Turn.latestReply`, set by TurnOf). The scope is `ReplyScope(upstream, session)`: `claudecode:<acc.ID>` or `endpoint:<url>`, plus the session key. An unkeyed request has an empty scope and sends nothing.
Cache-bump replays look up a value but never record their own reply, because the client never sees it.

## Tried and ruled out
A per-session "last request id" store, which my earlier learning today proposed. Subagents and side queries share the session id, so the chains of different conversations would mix. The real value comes from the conversation's own latest reply.
Keying on a hash of the conversation prefix. Claude Code moves cache_control and microcompacts old tool results, so the prefix changes between requests.
Walking further back when the latest assistant message is a miss (the real client does this). On the proxy side, a miss means the log dropped or never saw the reply, so the walk would name an older request. The field is omitted instead.

## Watch out
Setting HOME for a smoke run of the built binary breaks config loading. Set only ANTIGRAVITY_CONFIG_DIR.
The log is in memory and bounded (1<<16 keys, oldest first). After a restart, each conversation's next request sends no cc_prev_req until it chains again.
If a client rewrites the text of a reply and drops its thinking, and the reply has no tool call, nothing matches and the field is omitted.
