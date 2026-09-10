package cachebump

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// extendedCacheTTLBeta is the anthropic-beta value that enables the 1h
// prompt-cache TTL. A body-level {"ttl":"1h"} marker is inert without it.
const extendedCacheTTLBeta = "extended-cache-ttl-2025-04-11"

// InspectCacheControl reports, in a single pass over the request body,
// whether it carries any cache_control marker and whether any marker opts
// into the extended 1h TTL.
//
// It walks the JSON token stream instead of decoding into a map[string]any
// tree: a recorded body is a whole conversation, and this runs on the
// response path of every cached turn.
func InspectCacheControl(body []byte) (found bool, extended bool) {
	dec := json.NewDecoder(bytes.NewReader(body))

	// One entry per open container. isObject distinguishes objects from
	// arrays; wantKey tracks whether the next token in an object is a
	// member name rather than a value.
	var isObject, wantKey []bool

	// valueRead marks that a value just ended, so the enclosing object
	// expects a member name next.
	valueRead := func() {
		if n := len(isObject); n > 0 && isObject[n-1] {
			wantKey[n-1] = true
		}
	}

	for {
		tok, err := dec.Token()
		if err != nil {
			return found, extended
		}

		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{':
				isObject = append(isObject, true)
				wantKey = append(wantKey, true)
			case '[':
				isObject = append(isObject, false)
				wantKey = append(wantKey, false)
			case '}', ']':
				if n := len(isObject); n > 0 {
					isObject, wantKey = isObject[:n-1], wantKey[:n-1]
				}
				valueRead()
			}
			continue
		}

		n := len(isObject)
		if n == 0 || !isObject[n-1] || !wantKey[n-1] {
			// A scalar root, an array element, or an object member's value.
			valueRead()
			continue
		}

		// tok is a member name.
		wantKey[n-1] = false
		if name, _ := tok.(string); name != "cache_control" {
			continue
		}
		found = true

		var marker struct {
			TTL string `json:"ttl"`
		}
		if err := dec.Decode(&marker); err != nil {
			// Malformed body: match the old Unmarshal-based behavior and
			// report no marker, since nothing after this point is reliable.
			return false, false
		}
		wantKey[n-1] = true
		if marker.TTL == "1h" {
			return true, true
		}
	}
}

// DetectTTL reports the prompt-cache entry TTL a request holds. A body that
// opts into {"ttl":"1h"} gets one hour only when the request also carries
// the extended-cache-ttl beta; anything else (including no marker at all)
// means the default five minutes.
func DetectTTL(body []byte, beta string) time.Duration {
	_, extended := InspectCacheControl(body)
	if extended && strings.Contains(beta, extendedCacheTTLBeta) {
		return time.Hour
	}
	return 5 * time.Minute
}

// HasCacheControl reports whether the request body carries any cache_control
// marker. A body without markers can never hold a cache entry, so bumping it
// would only ever pay a cache write.
func HasCacheControl(body []byte) bool {
	found, _ := InspectCacheControl(body)
	return found
}

// NextBumpTime computes when the next bump should fire: one lead interval
// before the cache entry expires. A non-positive or oversized lead is
// clamped to TTL/5, so it never eats a meaningful share of the window
// (60s for a 5m TTL at the default lead, at most 12m for a 1h TTL).
func NextBumpTime(lastSeen time.Time, ttl time.Duration, leadSeconds int) time.Time {
	lead := time.Duration(leadSeconds) * time.Second
	if maxLead := ttl / 5; lead <= 0 || lead > maxLead {
		lead = maxLead
	}
	return lastSeen.Add(ttl - lead)
}
