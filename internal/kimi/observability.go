package kimi

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// RequestMetrics encapsulates observability data for a completed Kimi gateway request.
type RequestMetrics struct {
	Model               string        `json:"model"`
	SessionID           string        `json:"session_id,omitempty"`
	InputTokens         int           `json:"input_tokens"`
	OutputTokens        int           `json:"output_tokens"`
	CacheReadTokens     int           `json:"cache_read_tokens"`
	CacheCreationTokens int           `json:"cache_creation_tokens"`
	Latency             time.Duration `json:"latency"`
	ThroughputTPS       float64       `json:"throughput_tps"`
	CacheHitRate        float64       `json:"cache_hit_rate"`
}

// ComputeFinalMetrics calculates TPS and cache hit rate.
func (m *RequestMetrics) ComputeFinalMetrics() {
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
}

// LogObservability emits structured and human-readable logs for Kimi requests.
func LogObservability(logger *slog.Logger, m RequestMetrics) {
	if logger == nil {
		logger = slog.Default()
	}

	msg := fmt.Sprintf("[Kimi] %s | tokens: %s in (%s cached, %.1f%% hit), %s out | %.1f TPS | %.2fs",
		m.Model,
		formatInt(m.InputTokens),
		formatInt(m.CacheReadTokens),
		m.CacheHitRate,
		formatInt(m.OutputTokens),
		m.ThroughputTPS,
		m.Latency.Seconds(),
	)

	attrs := []any{
		slog.String("gateway", "kimi"),
		slog.String("model", m.Model),
		slog.String("session_id", m.SessionID),
		slog.Int("input_tokens", m.InputTokens),
		slog.Int("output_tokens", m.OutputTokens),
		slog.Int("cache_read_tokens", m.CacheReadTokens),
		slog.Int("cache_creation_tokens", m.CacheCreationTokens),
		slog.Float64("cache_hit_rate_pct", m.CacheHitRate),
		slog.Float64("tps", m.ThroughputTPS),
		slog.Duration("latency", m.Latency),
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
