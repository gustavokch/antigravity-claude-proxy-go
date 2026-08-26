package openrouter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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

	// Cost per API call
	m.CallCost = CalculateCost(pricing, m.InputTokens+m.CacheReadTokens, m.OutputTokens, m.CacheReadTokens, m.CacheCreationTokens)

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

	msg := fmt.Sprintf("[OpenRouter] %s | tokens: %s in (%s cached, %.1f%% hit), %s out | %.1f TPS | %s | $%.4f ($%.4f session)",
		m.Model,
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

	log.Info(msg,
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
	)
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

// ParseUsageFromJSON extracts token counts from Anthropic or OpenAI JSON usage responses.
func ParseUsageFromJSON(body []byte) (inputTokens, outputTokens, cacheRead, cacheWrite int) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return
	}

	usage, ok := raw["usage"].(map[string]any)
	if !ok {
		return
	}

	isOpenAIFormat := false
	// Anthropic format
	if v, ok := usage["input_tokens"].(float64); ok {
		inputTokens = int(v)
	} else if v, ok := usage["prompt_tokens"].(float64); ok {
		inputTokens = int(v)
		isOpenAIFormat = true
	}

	if v, ok := usage["output_tokens"].(float64); ok {
		outputTokens = int(v)
	} else if v, ok := usage["completion_tokens"].(float64); ok {
		outputTokens = int(v)
	}

	if v, ok := usage["cache_read_input_tokens"].(float64); ok {
		cacheRead = int(v)
	}
	if v, ok := usage["cache_creation_input_tokens"].(float64); ok {
		cacheWrite = int(v)
	}

	// OpenAI format details
	if promptDetails, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if v, ok := promptDetails["cached_tokens"].(float64); ok && cacheRead == 0 {
			cacheRead = int(v)
		}
	}

	// OpenAI prompt_tokens includes cached tokens; normalize inputTokens to uncached input tokens
	if isOpenAIFormat && cacheRead > 0 {
		if inputTokens >= cacheRead {
			inputTokens -= cacheRead
		} else {
			inputTokens = 0
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
			if v, ok := usage["input_tokens"].(float64); ok && int(v) > *inputTokens {
				*inputTokens = int(v)
			}
			if v, ok := usage["cache_read_input_tokens"].(float64); ok && int(v) > *cacheRead {
				*cacheRead = int(v)
			}
			if v, ok := usage["cache_creation_input_tokens"].(float64); ok && int(v) > *cacheWrite {
				*cacheWrite = int(v)
			}
		}
	}

	// 2. Anthropic message_delta / OpenAI usage event: usage
	if usage, ok := event["usage"].(map[string]any); ok {
		if v, ok := usage["output_tokens"].(float64); ok && int(v) > *outputTokens {
			*outputTokens = int(v)
		}
		if v, ok := usage["completion_tokens"].(float64); ok && int(v) > *outputTokens {
			*outputTokens = int(v)
		}
		if v, ok := usage["input_tokens"].(float64); ok && int(v) > *inputTokens {
			*inputTokens = int(v)
		}
		if v, ok := usage["cache_read_input_tokens"].(float64); ok && int(v) > *cacheRead {
			*cacheRead = int(v)
		}
		if v, ok := usage["cache_creation_input_tokens"].(float64); ok && int(v) > *cacheWrite {
			*cacheWrite = int(v)
		}
		if promptDetails, ok := usage["prompt_tokens_details"].(map[string]any); ok {
			if v, ok := promptDetails["cached_tokens"].(float64); ok && int(v) > *cacheRead {
				*cacheRead = int(v)
			}
		}
		// OpenAI prompt_tokens
		if v, ok := usage["prompt_tokens"].(float64); ok {
			totalPrompt := int(v)
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
}

// ExtractProviderFromSSELine returns the top-level "provider" field of an SSE
// data payload (OpenRouter reports the served provider there), or "".
func ExtractProviderFromSSELine(line string) string {
	line = strings.TrimSpace(line)
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
	if s, ok := event["provider"].(string); ok {
		return s
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
		// Process any remaining bytes
		if s.buf.Len() > 0 {
			ParseUsageFromSSELine(s.buf.String(), &s.inTokens, &s.outTokens, &s.cacheRead, &s.cacheWrite)
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
