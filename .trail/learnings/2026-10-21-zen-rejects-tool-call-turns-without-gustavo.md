---
title: Zen rejects tool-call turns without reasoning_content
author: Gustavo
date: 2026-10-21
branch: fix/zen-reasoning-replay
touches:
  - docs/plans/2026-10-15-custom-endpoint-model-discovery.md
  - docs/superpowers/plans/2026-09-30-jev-classifier-backend.md
  - docs/superpowers/plans/2026-09-30-pr106-remediation.md
  - docs/superpowers/plans/2026-09-30-pr106-review-round2.md
  - docs/superpowers/plans/2026-09-30-pr106-review-round3.md
  - docs/superpowers/plans/2026-09-30-pr108-remediation.md
  - docs/superpowers/plans/2026-09-30-pr108-review-round2.md
  - docs/superpowers/plans/2026-09-30-pr109-remediation.md
  - docs/superpowers/plans/2026-09-30-pr111-remediation.md
  - docs/superpowers/plans/2026-09-30-responses-store-false.md
---

## Decided
The requirement is model-level, not request-level: Zen returns 400 for a replayed assistant turn with tool_calls and no reasoning_content even when the request carries no thinking field. The proxy fills a constant placeholder ("(reasoning unavailable)") in assistantToChat for that shape. Any non-empty reasoning string passes; an empty string fails.

## Tried and ruled out
- Reasoning memory alone (plan F1): cannot cover a turn where the model emitted no reasoning at all.
- Narrow fix on the last assistant turn only: rejected, because the same shape alternates 200/400 across zen backends, so position is not the predicate.
- Disabling thinking in the client: rejected, because the mandate applies without the thinking field.

## Watch out
The same body can return 200 or 400 within seconds depending on the upstream route. Bisect a fixed body at least three times before claiming a fix. Reproducer: assistant tool_use turn with no thinking, conversation ending in a bare tool_result.
