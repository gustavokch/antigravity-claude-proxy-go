// Package cachebump keeps Anthropic prompt-cache entries warm for idle
// sessions. It records the last /v1/messages body of a session and replays a
// minimal version of it shortly before the cache entry expires, so the next
// real client turn pays a cache read instead of a full cache write.
//
// The package is route-agnostic: callers supply a Sender that knows how to
// reach the upstream for a given record.
package cachebump

import (
	"net/http"
	"time"
)

// Route identifies the proxy route a recorded session arrived on.
type Route string

const (
	// RouteClaudeCode is the Claude Code account-pool route.
	RouteClaudeCode Route = "claudecode"
	// RouteKimi is the Kimi passthrough route.
	RouteKimi Route = "kimi"
	// RouteCustom is the custom-endpoint route.
	RouteCustom Route = "custom"
)

// Record is one cached session: the replay-ready body plus scheduling state.
type Record struct {
	Key        string        `json:"key"`
	SessionID  string        `json:"session_id"`
	Route      Route         `json:"route"`
	Model      string        `json:"model,omitempty"`
	AccountID  string        `json:"account_id,omitempty"`
	EndpointID string        `json:"endpoint_id,omitempty"`
	Body       []byte        `json:"-"`
	Headers    http.Header   `json:"-"`
	TTL        time.Duration `json:"ttl"`
	NextBump   time.Time     `json:"next_bump"`
	LastSeen   time.Time     `json:"last_seen"`
	Bumps      int           `json:"bumps"`
	Retries    int           `json:"retries"`
	Stopped    bool          `json:"stopped"`
	StopReason string        `json:"stop_reason,omitempty"`

	LastCacheReadTokens     int `json:"last_cache_read_tokens"`
	LastCacheCreationTokens int `json:"last_cache_creation_tokens"`
}

// RecordKey builds the store key for a session on a route.
func RecordKey(route Route, sessionID string) string {
	return string(route) + "|" + sessionID
}

// allowedBumpHeaders is the only header set replayed upstream. The cache key
// covers the request body; these headers select the endpoint behavior.
var allowedBumpHeaders = []string{"anthropic-version", "anthropic-beta"}

// AllowlistHeaders copies only the headers a bump replay may carry upstream.
// It never mutates the input.
func AllowlistHeaders(hdr http.Header) http.Header {
	out := http.Header{}
	if hdr == nil {
		return out
	}
	for _, name := range allowedBumpHeaders {
		for _, val := range hdr.Values(name) {
			out.Add(name, val)
		}
	}
	return out
}
