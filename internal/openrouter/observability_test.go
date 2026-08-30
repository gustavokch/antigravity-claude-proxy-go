package openrouter

import (
	"bytes"
	"io"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"
)

func TestRequestMetrics_ComputeFinalMetrics(t *testing.T) {
	st := NewSessionTracker(1*time.Hour, 100)
	pricing := Pricing{
		Prompt:          0.000003,  // $3/M
		Completion:      0.000015,  // $15/M
		InputCacheRead:  0.0000003, // $0.30/M
		InputCacheWrite: 0.00000375,
	}

	metrics := RequestMetrics{
		Model:               "anthropic/claude-3.7-sonnet",
		SessionID:           "test-session-1",
		InputTokens:         600, // uncached
		OutputTokens:        300,
		CacheReadTokens:     400,
		CacheCreationTokens: 0,
		Latency:             3 * time.Second,
	}

	metrics.ComputeFinalMetrics(pricing, st)

	// Cache hit rate = 400 / (600 + 400) * 100 = 40.0%
	if math.Abs(metrics.CacheHitRate-40.0) > 1e-6 {
		t.Errorf("expected cache hit rate 40.0%%, got %f", metrics.CacheHitRate)
	}

	// TPS = 300 output tokens / 3.0 sec = 100.0 TPS
	if math.Abs(metrics.ThroughputTPS-100.0) > 1e-6 {
		t.Errorf("expected TPS 100.0, got %f", metrics.ThroughputTPS)
	}

	// Cost = 600 * 0.000003 + 400 * 0.0000003 + 300 * 0.000015
	//      = 0.0018 + 0.00012 + 0.0045 = 0.00642
	expectedCost := 0.00642
	if math.Abs(metrics.CallCost-expectedCost) > 1e-6 {
		t.Errorf("expected call cost %f, got %f", expectedCost, metrics.CallCost)
	}
	if math.Abs(metrics.SessionCost-expectedCost) > 1e-6 {
		t.Errorf("expected session cost %f, got %f", expectedCost, metrics.SessionCost)
	}

	// Second call on same session
	metrics2 := RequestMetrics{
		Model:           "anthropic/claude-3.7-sonnet",
		SessionID:       "test-session-1",
		InputTokens:     200,
		OutputTokens:    100,
		CacheReadTokens: 800,
		Latency:         2 * time.Second,
	}
	metrics2.ComputeFinalMetrics(pricing, st)

	// Hit rate = 800 / (200 + 800) * 100 = 80.0%
	if math.Abs(metrics2.CacheHitRate-80.0) > 1e-6 {
		t.Errorf("expected cache hit rate 80.0%%, got %f", metrics2.CacheHitRate)
	}
	// TPS = 100 / 2.0 = 50.0 TPS
	if math.Abs(metrics2.ThroughputTPS-50.0) > 1e-6 {
		t.Errorf("expected TPS 50.0, got %f", metrics2.ThroughputTPS)
	}
	// Call 2 Cost = 200 * 0.000003 + 800 * 0.0000003 + 100 * 0.000015 = 0.0006 + 0.00024 + 0.0015 = 0.00234
	expectedCost2 := 0.00234
	if math.Abs(metrics2.CallCost-expectedCost2) > 1e-6 {
		t.Errorf("expected call 2 cost %f, got %f", expectedCost2, metrics2.CallCost)
	}
	// Session total = 0.00642 + 0.00234 = 0.00876
	expectedSessionTotal := expectedCost + expectedCost2
	if math.Abs(metrics2.SessionCost-expectedSessionTotal) > 1e-6 {
		t.Errorf("expected session total cost %f, got %f", expectedSessionTotal, metrics2.SessionCost)
	}
}

func TestParseUsageFromJSON(t *testing.T) {
	// Anthropic JSON
	anthropicJSON := []byte(`{
		"id": "msg_123",
		"type": "message",
		"role": "assistant",
		"usage": {
			"input_tokens": 1500,
			"output_tokens": 350,
			"cache_read_input_tokens": 800,
			"cache_creation_input_tokens": 120
		}
	}`)

	in, out, cr, cw := ParseUsageFromJSON(anthropicJSON)
	if in != 1500 || out != 350 || cr != 800 || cw != 120 {
		t.Errorf("unexpected anthropic parse: in=%d, out=%d, cr=%d, cw=%d", in, out, cr, cw)
	}

	// OpenAI JSON (prompt_tokens 1000 with 500 cached -> 500 uncached input)
	openaiJSON := []byte(`{
		"usage": {
			"prompt_tokens": 1000,
			"completion_tokens": 200,
			"prompt_tokens_details": {
				"cached_tokens": 500
			}
		}
	}`)

	in, out, cr, cw = ParseUsageFromJSON(openaiJSON)
	if in != 500 || out != 200 || cr != 500 || cw != 0 {
		t.Errorf("unexpected openai parse: in=%d, out=%d, cr=%d, cw=%d", in, out, cr, cw)
	}

	// Nested in message.usage
	nestedMessageJSON := []byte(`{
		"message": {
			"usage": {
				"input_tokens": 1100,
				"output_tokens": 400
			}
		}
	}`)
	in, out, cr, cw = ParseUsageFromJSON(nestedMessageJSON)
	if in != 1100 || out != 400 {
		t.Errorf("unexpected nested message parse: in=%d, out=%d", in, out)
	}

	// Nested in response.usage
	nestedResponseJSON := []byte(`{
		"response": {
			"usage": {
				"prompt_tokens": 900,
				"completion_tokens": 150
			}
		}
	}`)
	in, out, cr, cw = ParseUsageFromJSON(nestedResponseJSON)
	if in != 900 || out != 150 {
		t.Errorf("unexpected nested response parse: in=%d, out=%d", in, out)
	}
}

func TestParseUsageFromSSELine_VariousFormats(t *testing.T) {
	var in, out, cr, cw int

	// 1. Anthropic message_start with prompt_tokens and completion_tokens
	line1 := `data: {"type":"message_start","message":{"usage":{"prompt_tokens":450,"output_tokens":1}}}`
	ParseUsageFromSSELine(line1, &in, &out, &cr, &cw)
	if in != 450 || out != 1 {
		t.Errorf("expected in=450 out=1, got in=%d out=%d", in, out)
	}

	// 2. OpenRouter delta usage with output_tokens
	line2 := `data: {"type":"message_delta","delta":{"usage":{"output_tokens":120}}}`
	ParseUsageFromSSELine(line2, &in, &out, &cr, &cw)
	if out != 120 {
		t.Errorf("expected out=120, got out=%d", out)
	}

	// 3. OpenRouter choices[0].usage
	line3 := `data: {"id":"gen-1","choices":[{"delta":{},"usage":{"prompt_tokens":600,"completion_tokens":250}}]}`
	ParseUsageFromSSELine(line3, &in, &out, &cr, &cw)
	if in != 600 || out != 250 {
		t.Errorf("expected in=600 out=250, got in=%d out=%d", in, out)
	}

	// 4. Bare JSON line without "data: " prefix (e.g. from parseSSEStream rawData)
	line4 := `{"type":"message_start","message":{"usage":{"input_tokens":750,"output_tokens":40}}}`
	ParseUsageFromSSELine(line4, &in, &out, &cr, &cw)
	if in != 750 || out != 250 { // out was 250, 40 < 250 so out stays 250
		t.Errorf("expected in=750 out=250, got in=%d out=%d", in, out)
	}
}

func TestParseUsageFromSSELine_OpenAI(t *testing.T) {
	var in, out, cr, cw int

	// OpenAI streaming chunk usage format
	line := `data: {"id":"chatcmpl-1","choices":[{"delta":{}}],"usage":{"prompt_tokens":800,"completion_tokens":150,"prompt_tokens_details":{"cached_tokens":300}}}`
	ParseUsageFromSSELine(line, &in, &out, &cr, &cw)

	if in != 500 { // 800 - 300 = 500
		t.Errorf("expected 500 uncached input tokens, got %d", in)
	}
	if out != 150 {
		t.Errorf("expected 150 output tokens, got %d", out)
	}
	if cr != 300 {
		t.Errorf("expected 300 cache read tokens, got %d", cr)
	}
}

func TestFormatInt(t *testing.T) {
	tests := []struct {
		input    int
		expected string
	}{
		{0, "0"},
		{50, "50"},
		{999, "999"},
		{1000, "1,000"},
		{1234567, "1,234,567"},
		{-50, "-50"},
		{-500, "-500"},
		{-1000, "-1,000"},
		{-1234567, "-1,234,567"},
	}

	for _, tt := range tests {
		got := formatInt(tt.input)
		if got != tt.expected {
			t.Errorf("formatInt(%d) = %q, expected %q", tt.input, got, tt.expected)
		}
	}
}

func TestSSEInterceptor(t *testing.T) {
	sseData := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":1200,\"cache_read_input_tokens\":700,\"cache_creation_input_tokens\":50}}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello world\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":250}}\n\n"

	var capturedIn, capturedOut, capturedCR, capturedCW int
	interceptor := NewSSEInterceptor(io.NopCloser(strings.NewReader(sseData)), func(in, out, cr, cw int) {
		capturedIn = in
		capturedOut = out
		capturedCR = cr
		capturedCW = cw
	})

	// Read stream in small chunks to test stream buffering
	buf := make([]byte, 16)
	var output bytes.Buffer
	for {
		n, err := interceptor.Read(buf)
		if n > 0 {
			output.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	_ = interceptor.Close()

	if output.String() != sseData {
		t.Errorf("expected identical stream output, got: %s", output.String())
	}
	if capturedIn != 1200 {
		t.Errorf("expected input tokens 1200, got %d", capturedIn)
	}
	if capturedOut != 250 {
		t.Errorf("expected output tokens 250, got %d", capturedOut)
	}
	if capturedCR != 700 {
		t.Errorf("expected cache read tokens 700, got %d", capturedCR)
	}
	if capturedCW != 50 {
		t.Errorf("expected cache creation tokens 50, got %d", capturedCW)
	}
}

func TestExtractProviderFromSSELine(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{`data: {"type":"message_start","provider":"Anthropic","message":{"usage":{"input_tokens":1}}}`, "Anthropic"},
		{`data: {"type":"content_block_delta"}`, ""},
		{`data: [DONE]`, ""},
		{`event: message_start`, ""},
		{`data: {bad json`, ""},
		{`: provider: deepinfra`, "deepinfra"},
		{`: Provider: DeepInfra`, "DeepInfra"},
		{`: OPENROUTER PROCESSING: Novita`, "Novita"},
		{`: OpenRouter Processing: NovitaAI`, "NovitaAI"},
		{`: openrouter: Together`, "Together"},
		{`data: {"type":"message_start","message":{"provider":"Together"}}`, "Together"},
		{`{"type":"message_start","provider":"Z-AI"}`, "Z-AI"},
		{`{"message":{"provider":"Hyperbolic"}}`, "Hyperbolic"},
	}
	for _, c := range cases {
		if got := ExtractProviderFromSSELine(c.line); got != c.want {
			t.Errorf("ExtractProviderFromSSELine(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}

func TestSSEInterceptor_MultilineTrailingBuffer(t *testing.T) {
	// Stream ends with buffered data containing event + data without final flush before EOF
	sseData := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":500}}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":200}}" // no trailing \n

	var capturedIn, capturedOut int
	interceptor := NewSSEInterceptor(io.NopCloser(strings.NewReader(sseData)), func(in, out, cr, cw int) {
		capturedIn = in
		capturedOut = out
	})
	buf := make([]byte, 1024)
	for {
		_, err := interceptor.Read(buf)
		if err != nil {
			break
		}
	}
	_ = interceptor.Close()

	if capturedIn != 500 {
		t.Errorf("expected input tokens 500, got %d", capturedIn)
	}
	if capturedOut != 200 {
		t.Errorf("expected output tokens 200, got %d", capturedOut)
	}
}

func TestSSEInterceptor_CapturesProvider(t *testing.T) {
	sseData := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"provider\":\"Google\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n"

	interceptor := NewSSEInterceptor(io.NopCloser(strings.NewReader(sseData)), func(in, out, cr, cw int) {})
	buf := make([]byte, 32)
	for {
		_, err := interceptor.Read(buf)
		if err != nil {
			break
		}
	}
	_ = interceptor.Close()

	if got := interceptor.Provider(); got != "Google" {
		t.Errorf("expected provider Google, got %q", got)
	}
}

func TestSSEInterceptor_TerminalErr(t *testing.T) {
	// Normal EOF should result in nil TerminalErr.
	interceptor := NewSSEInterceptor(io.NopCloser(strings.NewReader("")), func(in, out, cr, cw int) {})
	_, err := interceptor.Read(make([]byte, 16))
	if err == nil {
		t.Fatal("expected EOF from empty reader")
	}
	if interceptor.TerminalErr() != nil {
		t.Errorf("expected nil terminal err on clean EOF, got %v", interceptor.TerminalErr())
	}
}

func TestSSEInterceptor_ConcurrentCloseNoRace(t *testing.T) {
	sseData := "data: {\"type\":\"message_start\"}\n\n"
	interceptor := NewSSEInterceptor(io.NopCloser(strings.NewReader(sseData)), func(in, out, cr, cw int) {})
	go func() {
		_ = interceptor.Close()
	}()
	buf := make([]byte, 16)
	for {
		_, err := interceptor.Read(buf)
		if err != nil {
			break
		}
	}
}

func TestLogObservability(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	m := RequestMetrics{
		Model:           "anthropic/claude-3.7-sonnet",
		SessionID:       "session-test",
		InputTokens:     1000,
		OutputTokens:    200,
		CacheReadTokens: 500,
		Latency:         2500 * time.Millisecond,
		ThroughputTPS:   80.0,
		CacheHitRate:    33.3,
		CallCost:        0.005,
		SessionCost:     0.015,
	}

	LogObservability(logger, m)

	out := buf.String()
	if !strings.Contains(out, "[OpenRouter]") {
		t.Errorf("expected [OpenRouter] prefix, got: %s", out)
	}
	if !strings.Contains(out, "80.0 TPS") {
		t.Errorf("expected TPS in log, got: %s", out)
	}
	if !strings.Contains(out, "SUCCESS") {
		t.Errorf("expected SUCCESS tag in log, got: %s", out)
	}
}

func TestRequestMetrics_ComputeFinalMetrics_ResponseCacheHit(t *testing.T) {
	st := NewSessionTracker(1*time.Hour, 100)
	pricing := Pricing{
		Prompt:     0.000003,
		Completion: 0.000015,
	}

	// Normal call first
	m1 := RequestMetrics{
		Model:        "anthropic/claude-3.7-sonnet",
		SessionID:    "session-cache-test",
		InputTokens:  1000,
		OutputTokens: 200,
		Latency:      1 * time.Second,
	}
	m1.ComputeFinalMetrics(pricing, st)
	if m1.CallCost <= 0 {
		t.Fatalf("expected non-zero call cost for miss, got %f", m1.CallCost)
	}
	firstCost := m1.CallCost

	// Cache HIT call with upstream-zeroed usage
	m2 := RequestMetrics{
		Model:         "anthropic/claude-3.7-sonnet",
		SessionID:     "session-cache-test",
		CacheStatus:   "HIT",
		CacheAge:      120,
		CacheTTL:      180,
		CacheSourceID: "gen-src-456",
		InputTokens:   0,
		OutputTokens:  0,
		Latency:       50 * time.Millisecond,
	}
	m2.ComputeFinalMetrics(pricing, st)

	if m2.CallCost != 0.0 {
		t.Errorf("expected $0.00 call cost on HIT, got %f", m2.CallCost)
	}
	if m2.CacheHitRate != 0.0 {
		t.Errorf("expected 0.0%% cache hit rate for zeroed prompt, got %f", m2.CacheHitRate)
	}
	if m2.ThroughputTPS != 0.0 {
		t.Errorf("expected 0.0 TPS for zeroed output, got %f", m2.ThroughputTPS)
	}
	if math.Abs(m2.SessionCost-firstCost) > 1e-6 {
		t.Errorf("expected session cost to remain %f, got %f", firstCost, m2.SessionCost)
	}
}

// TestRequestMetrics_ComputeFinalMetrics_CacheHitWithNonZeroUsage pins the
// accounting contract for a HIT that reports real usage: the call is free, but
// the tokens still land in the session tracker because they describe context
// that was actually consumed.
func TestRequestMetrics_ComputeFinalMetrics_CacheHitWithNonZeroUsage(t *testing.T) {
	st := NewSessionTracker(1*time.Hour, 100)
	pricing := Pricing{
		Prompt:     0.000003,
		Completion: 0.000015,
	}

	m := RequestMetrics{
		Model:         "anthropic/claude-3.7-sonnet",
		SessionID:     "session-hit-usage",
		CacheStatus:   "HIT",
		CacheAge:      30,
		CacheTTL:      270,
		CacheSourceID: "gen-src-789",
		InputTokens:   1200,
		OutputTokens:  350,
		Latency:       100 * time.Millisecond,
	}
	m.ComputeFinalMetrics(pricing, st)

	if m.CallCost != 0.0 {
		t.Errorf("expected $0.00 call cost on HIT with reported usage, got %f", m.CallCost)
	}
	if m.SessionCost != 0.0 {
		t.Errorf("expected session cost to stay at $0.00, got %f", m.SessionCost)
	}
	if m.ThroughputTPS <= 0 {
		t.Errorf("expected TPS to be computed from reported output tokens, got %f", m.ThroughputTPS)
	}

	stats, ok := st.Get("session-hit-usage")
	if !ok {
		t.Fatalf("expected session to be tracked on HIT")
	}
	if stats.InputTokens != 1200 {
		t.Errorf("expected 1200 input tokens recorded on HIT, got %d", stats.InputTokens)
	}
	if stats.OutputTokens != 350 {
		t.Errorf("expected 350 output tokens recorded on HIT, got %d", stats.OutputTokens)
	}
	if stats.TotalCost != 0.0 {
		t.Errorf("expected $0.00 session total on a HIT-only session, got %f", stats.TotalCost)
	}
}

func TestLogObservability_ResponseCacheFormatting(t *testing.T) {
	t.Run("HIT log formatting", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

		m := RequestMetrics{
			Model:         "anthropic/claude-3.7-sonnet",
			SessionID:     "session-hit",
			CacheStatus:   "HIT",
			CacheAge:      45,
			CacheTTL:      255,
			CacheSourceID: "gen-xyz-789",
			Latency:       120 * time.Millisecond,
		}
		LogObservability(logger, m)
		out := buf.String()

		if !strings.Contains(out, "response cache: HIT (age: 45s)") {
			t.Errorf("expected 'response cache: HIT (age: 45s)' in msg, got: %s", out)
		}
		if !strings.Contains(out, "response_cache_status=HIT") {
			t.Errorf("expected response_cache_status=HIT attribute, got: %s", out)
		}
		if !strings.Contains(out, "response_cache_age=45") {
			t.Errorf("expected response_cache_age=45 attribute, got: %s", out)
		}
		if !strings.Contains(out, "response_cache_ttl=255") {
			t.Errorf("expected response_cache_ttl=255 attribute, got: %s", out)
		}
		if !strings.Contains(out, "response_cache_source_id=gen-xyz-789") {
			t.Errorf("expected response_cache_source_id=gen-xyz-789 attribute, got: %s", out)
		}
	})

	t.Run("MISS log formatting", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

		m := RequestMetrics{
			Model:        "anthropic/claude-3.7-sonnet",
			SessionID:    "session-miss",
			CacheStatus:  "MISS",
			CacheTTL:     300,
			InputTokens:  500,
			OutputTokens: 100,
			Latency:      1500 * time.Millisecond,
		}
		LogObservability(logger, m)
		out := buf.String()

		if !strings.Contains(out, "response cache: MISS") {
			t.Errorf("expected 'response cache: MISS' in msg, got: %s", out)
		}
		if !strings.Contains(out, "response_cache_status=MISS") {
			t.Errorf("expected response_cache_status=MISS attribute, got: %s", out)
		}
	})
}
