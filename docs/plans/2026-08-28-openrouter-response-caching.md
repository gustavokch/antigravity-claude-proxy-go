# OpenRouter Response Caching — Implementation Plan

## Context
OpenRouter supports server-side response caching across providers for both streaming and non-streaming requests. Caching is executed at OpenRouter's routing layer, keyed by API key, model, endpoint type, streaming mode, and SHA-256 of the request payload. Cache hits are 100% free ($0 token cost, 0 tokens consumed) and bypass upstream provider rate limits. The gateway forwards to OpenRouter's `/v1/messages` endpoint (Anthropic Messages), which is in the supported-endpoint set.

Upstream behaviors that shape this design:
- On a cache HIT, OpenRouter itself zeroes all usage counters in the response body (`prompt_tokens`/`completion_tokens`/`total_tokens`; `input_tokens`/`output_tokens` on the Anthropic Messages endpoint). $0 cost therefore falls out of normal cost calculation; the explicit zeroing in this plan is a guard, not the mechanism.
- Only `200 OK` responses are cached. Streaming hits are replayed through the streaming pipeline; each hit gets a fresh generation ID (`X-Generation-Id`), with the original in `X-OpenRouter-Cache-Source-Id`.
- No request coalescing: concurrent identical requests both MISS and are billed independently.
- Cache entries may be evicted before TTL expiry under memory pressure; TTL is not guaranteed.
- Response caching is disabled entirely for accounts with Zero Data Retention (ZDR) enforced.
- The cache key is sensitive to JSON property order, but this gateway re-marshals request bodies (Go maps emit sorted keys), so logically identical requests through the proxy normalize to one key. Explicit vs. omitted optional fields (e.g. `temperature: 1.0`) still produce distinct keys — expected, no action.

This plan integrates OpenRouter response caching into the `antigravity-claude-proxy-go` gateway:
1. Enabling global and per-model response caching configuration.
2. Handling request header injection and client override pass-through (`X-OpenRouter-Cache`, `X-OpenRouter-Cache-TTL`, `X-OpenRouter-Cache-Clear`).
3. Ensuring upstream response cache headers (`X-OpenRouter-Cache-Status`, `X-OpenRouter-Cache-Age`, `X-OpenRouter-Cache-TTL`, `X-OpenRouter-Cache-Source-Id`) are propagated to clients.
4. Updating metrics and cost calculation to reflect 100% discount on cache hits with clear observability logging.
5. Exposing response caching settings in the management API.

---

## Technical Specifications & Architecture

### 1. OpenRouter Response Caching Contract
* **Request Headers (Client/Proxy -> OpenRouter):**
  * `X-OpenRouter-Cache`: `"true"` (enable) or `"false"` (bypass/disable).
  * `X-OpenRouter-Cache-TTL`: Integer seconds `[1, 86400]` (default 300).
  * `X-OpenRouter-Cache-Clear`: `"true"` (force cache invalidation & fetch fresh completion).
* **Response Headers (OpenRouter -> Proxy -> Client):**
  * `X-OpenRouter-Cache-Status`: `"HIT"` or `"MISS"`.
  * `X-OpenRouter-Cache-Age`: Integer seconds (duration cached on HIT).
  * `X-OpenRouter-Cache-TTL`: Integer seconds (remaining TTL on HIT, total TTL on MISS).
  * `X-OpenRouter-Cache-Source-Id`: Generation ID of the source request on HIT.
* **Cache Discount & Billing:**
  * Cache hits are **100% free** ($0.0000 cost, 0 tokens billed).
  * All usage metrics (input, output, cache read/write) on hit are reported as `0`.

---

### 2. Configuration Schema (`internal/config/config.go`)
```go
// OpenRouterResponseCacheConfig holds response caching settings.
// Pointer fields distinguish "unset" from explicit values:
//   - Enabled:             nil = false at global level; nil = inherit global at model level.
//   - AllowClientOverride: nil = true (clients may override unless explicitly denied).
//   - TTLSeconds:          0 = inherit/default 300.
type OpenRouterResponseCacheConfig struct {
    Enabled             *bool `json:"enabled,omitempty"`
    TTLSeconds          int   `json:"ttlSeconds,omitempty"`
    AllowClientOverride *bool `json:"allowClientOverride,omitempty"`
}
```

Pointer fields are deliberate: with plain `bool`, a field omitted from JSON is indistinguishable from explicit `false`. That made the originally drafted "default true" for `AllowClientOverride` unimplementable, and under whole-struct replacement a per-model entry setting only `ttlSeconds` would silently disable caching. Per-model config merges field-by-field over global instead of replacing it.

```go

type OpenRouterConfig struct {
    Enabled       bool                           `json:"enabled"`
    APIKey        string                         `json:"apiKey,omitempty"`
    BaseURL       string                         `json:"baseUrl,omitempty"`
    Allowlist     []OpenRouterModelConfig        `json:"allowlist,omitempty"`
    Routing       OpenRouterRoutingConfig        `json:"routing,omitempty"`
    AppSpoof      OpenRouterAppSpoofConfig       `json:"appSpoof,omitempty"`
    ResponseCache OpenRouterResponseCacheConfig  `json:"responseCache,omitempty"`
}

type OpenRouterModelConfig struct {
    ID              string                          `json:"id"`
    Alias           string                          `json:"alias,omitempty"`
    DisplayName     string                          `json:"displayName,omitempty"`
    ContextLen      int                             `json:"contextLength,omitempty"`
    MaxOutputTokens int                             `json:"maxOutputTokens,omitempty"`
    Enabled         bool                            `json:"enabled"`
    ProviderMode    string                          `json:"providerMode,omitempty"`
    PinnedProvider  string                          `json:"pinnedProvider,omitempty"`
    ProviderOrder   []string                        `json:"providerOrder,omitempty"`
    ResponseCache   *OpenRouterResponseCacheConfig  `json:"responseCache,omitempty"`
}
```

---

### 3. OpenRouter Cache Helpers (`internal/openrouter/cache.go`)
Define header constants and helper functions:
```go
package openrouter

import (
    "net/http"
    "strconv"
    "strings"

    "antigravity-go-proxy/internal/config"
)

const (
    HeaderCache            = "X-OpenRouter-Cache"
    HeaderCacheTTL         = "X-OpenRouter-Cache-TTL"
    HeaderCacheClear       = "X-OpenRouter-Cache-Clear"
    HeaderCacheStatus      = "X-OpenRouter-Cache-Status"
    HeaderCacheAge         = "X-OpenRouter-Cache-Age"
    HeaderCacheSourceID    = "X-OpenRouter-Cache-Source-Id"

    DefaultCacheTTLSeconds = 300
    MinCacheTTLSeconds     = 1
    MaxCacheTTLSeconds     = 86400
)

type ResponseCacheInfo struct {
    Status   string // "HIT", "MISS", or ""
    Age      int    // Age in seconds
    TTL      int    // Remaining/total TTL in seconds
    SourceID string // Original generation ID
}

// ClampCacheTTL clamps TTL seconds to [1, 86400]. Returns 300 if ttl <= 0.
func ClampCacheTTL(ttl int) int {
    if ttl <= 0 {
        return DefaultCacheTTLSeconds
    }
    if ttl < MinCacheTTLSeconds {
        return MinCacheTTLSeconds
    }
    if ttl > MaxCacheTTLSeconds {
        return MaxCacheTTLSeconds
    }
    return ttl
}

// ResolvedResponseCacheConfig is the effective configuration after merging
// per-model settings over global settings and applying defaults.
type ResolvedResponseCacheConfig struct {
    Enabled             bool
    TTLSeconds          int  // already clamped to [1, 86400]
    AllowClientOverride bool
}

// ResolveResponseCacheConfig merges a per-model config over the global config.
// Nil pointer fields inherit; TTL 0 inherits. Defaults when unset at both
// levels: Enabled=false, TTLSeconds=300, AllowClientOverride=true.
func ResolveResponseCacheConfig(global config.OpenRouterResponseCacheConfig, model *config.OpenRouterResponseCacheConfig) ResolvedResponseCacheConfig {
    eff := ResolvedResponseCacheConfig{
        Enabled:             global.Enabled != nil && *global.Enabled,
        TTLSeconds:          ClampCacheTTL(global.TTLSeconds),
        AllowClientOverride: global.AllowClientOverride == nil || *global.AllowClientOverride,
    }
    if model != nil {
        if model.Enabled != nil {
            eff.Enabled = *model.Enabled
        }
        if model.TTLSeconds > 0 {
            eff.TTLSeconds = ClampCacheTTL(model.TTLSeconds)
        }
        if model.AllowClientOverride != nil {
            eff.AllowClientOverride = *model.AllowClientOverride
        }
    }
    return eff
}

// ApplyResponseCacheHeaders sets upstream cache request headers from the
// resolved config and any allowed client overrides.
func ApplyResponseCacheHeaders(upReq *http.Request, incomingHeader http.Header, cfg ResolvedResponseCacheConfig) {
    clientCache := incomingHeader.Get(HeaderCache)
    clientTTL := incomingHeader.Get(HeaderCacheTTL)
    clientClear := incomingHeader.Get(HeaderCacheClear)

    cacheEnabled := false
    switch {
    case clientCache != "" && cfg.AllowClientOverride:
        // 1. Client override. TTL passed verbatim: upstream clamps
        // out-of-range values and ignores unparseable ones.
        upReq.Header.Set(HeaderCache, clientCache)
        if clientTTL != "" {
            upReq.Header.Set(HeaderCacheTTL, clientTTL)
        }
        cacheEnabled = clientCache == "true"
    case cfg.Enabled:
        // 2. Proxy configuration (client headers stripped when override denied)
        upReq.Header.Set(HeaderCache, "true")
        upReq.Header.Set(HeaderCacheTTL, strconv.Itoa(cfg.TTLSeconds))
        cacheEnabled = true
    }

    // 3. Clear is forwarded only when this request actually carries caching.
    //    Upstream treats Clear as a no-op when caching is disabled, so
    //    forwarding it then would be misleading.
    if clientClear == "true" && cacheEnabled {
        upReq.Header.Set(HeaderCacheClear, "true")
    }
}

// ExtractResponseCacheHeaders extracts cache headers from an upstream response.
func ExtractResponseCacheHeaders(header http.Header) ResponseCacheInfo {
    var info ResponseCacheInfo
    info.Status = strings.ToUpper(strings.TrimSpace(header.Get(HeaderCacheStatus)))
    if ageStr := header.Get(HeaderCacheAge); ageStr != "" {
        info.Age, _ = strconv.Atoi(ageStr)
    }
    if ttlStr := header.Get(HeaderCacheTTL); ttlStr != "" {
        info.TTL, _ = strconv.Atoi(ttlStr)
    }
    info.SourceID = strings.TrimSpace(header.Get(HeaderCacheSourceID))
    return info
}
```

---

### 4. Observability & Pricing Integration (`internal/openrouter/observability.go`)
Extend `RequestMetrics`:
```go
type RequestMetrics struct {
    Model               string        `json:"model"`
    SessionID           string        `json:"session_id"`
    Provider            string        `json:"provider,omitempty"`
    InputTokens         int           `json:"input_tokens"`
    OutputTokens        int           `json:"output_tokens"`
    CacheReadTokens     int           `json:"cache_read_tokens"`
    CacheCreationTokens int           `json:"cache_creation_tokens"`
    Latency             time.Duration `json:"latency"`
    ThroughputTPS       float64       `json:"throughput_tps"`
    CacheHitRate        float64       `json:"cache_hit_rate"`
    CallCost            float64       `json:"call_cost"`
    SessionCost         float64       `json:"session_cost"`
    
    // Response Caching
    CacheStatus         string        `json:"cache_status,omitempty"`     // "HIT", "MISS", or ""
    CacheAge            int           `json:"cache_age_seconds,omitempty"` // Age in seconds on HIT
    CacheTTL            int           `json:"cache_ttl_seconds,omitempty"`
    CacheSourceID       string        `json:"cache_source_id,omitempty"`
}
```

Update `ComputeFinalMetrics`:
```go
func (m *RequestMetrics) ComputeFinalMetrics(pricing Pricing, sessionTracker *SessionTracker) {
    ...
    // Cost per API call: 100% free if response cache HIT
    if m.CacheStatus == "HIT" {
        m.CallCost = 0.0
    } else {
        m.CallCost = CalculateCost(pricing, m.InputTokens+m.CacheReadTokens, m.OutputTokens, m.CacheReadTokens, m.CacheCreationTokens)
    }
    ...
}
```

Note: on a HIT the upstream body already reports zeroed usage, so token fields read `0 in / 0 out`, TPS is 0, and the prompt-cache hit-rate line shows `0.0%` (the existing `totalPrompt > 0` division guard in `ComputeFinalMetrics` covers the 0/0 case). This is expected: the numbers describe billing, not work performed. The explicit `CallCost = 0.0` above is a guard against future upstream change, not the mechanism that produces $0.

Update `LogObservability`:
```go
func LogObservability(log *slog.Logger, m RequestMetrics) {
    if log == nil {
        log = slog.Default()
    }

    cacheTag := ""
    if m.CacheStatus == "HIT" {
        cacheTag = fmt.Sprintf(" | response cache: HIT (age: %ds)", m.CacheAge)
    } else if m.CacheStatus == "MISS" {
        cacheTag = " | response cache: MISS"
    }

    msg := fmt.Sprintf("[OpenRouter] %s%s | tokens: %s in (%s cached, %.1f%% hit), %s out | %.1f TPS | %s | $%.4f ($%.4f session)",
        m.Model,
        cacheTag,
        formatInt(m.InputTokens),
        formatInt(m.CacheReadTokens),
        m.CacheHitRate,
        formatInt(m.OutputTokens),
        m.ThroughputTPS,
        m.Latency.Round(10*time.Millisecond),
        m.CallCost,
        m.SessionCost,
    )
    if m.Provider != "" {
        msg += fmt.Sprintf(" | provider: %s", m.Provider)
    }

    attrs := []any{
        slog.String("gateway", "openrouter"),
        slog.String("model", m.Model),
        slog.String("session_id", m.SessionID),
        slog.String("provider", m.Provider),
        slog.Int("input_tokens", m.InputTokens),
        slog.Int("output_tokens", m.OutputTokens),
        slog.Int("cache_read_tokens", m.CacheReadTokens),
        slog.Int("cache_creation_tokens", m.CacheCreationTokens),
        slog.Float64("cache_hit_rate_pct", m.CacheHitRate),
        slog.Float64("tps", m.ThroughputTPS),
        slog.Duration("latency", m.Latency),
        slog.Float64("call_cost_usd", m.CallCost),
        slog.Float64("session_cost_usd", m.SessionCost),
        slog.String("level_tag", "SUCCESS"),
    }
    if m.CacheStatus != "" {
        attrs = append(attrs,
            slog.String("response_cache_status", m.CacheStatus),
            slog.Int("response_cache_age", m.CacheAge),
            slog.Int("response_cache_ttl", m.CacheTTL),
            slog.String("response_cache_source_id", m.CacheSourceID),
        )
    }

    log.Info(msg, attrs...)
}
```

---

### 5. Proxy Request & Response Flow (`internal/api/server.go`)
1. **Request Header Setup:**
   In `forwardToOpenRouter`:
   ```go
   cacheCfg := openrouter.ResolveResponseCacheConfig(openRouterCfg.ResponseCache, perModel.ResponseCache)
   openrouter.ApplyResponseCacheHeaders(upReq, request.Header, cacheCfg)
   ```
2. **Response Header Extraction & Downstream Copying:**
   - `copyUpstreamHeaders` automatically copies `X-OpenRouter-Cache-*` response headers to the client.
   - Extract cache info from `resp.Header`:
     ```go
     cacheInfo := openrouter.ExtractResponseCacheHeaders(resp.Header)
     ```
3. **Record Metrics with Cache Info:**
   - Update `recordOpenRouterMetrics` to accept `cacheInfo openrouter.ResponseCacheInfo`:
     ```go
     func (server *Server) recordOpenRouterMetrics(model, sessionID string, pricing openrouter.Pricing, startTime time.Time, in, out, cr, cw int, provider string, cacheInfo openrouter.ResponseCacheInfo) openrouter.RequestMetrics
     ```
   - In `proxyStreamResponse`, capture `cacheInfo` from `resp.Header` and pass it to `recordOpenRouterMetrics` when stream completes.

---

### 6. Management API (`internal/api/management.go`)
Both handlers are generic: `handleOpenRouterConfigSave` merges the posted body verbatim into the stored config (management.go:1105), and `handleOpenRouterConfigGet` returns the public config map. No handler code changes are required once the field exists on the config structs. Verify with tests only:
- POST round-trips `responseCache` (global and per-model) through `config.Save` without dropping pointer-field defaults.
- GET exposes `responseCache` after a save.

---

## File Change Summary

| Action | File | Description |
|---|---|---|
| **Create** | `internal/openrouter/cache.go` | Header constants, TTL clamping, config resolve/merge, request header injection, response header extraction |
| **Create** | `internal/openrouter/cache_test.go` | Unit tests for cache header logic, TTL bounds, overrides |
| **Modify** | `internal/config/config.go` | Add `OpenRouterResponseCacheConfig` to `OpenRouterConfig` and `OpenRouterModelConfig` |
| **Modify** | `internal/openrouter/observability.go` | Add cache fields to `RequestMetrics`, $0 cost on HIT, enhanced logging |
| **Modify** | `internal/openrouter/observability_test.go` | Unit tests verifying $0 cost on HIT and structured log output |
| **Modify** | `internal/api/server.go` | Inject request cache headers and capture response cache headers in unary & stream paths |
| **Modify** | `internal/api/management_test.go` | Round-trip coverage for `responseCache` (handlers are generic; no production code change expected) |
| **Create** | `internal/api/openrouter_cache_test.go` | End-to-end proxy tests for unary & streaming cache HIT/MISS and header forwarding |

---

## Step-by-Step Execution Sequence

### Phase 1: Core OpenRouter Cache Package
1. Create `internal/openrouter/cache.go` with constants, `ClampCacheTTL`, `ResolveResponseCacheConfig`, `ApplyResponseCacheHeaders`, and `ExtractResponseCacheHeaders`.
2. Create `internal/openrouter/cache_test.go` verifying TTL clamping, resolve/merge inheritance (nil fields inherit, explicit per-model values win, unset-at-both-levels defaults `enabled=false` / `ttl=300` / `allowOverride=true`), client override allow/deny, Clear gating (forwarded only when the request carries caching), and response header parsing.

### Phase 2: Configuration & Management
1. Add `OpenRouterResponseCacheConfig` to `internal/config/config.go` (pointer fields per Section 2).
2. Management handlers need no code change (generic merge/save) — extend `internal/api/management_test.go` to prove `responseCache` round-trips through GET/POST, including omitted pointer fields.

### Phase 3: Observability & Metrics
1. Add cache fields to `RequestMetrics` in `internal/openrouter/observability.go`.
2. Update `ComputeFinalMetrics` to set `CallCost = 0.0` when `CacheStatus == "HIT"`.
3. Update `LogObservability` to format response cache tag and slog attributes.
4. Add tests in `internal/openrouter/observability_test.go`.

### Phase 4: Proxy Forwarding & Streaming Integration
1. Update `forwardToOpenRouter` in `internal/api/server.go` to apply cache request headers.
2. Update `recordOpenRouterMetrics` and `proxyStreamResponse` to extract and record `ResponseCacheInfo`.
3. Create `internal/api/openrouter_cache_test.go` with full end-to-end test scenarios:
   - Unary request with cache enabled -> verify upstream received headers.
   - Client override with `X-OpenRouter-Cache: false` -> verify upstream received `false`.
   - Client override denied (`allowClientOverride: false`) with proxy caching off -> verify client `X-OpenRouter-Cache`, `X-OpenRouter-Cache-TTL`, and `X-OpenRouter-Cache-Clear` are all stripped.
   - Client `X-OpenRouter-Cache-Clear: true` with proxy caching on -> verify Clear forwarded; with caching effectively off -> verify Clear stripped.
   - Upstream cache HIT response -> verify downstream received `X-OpenRouter-Cache-Status: HIT` and metrics recorded $0 cost.
   - HIT body with zeroed usage -> verify metrics record 0 tokens / $0 with no division-by-zero in hit-rate math.
   - MISS response -> verify `X-OpenRouter-Cache-TTL` reaches the client.
   - Streaming SSE response with cache HIT -> verify SSE forwarded, response headers preserved, and metrics recorded $0 cost.
4. Manual smoke test against real OpenRouter: send the same unary request twice with caching enabled; expect `MISS` then `HIT` with zeroed usage, and confirm the proxy log shows `response cache: HIT` at $0.

---

## Verification & Testing

### Commands to Run
```bash
# 1. Run openrouter unit tests
go test -v ./internal/openrouter -run "TestResponseCache|TestObservability"

# 2. Run proxy cache integration tests
go test -v ./internal/api -run "TestOpenRouter.*Cache|TestOpenRouterForwarding"

# 3. Run full test suite
go test ./...
```
