package cachebump

import (
	"encoding/json"
	"time"
)

// DetectTTL reports the prompt-cache entry TTL a request body opted into.
// A cache_control marker with {"ttl":"1h"} means one hour; anything else
// (including no marker at all) means the default five minutes.
func DetectTTL(body []byte) time.Duration {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return 5 * time.Minute
	}
	if scanCacheControl(root) {
		return time.Hour
	}
	return 5 * time.Minute
}

// HasCacheControl reports whether the request body carries any cache_control
// marker. A body without markers can never hold a cache entry, so bumping it
// would only ever pay a cache write.
func HasCacheControl(body []byte) bool {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return false
	}
	return findCacheControl(root)
}

func findCacheControl(val any) bool {
	switch v := val.(type) {
	case map[string]any:
		if _, ok := v["cache_control"]; ok {
			return true
		}
		for _, child := range v {
			if findCacheControl(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if findCacheControl(child) {
				return true
			}
		}
	}
	return false
}

// scanCacheControl walks a decoded JSON value looking for cache_control
// markers. It reports whether any marker opts into the extended 1h TTL.
func scanCacheControl(val any) bool {
	switch v := val.(type) {
	case map[string]any:
		if cc, ok := v["cache_control"].(map[string]any); ok {
			if ttl, _ := cc["ttl"].(string); ttl == "1h" {
				return true
			}
		}
		for _, child := range v {
			if scanCacheControl(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if scanCacheControl(child) {
				return true
			}
		}
	}
	return false
}

// NextBumpTime computes when the next bump should fire: one lead interval
// before the cache entry expires. The lead is clamped to TTL/5 so it never
// eats a meaningful share of a short window (60s default for a 5m TTL,
// 5m for a 1h TTL when leadSeconds is large).
func NextBumpTime(lastSeen time.Time, ttl time.Duration, leadSeconds int) time.Time {
	lead := time.Duration(leadSeconds) * time.Second
	if max := ttl / 5; lead <= 0 || lead > max {
		lead = max
	}
	return lastSeen.Add(ttl - lead)
}
