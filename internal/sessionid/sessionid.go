// Package sessionid extracts stable session identifiers from proxy requests.
package sessionid

import (
	"encoding/json"
	"strings"
)

// ParseNested returns the inner session_id (or user_id fallback) when val is
// a stringified JSON object; otherwise returns val trimmed, unchanged.
func ParseNested(val string) string {
	val = strings.TrimSpace(val)
	if strings.HasPrefix(val, "{") && strings.HasSuffix(val, "}") {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(val), &parsed); err == nil {
			if s, ok := parsed["session_id"].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
			if uid, ok := parsed["user_id"].(string); ok && strings.TrimSpace(uid) != "" {
				return strings.TrimSpace(uid)
			}
		}
	}
	return val
}

// FromValue extracts a session identifier from a string (possibly
// JSON-encoded) or a pre-parsed map[string]any. Returns "" on nil/no match.
func FromValue(val any) string {
	if val == nil {
		return ""
	}
	switch v := val.(type) {
	case string:
		return ParseNested(v)
	case map[string]any:
		if s, ok := v["session_id"].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
		if uid, ok := v["user_id"].(string); ok && strings.TrimSpace(uid) != "" {
			return strings.TrimSpace(uid)
		}
	}
	return ""
}
