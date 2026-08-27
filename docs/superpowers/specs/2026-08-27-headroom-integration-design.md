# Headroom Native Context Compression & Shaping Engine Design

## 1. Overview
This specification details the native Go integration of **Headroom** ([headroomlabs-ai/headroom](https://github.com/headroomlabs-ai/headroom)) into `antigravity-claude-proxy-go`. The engine provides seamless, provider-agnostic context compression, prompt cache alignment, content-conditioned retrieval (CCR), and output shaping across Cloud Code (Gemini/Claude), OpenRouter, Kimi, and Custom Endpoints.

---

## 2. Core Architecture & Pipeline Lifecycle

### 2.1 Pipeline Flow
```
HTTP /v1/messages Request
   ↓
[Headroom Engine Pipeline]
   ├─ Stage 1: SmartCrusher (Compact JSON tool results position-independently)
   ├─ Stage 2: CodeCompressor (Prune trailing whitespace, blank lines, and repeated log lines)
   ├─ Stage 3: CCR Demotion [Phase 2] (Cache chunks outside liveTurns window; inject headroom_retrieve tool)
   └─ Stage 4: OutputShaper (Append verbosity steering prompt; clamp mechanical thinking effort)
   ↓
[Provider Dispatcher] (Cloud Code / OpenRouter / Kimi / Custom Endpoints)
   ↓
[Stream Response Interceptor [Phase 2]] (Intercept headroom_retrieve tool calls, hydrate from CCR store transparently for Cloud Code / OpenRouter)
   ↓
Client HTTP Response Stream
```

### 2.2 Core Package: `internal/headroom`
* **`Engine`**: Coordinates execution of enabled stages on inbound requests and manages the global CCR chunk store.
* **`Pipeline`**: Sequential container for modular `Stage` implementations.
* **`RequestContext`**: Transient state tracking request map mutated in place, `FrozenPrefixIndex`, byte accounting (`BytesBefore`, `BytesAfter`), effort routing telemetry (`EffortClamped`, `OriginalThinking`, `ClampedThinking`), and CCR chunk mappings.

---

## 3. Modular Pipeline Stages

### 3.1 Cache Alignment Strategy (Invariant I1)
* **Goal**: Prevent invalidation of provider KV caches / prompt cache prefixes across conversation turns.
* **Determinism**: Every compression transform (`SmartCrusher`, `CodeCompressor`) is a pure, position-independent function applied to all `tool_result` blocks in the request, history included.
* **Live Turns**: The `liveTurns` parameter only scopes CCR chunk demotion (Phase 2), ensuring that recent message turns stay inline where the model needs immediate access.

### 3.2 Stage 1: SmartCrusher (Structured JSON Compactor)
* **Goal**: Minimize token consumption of structured API outputs and data in `tool_result` blocks.
* **Algorithm**:
  1. Inspects text inside `tool_result` content blocks for JSON objects/arrays.
  2. Uses `json.Compact` to strip insignificant whitespace while preserving key order and number literals byte-for-byte.
  3. Non-JSON and malformed JSON payloads pass through untouched.
  4. *Note*: Uniform-array to tabular (pipe/tab-delimited) conversion is deferred as a follow-up to preserve schema validity and model expectations.

### 3.3 Stage 2: CodeCompressor (Code & Text Pruning)
* **Goal**: Prune token-heavy terminal outputs, logs, and code listings.
* **Algorithm**:
  1. Trims trailing whitespace from each line (leading whitespace is strictly preserved as code indentation).
  2. Collapses 3+ consecutive newlines (2+ blank lines) into a single blank line.
  3. Detects recurring identical non-empty lines (runs of 3+) and folds them into `[... repeated N times ...]`.

### 3.4 Stage 3: CCR (Content-Conditioned Retrieval) & Transparent Interception (Phase 2)
* **Goal**: Enable reversible compression for oversized older outputs.
* **Scope**: CCR hydration is feasible only on paths where the proxy controls the continuation request loop (Cloud Code and OpenRouter). Kimi and reverse-proxy custom endpoints bypass CCR demotion.
* **In-Memory Store**:
  * Thread-safe LRU Cache with configurable capacity (`maxStoreMB`, default 64MB).
  * Payloads outside the `liveTurns` window exceeding `minChunkBytes` (default 2048 bytes) are stored in the LRU with ID `chunk_<sha256[:12]>`.
* **Tool Injection & Hydration**:
  * Injects `headroom_retrieve` tool definition when CCR is enabled and client sent a `tools` list.
  * Intercepts `headroom_retrieve` calls, retrieves chunk from store, and streams continuation.

### 3.5 Stage 4: OutputShaper (Verbosity Steering & Effort Routing)
* **Goal**: Reduce unnecessary output tokens and excessive thinking on mechanical continuations.
* **Verbosity Steering**:
  * Appends concise technical instruction text to the system prompt tail.
* **Effort Routing**:
  * Detects mechanical continuation turns (where the last turn consists purely of non-error `tool_result` blocks).
  * Clamps `thinking.budget_tokens` to `mechanicalThinkingBudget` (default 1024) or OpenAI `reasoning.effort` / `reasoning_effort` to `low`.

---

## 4. Configuration & WebUI Integration

### 4.1 `internal/config` Schema
```json
{
  "headroom": {
    "enabled": false,
    "smartCrusher": true,
    "codeCompressor": true,
    "liveTurns": 2,
    "ccr": {
      "enabled": false,
      "maxStoreMB": 64,
      "minChunkBytes": 2048
    },
    "outputShaper": {
      "enabled": false,
      "verbositySteering": true,
      "steeringText": "",
      "effortRouting": true,
      "mechanicalThinkingBudget": 1024
    }
  }
}
```

### 4.2 WebUI Settings & Dashboard
* Dedicated **Headroom** section in WebUI Server settings.
* Instant toggles for master engine, individual compressors, Output Shaper, and CCR.
* Real-time metrics card on Dashboard showing Bytes Saved, Compression Ratio, Requests Compressed, and Thinking Tokens Clamped.

---

## 5. Metrics & Observability

* Integrated into `internal/stats.Tracker`:
  * `bytesBefore`: Total uncompressed bytes across rewritten tool result blocks.
  * `bytesAfter`: Total compressed bytes across rewritten tool result blocks.
  * `thinkingTokensClamped`: Cumulative thinking token budget delta applied on mechanical turns.
  * `requestsCompressed`: Total count of requests processed by Headroom.
  * `ccrRetrievals`: Total count of dynamic CCR chunk retrieval roundtrips (Phase 2).
* Telemetry accessible via `GET /api/headroom/stats`.
