# Zen gateway: OMP client notes

## `models.yml`: disable unsigned-thinking replay

Translated Zen wires (Chat Completions, Responses) return `thinking` blocks
with an empty `signature` (`ChatResponseToAnthropic`). Those blocks are not
Anthropic-signed, so OMP must not replay them to the model on later turns. Set
this on every OMP model that routes through the Zen gateway:

```yaml
compat:
  replayUnsignedThinking: false
```

## OpenCode identity preservation

On `/v1/messages`, `/v1/systemone` and the translated Chat/Responses wires, a
caller is treated as a genuine OpenCode client when its `User-Agent` starts
with `opencode/` or it sends any of `x-opencode-session`, `-client`,
`-project`, `-request`. Those values are forwarded unchanged; missing fields
come from config / `OPENCODE_VERSION` / `OPENCODE_CLIENT` / defaults. A foreign
`User-Agent` on an opencode-labelled request is replaced with
`opencode/<version>`. Every other caller gets the full disguise.

## Empty-stop fallback

A translated turn that ends with `end_turn` and no non-whitespace text (all
tool calls dropped as gate-injected, reasoning only, no `choices`) gets a
synthetic text block, `No response text.` (`emptyStopFallbackText`). Without
it OMP classifies the turn as an empty assistant stop and retries until its
cap.

The block is a real assistant text block, so it is part of the conversation
history and the model sees it on later turns. Turns ending in `tool_use` or
`max_tokens` are never padded.
