---
title: "agy generation body: stamp the requestId at send, hash a string first prompt as a text block, finish the client at max_tokens and read upstream to the end"
author: Gustavo
date: 2026-10-30
cost: { minutes: 1, tokens: 1505 }
id: l-224fa0c8
scope: repo
status: active
replaces: l-d7fa5153
src: agent
touches:
  - internal/format/builder.go
  - internal/format/agyrequest.go
  - internal/format/outputguard.go
  - internal/accounts/outputlimit.go
  - cmd/proxy/main.go
---

## Decided
- The requestId time (agent/<cascade>/<unix ms>/...) is set when the request is sent, not when the payload is built. agy builds it 1-160 ms before the request leaves. The dispatcher sleeps between build and send (throttle delay, capacity backoff, the short 429 retry).
- The conversation seed hashes a string first prompt as the one text block it stands for. Clients that mark only their newest messages for caching send the first prompt as a block on turn 1 and as a string later. Without this, the cascade changed at turn 2.
- writeTrajectoryAcls and the title checkpoint go out only with a labelled agent call: agy's ACL trajectoryId is always the first call's labels.trajectory_id.
- At the client's max_tokens (the owner chose this, 2026-10-30): the client gets the MAX_TOKENS finish at once. Upstream is read to EOF in the background and dropped, never cancelled, because agy reads every stream to its end and a closed body resets the HTTP/2 stream. The read's context is WithoutCancel(client ctx). The client's cancel reaches it only until the finish. After that only Dispatcher.Close (called after the HTTP drain) cancels it. No new size or time limit: a hung stream keeps only the transport's timeouts.
- The background read keeps the account's in-flight count until EOF, because upstream still serves the request and the 429 journal calibrates concurrency from that count. It records credits and the live quota at EOF, because upstream reports consumption at the end. The client side (context, MarkSuccess, recovery journal, execution metadata) is released at the finish.

## Tried and ruled out
- Cancelling at the limit (the first version): every small-max_tokens request ended with a reset agy never sends.
- Cutting the tail with a size or time limit: the owner ruled it out.

## Watch out
- A request with a small max_tokens now uses upstream quota up to the catalog maximum, as agy's do.
- consume runs on the reader's goroutine for a guarded stream. A panic in it before the finish is raised again on the caller's goroutine. Nothing may call consume after the dispatcher returns.

In the code: Builder.Stamp (internal/format/builder.go); firstUserText and ConversationStart (internal/format/agyrequest.go); OutputGuard (internal/format/outputguard.go); Dispatcher.sendStream, Close, goBackground, detach and settle (internal/accounts/outputlimit.go); Dispatcher.Close in cmd/proxy/main.go. Review: PR 142 comment 6524802030.
