package cloudcode

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxSessionEntries bounds the sessions map to prevent unbounded memory growth.
const maxSessionEntries = 10000

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
	return st.RecordWithTime(sessionID, inTokens, outTokens, cacheRead, cost, time.Now())
}

// RecordWithTime records a request outcome using the provided timestamp for LastActive.
func (st *SessionTracker) RecordWithTime(sessionID string, inTokens, outTokens, cacheRead int, cost float64, now time.Time) SessionStats {
	if sessionID == "" {
		sessionID = "default"
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	stats, ok := st.sessions[sessionID]
	if !ok {
		if len(st.sessions) >= maxSessionEntries {
			st.evictOldestSessions(maxSessionEntries / 10)
		}
		stats = &SessionStats{}
		st.sessions[sessionID] = stats
	}

	stats.TotalRequests++
	stats.InputTokens += inTokens
	stats.OutputTokens += outTokens
	stats.CacheRead += cacheRead
	stats.TotalCost += cost
	stats.LastActive = now

	return *stats
}

func (st *SessionTracker) evictOldestSessions(count int) {
	if count <= 0 {
		count = 1
	}
	type entry struct {
		id         string
		lastActive time.Time
	}
	entries := make([]entry, 0, len(st.sessions))
	for id, s := range st.sessions {
		entries = append(entries, entry{id: id, lastActive: s.LastActive})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].lastActive.Before(entries[j].lastActive)
	})
	limit := count
	if limit > len(entries) {
		limit = len(entries)
	}
	for i := 0; i < limit; i++ {
		delete(st.sessions, entries[i].id)
	}
}

// ModelPricing represents per-token USD rates for a model.
type ModelPricing struct {
	Prompt     float64
	Completion float64
	CacheRead  float64
}

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

var basePricingTable = map[string]ModelPricing{
	"claude-opus-4-6-thinking": {
		Prompt:     15.0 / 1e6,
		Completion: 75.0 / 1e6,
		CacheRead:  1.50 / 1e6,
	},
	"claude-sonnet-4-6": {
		Prompt:     3.0 / 1e6,
		Completion: 15.0 / 1e6,
		CacheRead:  0.30 / 1e6,
	},
	"gemini-3.1-pro-high": {
		Prompt:     2.0 / 1e6,
		Completion: 12.0 / 1e6,
		CacheRead:  0.50 / 1e6,
	},
	"gemini-3.1-pro-low": {
		Prompt:     2.0 / 1e6,
		Completion: 12.0 / 1e6,
		CacheRead:  0.50 / 1e6,
	},
	"gpt-oss-120b-medium": {
		Prompt:     0.15 / 1e6,
		Completion: 0.60 / 1e6,
		CacheRead:  0.0375 / 1e6,
	},
	"claude-haiku-4-5": {
		Prompt:     0.80 / 1e6,
		Completion: 4.00 / 1e6,
		CacheRead:  0.08 / 1e6,
	},
}

func lookupModelPricing(model string, now time.Time) ModelPricing {
	norm := strings.TrimSpace(strings.ToLower(model))
	if p, ok := basePricingTable[norm]; ok {
		return p
	}

	switch {
	case strings.Contains(norm, "flash"):
		return lookupGeminiFlashPricing(now)
	case strings.Contains(norm, "opus"):
		return basePricingTable["claude-opus-4-6-thinking"]
	case strings.Contains(norm, "pro"):
		return basePricingTable["gemini-3.1-pro-high"]
	case strings.Contains(norm, "gpt-oss"):
		return basePricingTable["gpt-oss-120b-medium"]
	case strings.Contains(norm, "haiku"):
		return basePricingTable["claude-haiku-4-5"]
	case strings.Contains(norm, "sonnet") || strings.Contains(norm, "claude"):
		return basePricingTable["claude-sonnet-4-6"]
	default:
		return basePricingTable["claude-sonnet-4-6"]
	}
}

// CalculateRetailCost computes total retail cost in USD.
func CalculateRetailCost(model string, inputTokens, outputTokens, cacheReadTokens int, now time.Time) float64 {
	pricing := lookupModelPricing(model, now)
	return (float64(inputTokens) * pricing.Prompt) +
		(float64(outputTokens) * pricing.Completion) +
		(float64(cacheReadTokens) * pricing.CacheRead)
}

// RequestMetrics encapsulates observability data for a completed Antigravity / Cloud Code request.
type RequestMetrics struct {
	Model            string        `json:"model"`
	Account          string        `json:"account,omitempty"`
	ProjectID        string        `json:"project_id,omitempty"`
	SessionID        string        `json:"session_id,omitempty"`
	InputTokens      int           `json:"input_tokens"`
	OutputTokens     int           `json:"output_tokens"`
	CacheReadTokens  int           `json:"cache_read_tokens"`
	ThinkingTokens   int           `json:"thinking_tokens,omitempty"`
	CCRRetrievals    int           `json:"ccr_retrievals,omitempty"`
	Latency          time.Duration `json:"latency"`
	ThroughputTPS    float64       `json:"throughput_tps"`
	CacheHitRate     float64       `json:"cache_hit_rate"`
	RetailCostUSD    float64       `json:"retail_cost_usd"`
	SessionRetailUSD float64       `json:"session_retail_usd"`
}

// ComputeFinalMetrics calculates TPS, cache hit rate, retail cost, and session retail totals.
func (m *RequestMetrics) ComputeFinalMetrics(sessionTracker *SessionTracker, now time.Time) {
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

	m.RetailCostUSD = CalculateRetailCost(m.Model, m.InputTokens, m.OutputTokens, m.CacheReadTokens, now)

	if sessionTracker == nil {
		sessionTracker = DefaultSessionTracker
	}
	sStats := sessionTracker.RecordWithTime(m.SessionID, m.InputTokens, m.OutputTokens, m.CacheReadTokens, m.RetailCostUSD, now)
	m.SessionRetailUSD = sStats.TotalCost
}

// LogObservability emits structured and human-readable logs for Antigravity requests.
func LogObservability(logger *slog.Logger, m RequestMetrics) {
	if logger == nil {
		logger = slog.Default()
	}

	modelPart := m.Model
	if m.Account != "" {
		modelPart = fmt.Sprintf("%s (%s)", m.Model, m.Account)
	}

	outPart := formatInt(m.OutputTokens) + " out"
	if m.ThinkingTokens > 0 {
		outPart += fmt.Sprintf(" (%s thinking)", formatInt(m.ThinkingTokens))
	}

	ccrPart := ""
	if m.CCRRetrievals == 1 {
		ccrPart = " | CCR: 1 retrieval"
	} else if m.CCRRetrievals > 1 {
		ccrPart = fmt.Sprintf(" | CCR: %d retrievals", m.CCRRetrievals)
	}

	msg := fmt.Sprintf("[Antigravity] %s | tokens: %s in (%s cached, %.1f%% hit), %s | %.1f TPS | %.2fs | $%.4f saved ($%.4f session)%s",
		modelPart,
		formatInt(m.InputTokens),
		formatInt(m.CacheReadTokens),
		m.CacheHitRate,
		outPart,
		m.ThroughputTPS,
		m.Latency.Seconds(),
		m.RetailCostUSD,
		m.SessionRetailUSD,
		ccrPart,
	)

	attrs := []any{
		slog.String("gateway", "antigravity"),
		slog.String("model", m.Model),
		slog.String("account", m.Account),
		slog.String("project_id", m.ProjectID),
		slog.String("session_id", m.SessionID),
		slog.Int("input_tokens", m.InputTokens),
		slog.Int("output_tokens", m.OutputTokens),
		slog.Int("cache_read_tokens", m.CacheReadTokens),
		slog.Int("thinking_tokens", m.ThinkingTokens),
		slog.Int("ccr_retrievals", m.CCRRetrievals),
		slog.Float64("cache_hit_rate_pct", m.CacheHitRate),
		slog.Float64("tps", m.ThroughputTPS),
		slog.Duration("latency", m.Latency),
		slog.Float64("retail_cost_usd", m.RetailCostUSD),
		slog.Float64("session_retail_usd", m.SessionRetailUSD),
		slog.String("level_tag", "SUCCESS"),
	}

	logger.Info(msg, attrs...)
}

func formatInt(n int) string {
	in := fmt.Sprintf("%d", n)
	sign := ""
	if strings.HasPrefix(in, "-") {
		sign = "-"
		in = in[1:]
	}
	if len(in) <= 3 {
		return sign + in
	}
	var out []byte
	rem := len(in) % 3
	if rem > 0 {
		out = append(out, in[:rem]...)
		out = append(out, ',')
	}
	for i := rem; i < len(in); i += 3 {
		out = append(out, in[i:i+3]...)
		if i+3 < len(in) {
			out = append(out, ',')
		}
	}
	return sign + string(out)
}

// ExecutionMetadata carries dynamic per-request execution properties (account email and project ID).
type ExecutionMetadata struct {
	Account   string
	ProjectID string
}

type metadataKey struct{}

// WithExecutionMetadata attaches an ExecutionMetadata container to ctx.
func WithExecutionMetadata(ctx context.Context) (context.Context, *ExecutionMetadata) {
	if meta, ok := ctx.Value(metadataKey{}).(*ExecutionMetadata); ok && meta != nil {
		return ctx, meta
	}
	meta := &ExecutionMetadata{}
	return context.WithValue(ctx, metadataKey{}, meta), meta
}

// ExecutionMetadataFromContext retrieves the ExecutionMetadata pointer if present.
func ExecutionMetadataFromContext(ctx context.Context) *ExecutionMetadata {
	if meta, ok := ctx.Value(metadataKey{}).(*ExecutionMetadata); ok {
		return meta
	}
	return nil
}

// SetExecutionMetadata updates account and projectID in ctx if ExecutionMetadata is present.
func SetExecutionMetadata(ctx context.Context, account, projectID string) {
	if meta, ok := ctx.Value(metadataKey{}).(*ExecutionMetadata); ok && meta != nil {
		if account != "" {
			meta.Account = account
		}
		if projectID != "" {
			meta.ProjectID = projectID
		}
	}
}
