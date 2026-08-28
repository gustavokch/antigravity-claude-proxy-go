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

// ClampCacheTTL clamps TTL seconds to [MinCacheTTLSeconds, MaxCacheTTLSeconds].
// A non-positive TTL means "unset" and yields the default.
func ClampCacheTTL(ttl int) int {
	if ttl <= 0 {
		return DefaultCacheTTLSeconds
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

// parseCacheFlag reads a boolean cache header. Only "true" and "false" are
// recognized, case-insensitively; anything else reports ok=false so the caller
// falls back to proxy configuration instead of forwarding a value upstream
// cannot be assumed to understand.
func parseCacheFlag(raw string) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// parseCacheTTL reads a TTL header and clamps it to the supported range.
// An unparseable or empty value reports ok=false.
func parseCacheTTL(raw string) (value int, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	ttl, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return ClampCacheTTL(ttl), true
}

// ApplyResponseCacheHeaders sets upstream cache request headers from the
// resolved config and any allowed client overrides.
//
// Client headers are never merely passed along: values are parsed here and
// re-emitted in canonical form, and every TTL is clamped locally rather than
// trusting upstream to reject an out-of-range one. The upstream request is
// built fresh by the caller, so a client header that is not re-emitted here
// simply never reaches OpenRouter.
func ApplyResponseCacheHeaders(upReq *http.Request, incomingHeader http.Header, cfg ResolvedResponseCacheConfig) {
	clientCache, clientCacheOK := parseCacheFlag(incomingHeader.Get(HeaderCache))
	clientTTL, clientTTLOK := parseCacheTTL(incomingHeader.Get(HeaderCacheTTL))
	clientClear, _ := parseCacheFlag(incomingHeader.Get(HeaderCacheClear))

	cacheEnabled := cfg.Enabled
	ttl := cfg.TTLSeconds
	if cfg.AllowClientOverride {
		if clientCacheOK {
			cacheEnabled = clientCache
		}
		// A TTL on its own tunes caching that is already on; it never turns
		// caching on by itself.
		if clientTTLOK {
			ttl = clientTTL
		}
	}

	switch {
	case cacheEnabled:
		upReq.Header.Set(HeaderCache, "true")
		upReq.Header.Set(HeaderCacheTTL, strconv.Itoa(ttl))
		// Clear only rides along with a request that actually carries caching:
		// upstream treats it as a no-op otherwise, so forwarding it would be
		// misleading.
		if clientClear {
			upReq.Header.Set(HeaderCacheClear, "true")
		}
	case cfg.AllowClientOverride && clientCacheOK:
		// The client explicitly opted out of caching this request.
		upReq.Header.Set(HeaderCache, "false")
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
