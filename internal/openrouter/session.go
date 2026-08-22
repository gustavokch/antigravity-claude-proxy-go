package openrouter

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SessionStats aggregates cost and token metrics for a single conversation/session.
type SessionStats struct {
	SessionID       string    `json:"session_id"`
	RequestCount    int       `json:"request_count"`
	InputTokens     int       `json:"input_tokens"`
	OutputTokens    int       `json:"output_tokens"`
	CacheReadTokens int       `json:"cache_read_tokens"`
	TotalCost       float64   `json:"total_cost"`
	FirstSeen       time.Time `json:"first_seen"`
	LastSeen        time.Time `json:"last_seen"`
}

// SessionTracker tracks cumulative usage metrics by session ID.
type SessionTracker struct {
	mu         sync.RWMutex
	sessions   map[string]*SessionStats
	ttl        time.Duration
	maxEntries int
}

// DefaultSessionTracker is a shared package-level session tracker instance.
var DefaultSessionTracker = NewSessionTracker(24*time.Hour, 10000)

// NewSessionTracker initializes a new SessionTracker with the specified TTL and capacity.
func NewSessionTracker(ttl time.Duration, maxEntries int) *SessionTracker {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if maxEntries <= 0 {
		maxEntries = 10000
	}
	return &SessionTracker{
		sessions:   make(map[string]*SessionStats),
		ttl:        ttl,
		maxEntries: maxEntries,
	}
}

// Record updates cumulative session statistics with the metrics of a completed API call.
func (st *SessionTracker) Record(sessionID string, inputTokens, outputTokens, cacheReadTokens int, cost float64) SessionStats {
	if sessionID == "" {
		sessionID = "default"
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	now := time.Now()

	// If over capacity, prune expired sessions
	if len(st.sessions) >= st.maxEntries {
		cutoff := now.Add(-st.ttl)
		for id, s := range st.sessions {
			if s.LastSeen.Before(cutoff) {
				delete(st.sessions, id)
			}
		}
		// If still at or over capacity after pruning, evict oldest session
		if len(st.sessions) >= st.maxEntries {
			var oldestID string
			var oldestTime time.Time
			for id, s := range st.sessions {
				if oldestID == "" || s.LastSeen.Before(oldestTime) {
					oldestID = id
					oldestTime = s.LastSeen
				}
			}
			if oldestID != "" {
				delete(st.sessions, oldestID)
			}
		}
	}

	stats, exists := st.sessions[sessionID]
	if !exists {
		stats = &SessionStats{
			SessionID: sessionID,
			FirstSeen: now,
		}
		st.sessions[sessionID] = stats
	}

	stats.RequestCount++
	stats.InputTokens += inputTokens
	stats.OutputTokens += outputTokens
	stats.CacheReadTokens += cacheReadTokens
	stats.TotalCost += cost
	stats.LastSeen = now

	return *stats
}

// Get returns the session stats for a session ID if present.
func (st *SessionTracker) Get(sessionID string) (SessionStats, bool) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	stats, exists := st.sessions[sessionID]
	if !exists {
		return SessionStats{}, false
	}
	return *stats, true
}

// Prune removes sessions that haven't been active within the tracker's TTL.
func (st *SessionTracker) Prune() {
	st.mu.Lock()
	defer st.mu.Unlock()
	cutoff := time.Now().Add(-st.ttl)
	for id, s := range st.sessions {
		if s.LastSeen.Before(cutoff) {
			delete(st.sessions, id)
		}
	}
}

// Reset clears all tracked sessions.
func (st *SessionTracker) Reset() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sessions = make(map[string]*SessionStats)
}

// ExtractSessionID extracts the session or conversation identifier from request headers or body.
func ExtractSessionID(req *http.Request, reqBody map[string]any) string {
	if req != nil {
		for _, header := range []string{"x-session-id", "session-id", "anthropic-session-id", "x-conversation-id"} {
			if val := strings.TrimSpace(req.Header.Get(header)); val != "" {
				return val
			}
		}
	}

	if reqBody != nil {
		if meta, ok := reqBody["metadata"].(map[string]any); ok {
			if s, ok := meta["session_id"].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
			if u, ok := meta["user_id"].(string); ok && strings.TrimSpace(u) != "" {
				return strings.TrimSpace(u)
			}
		}
		if s, ok := reqBody["session_id"].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}

	// Fallback to client address hash if available
	if req != nil && req.RemoteAddr != "" {
		host := req.RemoteAddr
		if h, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
			host = h
		}
		h := sha256.Sum256([]byte(host + ":" + req.UserAgent()))
		return "session_" + hex.EncodeToString(h[:8])
	}

	return "default"
}
