package kimi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

func TestRequestMetrics_ComputeFinalMetrics(t *testing.T) {
	m := RequestMetrics{
		Model:               "moonshot-v1-8k",
		SessionID:           "sess-123",
		InputTokens:         1000,
		OutputTokens:        200,
		CacheReadTokens:     500,
		CacheCreationTokens: 0,
		Latency:             2 * time.Second,
	}

	m.ComputeFinalMetrics()

	if m.ThroughputTPS != 100.0 {
		t.Errorf("expected 100 TPS, got %f", m.ThroughputTPS)
	}

	if m.CacheHitRate < 33.3 || m.CacheHitRate > 33.4 {
		t.Errorf("expected ~33.3%% cache hit rate, got %f", m.CacheHitRate)
	}
}

func TestLogObservability(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	m := RequestMetrics{
		Model:               "moonshot-v1-32k",
		SessionID:           "sess-kimi",
		InputTokens:         1000,
		OutputTokens:        200,
		CacheReadTokens:     500,
		CacheCreationTokens: 50,
		Latency:             4 * time.Second,
		ThroughputTPS:       50.0,
		CacheHitRate:        33.3,
	}

	LogObservability(logger, m)

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("failed to parse JSON record: %v", err)
	}

	msg, _ := record["msg"].(string)
	wantPrefix := "[Kimi] moonshot-v1-32k | tokens: 1,000 in (500 cached, 33.3% hit), 200 out | 50.0 TPS | 4.00s"
	if msg != wantPrefix {
		t.Errorf("msg mismatch:\n got:  %q\nwant: %q", msg, wantPrefix)
	}

	if record["gateway"] != "kimi" {
		t.Errorf("gateway = %v, want kimi", record["gateway"])
	}
	if record["level_tag"] != "SUCCESS" {
		t.Errorf("level_tag = %v, want SUCCESS", record["level_tag"])
	}
	if record["input_tokens"] != float64(1000) {
		t.Errorf("input_tokens = %v, want 1000", record["input_tokens"])
	}
	if record["output_tokens"] != float64(200) {
		t.Errorf("output_tokens = %v, want 200", record["output_tokens"])
	}
}

func TestFormatInt(t *testing.T) {
	tests := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{100, "100"},
		{1000, "1,000"},
		{1234567, "1,234,567"},
		{-500, "-500"},
		{-1000, "-1,000"},
	}

	for _, tc := range tests {
		if got := formatInt(tc.in); got != tc.want {
			t.Errorf("formatInt(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
