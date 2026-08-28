package openrouter

import (
	"net/http"
	"strconv"
	"strings"
)

const (
	HeaderCache         = "X-OpenRouter-Cache"
	HeaderCacheTTL      = "X-OpenRouter-Cache-TTL"
	HeaderCacheClear    = "X-OpenRouter-Cache-Clear"
	HeaderCacheStatus   = "X-OpenRouter-Cache-Status"
	HeaderCacheAge      = "X-OpenRouter-Cache-Age"
	HeaderCacheSourceID = "X-OpenRouter-Cache-Source-Id"

	DefaultCacheTTLSeconds = 300
	MinCacheTTLSeconds     = 1
	MaxCacheTTLSeconds     = 86400
)

// ResponseCacheConfig holds response caching settings.
// Pointer fields distinguish "unset" from explicit values:
//   - Enabled:             nil = false at global level; nil = inherit global at model level.
//   - AllowClientOverride: nil = true (clients may override unless explicitly denied).
//   - TTLSeconds:          0 = inherit/default 300.
type ResponseCacheConfig struct {
	Enabled             *bool `json:"enabled,omitempty"`
	TTLSeconds          int   `json:"ttlSeconds,omitempty"`
	AllowClientOverride *bool `json:"allowClientOverride,omitempty"`
}

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
	TTLSeconds          int // already clamped to [1, 86400]
	AllowClientOverride bool
}

// ResolveResponseCacheConfig merges a per-model config over the global config.
// Nil pointer fields inherit; TTL 0 inherits. Defaults when unset at both
// levels: Enabled=false, TTLSeconds=300, AllowClientOverride=true.
func ResolveResponseCacheConfig(global ResponseCacheConfig, model *ResponseCacheConfig) ResolvedResponseCacheConfig {
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
