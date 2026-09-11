# Spec: Gemini Prompt-Cache Prefix Stability

**Status:** investigated, verified, ready to implement
**Investigation input:** `/tmp/handoff-cache-misses-plan.md` (prior session handoff)
**Reproduction tests (parked, not in tree):** `/tmp/cacheprefix_test.go.repro`

## Problem statement

A Pi coding-agent session against `gemini-3.8-flash-high[1m]` through this proxy
reported 1,522,379 input tokens with only 43.9% cached, 632,782 tokens
"re-billed" across 28 cache misses. Gemini implicit context caching requires the
request prefix to be byte-identical from one turn to the next. The proxy mutates
that prefix.

## Verified evidence

### E1 — the client cannot round-trip thought signatures

Source session
`/Users/gus/.pi/agent/sessions/--Users-gus-Git-zimqa--/2026-09-11T02-40-41-502Z_01a08e56-b21e-70e2-9ece-1b6af8fe0ee9.jsonl`
(85 lines, 82 messages) contains:

- 41 `assistant` messages, 40 `toolCall` blocks, 40 `toolResult` messages
- 5 `thinking` blocks, all with `thinkingSignature: ""` (empty)
- every `toolCall` block has exactly the keys `arguments, id, name, type` — no
  `thoughtSignature`

So on every subsequent request the proxy sees zero valid thinking signatures and
zero tool-call signatures.

### E2 — `needsThinkingRecovery` therefore fires on every tool-loop turn

`internal/format/thinking.go:187` returns true when the conversation is in a
tool loop and the last assistant message has no *thinking* part carrying a valid
signature. `messageHasValidThinking` (`thinking.go:268`) only inspects thinking
parts; a `tool_use` block carrying a `thoughtSignature` does not count. Combined
with E1, recovery fires on essentially every turn.

### E3 — recovery appends two messages the client never stores

`closeToolLoopForThinking` (`thinking.go:206-213`) appends
`{role: assistant, text: "[Tool execution completed.]"}` and
`{role: user, text: "[Continue]"}`. The client does not record them. On the next
turn the same array index holds a real `functionCall` instead.

Reproduced deterministically — turn 3 vs turn 4 of a Pi-shaped tool loop:

```
prefix diverged at index 7
 turn N:   {"parts":[{"text":"[Tool execution completed.]"}],"role":"model"}
 turn N+1: {"parts":[{"functionCall":{"args":{"path":"file.go"},"name":"read"},
             "thoughtSignature":"skip_thought_signature_validator"}],"role":"model"}
```

### E4 — signature-cache expiry rewrites historical parts

`internal/format/content.go:59-68` fills a missing `thoughtSignature` from the
process-local `SignatureCache`, else uses the constant
`skip_thought_signature_validator` (`model.go:13`). The cache has a 2-hour TTL
(`signature_cache.go:8`) and no persistence, so the same client history converts
differently before and after expiry or a proxy restart. Reproduced:

```
history part changed when signature cache expired
 warm: {"functionCall":{...},"thoughtSignature":"sig-..."}
 cold: {"functionCall":{...},"thoughtSignature":"skip_thought_signature_validator"}
```

This breaks the prefix at the *first* tool call, not just the tail.

### E5 — Google's documented contract

Gemini 3 validates thought signatures **only for function calls in the current
turn**; calls in previous turns are not validated. The current turn starts at the
newest user message with ordinary content (a `functionResponse` does not open a
new turn). `skip_thought_signature_validator` is the documented bypass value.
Google explicitly discourages replacing tool turns with plain text, which is what
`closeToolLoopForThinking` does.

### E6 — the "re-billed" number is partly a client-side metric artifact

Pi computes `missedTokens = min(prev.promptTokens, promptTokens) - cacheRead`
(`.../pi-coding-agent/dist/core/cache-stats.js`). That formula assumes Anthropic
semantics, where a cache hit covers the whole reusable prefix. Gemini implicit
caching activates only above ~32k tokens and quantizes checkpoints to ~4k blocks,
so an unaligned tail is always uncached and always counted as "missed". The
proxy's mapping (`input_tokens = promptTokenCount - cachedContentTokenCount`,
`cache_read_input_tokens = cachedContentTokenCount`) is already correct Anthropic
semantics. **No mis-reporting fix is warranted; reporting anything else would be
a lie.** Only documentation and small hardening apply.

### E7 — one cold miss was a legitimate TTL expiry

Turn 64 at 02:45:31.822Z, turn 66 at 02:52:08.965Z — 6m37s apart against a
5-minute implicit-cache TTL. Not a defect.

### E8 — open V1 violation: thinking parts still depend on process-local cache state

`internal/format/content.go:105-119`, specifically the `cache.ThinkingFamily`
read at `:110` and the drop at `:114-116` (mirrored at
`internal/format/thinking.go:244-252`).

A `thinking` block with a valid signature is emitted into `contents` only if
`SignatureCache` still vouches that a Gemini model produced that signature.
The cache is process-local with a 2-hour TTL (`signature_cache.go:8`) and no
persistence.

Reproduced warm-vs-cold divergence:

```
warm: [{"parts":[{"text":"hi"}],"role":"user"},
       {"parts":[{"text":"reasoning text","thought":true,"thoughtSignature":"zzz…"},
                 {"functionCall":{…},"thoughtSignature":"skip_thought_signature_validator"}],"role":"model"},
       {"parts":[{"functionResponse":…}],"role":"user"}]
cold: [{"parts":[{"text":"hi"}],"role":"user"},
       {"parts":[{"functionCall":{…},"thoughtSignature":"skip_thought_signature_validator"}],"role":"model"},
       {"parts":[{"functionResponse":…}],"role":"user"}]
```

Affected client class: any client that echoes `signature` back on thinking blocks
(Claude Code does; the Pi client studied in E1 does not, sending `thinkingSignature: ""`).
Turn N served from a warm cache carries the thought part. Two hours later, or
after proxy restart or redeploy, turn N+1 drops it. The prefix breaks at the
first thinking block, and the model loses its prior reasoning from history.

Why not fixed here: the family decision must travel in the block the client
echoes back rather than live in process state. That is a design change with its
own spec. A naive deletion of the lookup would let a Claude-originated signature
be posted to Gemini.

## Invariant to establish

**V1 (narrowed to tool calls).** For a fixed client-visible message history, the
Google `contents` array produced by `ConvertAnthropicToGoogle` must be a pure
function of that history for all `functionCall` parts: no synthetic messages the
client cannot echo back, and no dependence on process-local ephemeral state for
tool calls. Formally: `contents(turn N)` must be an exact prefix of
`contents(turn N+1)` across tool-use loops. This invariant holds for
`functionCall` parts. It does NOT yet hold for thinking parts (see E8).

## Open question gating the fix

Does the Cloud Code / Antigravity backend honor
`skip_thought_signature_validator` on a **current-turn** `functionCall`? Public
Gemini docs say yes; some Vertex surfaces reportedly reject it. Removing the
synthetic recovery pair moves the real unsigned `functionCall` into the current
turn, so this must be probed live before the change is trusted. A kill switch
restores the old behavior if the probe fails.

## Non-goals

- Explicit (paid) Gemini context caching.
- Changing how `cache_read_input_tokens` is computed.
- Fixing Pi's miss formula.

## Follow-up work

- **Thinking signature statelessness (E8):** Make the thinking-block family decision
  derivable from the request itself rather than process-local `SignatureCache` state.
  Tag the signature at emission or key off the request family so the family travels
  in the block the client echoes back.
