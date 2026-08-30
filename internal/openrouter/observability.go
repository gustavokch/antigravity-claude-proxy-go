package openrouter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RequestMetrics contains observability metrics for a completed OpenRouter request.
type RequestMetrics struct {
	Model               string        `json:"model"`
	SessionID           string        `json:"session_id"`
	Provider            string        `json:"provider,omitempty"`
	InputTokens         int           `json:"input_tokens"`
	OutputTokens        int           `json:"output_tokens"`
	CacheReadTokens     int           `json:"cache_read_tokens"`
	CacheCreationTokens int           `json:"cache_creation_tokens"`
	Latency             time.Duration `json:"latency"`
	ThroughputTPS       float64       `json:"throughput_tps"`
	CacheHitRate        float64       `json:"cache_hit_rate"`
	CallCost            float64       `json:"call_cost"`
	SessionCost         float64       `json:"session_cost"`

	// Response Caching
	CacheStatus   string `json:"cache_status,omitempty"`      // "HIT", "MISS", or ""
	CacheAge      int    `json:"cache_age_seconds,omitempty"` // Age in seconds on HIT
	CacheTTL      int    `json:"cache_ttl_seconds,omitempty"`
	CacheSourceID string `json:"cache_source_id,omitempty"`
}

// ComputeFinalMetrics calculates TPS, cache hit rate, call cost, and session cost.
func (m *RequestMetrics) ComputeFinalMetrics(pricing Pricing, sessionTracker *SessionTracker) {
	// Total prompt tokens
	totalPrompt := m.InputTokens + m.CacheReadTokens
	if totalPrompt > 0 && m.CacheReadTokens > 0 {
		m.CacheHitRate = (float64(m.CacheReadTokens) / float64(totalPrompt)) * 100.0
	} else {
		m.CacheHitRate = 0.0
	}

	// Generation Throughput (Tokens Per Second) based on output tokens
	sec := m.Latency.Seconds()
	if sec > 0 && m.OutputTokens > 0 {
		m.ThroughputTPS = float64(m.OutputTokens) / sec
	} else {
		m.ThroughputTPS = 0.0
	}

	// Cost per API call. A response cache HIT is served free, so it bills
	// nothing — but any usage the upstream still reports is left intact and
	// recorded below, because it describes context the request consumed.
	if m.CacheStatus == "HIT" {
		m.CallCost = 0.0
	} else {
		m.CallCost = CalculateCost(pricing, m.InputTokens+m.CacheReadTokens, m.OutputTokens, m.CacheReadTokens, m.CacheCreationTokens)
	}

	// Cost per session
	if sessionTracker == nil {
		sessionTracker = DefaultSessionTracker
	}
	sStats := sessionTracker.Record(m.SessionID, m.InputTokens, m.OutputTokens, m.CacheReadTokens, m.CallCost)
	m.SessionCost = sStats.TotalCost
}

// LogObservability emits structured and human-readable logs for OpenRouter requests.
func LogObservability(log *slog.Logger, m RequestMetrics) {
	if log == nil {
		log = slog.Default()
	}

	cacheTag := ""
	if m.CacheStatus == "HIT" {
		cacheTag = fmt.Sprintf(" | response cache: HIT (age: %ds)", m.CacheAge)
	} else if m.CacheStatus == "MISS" {
		cacheTag = " | response cache: MISS"
	}

	msg := fmt.Sprintf("[OpenRouter] %s%s | tokens: %s in (%s cached, %.1f%% hit), %s out | %.1f TPS | %s | $%.4f ($%.4f session)",
		m.Model,
		cacheTag,
		formatInt(m.InputTokens),
		formatInt(m.CacheReadTokens),
		m.CacheHitRate,
		formatInt(m.OutputTokens),
		m.ThroughputTPS,
		m.Latency.Round(10*time.Millisecond),
		m.CallCost,
		m.SessionCost,
	)
	if m.Provider != "" {
		msg += fmt.Sprintf(" | provider: %s", m.Provider)
	}

	attrs := []any{
		slog.String("gateway", "openrouter"),
		slog.String("model", m.Model),
		slog.String("session_id", m.SessionID),
		slog.String("provider", m.Provider),
		slog.Int("input_tokens", m.InputTokens),
		slog.Int("output_tokens", m.OutputTokens),
		slog.Int("cache_read_tokens", m.CacheReadTokens),
		slog.Int("cache_creation_tokens", m.CacheCreationTokens),
		slog.Float64("cache_hit_rate_pct", m.CacheHitRate),
		slog.Float64("tps", m.ThroughputTPS),
		slog.Duration("latency", m.Latency),
		slog.Float64("call_cost_usd", m.CallCost),
		slog.Float64("session_cost_usd", m.SessionCost),
		slog.String("level_tag", "SUCCESS"),
	}
	if m.CacheStatus != "" {
		attrs = append(attrs,
			slog.String("response_cache_status", m.CacheStatus),
			slog.Int("response_cache_age", m.CacheAge),
			slog.Int("response_cache_ttl", m.CacheTTL),
			slog.String("response_cache_source_id", m.CacheSourceID),
		)
	}

	log.Info(msg, attrs...)
}

func formatInt(n int) string {
	if n < 0 {
		return "-" + formatInt(-n)
	}
	in := fmt.Sprintf("%d", n)
	if len(in) <= 3 {
		return in
	}
	var out []byte
	rem := len(in) % 3
	if rem > 0 {
		out = append(out, in[:rem]...)
		if len(in) > rem {
			out = append(out, ',')
		}
	}
	for i := rem; i < len(in); i += 3 {
		out = append(out, in[i:i+3]...)
		if i+3 < len(in) {
			out = append(out, ',')
		}
	}
	return string(out)
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
		if f, err := n.Float64(); err == nil {
			return int(f), true
		}
	}
	return 0, false
}

func parseUsageMap(usage map[string]any, inputTokens, outputTokens, cacheRead, cacheWrite *int) {
	if usage == nil {
		return
	}

	var promptTokens int
	var hasPromptTokens bool

	if v, ok := toInt(usage["input_tokens"]); ok && v > *inputTokens {
		*inputTokens = v
	} else if v, ok := toInt(usage["prompt_tokens"]); ok {
		promptTokens = v
		hasPromptTokens = true
	}

	if v, ok := toInt(usage["output_tokens"]); ok && v > *outputTokens {
		*outputTokens = v
	} else if v, ok := toInt(usage["completion_tokens"]); ok && v > *outputTokens {
		*outputTokens = v
	}

	if v, ok := toInt(usage["cache_read_input_tokens"]); ok && v > *cacheRead {
		*cacheRead = v
	} else if v, ok := toInt(usage["cache_read_tokens"]); ok && v > *cacheRead {
		*cacheRead = v
	} else if v, ok := toInt(usage["cached_tokens"]); ok && v > *cacheRead {
		*cacheRead = v
	}

	if v, ok := toInt(usage["cache_creation_input_tokens"]); ok && v > *cacheWrite {
		*cacheWrite = v
	} else if v, ok := toInt(usage["cache_write_tokens"]); ok && v > *cacheWrite {
		*cacheWrite = v
	}

	if promptDetails, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if v, ok := toInt(promptDetails["cached_tokens"]); ok && v > *cacheRead {
			*cacheRead = v
		}
	}

	if hasPromptTokens {
		totalPrompt := promptTokens
		uncached := totalPrompt
		if *cacheRead > 0 {
			if uncached >= *cacheRead {
				uncached -= *cacheRead
			} else {
				uncached = 0
			}
		}
		if uncached > *inputTokens {
			*inputTokens = uncached
		}
	}
}

// ParseUsageFromJSON extracts token counts from Anthropic or OpenAI JSON usage responses.
func ParseUsageFromJSON(body []byte) (inputTokens, outputTokens, cacheRead, cacheWrite int) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return
	}

	if usage, ok := raw["usage"].(map[string]any); ok {
		parseUsageMap(usage, &inputTokens, &outputTokens, &cacheRead, &cacheWrite)
		return
	}
	if msg, ok := raw["message"].(map[string]any); ok {
		if usage, ok := msg["usage"].(map[string]any); ok {
			parseUsageMap(usage, &inputTokens, &outputTokens, &cacheRead, &cacheWrite)
			return
		}
	}
	if resp, ok := raw["response"].(map[string]any); ok {
		if usage, ok := resp["usage"].(map[string]any); ok {
			parseUsageMap(usage, &inputTokens, &outputTokens, &cacheRead, &cacheWrite)
			return
		}
	}
	if choices, ok := raw["choices"].([]any); ok && len(choices) > 0 {
		if ch, ok := choices[0].(map[string]any); ok {
			if usage, ok := ch["usage"].(map[string]any); ok {
				parseUsageMap(usage, &inputTokens, &outputTokens, &cacheRead, &cacheWrite)
				return
			}
		}
	}
	return
}

// ParseUsageFromSSELine parses a single SSE line (e.g. data: {...}) and updates token counts.
func ParseUsageFromSSELine(line string, inputTokens, outputTokens, cacheRead, cacheWrite *int) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}

	var event map[string]any
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return
	}

	// 1. Anthropic message_start event: message.usage
	if msg, ok := event["message"].(map[string]any); ok {
		if usage, ok := msg["usage"].(map[string]any); ok {
			parseUsageMap(usage, inputTokens, outputTokens, cacheRead, cacheWrite)
		}
	}

	// 2. Anthropic message_delta / OpenAI usage event: usage
	if usage, ok := event["usage"].(map[string]any); ok {
		parseUsageMap(usage, inputTokens, outputTokens, cacheRead, cacheWrite)
	}

	// 3. Nested delta.usage
	if delta, ok := event["delta"].(map[string]any); ok {
		if usage, ok := delta["usage"].(map[string]any); ok {
			parseUsageMap(usage, inputTokens, outputTokens, cacheRead, cacheWrite)
		}
	}

	// 4. Nested response.usage
	if resp, ok := event["response"].(map[string]any); ok {
		if usage, ok := resp["usage"].(map[string]any); ok {
			parseUsageMap(usage, inputTokens, outputTokens, cacheRead, cacheWrite)
		}
	}

	// 5. OpenAI choices[0].usage or choices[0].delta.usage
	if choices, ok := event["choices"].([]any); ok && len(choices) > 0 {
		if ch, ok := choices[0].(map[string]any); ok {
			if usage, ok := ch["usage"].(map[string]any); ok {
				parseUsageMap(usage, inputTokens, outputTokens, cacheRead, cacheWrite)
			}
			if delta, ok := ch["delta"].(map[string]any); ok {
				if usage, ok := delta["usage"].(map[string]any); ok {
					parseUsageMap(usage, inputTokens, outputTokens, cacheRead, cacheWrite)
				}
			}
		}
	}
}

// ExtractProviderFromHeader extracts the upstream provider from OpenRouter response headers.
func ExtractProviderFromHeader(h http.Header) string {
	if h == nil {
		return ""
	}
	for _, key := range []string{"OpenRouter-Provider", "X-OpenRouter-Provider", "X-Provider"} {
		if v := strings.TrimSpace(h.Get(key)); v != "" {
			return v
		}
	}
	return ""
}

// ExtractProviderFromSSELine returns the provider from an SSE line
// (comment lines like ": provider: deepinfra" or data payloads with "provider"), or "".
func ExtractProviderFromSSELine(line string) string {
	line = strings.TrimSpace(line)
	// Check SSE comment lines
	if strings.HasPrefix(line, ":") {
		trimmed := strings.TrimSpace(strings.TrimPrefix(line, ":"))
		for _, prefix := range []string{
			"provider:", "PROVIDER:",
			"OPENROUTER PROCESSING:", "openrouter processing:",
			"OPENROUTER PROVIDER:", "openrouter provider:",
			"openrouter:", "OPENROUTER:",
		} {
			if strings.HasPrefix(trimmed, prefix) {
				p := strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
				if p != "" {
					return p
				}
			}
		}
		return ""
	}

	if !strings.HasPrefix(line, "data:") {
		return ""
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return ""
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return ""
	}
	if s, ok := event["provider"].(string); ok && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	if msg, ok := event["message"].(map[string]any); ok {
		if s, ok := msg["provider"].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	if resp, ok := event["response"].(map[string]any); ok {
		if s, ok := resp["provider"].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// SSEInterceptor wraps an io.ReadCloser to parse SSE usage events while streaming.
type SSEInterceptor struct {
	reader      io.ReadCloser
	onComplete  func(inputTokens, outputTokens, cacheRead, cacheWrite int)
	mu          sync.Mutex
	buf         bytes.Buffer
	inTokens    int
	outTokens   int
	cacheRead   int
	cacheWrite  int
	provider    string
	terminalErr error
	closed      bool
	once        sync.Once
}

// NewSSEInterceptor creates an SSE stream interceptor.
func NewSSEInterceptor(reader io.ReadCloser, onComplete func(inputTokens, outputTokens, cacheRead, cacheWrite int)) *SSEInterceptor {
	return &SSEInterceptor{
		reader:     reader,
		onComplete: onComplete,
	}
}

func (s *SSEInterceptor) Read(p []byte) (n int, err error) {
	n, err = s.reader.Read(p)
	if n > 0 {
		s.mu.Lock()
		s.buf.Write(p[:n])
		s.processLines()
		s.mu.Unlock()
	}
	if err != nil {
		s.mu.Lock()
		if err != io.EOF {
			s.terminalErr = err
		}
		s.mu.Unlock()
		s.finalize()
	}
	return n, err
}

func (s *SSEInterceptor) processLines() {
	for {
		line, err := s.buf.ReadString('\n')
		if err != nil {
			// Incomplete line, put back into buffer
			s.buf.WriteString(line)
			break
		}
		ParseUsageFromSSELine(line, &s.inTokens, &s.outTokens, &s.cacheRead, &s.cacheWrite)
		if p := ExtractProviderFromSSELine(line); p != "" {
			s.provider = p
		}
	}
}

// Provider returns the served provider captured from SSE events, if any.
func (s *SSEInterceptor) Provider() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.provider
}

// TerminalErr returns the non-EOF terminal error if the stream ended prematurely.
func (s *SSEInterceptor) TerminalErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminalErr == io.EOF {
		return nil
	}
	return s.terminalErr
}

func (s *SSEInterceptor) finalize() {
	s.once.Do(func() {
		s.mu.Lock()
		// Process any remaining bytes by splitting on newlines
		if s.buf.Len() > 0 {
			remaining := s.buf.String()
			for _, line := range strings.Split(remaining, "\n") {
				line = strings.TrimRight(line, "\r")
				ParseUsageFromSSELine(line, &s.inTokens, &s.outTokens, &s.cacheRead, &s.cacheWrite)
				if p := ExtractProviderFromSSELine(line); p != "" && s.provider == "" {
					s.provider = p
				}
			}
			s.buf.Reset()
		}
		in := s.inTokens
		out := s.outTokens
		cr := s.cacheRead
		cw := s.cacheWrite
		s.mu.Unlock()

		if s.onComplete != nil {
			s.onComplete(in, out, cr, cw)
		}
	})
}

// Close closes the underlying reader and invokes the completion callback.
func (s *SSEInterceptor) Close() error {
	s.finalize()
	return s.reader.Close()
}
