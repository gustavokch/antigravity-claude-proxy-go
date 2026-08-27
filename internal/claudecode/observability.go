package claudecode

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// SessionStats aggregates tokens and cost for a given session.
type SessionStats struct {
	TotalRequests int
	InputTokens   int
	OutputTokens  int
	CacheRead     int
	TotalCost     float64
	LastActive    time.Time
}

// SessionTracker tracks cumulative session statistics.
type SessionTracker struct {
	mu       sync.RWMutex
	sessions map[string]*SessionStats
}

// DefaultSessionTracker is the global default session tracking instance.
var DefaultSessionTracker = NewSessionTracker()

// NewSessionTracker initializes a SessionTracker.
func NewSessionTracker() *SessionTracker {
	return &SessionTracker{
		sessions: make(map[string]*SessionStats),
	}
}

// Record records a request outcome into the session stats and returns the updated state.
func (st *SessionTracker) Record(sessionID string, inTokens, outTokens, cacheRead int, cost float64) SessionStats {
	if sessionID == "" {
		sessionID = "default"
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	stats, ok := st.sessions[sessionID]
	if !ok {
		stats = &SessionStats{}
		st.sessions[sessionID] = stats
	}

	stats.TotalRequests++
	stats.InputTokens += inTokens
	stats.OutputTokens += outTokens
	stats.CacheRead += cacheRead
	stats.TotalCost += cost
	stats.LastActive = time.Now()

	return *stats
}

// RequestMetrics encapsulates observability data for a completed Claude Code gateway request.
type RequestMetrics struct {
	Model               string        `json:"model"`
	AccountID           string        `json:"account_id,omitempty"`
	AccountName         string        `json:"account_name,omitempty"`
	SessionID           string        `json:"session_id,omitempty"`
	InputTokens         int           `json:"input_tokens"`
	OutputTokens        int           `json:"output_tokens"`
	CacheReadTokens     int           `json:"cache_read_tokens"`
	CacheCreationTokens int           `json:"cache_creation_tokens"`
	Latency             time.Duration `json:"latency"`
	ThroughputTPS       float64       `json:"throughput_tps"`
	CacheHitRate        float64       `json:"cache_hit_rate"`
	CallCost            float64       `json:"call_cost"`
	SessionCost         float64       `json:"session_cost"`
}

// ComputeFinalMetrics calculates TPS, cache hit rate, and costs.
func (m *RequestMetrics) ComputeFinalMetrics(sessionTracker *SessionTracker) {
	totalPrompt := m.InputTokens + m.CacheReadTokens
	if totalPrompt > 0 && m.CacheReadTokens > 0 {
		m.CacheHitRate = (float64(m.CacheReadTokens) / float64(totalPrompt)) * 100.0
	} else {
		m.CacheHitRate = 0.0
	}

	sec := m.Latency.Seconds()
	if sec > 0 && m.OutputTokens > 0 {
		m.ThroughputTPS = float64(m.OutputTokens) / sec
	} else {
		m.ThroughputTPS = 0.0
	}

	m.CallCost = CalculateCost(m.Model, m.InputTokens, m.OutputTokens, m.CacheCreationTokens, m.CacheReadTokens)

	if sessionTracker == nil {
		sessionTracker = DefaultSessionTracker
	}
	sStats := sessionTracker.Record(m.SessionID, m.InputTokens, m.OutputTokens, m.CacheReadTokens, m.CallCost)
	m.SessionCost = sStats.TotalCost
}

// LogObservability emits structured and formatted logging for Claude Code requests.
func LogObservability(log *slog.Logger, m RequestMetrics) {
	if log == nil {
		log = slog.Default()
	}

	accLabel := m.AccountName
	if accLabel == "" {
		accLabel = m.AccountID
	}
	if accLabel == "" {
		accLabel = "default"
	}

	msg := fmt.Sprintf("[ClaudeCode] %s (%s) | tokens: %d in (%d cached, %.1f%% hit), %d out | %.1f TPS | %s | $%.4f ($%.4f session)",
		m.Model,
		accLabel,
		m.InputTokens,
		m.CacheReadTokens,
		m.CacheHitRate,
		m.OutputTokens,
		m.ThroughputTPS,
		m.Latency.Round(10*time.Millisecond),
		m.CallCost,
		m.SessionCost,
	)

	log.Info(msg,
		"model", m.Model,
		"account_id", m.AccountID,
		"session_id", m.SessionID,
		"input_tokens", m.InputTokens,
		"output_tokens", m.OutputTokens,
		"cache_read_tokens", m.CacheReadTokens,
		"cache_creation_tokens", m.CacheCreationTokens,
		"latency_ms", m.Latency.Milliseconds(),
		"call_cost", m.CallCost,
		"session_cost", m.SessionCost,
	)
}
