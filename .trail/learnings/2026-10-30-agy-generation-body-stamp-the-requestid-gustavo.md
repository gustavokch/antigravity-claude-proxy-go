---
title: "agy generation body: stamp the requestId at send, hash a string first prompt as a text block, guard cancel left open"
author: Gustavo
date: 2026-10-30
cost: { minutes: 16, tokens: 383630 }
id: l-d7fa5153
scope: repo
status: active
src: agent
touches:
  - internal/format/agyrequest.go
  - internal/format/format_test.go
  - internal/format/outputguard.go
  - docs/superpowers/plans/2026-10-30-pr142-remediation.md
  - internal/accounts/agy_conversation_test.go
  - internal/accounts/dispatcher.go
  - internal/format/builder.go
---

## Decided
- The requestId time (agent/<cascade>/<unix ms>/...) is set when the request is sent, not when the payload is built. agy builds it 1-160 ms before the request leaves; the dispatcher sleeps between build and send (throttle delay, capacity backoff, the short 429 retry).
- The conversation seed hashes a string first prompt as the one text block it stands for. Clients that mark only their newest messages for caching send the first prompt as a block on turn 1 and as a string later; without this the cascade changed at turn 2.
- writeTrajectoryAcls and the title checkpoint go out only with a labelled agent call: agy's ACL trajectoryId is always the first call's labels.trajectory_id.

## Tried and ruled out
- Nothing tried and dropped in this round.

## Watch out
- Open decision: the OutputGuard ends a response at the client's max_tokens by closing the upstream body (an HTTP/2 RST_STREAM). agy sends the catalog maximum and reads every stream to its finish, so small-max_tokens side requests now end with a cancel. The other option, deliver the MAX_TOKENS finish to the client and drain upstream in the background, costs quota. Not changed; ask the owner before changing it.

In the code: Builder.Stamp (internal/format/builder.go), firstUserText and ConversationStart (internal/format/agyrequest.go), OutputGuard (internal/format/outputguard.go), Dispatcher.StreamGenerateContent (internal/accounts/dispatcher.go). Review: PR 142 comment 6524802030.
