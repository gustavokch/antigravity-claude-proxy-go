# Plan: Antigravity / Cloud Code Observability Enhancement

## 1. Goal
Upgrade Antigravity / Cloud Code request logging to the observability standard established by OpenRouter and Claude Code gateways. Provide unified one-line log summaries, structured `slog` fields, session tracking, token throughput (TPS), cache hit rates, retail cost calculations, and real-time WebUI stream integration.

---

## 2. Gap Analysis

| Feature | OpenRouter Gateway | Claude Code Gateway | Antigravity / Cloud Code (Current) |
| :--- | :--- | :--- | :--- |
| **Log Summary** | `[OpenRouter] model \| tokens: ... \| TPS \| Latency \| Cost` | `[ClaudeCode] model (acc) \| tokens: ... \| TPS \| Latency \| Cost` | **None** (Only generic HTTP middleware log) |
| **Structured slog** | `gateway=openrouter`, token breakdown, TPS, latency | `gateway=claudecode`, token breakdown, TPS, latency | **None** |
| **WebUI Stream** | Emits `level_tag=SUCCESS` | Emits `level_tag=SUCCESS` | Emits generic `[API] Request succeeded` without token metrics |
| **Session Tracker** | LRU session map, cumulative cost & tokens | LRU session map, cumulative cost & tokens | **None** |
| **Cache Hit Rate** | Calculated `(cr / (in + cr)) * 100` | Calculated `(cr / (in + cr)) * 100` | Tracked in stats tracker, not logged |
| **Throughput (TPS)**| `output_tokens / duration.Seconds()` | `output_tokens / duration.Seconds()` | **None** |
| **Cost / Value** | Calculated from provider pricing | Calculated from Anthropic pricing | **None** (Shows $0 or omits value) |
| **Thinking Tokens** | N/A | Logged in usage | Parsed in stream converter, omitted in log |
| **CCR Retrievals** | N/A | N/A | Recorded in tracker, omitted in log |

---

## 3. Architecture Specification

### 3.1 Package: `internal/cloudcode/observability.go`

Define `RequestMetrics`:
```go
type RequestMetrics struct {
    Model               string        `json:"model"`
    Account             string        `json:"account,omitempty"`
    ProjectID           string        `json:"project_id,omitempty"`
    SessionID           string        `json:"session_id,omitempty"`
    InputTokens         int           `json:"input_tokens"`
    OutputTokens        int           `json:"output_tokens"`
    CacheReadTokens     int           `json:"cache_read_tokens"`
    ThinkingTokens      int           `json:"thinking_tokens,omitempty"`
    CCRRetrievals       int           `json:"ccr_retrievals,omitempty"`
    Latency             time.Duration `json:"latency"`
    ThroughputTPS       float64       `json:"throughput_tps"`
    CacheHitRate        float64       `json:"cache_hit_rate"`
    RetailCostUSD       float64       `json:"retail_cost_usd"`
    SessionRetailUSD    float64       `json:"session_retail_usd"`
}
```

### 3.2 Session Tracking
- Implement `SessionTracker` with sync.RWMutex and LRU eviction (cap at 10,000 active sessions).
- Extract session key using `ccExtractSessionID` (header: `x-session-id`, `session-id`, `anthropic-session-id`; body: `metadata.session_id`, `metadata.user_id`).
- Aggregate cumulative prompt tokens, completion tokens, cache reads, and retail value saved.

### 3.3 Current `agy` Model List & Pricing

Model list from `agy 1.1.25` baseline (`.reference/agy-current-models.txt`):

| Model ID | Display Name | Prompt (USD/M) | Completion (USD/M) | Cache Read (USD/M) |
| :--- | :--- | :--- | :--- | :--- |
| **`claude-opus-4-6-thinking`** | Claude Opus 4.6 (Thinking) | $15.00 | $75.00 | $1.50 |
| **`claude-sonnet-4-6`** | Claude Sonnet 4.6 (Thinking) | $3.00 | $15.00 | $0.30 |
| **`gemini-3.1-pro-high`** | Gemini 3.1 Pro (High) | $2.00 | $12.00 | $0.50 |
| **`gemini-3.1-pro-low`** | Gemini 3.1 Pro (Low) | $2.00 | $12.00 | $0.50 |
| **`gemini-3.8-flash-high`** | Gemini 3.8 Flash (High) | *$1.35* / *$2.70* | *$6.75* / *$13.50* | *$0.3375* / *$0.675* |
| **`gemini-3.8-flash-medium`** | Gemini 3.8 Flash (Medium) | *$1.35* / *$2.70* | *$6.75* / *$13.50* | *$0.3375* / *$0.675* |
| **`gemini-3.8-flash-low`** | Gemini 3.8 Flash (Low) | *$1.35* / *$2.70* | *$6.75* / *$13.50* | *$0.3375* / *$0.675* |
| **`gemini-3.7-flash-high`** | Gemini 3.7 Flash (High) | *$1.35* / *$2.70* | *$6.75* / *$13.50* | *$0.3375* / *$0.675* |
| **`gemini-3.7-flash-medium`** | Gemini 3.7 Flash (Medium) | *$1.35* / *$2.70* | *$6.75* / *$13.50* | *$0.3375* / *$0.675* |
| **`gemini-3.7-flash-low`** | Gemini 3.7 Flash (Low) | *$1.35* / *$2.70* | *$6.75* / *$13.50* | *$0.3375* / *$0.675* |
| **`gemini-3.6-flash-high`** | Gemini 3.6 Flash (High) | *$1.35* / *$2.70* | *$6.75* / *$13.50* | *$0.3375* / *$0.675* |
| **`gemini-3.6-flash-medium`** | Gemini 3.6 Flash (Medium) | *$1.35* / *$2.70* | *$6.75* / *$13.50* | *$0.3375* / *$0.675* |
| **`gemini-3.6-flash-low`** | Gemini 3.6 Flash (Low) | *$1.35* / *$2.70* | *$6.75* / *$13.50* | *$0.3375* / *$0.675* |
| **`gpt-oss-120b-medium`** | GPT-OSS 120B (Medium) | $0.15 | $0.60 | $0.0375 |

*Note: Gemini Flash rates apply $1.35 in / $6.75 out through Dec 31, 2026; $2.70 in / $13.50 out starting Jan 1, 2027.*

```go
var flashTierCutoff = time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC)

func lookupGeminiFlashPricing(now time.Time) ModelPricing {
    if now.Before(flashTierCutoff) {
        return ModelPricing{
            Prompt:     1.35 / 1e6,
            Completion: 6.75 / 1e6,
            CacheRead:  0.3375 / 1e6,
        }
    }
    return ModelPricing{
        Prompt:     2.70 / 1e6,
        Completion: 13.50 / 1e6,
        CacheRead:  0.675 / 1e6,
    }
}

func CalculateRetailCost(model string, inputTokens, outputTokens, cacheReadTokens int, now time.Time) float64 {
    pricing := lookupModelPricing(model, now)
    return (float64(inputTokens) * pricing.Prompt) +
           (float64(outputTokens) * pricing.Completion) +
           (float64(cacheReadTokens) * pricing.CacheRead)
}
```

### 3.4 Target Log Output Formats

#### Human-Readable Console & WebUI Summary:
```text
[Antigravity] claude-sonnet-4-6 (dev@example.com) | tokens: 14,200 in (8,192 cached, 57.7% hit), 640 out (256 thinking) | 38.5 TPS | 16.60s | $0.0520 saved ($0.8420 session)
```
*(When CCR is active, append ` | CCR: 2 retrievals`)*

#### Structured slog Attributes:
- `gateway`: `"antigravity"`
- `model`: `m.Model`
- `account`: `m.Account`
- `project_id`: `m.ProjectID`
- `session_id`: `m.SessionID`
- `input_tokens`: `m.InputTokens`
- `output_tokens`: `m.OutputTokens`
- `cache_read_tokens`: `m.CacheReadTokens`
- `thinking_tokens`: `m.ThinkingTokens`
- `ccr_retrievals`: `m.CCRRetrievals`
- `cache_hit_rate_pct`: `m.CacheHitRate`
- `tps`: `m.ThroughputTPS`
- `latency`: `m.Latency`
- `retail_cost_usd`: `m.RetailCostUSD`
- `session_retail_usd`: `m.SessionRetailUSD`
- `level_tag`: `"SUCCESS"` *(Enables green badge & filter in WebUI)*

---

## 4. Implementation Steps

### Phase 1: Observability Core (`internal/cloudcode/observability.go`)
1. Create `internal/cloudcode/observability.go` and unit tests in `internal/cloudcode/observability_test.go`.
2. Implement `SessionTracker` with sync.RWMutex and LRU eviction.
3. Implement `CalculateRetailCost(model string, in, out, cr int, now time.Time) float64` supporting Flash 2026/2027 date cutoffs.
4. Implement `RequestMetrics.ComputeFinalMetrics(tracker *SessionTracker, now time.Time)`.
5. Implement `LogObservability(logger *slog.Logger, m RequestMetrics)`.

### Phase 2: Metadata Extraction & Upstream Plumbing
1. Update `streamSender` context in `internal/api/server.go` to carry resolved account email and project ID.
2. In `internal/accounts/dispatcher.go`, inject `account.Email` and `project` into the context during execution.
3. Expose thinking token extraction from `proxyformat.ThinkingAccumulator` and `proxyformat.StreamConverter`.

### Phase 3: Integration in Request Handlers (`internal/api/server.go`)
1. In `unaryMessage`:
   - Compute `latency = server.now().Sub(startTime)`.
   - Extract `sessionID = ccExtractSessionID(request, anthropicRequest)`.
   - Populate `cloudcode.RequestMetrics` with token counts, thinking tokens, CCR count, account email, and project ID.
   - Run `metrics.ComputeFinalMetrics(cloudcode.DefaultSessionTracker, server.now())`.
   - Run `cloudcode.LogObservability(server.logger, metrics)`.
2. In `streamMessage`:
   - Compute metrics upon terminal event emission.
   - Run `metrics.ComputeFinalMetrics(cloudcode.DefaultSessionTracker, server.now())`.
   - Run `cloudcode.LogObservability(server.logger, metrics)`.
3. In `internal/accounts/dispatcher.go`:
   - Remove redundant `logger.LogSuccess("[API] Request succeeded using account...")` to prevent duplicate entries in WebUI logs.
   - Keep structured retry/throttling logs.

### Phase 4: Kimi Gateway Alignment (`internal/kimi/`)
1. Add matching observability logging in `internal/kimi/` (`[Kimi] model | tokens: ... | TPS | Latency | level_tag=SUCCESS`).
2. Extract tokens from `ProxyAnthropicJSONWithCCR` and `ProxyAnthropicStreamWithCCR`.

---

## 5. Verification Plan

1. **Unit Tests**:
   - `internal/cloudcode/observability_test.go`:
     - Test TPS and cache hit rate calculation math.
     - Test session tracker accumulation and LRU eviction over 10,000 sessions.
     - Test pricing calculations for Gemini Pro ($2/$12), Gemini Flash 2026 vs 2027 tiers, Claude Sonnet/Opus, and GPT-OSS.
     - Test log line formatting with and without CCR/thinking tokens.
     - Verify `level_tag=SUCCESS` attribute emission.
2. **Integration Tests**:
   - `internal/api/antigravity_observability_test.go`:
     - Verify unary and streaming Cloud Code requests emit structured `[Antigravity]` logs.
     - Verify WebUI `Broadcaster` receives log entries with `level=SUCCESS`.
3. **Fingerprint & TLS Check**:
   - Verify `tls.Config` remains empty.
   - Confirm JA4 handshake match against `.reference/agy-current-baseline.txt`.
