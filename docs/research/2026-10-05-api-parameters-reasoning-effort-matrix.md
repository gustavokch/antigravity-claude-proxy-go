# API Parameters & Reasoning-Effort Matrix: Anthropic ↔ Cloud Code/Antigravity ↔ Kimi Code

**Date:** 2026-10-05
**Scope:** How the proxy (`antigravity-claude-proxy-go`) accepts Anthropic `/v1/messages` parameters (as sent by Claude Code CLI), translates them to Google Cloud Code / Antigravity (`v1internal` + AI Platform `GenerateContent`) and to Kimi Code (Moonshot), and where the proxy drifts from each upstream's conventions.
**Method:** Primary sources only — vendored protobuf definitions, live `.reference/` captures, proxy source, and the official Anthropic / Google / Moonshot docs fetched during research. Every claim cites its source.

---

## 1. Executive Summary

- **Anthropic's current control plane is `output_config.effort` + `thinking: {type: "adaptive"}`**, not `budget_tokens`. Manual `type: "enabled"` + `budget_tokens` is **deprecated on Claude 4.6 and a hard 400 on 4.7+** (sources §2.1). The proxy's Cloud Code path still speaks only budgets: it flattens *every* thinking mode (including `adaptive`) into a fixed `thinking_budget`/`thinkingBudget` and **never reads `output_config.effort`** (`internal/format/request.go:155-249`).
- **Google's wire has two knobs, and the proxy uses both but never together:** `ThinkingConfig.thinking_budget` (token budget, Gemini 2.5-style) and `ThinkingConfig.thinking_level` (`LOW/MEDIUM/HIGH/MINIMAL` enum, Gemini 3-style). Which one is emitted is decided solely by whether the catalog supplied a `ThinkingLevel` (`internal/format/request.go:176-249`). The `MINIMAL` enum value (proto `4`) is defined upstream but **unreachable** — the proxy folds `"minimal"` into `"low"` before it ever reaches the wire.
- **Kimi is Anthropic-compatible on the direct gateway** (`internal/kimi/passthrough.go` — byte-transparent forward to `https://api.moonshot.ai/anthropic` / OAuth `https://api.kimi.ai/coding/v1`), so whatever Claude Code sends arrives intact. Kimi's own `output_config.effort` vocabulary is **`low/high/max` (default `max`), no `medium`, no `xhigh`** — a three-way mismatch with both Anthropic (five levels) and the proxy's normalizer (which rewrites `max→high` before tier routing).
- **Biggest functional gaps** (details in §6): (a) `output_config.effort` dropped on the Cloud Code path; (b) `xhigh`/`max` collapsed to `high` while Anthropic documents them as distinct, stronger levels; (c) Gemini output hard-clamped to 16,384 although upstream advertises 65,536; (d) no `temperature=1.0` coercion under thinking although Anthropic 400s otherwise; (e) `thinking.display`, `redacted_thinking` history blocks, and client `max_output_tokens` silently ignored.

---

## 2. Anthropic `/v1/messages` Canonical Parameters & Thinking Spec

Primary sources: [Messages API reference](https://platform.claude.com/docs/en/api/messages/create) (`max_tokens`, `output_config`, `thinking`, response blocks), [Thinking](https://platform.claude.com/docs/en/build-with-claude/thinking) (per-model mode table, sampling-parameter rule, output-limits table), [Extended thinking](https://platform.claude.com/docs/en/build-with-claude/extended-thinking) (budget rules, migration), [Effort](https://platform.claude.com/docs/en/build-with-claude/effort) (levels, defaults), [Models overview](https://platform.claude.com/docs/en/models/overview) (context/output limits).

### 2.1 `thinking` — three modes, and `budget_tokens` is legacy

| `thinking.type` | Meaning | Model support (per Thinking doc mode table) |
|---|---|---|
| *(absent)* | Adaptive on newer models (Opus 5.5, Sonnet 5.5, Fable 5.x, Mythos 5.x, Opus 5, Sonnet 5, Mythos Preview: on by default); off on 4.6/4.8/4.7/4.5/Haiku 4.5 | Thinking doc, "Configuring thinking" table |
| `"adaptive"` | Model decides whether/how deeply to think per request. **The recommended control; pair with `output_config.effort`.** | Accepted on 4.6+ (not on 4.5/Haiku 4.5 — 400) |
| `"enabled"` + `budget_tokens` | Manual fixed-budget thinking ("extended thinking"). **Deprecated on Opus/Sonnet 4.6 (still succeeds); 400 on 4.7 and later** (`"thinking.type.enabled" is not supported`) | 4.6 and earlier only |
| `"disabled"` | Thinking off. **Rejected (400) on Opus 5.5, Sonnet 5.5, Fable/Mythos 5.x, Mythos Preview** (always-on models); on Sonnet 5.5 use `between_tools` for lowest thinking; on Opus 5 rejected only at `xhigh`/`max` effort | Thinking doc, "Turning thinking off" |
| `"between_tools"` | Up-front thinking off, progress updates between tool calls only (Sonnet 5.5's lowest setting) | Thinking doc |

Budget rules (`thinking: {type:"enabled", budget_tokens: N}`), from the Extended-thinking doc:
- `budget_tokens` **≥ 1,024** (API rejects smaller values) and **< `max_tokens`** (thinking tokens count toward `max_tokens`; the only exception is interleaved manual mode where the budget spans a whole turn).
- The budget is a *target*, not a cap; `max_tokens` is the hard ceiling. Budgets above 32k should use batch processing.
- `display: "summarized" | "omitted"` controls whether thinking text is returned; default `summarized` on 4.6-and-earlier, `omitted` on newer models. Either way the block (with encrypted `signature`) must be passed back unchanged in multi-turn/tool conversations.

### 2.2 `output_config.effort` — the current depth dial

From the Effort doc (`output_config.effort`, top-level request param, no beta header; GA on listed models):

| Level | Semantics | Availability |
|---|---|---|
| `low` | Most efficient; subagents, simple tasks | All effort models |
| `medium` | Balanced; **the default on Opus 5.5** | All effort models |
| `high` | "As many tokens as the task needs"; **default on every other effort model**. Setting effort to the model's default ≡ omitting it | All effort models |
| `xhigh` | Extended capability for 30min+ agentic/coding work, token budgets in the millions | Fable 5.1, Mythos 5.1, Fable 5, Mythos 5, Opus 5.5/5/4.8/4.7, Sonnet 5.5/5 |
| `max` | Absolute maximum, unconstrained token spend | Fable 5.1, Mythos 5.1, Fable 5, Mythos 5, Mythos Preview, Opus 5.5/5/4.8/4.7/**4.6**, Sonnet 5.5/5/**4.6** |

Key interactions (Effort + Thinking docs):
- Effort affects **all** output tokens (text, tool calls, thinking) and works whether or not thinking is enabled. It is a behavioral signal, not a token budget.
- At `high`/`xhigh` (`max` on Fable), set a **large `max_tokens`** — it is a hard limit on thinking + response; Opus 5 guidance starts at 64k. `max_tokens: 0` (cache pre-warming) cannot be combined with extended thinking.
- Changing effort (like changing `budget_tokens`) **invalidates prompt-cache breakpoints**.
- The proxy's served Claude models are **Sonnet 4.6 / Opus 4.6** (`.reference/cloudcode-models-20260903.json`; `internal/modelcatalog/catalog.go:125-128`): per the Effort doc both support all of `low/medium/high/xhigh/max` and both accept `adaptive` thinking — while manual `enabled` budgets are deprecated-but-accepted on exactly these two models.

### 2.3 `max_tokens` vs `max_output_tokens` (Anthropic side)

- The **request** field is `max_tokens` (required, `minimum: 0`; `0` = cache pre-warm with no generation) — API reference, Body parameters.
- The **Models API** reports per-model `max_tokens` (output ceiling) and `max_input_tokens` — Models overview, "Using the Models API". There is **no `max_output_tokens` request parameter** in the Anthropic Messages API; `max_output_tokens` is OpenAI/Responses-style vocabulary.
- Anthropic output ceilings (Thinking doc, "Output limits" table): current flagships (Fable 5.1, Mythos 5.1, Fable/Mythos 5, Opus 5.5/5/4.8/4.7/4.6, Sonnet 5.5/5/4.6) **128K** (300K on Batches beta for most); Opus 4.5 / Sonnet 4.5 / Haiku 4.5 **64K**. Context windows (Models overview): Fable 5.1 / Opus 5.5 / Sonnet 5.5 **1M**; Haiku 4.5 **200K**.

### 2.4 Sampling parameters under thinking (Anthropic rule)

Thinking doc, "Sampling parameters" (§ "Limits and feature compatibility"):
- On the newer models (Fable 5.1/5, Mythos 5.1/5/Preview, Opus 5.5/5/4.8/4.7, Sonnet 5.5/5): **any non-default `temperature`, `top_p`, or `top_k` → 400 on every request**, thinking or not.
- On older models the restriction applies only while thinking is on: `temperature` and `top_k` incompatible with thinking; `top_p` allowed only in `[0.95, 1]`.
- The proxy forwards whatever the client sends with **no coercion** (gap G7, §6).

---

## 3. Cloud Code / Antigravity (Gemini) Protobuf & Wire Format

Primary sources: vendored protos `gen/google/cloud/aiplatform/master/content.pb.go`, `gen/google/cloud/aiplatform/master/prediction_service.pb.go`, `gen/v1internal/*.pb.go`; transport `internal/cloudcode/client.go`; live catalog `.reference/cloudcode-models-20260903.json`.

### 3.1 Transport and envelope

- Endpoints (`internal/cloudcode/client.go:22-45`): `DailyEndpoint = https://daily-cloudcode-pa.googleapis.com`, `ProdEndpoint = https://cloudcode-pa.googleapis.com`. Generation traffic is **pinned to Daily** (`GenerationEndpoints = [Daily]`) because a thought signature issued by one host is rejected by the other ("Corrupted thought signature").
- RPC paths are `v1internal` JSON-over-HTTP, not gRPC on the wire: `PathGenerateContent = "/v1internal:generateContent"`, `PathStreamGenerate = "/v1internal:streamGenerateContent?alt=sse"` (`client.go:31-32`, methods at `304-310`). The gRPC stubs exist (`gen/v1internal/prediction_service_grpc.pb.go:33-40` — `GenerateContent`, `StreamGenerateContent`, `CountTokens`, `RetrieveUserQuota`, `FetchAvailableModels`) but the proxy speaks the HTTP/SSE form agy uses.
- Envelope (`internal/format/builder.go:66-105`, corroborated by scout): the Anthropic→Google inner request becomes `master.GenerateContentRequest`, wrapped as `v1internal.GenerateContentRequest` (`gen/v1internal/prediction_service.pb.go:80-91`: fields `project` 1, `requestId` 2, `request` 3 = embedded master request, `model` 4, `userPromptId` 5, `userAgent` 6, `requestType` 7, `enabledCreditTypes` 8) with `userAgent: "antigravity"`, `requestType: "agent"`, `requestId: "agent-<uuid>"`, per-account `sessionId`, plus the `AntigravitySystemInstruction` prefix and `x-anthropic-billing-header` line stripping (`builder.go:107-133`).

### 3.2 `GenerationConfig` (upstream fields the proxy can set)

`gen/google/cloud/aiplatform/master/content.pb.go:3021-3053` (all `proto3,oneof` unless noted):

| Proto field | Tag | Type | Proxy source |
|---|---|---|---|
| `temperature` | 1 | `*float32` | copied verbatim from Anthropic `temperature` (`request.go:148`) |
| `top_p` | 2 | `*float32` | from `top_p`→`topP` (`request.go:149`) |
| `top_k` | 3 | `*float32` | from `top_k`→`topK` (`request.go:150`) |
| `candidate_count` | 4 | `*int32` | never set |
| `max_output_tokens` | 5 | `*int32` | from Anthropic `max_tokens` (`request.go:145-147`) |
| `stop_sequences` | 6 | `[]string` (repeated) | from `stop_sequences` (`request.go:151-153`) |
| `thinking_config` | 25 | `*GenerationConfig_ThinkingConfig` | §3.3 |
| others (`seed` 12, `response_mime_type` 13, `logit_bias`, penalties, `response_schema`, `routing_config`, …) | — | — | never set by the proxy |

Google semantics (from [GenerateContent thinking doc](https://ai.google.dev/gemini-api/docs/generate-content/thinking), fetched 2026-10-05): `max_output_tokens` **includes thought tokens** and is a hard infrastructure cutoff (`finish_reason: MAX_TOKENS`, possibly truncated/empty output, thinking still billed). Google explicitly recommends lowering `thinking_level` instead of squeezing `max_output_tokens`.

### 3.3 `ThinkingConfig` — the two-knob wire format

`gen/google/cloud/aiplatform/master/content.pb.go:6113-6121`:

| Proto field | Tag | Type | Notes |
|---|---|---|---|
| `include_thoughts` | 1 | `*bool` (oneof) | Proxy always sets `true` when thinking is active |
| *(tag 2 reserved)* | 2 | — | Descriptor confirms tag 2 retired; budget moved to 3 |
| `thinking_budget` | 3 | `*int32` (oneof) | Token budget (Gemini 2.5-style) |
| `thinking_level` | 4 | `*ThinkingLevel` enum (oneof) | Categorical level (Gemini 3-style). **Oneof with budget: only one may be set** |
| `include_raw_thoughts` | 5 | `bool` | Never set by the proxy |

`ThinkingLevel` enum (`content.pb.go:530-538`): `UNSPECIFIED=0, LOW=1, MEDIUM=2, HIGH=3, MINIMAL=4`.

Google semantics (GenerateContent thinking doc):
- `thinkingLevel` (Gemini 3+ recommended): per-family support matrix — 3.8/3.7 Flash support `low/medium/high` (`minimal` → **error**); 3.6/3.5 Flash add `minimal`; 3.1 Pro supports `low/medium/high` (default `high` Dynamic; **thinking cannot be disabled**); Flash-Lite families default to `minimal`. **2.5 series does not support `thinkingLevel` — use `thinkingBudget`.**
- `thinkingBudget` (2.5 series): 2.5 Pro range `128–32768` (cannot disable); 2.5 Flash `0–24576` (`0` = off); 2.5 Flash-Lite `512–24576` (off by default); `-1` = dynamic thinking.
- Proxy gap: `clampGeminiThinkingBudget` (`internal/format/model.go:99-112`) caps **all** `gemini-2.5*` at 24,576, cutting the top of 2.5 Pro's documented `128–32768` range (gap G10).

### 3.4 `ModelDetails` — what the catalog tells the proxy

`gen/v1internal/model_configs.pb.go:153-192`: `supports_thinking` (3), `thinking_budget` (4), `min_thinking_budget` (5), `max_tokens` = context window (7), `max_output_tokens` (8), `supports_raw_thinking` (25), `thinking_level` (35), `supports_thought_circulation` (38), `supports_adaptive_thinking` (42). The proxy parses the JSON names into `modelDetails` (`internal/modelcatalog/catalog.go:71-83`) and carries them as `Model{SupportsThinking, SupportsAdaptiveThinking, ThinkingBudget, MinThinkingBudget, ThinkingLevel, MaxTokens, MaxOutputTokens}` (`catalog.go:13-29`, `492-512`).
Also: `GenerateChatRequest.include_thinking_summaries` (tag 12, `gen/v1internal/cloudcode.pb.go`) exists upstream; the proxy never sets it.
Response side: upstream returns `thought: true` parts with `thoughtSignature`; the proxy converts them to Anthropic `thinking` blocks (requiring signature ≥ 50 chars, `MinSignatureLength`, `internal/format/model.go:11`; `response.go:27-40`, `stream.go`), and echoes `thoughtsTokenCount`-family usage fields (`response.go:185-191`).

---

## 4. Kimi Code API & Reasoning Spec

Primary sources: [Moonshot Messages API reference](https://platform.kimi.ai/docs/api/messages) (OpenAPI, fetched 2026-10-05), [Use Kimi in Claude Code](https://platform.kimi.ai/docs/guide/claude-code-kimi) (endpoint, env vars, thinking behavior), proxy `internal/kimi/*`, `internal/auth/kimi_oauth.go`, `internal/zen/chatwire.go`, plan docs `docs/superpowers/plans/2026-08-26-kimi-code-gateway.md` / `2026-09-25-kimi-code-auth.md`.

### 4.1 Endpoint, auth, and wire compatibility

- Kimi exposes an **Anthropic-compatible** `POST /v1/messages` at `https://api.moonshot.ai/anthropic` (Messages API doc; proxy default `internal/kimi/kimi.go:13-21`). Claude Code integration is `ANTHROPIC_BASE_URL=https://api.moonshot.ai/anthropic` + `ANTHROPIC_AUTH_TOKEN=<Moonshot key>` (Kimi guide; plan doc `2026-08-26-kimi-code-gateway.md:11-12`).
- OAuth subscription path uses `https://api.kimi.ai/coding/v1` with device flow client ID `17e5f671-d194-4dfb-9706-5516cb48c098` (`internal/auth/kimi_oauth.go:20-28`) and `KimiCLI/1.0.0` fingerprint headers (`X-Msh-*`, `internal/kimi/identity.go:12-35`).
- The proxy's **direct Kimi gateway is a transparent reverse proxy** (`internal/kimi/passthrough.go:20-91`): rewrites `Authorization: Bearer`, forwards `anthropic-version`/`anthropic-beta` untouched, re-emits the (possibly mutated) body. Dispatch: allowlist match by ID/alias → strip `[1m]` suffix → `applyMaxTokensPolicy(body, manualOverride=kimiEntry.MaxOutputTokens)` → `forwardToKimi` (`internal/api/dispatch.go:65-83`).
- Therefore on the direct path **no reasoning translation happens at all** — `thinking`, `output_config.effort`, `temperature` all pass through verbatim. Whatever Claude Code sends is what Kimi judges.

### 4.2 Kimi's own reasoning vocabulary

From the Messages API OpenAPI schema (the normative source):
- `output_config.effort`: enum **`low | high | max`**, **default `max`**. No `medium`, no `xhigh`. "Changing the level breaks prefix-cache hits, so decide it before the session starts."
- `max_tokens`: **required**, `minimum: 1` (Kimi rejects its absence — unlike Anthropic's cache-warming `0`).
- Request/response `thinking` blocks mirror Anthropic's (`type: "thinking"`, `thinking` text, `signature` — pass back unchanged), plus `reasoning_content` on the Chat Completions wire and `thinking_tokens` usage.
- From the Kimi Claude Code guide: `CLAUDE_CODE_EFFORT_LEVEL=max` is the recommended setting; `kimi-k3` thinking is on-by-default and disable-able; **`kimi-k2.7-code` thinking is forced on — requests with thinking off are rejected `400 invalid thinking: only type=enabled is allowed for this model`**, and Thinking must be toggled on in Claude Code before use.
- Catalog shape: `/v1/models` entries carry `context_length` + `max_tokens` (`internal/kimi/client.go:15-20`); proxy config per model is `{id, alias, displayName, contextLength, maxOutputTokens (0 = omit/derive), enabled}` (`internal/config/config.go:85-101`).
- Zen-routed Kimi models (`kimi-k3`, `kimi-k2.7-code`, `kimi-k2.6`, `kimi-k2.5` → ChatWire, `internal/zen/client.go:76-79`) get thinking bridged as OpenAI-style `reasoning_content` on the way in (`chatwire.go:349-353`) and back to Anthropic `thinking` blocks on the way out (`chatwire.go:476-478, 866-874`). On the Responses wire, `thinking`/`temperature`/`top_p` are deliberately dropped (no counterpart; `responseswire.go:27-31`).

---

## 5. Cross-API Translation & Mapping Matrix

### 5.1 Model naming & aliasing (proxy view)

| Client-facing ID | Upstream / serving identity | Source |
|---|---|---|
| `gemini-3.8-flash-{high,medium,low}` | Tiered runtime `gemini-3.8-flash-tiered` + `thinkingLevel HIGH/MEDIUM/LOW` when present; else verbatim upstream tier IDs; else 3.7-tier fallback | `catalog.go:514-575` (`applyGemini37`), `:577-634` (`applyGemini38`) |
| `gemini-3.7-flash-{high,medium,low}` | Same pattern on `gemini-3.7-flash-tiered` → 3.6 fallback | `catalog.go:514-575` |
| `gemini-3.5-flash{,-high,-medium,-low,-extra-low}` | **Repointed to the 3.8 family** (operator decision 2026-09-03); real 3.5 entries win only if no 3.8 exists | `catalog.go:99-115`, `ResolveWithRequest:390-413` |
| `gemini-3.1-pro{,-high}`, `gemini-pro` | `Gemini 3.1 Pro (High)` agent route | `catalog.go:90-95` |
| `claude-sonnet-4-6{-thinking}`, `claude-opus-4-6{-thinking}` | `Claude Sonnet/Opus 4.6 (Thinking)` display names | `catalog.go:117-129` |
| Anthropic spellings (`claude-3-5-sonnet`, `sonnet`, `opus`, `fable`) | **Deliberately unmapped** → honest `SelectionError` (PR #75 decision) | `catalog.go:117-124` |
| `kimi-*` (direct gateway) | Allowlist ID/alias → upstream ID verbatim, `[1m]` stripped | `dispatch.go:65-83`, `server.go:2103-2109` |
| `kimi-k3`, `kimi-k2.7-code`, `kimi-k2.6`, `kimi-k2.5` (Zen) | OpenAI ChatWire IDs | `internal/zen/client.go:76-79` |
| No `k1` / `moonshot-v1` / `kimi-k2*` in Cloud Code catalog | `modelcatalog` is Google-models-only; Kimi IDs fall through to gateways | Kimi scout; `dispatch.go:65-83` |

### 5.2 Context window & max output tokens per model

Live upstream values (`.reference/cloudcode-models-20260903.json`, captured 2026-09-03; `/v1/models` shape `internal/api/server.go:592-598`):

| Model (upstream ID) | Context (`maxTokens`) | Max output (`maxOutputTokens`) | Thinking |
|---|---|---|---|
| `gemini-3.7-flash` / `gemini-3.6-flash` / `gemini-3.5-flash` | 1,048,576 | 65,536 | yes |
| `gemini-3.1-pro` | 1,048,576 | 65,535 | yes |
| `claude-sonnet-4-6` / `claude-opus-4-6` | 250,000 | 64,000 | yes |
| `gpt-oss-120b` | 131,072 | 32,768 | yes |
| Claude 4.5/5-era pass-through IDs (`claude-opus-5`, `claude-sonnet-5`, `claude-haiku-4-5*`, `claude-3-7-sonnet*`, `claude-3-5-sonnet*`) | 200,000 | 8,192 | varies (3.5 Haiku/Opus 3/Haiku 3/Sonnet 3: no) |
| Claude 3.0 family (`claude-3-opus/haiku/sonnet*`) | 200,000 | 4,096 | no |

Reference for the task's named older models (Anthropic docs, since retired from the live catalog): 3.5 Sonnet / 3.7 Sonnet — 200K context, 8,192 output; Opus 3 / Haiku 3 — 200K context, 4,096 output (matches the pass-through rows above). Current Anthropic flagships (§2.3): 1M context / 128K output (Fable 5.1, Opus 5.5, Sonnet 5.5), 200K / 64K (Haiku 4.5). Test-fixture budgets corroborate plumbing, not limits: 3.5-flash-low 4000 / agent 10000, 3.1-pro-high 10001, opus-4-6-thinking 1024, gpt-oss-120b-medium 8192 (`catalog_test.go:18-23`); tier fixtures 3.6-high 16000 / medium 8000 / low 1024 (`catalog_test.go:87-89`).

Proxy-side ceilings that override the above on the Cloud Code path:
- `GeminiMaxOutputTokens = 16384` — **all Gemini-family `maxOutputTokens` clamped to 16K** (`model.go:12`, `request.go:284-286`), i.e. ¼ of the advertised 64K (gap G6).
- `DefaultClaudeThinkBudget = 32000`, `DefaultGeminiThinkBudget = 16000` (`model.go:14-15`).
- Provider floor `minMaxTokensFloor = 16` — values actually sent are raised to 16 (muse-spark 1.3 400s below it; `server.go:1978-1981`).
- `/v1/models` fallback `defaultDiscoveryMaxOutputTokens = 200000` — identical to the context-window fallback, contradicting its own comment (gap G11).
- Claude Code gateway entries carry each model's max output as the derived limit; Zen fill defaults to `DefaultMaxOutputTokens = 32768` (`zen.go:24-28`).

### 5.3 Temperature / top_p / top_k / stop_sequences

| Knob | Anthropic rule (§2.4) | Gemini/Cloud Code wire | Kimi direct | Kimi via Zen | Proxy behavior (Cloud Code path) |
|---|---|---|---|---|---|
| `temperature` | New models: any non-default → 400 always. Older: must be 1.0/omitted under thinking | `temperature` (proto tag 1) accepted | Pass-through (whatever Kimi enforces) | Dropped on Responses wire | **Copied verbatim, never coerced** (`request.go:148`); stripped only in ccidentity impersonation (`ccidentity/apply.go:122-129`) |
| `top_p` | New models: 400 if non-default. Older + thinking: only 0.95–1 | `topP` | Pass-through | Dropped on Responses wire | Copied verbatim (`request.go:149`) |
| `top_k` | New models: 400 if non-default. Older: incompatible with thinking | `topK` | Pass-through (Anthropic-schema unknown) | Dropped (no Responses counterpart) | Copied verbatim (`request.go:150`) |
| `stop_sequences` | Supported (≤~4) | `stopSequences` | Supported (≤5 entries, ≤32B each — Kimi OpenAPI) | Dropped on Responses wire | Cloned verbatim (`request.go:151-153`) |

### 5.4 Reasoning-effort / thinking-budget mapping (the core matrix)

Upstream vocabularies:
- **Anthropic `output_config.effort`**: `low | medium | high | xhigh | max` (default `high`; `medium` on Opus 5.5).
- **Gemini `ThinkingConfig`**: `thinking_level LOW | MEDIUM | HIGH | MINIMAL` **xor** `thinking_budget` (int tokens; `-1` dynamic, `0` off where supported).
- **Kimi `output_config.effort`**: `low | high | max` (default `max`).

What the proxy does with an incoming Claude Code request on the **Cloud Code path** (`request.go:155-249`, `catalog.go:275-420`):

| Incoming signal | Normalized | Claude-family upstream (`thinking_budget`, snake_case) | Gemini-family upstream (`thinkingBudget`, camelCase) | Tiered 3.7/3.8 (`thinkingLevel`, no budget) |
|---|---|---|---|---|
| `reasoning_effort` / `reasoning`: `low` (`minimal`→`low`) | `low` | **1024** | **1024** | route to `-low` tier / `LOW` |
| `medium` | `medium` | **8000** | **8000** | route to `-medium` / `MEDIUM` |
| `high` | `high` | **32000** | **16000** | route to `-high` / `HIGH` |
| `xhigh/extreme/max/maximum/…` | **`high` (collapsed)** | 32000 | 16000 | `HIGH` |
| `none/disabled/off/false/0`, `thinking.type=disabled`, `budget≤0` | `disabled` | `thinkingConfig` deleted; `maxOutputTokens` untouched | deleted | forced to `LOW` (cannot fully off) |
| `thinking.budget_tokens` / `thinking_budget` (explicit) | budget wins over effort table | explicit value (default 32000; `max≤budget` → `max=budget+8192`) | explicit value (default 16000; 2.5-family clamp 24576, else 128000) | **ignored — level wins** |
| `thinking.type=adaptive` | *(not detected)* | flattened to fixed budget (32000 default) | flattened to fixed budget (16000 default) | level route |
| `output_config.effort` (any) | *(not read)* | **dropped** | **dropped** | dropped (tier from model ID only) |
| `thinking.display` | *(not read)* | dropped | dropped | dropped |

Catalog tier routing detail (`ResolveWithRequest`, `catalog.go:349-420`): effort selects `gemini-3.{8,7,6}-flash-<effort>` (legacy `3.5` → `3.8-<effort>`); an explicit positive budget overrides the variant's `ThinkingBudget`; a bare numeric budget with no effort back-maps to low (≤2048) / medium (<12000) / high in `ExtractReasoningParams` (`catalog.go:336-345`) — a mapping the `request.go` converter itself does **not** share (two implementations, gap G9).
Headroom shaping (`headroom/stages/shaper/stage.go:265-296`, floor `minThinkingBudget = 1024` at `:12-13`): on mechanical continuations, `thinking.budget_tokens` is clamped down to `MechanicalThinkingBudget` (keeping `max_tokens > budget_tokens`) and `reasoning_effort "high"` is rewritten to `"low"`.

---

## 6. Audit: Current Proxy Implementation vs Upstream Conventions

Severity: 🔴 behavior divergence a client can observe · 🟡 latent/inconsistency · 🟢 noted intentional.

- **G1 🔴 `output_config.effort` ignored on the Cloud Code path.** `request.go` never reads `output_config` (Anthropic slice §5.6); the translate step drops the one knob Anthropic now calls "the recommended way to control thinking depth." (Correction to an early reading: `ccidentity/apply.go:114-129` does *not* strip it — its comment says Claude Code *sends* `output_config`/`context_management`/`diagnostics` and they are preserved; they just die later in `format`.) Impact: Claude Code's `CLAUDE_CODE_EFFORT_LEVEL` / per-request effort has **zero effect** on Gemini-backed models, while the same request via the Kimi gateway honors it (Kimi default `max`).
- **G2 🔴 `xhigh`/`max` collapsed to `high`.** Both normalizers (`request.go:160-167`, `catalog.go:287-295`) fold Anthropic's two strongest levels into `high`. On the served 4.6 models Anthropic documents `max` as available and distinct (stronger than `high`), and Kimi's default is `max` — so a client asking for maximum reasoning silently gets high-tier budgets (32K/16K) instead.
- **G3 🟡 `minimal` collapsed to `low`; `MINIMAL` enum unreachable.** Upstream proto defines `MINIMAL=4` (`content.pb.go:537`) and Google documents `minimal` as the Lite-family default / near-off setting, but the proxy maps `"minimal"→"low"` (`request.go:163-164`). There is no path that emits `thinkingLevel: MINIMAL`.
- **G4 🟡 `thinking.type: "adaptive"` flattened to a fixed budget.** Only `type == "disabled"` is branched (`request.go:169-174`); `adaptive` falls through to static defaults (32K Claude / 16K Gemini). That is the opposite of adaptive's contract (model decides per request, may skip thinking). Upstream `supports_adaptive_thinking` (`model_configs.pb.go:192`) is parsed into the catalog but never consulted when building `thinkingConfig`.
- **G5 🟡 `thinkingConfig` key-casing inconsistency: Claude branch emits snake_case (`include_thoughts`, `thinking_budget`, `request.go:211`), Gemini/tiered branches emit camelCase (`includeThoughts`, `thinkingBudget`/`thinkingLevel`, `request.go:184-187, 245-248`).** The proto JSON names are camelCase (`content.pb.go:6116-6118`); ProtoJSON accepts both, so this works today, but the two branches disagree and only one matches the schema.
- **G6 🔴 Gemini `maxOutputTokens` hard-clamped to 16,384** (`model.go:12`, `request.go:284-286`) against upstream-advertised 65,536/65,535. Every long Gemini generation is cut to ¼ of the catalog limit; combined with Google's "thoughts count toward `max_output_tokens`" rule (§3.2), big-thinking + 16K cap is a truncation engine. (If the clamp is a cost guardrail, it should be operator-configurable, not a constant.)
- **G7 🔴 No `temperature`/`top_p`/`top_k` guard under thinking.** Verbatim copy (`request.go:148-150`) means a client sending `temperature: 0.2` with thinking on gets an upstream 400 that the proxy could have prevented by coercing to 1.0/omitting (Anthropic rule §2.4). Same exposure on Kimi-direct (pass-through by design — acceptable there, since Kimi owns the error).
- **G8 🟡 `thinking.display` ignored; `redacted_thinking` history dropped.** No `display` handling anywhere (grep: zero non-test hits); `convertContentToParts` has `case "thinking"` but no `case "redacted_thinking"` (`content.go:113-140`, Anthropic slice §5.2) — safety-redacted history blocks vanish in translation while `thinking.go:129-137` sanitizes them fine elsewhere.
- **G9 🟡 Two divergent effort normalizers.** `ExtractReasoningParams` (`catalog.go:275-347`: disabled detection, `thinking_budget` top-level, budget→effort back-mapping ≤2048/<12000/else) vs the inline block in `request.go:155-174` (no back-mapping, `thinking_budget` only via `intValue`). Tier routing and budget emission can disagree on the same request (e.g. bare `budget_tokens: 40000` with no effort string).
- **G10 🟡 2.5-Pro budget ceiling cut.** `clampGeminiThinkingBudget` caps every `gemini-2.5*` at 24,576 (`model.go:104-107`), but Google documents 2.5 Pro `128–32768`. The 24,577–32,768 band is unreachable. (Only matters if 2.5 models return to the catalog.)
- **G11 🟡 `defaultDiscoveryMaxOutputTokens = 200000` equals the context-window fallback** (`server.go:553-559`) — the exact misadvertisement its own comment warns "invites clients to send a max_tokens the provider rejects." Any fallback-model entry advertises 200K output.
- **G12 🟡 Incoming `max_output_tokens` ignored; `max_tokens` optional.** `request.go:145` reads only `max_tokens`, so OpenAI/Responses-shaped clients are silently uncapped; conversely the proxy tolerates missing `max_tokens` (Anthropic requires it; Kimi requires ≥1 — on the Kimi path an omitted value survives only if the allowlist entry supplies `MaxOutputTokens`, else Kimi 400s: `dispatch.go:80`, `server.go:1983-1991`).
- **G13 🟡 Catalog clamp can break the `budget < max` invariant.** Claude auto-expansion (`max = budget+8192`, `request.go:212-215`) runs *before* the catalog clamp (`request.go:287-289`); if a model's `MaxOutputTokens` < `budget+8192`, the final `maxOutputTokens` can land at or below the thinking budget — the configuration Anthropic rejects.
- **G14 🟢 Intentional divergences (keep, but document):** Anthropic-spelling model IDs unmapped by design (`catalog.go:117-124`); non-standard `reasoning_effort`/`reasoning`/`thinking_budget` request params are proxy extensions (`request.go:156-158, 223`); Daily-endpoint pinning for signature validity (`client.go:40-45`); provider floor 16 (`server.go:1978-1981`); Zen Responses-wire dropping of thinking/sampling params (`responseswire.go:27-31`).

---

## 7. Concrete Recommendations for Proxy Alignment

1. **Parse `output_config.effort` on the Cloud Code path (fixes G1).** In `request.go`, read `output_config.effort` (`low/medium/high/xhigh/max`) as a first-class effort signal alongside `reasoning_effort`, and in `ResolveWithRequest` route tiers from it. Preserve-but-ignore `output_config.format` (structured output has no GenerationConfig counterpart in scope).
2. **Stop collapsing `xhigh`/`max` → `high` (fixes G2).** Keep the five Anthropic levels distinct through tier routing; map `xhigh`→`HIGH` tier + top-of-band budget and `max`→`HIGH` + explicit large budget with the documented large-`max_tokens` guidance (start 64k). On the Kimi-direct path this is already correct by pass-through — don't "normalize" it there.
3. **Emit `MINIMAL` where upstream means near-off (fixes G3).** Route `"minimal"` to `thinkingLevel: MINIMAL` for families that support it, and use it (instead of forcing `LOW`) for the disabled-thinking case on 3.7/3.8 Flash where upstream rejects full-off.
4. **Honor `adaptive` by omitting the fixed budget (fixes G4).** When `thinking.type == "adaptive"` (or absent on adaptive-default models), prefer `thinkingLevel`-style routing or omit `thinking_budget` and let the model decide, consulting `SupportsAdaptiveThinking` from the catalog; keep fixed budgets only for explicit `enabled`+`budget_tokens` (deprecated-but-valid on the served 4.6s).
5. **Unify `thinkingConfig` casing to camelCase (fixes G5)** to match the proto JSON names (`includeThoughts`, `thinkingBudget`, `thinkingLevel`).
6. **Raise or configure the Gemini output clamp (fixes G6).** Either lift `GeminiMaxOutputTokens` toward the catalog value (65,536) or make it operator configuration; at minimum exempt the thinking auto-expansion (`budget+8192`) from being clamped below the budget (also fixes G13 — apply catalog clamp *before* the budget/max reconciliation, then re-assert `max > budget`).
7. **Coerce sampling params under thinking (fixes G7).** When a thinking config will be emitted, force `temperature=1.0` (or drop non-default `temperature`/`top_p`/`top_k`) per the Anthropic rule, or surface a 400 early with the upstream message instead of forwarding a doomed request.
8. **Handle `display` and `redacted_thinking` (fixes G8).** Add `case "redacted_thinking"` to `convertContentToParts`; store (and forward, not interpret) `thinking.display`.
9. **Merge the two effort normalizers (fixes G9).** Make `request.go` use `ExtractReasoningParams` (or vice versa) so tier routing and budget emission share one normalization, including the budget→effort back-mapping.
10. **Fix the ceilings (G10–G12).** Split the 2.5 clamp by model (Pro 32768 vs Flash 24576); set the discovery output fallback to a real output-scale value (e.g. 32,768, not 200,000); accept incoming `max_output_tokens` as an alias for `max_tokens`, and on the Kimi path fill a missing `max_tokens` from the allowlist/default (Kimi requires ≥1) instead of risking a Kimi-side 400.
11. **Document the intentional divergences (G14)** in `docs/` so the next audit doesn't re-litigate them: unmapped Anthropic spellings, `reasoning_*` extensions, Daily pinning, floor-16, Zen Responses drops.

---

## Appendix A. Source index (every claim traces to one of these)

**Proxy — translation core:** `internal/format/request.go` (esp. `:25-291`: generation mapping `:144-153`, effort `:155-174`, tiered `:176-188`, Claude budget `:190-215`, Gemini budget `:216-249`, clamps `:284-289`); `internal/format/model.go` (`:10-16` constants, `:45-89` family heuristic, `:99-112` budget clamp); `internal/format/content.go` (`:113-140` thinking→parts); `internal/format/thinking.go` (`:22-150` block hygiene); `internal/format/response.go` (`:27-40, :185-214` thought→thinking, usage); `internal/format/stream.go` (thinking SSE events); `internal/format/builder.go` (`:66-133` envelope, billing-token strip).

**Proxy — catalog & policy:** `internal/modelcatalog/catalog.go` (`:13-29` Model, `:71-83` details, `:90-129` aliases, `:275-347` `ExtractReasoningParams`, `:349-420` `ResolveWithRequest`, `:492-575` tiers incl. 3.7/3.8); `internal/api/server.go` (`:549-613` discovery, `:817-830` defaults, `:1770-1801` Zen fill, `:1978-2050` `applyMaxTokensPolicy`/floor-16, `:2103-2109` Kimi suffix strip); `internal/api/dispatch.go` (`:65-140` gateway routing); `internal/api/discovery.go`; `internal/cloudcode/client.go` (`:22-55` endpoints, `:304-310` RPCs); `internal/ccidentity/apply.go` (`:114-129` omitted fields); `internal/headroom/stages/shaper/stage.go` (`:12-13, :265-296` mechanical shaping); `internal/config/config.go` (`:85-101` Kimi config); `internal/auth/kimi_oauth.go` (`:20-28` OAuth); `internal/zen/client.go` (`:76-79` Kimi Zen IDs); `internal/zen/chatwire.go` (`:349-353, :476-478` `reasoning_content` bridge); `internal/zen/responseswire.go` (`:27-31` drops); `internal/zen/zen.go` (`:24-28` Zen default 32768); `internal/kimi/{kimi,client,passthrough,identity}.go`; `internal/format/model.go:11` signature floor.

**Upstream protos:** `gen/google/cloud/aiplatform/master/content.pb.go` (`:530-538` level enum, `:3021-3053` GenerationConfig, `:6113-6121` ThinkingConfig); `gen/google/cloud/aiplatform/master/prediction_service.pb.go` (master request); `gen/v1internal/prediction_service.pb.go:80-91` + `prediction_service_grpc.pb.go:33-71` (v1internal RPCs); `gen/v1internal/model_configs.pb.go:153-192` (ModelDetails); `gen/v1internal/cloudcode.pb.go` (thinking summaries).

**Live captures:** `.reference/cloudcode-models-20260903.json` (context/output table §5.2); `.reference/agy-models-20260903.txt`; `docs/superpowers/plans/2026-08-26-kimi-code-gateway.md` + `2026-09-25-kimi-code-auth.md` (Kimi gateway design).

**Official docs (fetched 2026-10-05):** Anthropic [Effort](https://platform.claude.com/docs/en/build-with-claude/effort), [Thinking](https://platform.claude.com/docs/en/build-with-claude/thinking), [Extended thinking](https://platform.claude.com/docs/en/build-with-claude/extended-thinking), [Messages API reference](https://platform.claude.com/docs/en/api/messages/create), [Models overview](https://platform.claude.com/docs/en/models/overview); Google [GenerateContent thinking](https://ai.google.dev/gemini-api/docs/generate-content/thinking) + [Interactions thinking](https://ai.google.dev/gemini-api/docs/thinking); Moonshot [Messages API](https://platform.kimi.ai/docs/api/messages) + [Use Kimi in Claude Code](https://platform.kimi.ai/docs/guide/claude-code-kimi).

---

## Errata (verified 2026-10-05, see docs/superpowers/plans/2026-10-05-api-parameter-parity.md §0)

- **§3.3** `thinking_budget` and `thinking_level` are independent proto3 `optional` fields (separate synthetic oneofs in the descriptor, `content.pb.go:7584-7586`), not one oneof. "Only one may be set" is an unverified server rule; the proxy never sends both (probe GX records it).
- **§4 / G2 / Executive summary** The "three-way mismatch" holds only for the Moonshot Open Platform. Kimi Code (the OAuth gateway) uses model IDs `k3`, `k3-256k`, `kimi-for-coding`, `kimi-for-coding-highspeed`, and its server maps Claude Code's levels itself (`medium→high`, `xhigh→max`, unknown → 400).
- **G2** Collapsing `xhigh`/`max` to `high` is correct for the Gemini flash tiers and for `xhigh` on Claude 4.6 (Claude Code: "`xhigh` runs as `high` on Opus 4.6"). Only `max` on 4.6 is a real loss.
- **G4** With a live catalog entry, `adaptive` already gets the catalog's default budget (not 32000); the 32000/16000 defaults apply only without a catalog.
- **G7** Real Claude Code sends no `temperature`, `top_p`, `top_k` or `stop_sequences`; whether Cloud Code enforces Anthropic's thinking-time rule is a probe question (GC).
- **G8** `redacted_thinking` history is stripped and the tool-loop recovery runs; nothing leaks upstream. Not a bug.
- **G9** The legacy single-credential path ignored `reasoning_effort` and a top-level `thinking_budget` for Gemini (reproduced: `reasoning_effort:low` → budget 16000).
- **G10** Production-dead: only `options == nil` reaches it, and `cmd/proxy/main.go:261` always sets `Backend`.
- **G12** Not a gap: `/v1/messages` has no `max_output_tokens`, and Anthropic and Kimi both require `max_tokens`.
- **§5.2** The Claude "pass-through" rows (200K / 8192) described the Claude Code gateway defaults, which are 1M / 128K for the 5-series and, for Haiku 4.5, 200K / **8192 where Anthropic publishes 64K** (fixed in the plan, Task 9).
- **Missing from the report:** the agy CLI's own `--effort low|medium|high|xhigh|max`; the Claude Code gateway's deliberate omission of `claude-opus-5-5`/`claude-sonnet-5-5`; the OpenAI endpoint dropping `reasoning_effort`; the shaper/prompt-cache interaction.
