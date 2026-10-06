# Reasoning, Thinking and Output-Limit Parameters

How the proxy treats the parameters that control how much a model thinks and how much it may write, per route, and where it deliberately differs from the vendor. Last verified 2026-10-05. Every row names the document that owns the convention; evidence the proxy produced itself lives in `.reference/`.

## 1. Request fields the proxy reads

| Field | Convention (owner) | Proxy behavior |
|---|---|---|
| `max_tokens` | Anthropic Messages API: required, `minimum: 0` (`0` = cache pre-warm). Kimi Messages API: required, `minimum: 1`. | Cloud Code: sent as `maxOutputTokens`; absent stays absent. Gateways: `applyMaxTokensPolicy` — clamp down to the entry's limit, floor 16, never raise above the client's value, omit when no limit is known. |
| `thinking.type` | Anthropic Thinking doc: `enabled`+`budget_tokens` (deprecated on 4.6, rejected on 4.7+), `adaptive`, `disabled`. | `disabled` → thinking off (a tiered Gemini 3.7/3.8 route drops to its lowest tier: Google does not allow full off). `adaptive` → the catalog's default depth. `enabled` → `budget_tokens` is honored. |
| `thinking.budget_tokens` | Anthropic: ≥ 1024 and < `max_tokens`. | Honored on budget-style routes; on Claude it is kept below the final `maxOutputTokens`. |
| `thinking.display` | Anthropic: `summarized` / `omitted`. | Not honored, by design: Cloud Code returns the thought text and the client collapses it. Signatures are always returned and must be replayed. |
| `output_config.effort` | Anthropic Effort doc: `low`, `medium`, `high`, `xhigh`, `max`; Claude Code sends it on every request; `agy --effort` accepts the same five. | Read, at the lowest precedence (§3). Forwarded untouched by every gateway. |
| `output_config.format` | Anthropic structured outputs. | Not translated on the Cloud Code route. |
| `reasoning_effort`, `reasoning` (string or `{"effort"}`), `thinking_budget` | Proxy extensions (OpenAI / Gemini spellings). | Highest precedence. `none`/`off`/`false`/`0` turn thinking off; an unknown spelling is ignored. On `/v1/chat/completions` the OpenAI spellings `reasoning_effort` and `reasoning.effort` are translated to `output_config.effort` (`minimal` becomes `low`, `none` becomes `thinking: {type: disabled}`, unknown spellings are dropped) and the proxy-only field itself is never forwarded; the resulting effort is ambient (§3) and flows to gateway routes untouched. |
| `temperature`, `top_p`, `top_k`, `stop_sequences` | Anthropic accepts them; real Claude Code sends none of them (`.reference/claude-code-headers-20260923.txt`). | Cloud Code route: copied (see §9 for the thinking-time rule). Gateways: untouched. |

## 2. Levels and the fallback rule

Levels: `low < medium < high < xhigh < max` (plus `minimal`, which only OpenAI-shaped clients send). A level a model does not publish runs as **the highest published level at or below it**; a level below everything published takes the lowest. This is Claude Code's own rule ("`xhigh` runs as `high` on Opus 4.6").

| Route | Published levels | Consequence |
|---|---|---|
| Gemini flash tiers (3.8 / 3.7 / 3.6, legacy 3.5 repointed) | `low`, `medium`, `high` | `xhigh` and `max` route to the `-high` tier; `minimal` to `-low`. |
| Budget-style Gemini (3.1 Pro), GPT-OSS | `low`, `medium`, `high` | same fallback, then the budget in §5. |
| Claude 4.6 (Opus, Sonnet) | `low`, `medium`, `high`, `max` (Effort doc: no `xhigh` on 4.6) | `xhigh` → `high`; `max` → `high` as well: the proxy's budget table has no level above `high` and the larger `max` budget (gate GD) is UNVERIFIED, so it was not applied. |

## 3. Precedence

Tier routing (catalog), highest first: `reasoning_effort` / `reasoning` → a bare thinking budget (back-mapped: ≤ 2048 `low`, < 12000 `medium`, else `high`) → a tier named in the model ID (`…-low|-medium|-high|-extra-low`) → `output_config.effort` → the catalog default. Budget emission (converter), highest first: `reasoning_effort` → explicit `budget_tokens` / `thinking_budget` → `output_config.effort` → the catalog's default budget.

Why `output_config.effort` is last: Claude Code sends it on every request. If it outranked the model ID, anyone who picked `gemini-3.8-flash-low` would be rerouted to `-high` by their default `high` effort.

## 4. Per-route behavior

| Route | Reasoning fields | Limits |
|---|---|---|
| **Cloud Code** (Gemini, Claude 4.6, GPT-OSS via agy) | Translated (§§2–3, 5). Tiered flash routes get `thinkingLevel`; everything else gets a budget; never both. | Gemini output is capped at the fixed 16,384 ceiling, and `/v1/models` advertises the capped value, because gate GE (is `maxOutputTokens` 65536 accepted?) is UNVERIFIED; a live catalog limit below the ceiling still applies. Claude and GPT-OSS use the live catalog's `maxOutputTokens`. |
| **Claude Code gateway** (OAuth → Anthropic) | Forwarded untouched (ADR-0001/0004). | Defaults below (§6); client `max_tokens` is clamped down to the entry's limit. |
| **Kimi gateway** | Forwarded untouched. | Operator-set per entry; discovery fallback 32768 (§6). |
| **OpenRouter / custom endpoints** | Forwarded untouched. | OpenRouter catalog or operator-set. |
| **Zen** (Chat / Responses wires) | `thinking`, `output_config`, `top_k`, `stop_sequences` are dropped on the Responses wire and `thinking` is not translated on the Chat wire: there is no per-model evidence for a safe mapping, and the free-tier gate pins the body shape. | `max_tokens` → `max_output_tokens`; fill default 32768. |

## 5. Effort → thinking budget (budget-style routes)

Cloud Code's `ThinkingConfig` carries a token budget (`thinking_budget`, tag 3) or a tier (`thinking_level`, tag 4) and no effort level, so the proxy has to turn a level into a number. The table is the proxy's own choice, not a vendor number:

| Level | Claude | Gemini / GPT-OSS |
|---|---|---|
| `low` / `minimal` | 1024 | 1024 |
| `medium` | 8000 | 8000 |
| `high` / `xhigh` / `max` | 32000 | 16000 |

The catalog's default budget (1024 for the Opus 4.6 fixture, which is also what agy sends by default) applies when the client states no effort and no budget. `max` on Claude runs as `high` (32000): a larger `max` budget depends on gate GD, which is UNVERIFIED. Anthropic's rule `budget_tokens < max_tokens` is enforced on the final `maxOutputTokens` after the model cap.

## 6. Output limits and context windows

| Surface | Model | Context | Max output | Source |
|---|---|---|---|---|
| Cloud Code | Gemini 3.x Flash | 1,048,576 | 65,536 | `.reference/agy-models-20260903.txt` (View B) |
| Cloud Code | Gemini 3.1 Pro | 1,048,576 | 65,535 | same |
| Cloud Code | Claude Sonnet / Opus 4.6 | 250,000 | 64,000 | same (Google's serving limits, not Anthropic's) |
| Cloud Code | GPT-OSS 120B | 131,072 | 32,768 | same |
| Claude Code gateway | Fable 5 / 5.1, Opus 5, Sonnet 5 | 1,000,000 | 128,000 | platform.claude.com/docs/en/models/overview; build-with-claude/context-windows |
| Claude Code gateway | Haiku 4.5 | 200,000 | 64,000 | Models overview |
| Kimi Open Platform | `kimi-k3` | 1M | not published | platform.kimi.ai/docs/models |
| Kimi Open Platform | `kimi-k2.7-code`, `kimi-k2.6` | 256K | default `max_tokens` 32768 | quickstart "Parameters Differences" |
| Kimi Code | `k3` | 1,048,576 (Pro+; 262,144 below) | not published | kimi.com/code/docs/en/kimi-code/models.html |
| Kimi Code | `k3-256k`, `kimi-for-coding-highspeed` | 262,144 | not published | same |
| Kimi Code | `kimi-for-coding` (K2.8 Preview) | 1,048,576 | not published | same |

The proxy advertises the limits of the upstream that serves the request, so Cloud Code rows are Google's, not Anthropic's. They are Google's catalog values, but the proxy sends and advertises at most 16,384 for the Gemini rows (see §4). `/v1/models` fallback when a gateway entry has no limits: context 200000, output 32768.

## 7. Kimi: two products, one gateway

| | Moonshot Open Platform | Kimi Code (subscription) |
|---|---|---|
| Base URL | `https://api.moonshot.ai/anthropic` (config default) | `https://api.kimi.ai/coding/` overseas, `https://api.kimi.com/coding/` China |
| Auth | API key | OAuth device flow / membership key |
| Model IDs | `kimi-k3`, `kimi-k2.7-code`, `kimi-k2.7-code-highspeed`, `kimi-k2.6`; discontinued: `kimi-k2.5`, `moonshot-v1-*`, `kimi-k2-*` | `k3`, `k3-256k`, `kimi-for-coding`, `kimi-for-coding-highspeed` |
| Effort | `output_config.effort` ∈ `low|high|max`, default `max` | `low|high|max`; defaults `high` (k3, k3-256k), `max` (kimi-for-coding); **server maps** `medium→high`, `xhigh→max`, `ultra/max→max`, `minimum/light→low`, `none→thinking disabled`, anything else → 400 |
| Thinking | K2.7: forced on, only `{"type":"enabled","keep":"all"}`; K2.6: enabled/disabled | thinking off routes K3 and K2.8 requests to K2.8 Preview without thinking |
| Sampling | `temperature` fixed 1.0, `top_p` 0.95, other values → error | not published |
| `[1m]` suffix | not a Kimi ID | "only needed for Claude Code env vars"; the proxy strips it |

The proxy forwards reasoning fields untouched on both products. A normalization for the Open Platform (mapping effort to its three-value enum) would exist only if gate GG showed that it rejects `medium` / `xhigh`; GG is UNVERIFIED, so nothing is rewritten. Kimi's `/v1/models` publishes `context_length` and `supports_*` flags but no output limit, so Kimi output limits are operator-set.

## 8. Claude Code settings that matter here

- Claude Code decides what to send from the model ID: effort and thinking capabilities are matched by ID pattern, and unknown IDs get none. To get `/effort` for a Gemini-named model, declare it: `ANTHROPIC_DEFAULT_<TIER>_MODEL_SUPPORTED_CAPABILITIES=effort,thinking` (add `xhigh_effort`, `max_effort` only for models that publish them).
- `opus` / `sonnet` resolve to Opus 5.5 / Sonnet 5.5 on the Anthropic API; the Claude Code gateway's default allowlist deliberately does **not** claim `claude-opus-5-5` or `claude-sonnet-5-5` (a distinct newer model must never be silently rewritten to an older one). To route them, add allowlist entries with context 1,000,000 and max output 128,000.
- Effort changes between requests invalidate the Anthropic prompt cache, so the OutputShaper never rewrites `output_config.effort`.

## 9. Intentional divergences

- Anthropic model spellings (`claude-3-5-sonnet`, `sonnet`, `opus`, `fable`) are unmapped on Cloud Code: hard-mapping them would silently change which model answers (PR #75).
- `reasoning_effort`, `reasoning`, `thinking_budget` are proxy extensions; `thinking_budget: -1` means "off" here, not Gemini's "dynamic".
- Generation is pinned to the Daily host because a thought signature is rejected by the other host.
- `max_output_tokens` is not a `/v1/messages` field and is not aliased; a missing `max_tokens` is tolerated on Cloud Code; gateways fill it from the entry's limit (raised to the floor of 16) and omit it only when no limit is known, in which case Anthropic and Kimi reject the request themselves.
- `redacted_thinking` and foreign thinking signatures in history are stripped (they cannot be replayed to another backend); `thinking.display` is accepted and ignored.
- Sampling parameters: copied on Cloud Code. Whether Cloud Code enforces Anthropic's thinking-time rule is gate GC, which is UNVERIFIED, so the copy is unchanged.
- `ThinkingLevel.MINIMAL` is never emitted: no catalog field says which routes accept it and Google documents errors on some families.
- The Zen wires drop reasoning (§4); the Claude Code gateway defaults omit 5.5 and 4.x IDs (§8).
- Gemini output is capped at 16,384 although the catalog advertises 65,536 / 65,535; lifting the cap needs probe GE and agy's own value (gate GM), both UNVERIFIED.

## 10. Probe verdicts (2026-10-05)

No probe or agy capture was run for this verification: every gate below is UNVERIFIED and the conservative branch of each was taken. To re-verify, follow §11, then apply the TAKEN line's branches.

```
Recorded 2026-10-05. NOT RUN: no probe, agy capture or Kimi request was executed (controller ruling R3 in the SDD ledger); every gate below is UNVERIFIED and takes its "rejected / unverified" branch. No evidence files exist.
GA casing:     snake=UNVERIFIED camel=UNVERIFIED  agy-claude-spelling=unverified
GB no-budget:  status=UNVERIFIED thinkingBlocks=UNVERIFIED
GC sampling:   temperature=UNVERIFIED top-k=UNVERIFIED
GD budgets:    equals-max-control=UNVERIFIED budget-55808=UNVERIFIED
GE output cap: 65536=UNVERIFIED
GF minimal:    UNVERIFIED
GX both:       UNVERIFIED
GH sweep:      thinkingTokens 1024=UNVERIFIED 8000=UNVERIFIED 32000=UNVERIFIED (no numbers exist for D3's sign-off)
GM agy sends:  UNVERIFIED (no capture; agy-claude-spelling and the effort budgets stay as the provisional constants)
GG kimi:       medium=UNVERIFIED xhigh=UNVERIFIED
PREFLIGHT build=ok paramprobe-list=3 mitmdump=present agy=present jq=present agy-effort-flag=seen agy-model-flag=seen
TAKEN GA=keep GB=keep GC=keep GD=shrink only GE=alternative GM=keep GG=keep
To replace this with evidence: run plan Task 3 Steps 2-6 (operator-run), overwrite this file, then apply the TAKEN line's branches (Task 7 default branch if GE accepted, Task 10b if GG rejected, Task 12 sub-tasks).
```

## 11. Re-verify

```bash
go run ./cmd/paramprobe -list                                  # the probe matrix, offline
go run ./cmd/paramprobe -out .reference/cloudcode-params-probe-$(date +%Y%m%d).jsonl
agy --help | grep -- --effort                                   # agy's effort vocabulary
curl -s https://api.anthropic.com/v1/models -H "x-api-key: $ANTHROPIC_API_KEY" -H 'anthropic-version: 2023-06-01' | jq '.data[] | {id, max_input_tokens, max_tokens, effort: .capabilities.effort}'
```

## 12. Sources

Anthropic: [Effort](https://platform.claude.com/docs/en/build-with-claude/effort), [Thinking](https://platform.claude.com/docs/en/build-with-claude/thinking), [Context windows](https://platform.claude.com/docs/en/build-with-claude/context-windows), [Models overview](https://platform.claude.com/docs/en/models/overview), [List models](https://platform.claude.com/docs/en/api/models/list). Claude Code: [Model configuration](https://code.claude.com/docs/en/model-config). Google: [Gemini thinking](https://ai.google.dev/gemini-api/docs/generate-content/thinking). Kimi: [Messages API](https://platform.kimi.ai/docs/api/messages), [Model parameter reference](https://platform.kimi.ai/docs/api/models-overview), [Model list](https://platform.kimi.ai/docs/models), [Kimi Code overview](https://www.kimi.com/code/docs/en/), [Kimi Code models](https://www.kimi.com/code/docs/en/kimi-code/models.html), [Kimi Code in Claude Code](https://www.kimi.com/code/docs/en/third-party-tools/claude-code.html). Proto: `gen/google/cloud/aiplatform/master/content.pb.go:530-538, 6113-6121`.
