package claudecode

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
)

// Usage is the detailed usage of one Anthropic Messages API response, as
// captured from the upstream body. Beyond the four token counts the shared
// OpenRouter parser reports, it keeps the identifiers a usage ledger needs to
// deduplicate against Claude Code's own logs (message ID, served model and the
// upstream request ID) and the 5m/1h cache write split that prices differently.
type Usage struct {
	MessageID string
	Model     string // served model from the response, not the requested one
	RequestID string // upstream "request-id" response header

	Input         int64
	Output        int64
	CacheRead     int64
	CacheCreate   int64
	CacheCreate5m int64
	CacheCreate1h int64
	Speed         string

	Iterations []IterationUsage
}

// IterationUsage mirrors one item of Anthropic's usage.iterations[] array,
// which breaks a single response's usage down by server-side iteration.
type IterationUsage struct {
	Type          string
	Model         string
	Input         int64
	Output        int64
	CacheRead     int64
	CacheCreate   int64
	CacheCreate5m int64
	CacheCreate1h int64
	Speed         string
}

// Pricer turns a model and its usage into an API-equivalent USD cost.
type Pricer func(model string, u Usage) float64

// DefaultPricer prices usage from the curated GetModelPricing table. Every
// cache write, whatever its TTL, is charged at the table's cache write rate,
// which is how Claude Code costs have always been computed here.
func DefaultPricer(model string, u Usage) float64 {
	return CalculateCost(model, int(u.Input), int(u.Output), int(u.CacheCreate), int(u.CacheRead))
}

var currentPricer atomic.Pointer[Pricer]

// SetPricer replaces the pricer behind UsageCost, so every Claude Code cost
// (RequestMetrics.CallCost, pool totals, and anything a usage ledger records)
// comes from one place. Passing nil restores DefaultPricer. Safe to call
// concurrently with UsageCost.
func SetPricer(p Pricer) {
	if p == nil {
		currentPricer.Store(nil)
		return
	}
	currentPricer.Store(&p)
}

// UsageCost is the single cost function for Claude Code usage. It delegates to
// the pricer installed with SetPricer, or DefaultPricer when none is set.
func UsageCost(model string, u Usage) float64 {
	if p := currentPricer.Load(); p != nil {
		return (*p)(model, u)
	}
	return DefaultPricer(model, u)
}

// wireCacheCreation is usage.cache_creation.
type wireCacheCreation struct {
	Ephemeral5m float64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h float64 `json:"ephemeral_1h_input_tokens"`
}

// wireUsage is an Anthropic usage object. Numeric fields are pointers so an
// explicit zero can be told apart from an absent field; they are float64 so a
// number written as 12.0 does not fail the whole event.
type wireUsage struct {
	InputTokens         *float64           `json:"input_tokens"`
	OutputTokens        *float64           `json:"output_tokens"`
	CacheReadTokens     *float64           `json:"cache_read_input_tokens"`
	CacheCreationTokens *float64           `json:"cache_creation_input_tokens"`
	CacheCreation       *wireCacheCreation `json:"cache_creation"`
	Speed               string             `json:"speed"`
	Iterations          []wireIteration    `json:"iterations"`
}

type wireIteration struct {
	Type                string             `json:"type"`
	Model               string             `json:"model"`
	InputTokens         float64            `json:"input_tokens"`
	OutputTokens        float64            `json:"output_tokens"`
	CacheReadTokens     float64            `json:"cache_read_input_tokens"`
	CacheCreationTokens float64            `json:"cache_creation_input_tokens"`
	CacheCreation       *wireCacheCreation `json:"cache_creation"`
	Speed               string             `json:"speed"`
}

type wireMessage struct {
	ID    string     `json:"id"`
	Model string     `json:"model"`
	Usage *wireUsage `json:"usage"`
}

// wireEvent covers both SSE events (message_start carries "message",
// message_delta carries top-level "usage") and a unary message body (id,
// model and usage at top level).
type wireEvent struct {
	Type    string       `json:"type"`
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Message *wireMessage `json:"message"`
	Usage   *wireUsage   `json:"usage"`
}

func (w *wireIteration) toIteration() IterationUsage {
	it := IterationUsage{
		Type:        w.Type,
		Model:       w.Model,
		Input:       int64(w.InputTokens),
		Output:      int64(w.OutputTokens),
		CacheRead:   int64(w.CacheReadTokens),
		CacheCreate: int64(w.CacheCreationTokens),
		Speed:       w.Speed,
	}
	if w.CacheCreation != nil {
		it.CacheCreate5m = int64(w.CacheCreation.Ephemeral5m)
		it.CacheCreate1h = int64(w.CacheCreation.Ephemeral1h)
		if sum := it.CacheCreate5m + it.CacheCreate1h; sum > it.CacheCreate {
			it.CacheCreate = sum
		}
	} else {
		it.CacheCreate5m = it.CacheCreate
	}
	return it
}

// usageAccumulator folds message_start and message_delta usage into a Usage.
type usageAccumulator struct {
	u         Usage
	splitSeen bool
}

func maxInto(dst *int64, v *float64) {
	if v != nil && int64(*v) > *dst {
		*dst = int64(*v)
	}
}

// apply merges one usage object. Anthropic reports cumulative figures, and
// message_delta can carry larger input and cache counts than message_start
// (server-side tools add input as the turn runs), so every count takes the
// per-field maximum across events and is never summed. That also covers
// translated upstreams that report zero in message_start and the real
// figures in message_delta, and an explicit zero never wipes a real value.
// This matches the shared OpenRouter parser the Claude Code path used before.
func (a *usageAccumulator) apply(w *wireUsage, fromDelta bool) {
	if w == nil {
		return
	}
	maxInto(&a.u.Input, w.InputTokens)
	maxInto(&a.u.Output, w.OutputTokens)
	maxInto(&a.u.CacheRead, w.CacheReadTokens)
	maxInto(&a.u.CacheCreate, w.CacheCreationTokens)
	if w.CacheCreation != nil {
		a.splitSeen = true
		v5, v1 := w.CacheCreation.Ephemeral5m, w.CacheCreation.Ephemeral1h
		maxInto(&a.u.CacheCreate5m, &v5)
		maxInto(&a.u.CacheCreate1h, &v1)
	}
	if a.u.Speed == "" {
		a.u.Speed = w.Speed
	}
	if len(w.Iterations) > 0 && (fromDelta || len(a.u.Iterations) == 0) {
		its := make([]IterationUsage, 0, len(w.Iterations))
		for i := range w.Iterations {
			its = append(its, w.Iterations[i].toIteration())
		}
		a.u.Iterations = its
	}
}

// applyEvent dispatches one decoded SSE payload. Events without a "type" are
// classified by shape.
func (a *usageAccumulator) applyEvent(ev *wireEvent) {
	switch {
	case ev.Type == "message_start" || (ev.Type == "" && ev.Message != nil):
		if ev.Message == nil {
			return
		}
		if a.u.MessageID == "" {
			a.u.MessageID = ev.Message.ID
		}
		if a.u.Model == "" {
			a.u.Model = ev.Message.Model
		}
		a.apply(ev.Message.Usage, false)
	case ev.Type == "message_delta" || (ev.Type == "" && ev.Usage != nil):
		a.apply(ev.Usage, true)
	}
}

// result returns the accumulated usage. A response that reports cache writes
// without the cache_creation breakdown is attributed wholly to the 5m TTL,
// the API default. When the breakdown was reported, CacheCreate is raised to
// the split's sum if that is larger, so the parts never exceed the total.
func (a *usageAccumulator) result() Usage {
	u := a.u
	if !a.splitSeen {
		u.CacheCreate5m, u.CacheCreate1h = u.CacheCreate, 0
	} else if sum := u.CacheCreate5m + u.CacheCreate1h; sum > u.CacheCreate {
		u.CacheCreate = sum
	}
	return u
}

// ParseUsageJSON extracts detailed usage from a unary Anthropic Messages API
// response body. Malformed or usage-less bodies yield a zero Usage (plus
// whatever id and model were present).
func ParseUsageJSON(body []byte) Usage {
	var ev wireEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return Usage{}
	}
	var acc usageAccumulator
	acc.u.MessageID = ev.ID
	acc.u.Model = ev.Model
	if ev.Usage != nil {
		acc.apply(ev.Usage, false)
	} else if ev.Message != nil {
		if acc.u.MessageID == "" {
			acc.u.MessageID = ev.Message.ID
		}
		if acc.u.Model == "" {
			acc.u.Model = ev.Message.Model
		}
		acc.apply(ev.Message.Usage, false)
	}
	return acc.result()
}

// UsageInterceptor wraps a streaming Anthropic response body, parsing usage
// out of the SSE events as the caller reads them. onComplete is called
// exactly once with the accumulated usage when the stream ends: on EOF, on a
// read error, or on Close, whichever comes first, so an aborted stream still
// reports the usage seen so far. The bytes read are passed through unchanged.
type UsageInterceptor struct {
	reader     io.ReadCloser
	onComplete func(Usage)
	mu         sync.Mutex
	buf        bytes.Buffer
	acc        usageAccumulator
	once       sync.Once
}

// NewUsageInterceptor wraps reader. onComplete may be nil.
func NewUsageInterceptor(reader io.ReadCloser, onComplete func(Usage)) *UsageInterceptor {
	return &UsageInterceptor{reader: reader, onComplete: onComplete}
}

// Read implements io.Reader.
func (s *UsageInterceptor) Read(p []byte) (int, error) {
	n, err := s.reader.Read(p)
	if n > 0 {
		s.mu.Lock()
		s.buf.Write(p[:n])
		s.processLines()
		s.mu.Unlock()
	}
	if err != nil {
		s.finalize()
	}
	return n, err
}

// Close closes the underlying reader and reports the usage seen so far, if it
// has not been reported yet.
func (s *UsageInterceptor) Close() error {
	s.finalize()
	return s.reader.Close()
}

// processLines consumes every complete line in the buffer; a trailing
// partial line is kept for the next read. Callers hold s.mu.
func (s *UsageInterceptor) processLines() {
	for {
		line, err := s.buf.ReadString('\n')
		if err != nil {
			s.buf.WriteString(line)
			return
		}
		s.parseLine(line)
	}
}

// parseLine handles one SSE "data:" line or bare JSON line. Callers hold s.mu.
func (s *UsageInterceptor) parseLine(line string) {
	line = strings.TrimSpace(line)
	var payload string
	switch {
	case strings.HasPrefix(line, "data:"):
		payload = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	case strings.HasPrefix(line, "{"):
		payload = line
	default:
		return
	}
	if payload == "" || payload == "[DONE]" {
		return
	}
	// Only usage-bearing events are decoded; content deltas are skipped
	// cheaply without a JSON parse.
	if !strings.Contains(payload, `"usage"`) && !strings.Contains(payload, `"message_start"`) {
		return
	}
	var ev wireEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		return
	}
	s.acc.applyEvent(&ev)
}

func (s *UsageInterceptor) finalize() {
	s.once.Do(func() {
		s.mu.Lock()
		if s.buf.Len() > 0 {
			for _, line := range strings.Split(s.buf.String(), "\n") {
				s.parseLine(line)
			}
			s.buf.Reset()
		}
		u := s.acc.result()
		s.mu.Unlock()
		if s.onComplete != nil {
			s.onComplete(u)
		}
	})
}
