# Classifier Variants Default to a Hosted Typed-Decision Backend

## Context

ADR-0003 confines classifier translation to a matched rule whose action is `reroute`. Later work added the `laya` and `jev` formats, which speak `POST /v1/systemone` (typed decisions) instead of a chat wire. Using them still required an operator to write a rule, a backend and a footer pattern. The one deployment that tried it ran `rules: []` for weeks, and its Stage 1 calls went to the teacher or a gateway model.

Zen's hosted `jev-1.13-free` answers in 0.4-0.9 s and costs nothing, and the proxy already holds a Zen key for operators who enabled Zen.

## Decision

1. **Scoped Boundary.** A built-in route answers `stage1-severity` and `block-prefilter` requests from a `jev` backend. It runs only when `classifier.enabled` (or the env fallback) is on, `zen.enabled` is true and a Zen key resolves, and never under `classifier.action: "passthrough"`. `stage2-severity` is never routed.
2. **Order.** Operator rules, then the variant backend, then the existing `targetModel` / canned verdict / teacher handling. A request an operator rule already handled is not sent to the variant backend.
3. **Verdicts a typed-decision model may issue.** For Stage 1, severities from the Laya serving map, with C and D escalated on the built-in backend. For block-prefilter, only `<block>no</block>`, from a yes/no score below `jevBlockThreshold`. Claude Code treats any other block reply as a block, so a model that cannot explain itself must never produce one.
4. **Fail open to the teacher.** Every error and every escalation returns control to the existing handling.
5. **Per-variant opt-out.** `variants.<kind>.targetBackend` is empty (built-in), `off`, or the name of a configured backend.

## Consequences

- With Zen enabled, a deployment sends each graded shell action to Zen and on to TypeSafe without writing a rule. Zen states that Jev inputs are not used for training and are retained under TypeSafe's privacy policy.
- ADR-0003's "a matched rule" boundary is widened for these two variants. The main-route invariant is unchanged: no chat traffic is translated.
- A `no` ends the check for that action, so a wrong `no` is an unreviewed allow. The default line of 0.10 rests on 21 synthetic probes and nine live Stage 1 rows; `scripts/replay_jev_block.py` exists to measure it on a real corpus.
- The free tier is time-limited. When it ends the route fails open to the teacher until a named backend sets `model: "jev-1.13"`.
