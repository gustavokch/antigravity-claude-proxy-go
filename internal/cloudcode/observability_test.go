package cloudcode

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
	"log/slog"
)

func almostEqual(a, b, tolerance float64) bool {
	return math.Abs(a-b) <= tolerance
}

func TestSessionTracker(t *testing.T) {
	tracker := NewSessionTracker()

	s1 := tracker.Record("sess-1", 100, 50, 20, 0.001)
	if s1.TotalRequests != 1 || s1.InputTokens != 100 || s1.OutputTokens != 50 || s1.CacheRead != 20 || s1.TotalCost != 0.001 {
		t.Errorf("unexpected session stats: %+v", s1)
	}

	s2 := tracker.Record("sess-1", 200, 100, 50, 0.002)
	if s2.TotalRequests != 2 || s2.InputTokens != 300 || s2.OutputTokens != 150 || s2.CacheRead != 70 || s2.TotalCost != 0.003 {
		t.Errorf("unexpected accumulated session stats: %+v", s2)
	}

	// Test default session key on empty ID
	sDefault := tracker.Record("", 10, 10, 0, 0.0001)
	if sDefault.TotalRequests != 1 {
		t.Errorf("expected default session stats: %+v", sDefault)
	}
}

func TestSessionTracker_RecordWithTime(t *testing.T) {
	tracker := NewSessionTracker()
	fixedTime := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)

	s := tracker.RecordWithTime("sess-fixed", 50, 25, 10, 0.0005, fixedTime)
	if !s.LastActive.Equal(fixedTime) {
		t.Errorf("LastActive = %v, want %v", s.LastActive, fixedTime)
	}
}

func TestSessionTracker_CapacityBounds(t *testing.T) {
	tracker := NewSessionTracker()

	for i := 0; i < maxSessionEntries+50; i++ {
		sessionID := "session-" + string(rune('a'+(i%26))) + "-" + time.Now().Format(time.RFC3339Nano)
		tracker.Record(sessionID, 10, 10, 0, 0.0001)
	}

	tracker.mu.RLock()
	sessionsLen := len(tracker.sessions)
	tracker.mu.RUnlock()

	if sessionsLen > maxSessionEntries {
		t.Errorf("expected sessions map size <= %d, got %d", maxSessionEntries, sessionsLen)
	}
}

func TestRequestMetrics_ComputeFinalMetrics(t *testing.T) {
	tracker := NewSessionTracker()
	now := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)

	m := RequestMetrics{
		Model:           "claude-sonnet-4-6",
		SessionID:       "sess-test",
		InputTokens:     1000,
		OutputTokens:    200,
		CacheReadTokens: 500,
		Latency:         2 * time.Second,
	}

	m.ComputeFinalMetrics(tracker, now)

	// Throughput: 200 tokens / 2s = 100 TPS
	if m.ThroughputTPS != 100.0 {
		t.Errorf("expected 100 TPS, got %f", m.ThroughputTPS)
	}

	// Total prompt = 1000 + 500 = 1500. Hit rate = 500 / 1500 = 33.333...%
	if m.CacheHitRate < 33.3 || m.CacheHitRate > 33.4 {
		t.Errorf("expected ~33.3%% cache hit rate, got %f", m.CacheHitRate)
	}

	// Pricing for claude-sonnet-4-6:
	// Prompt: $3/M, Completion: $15/M, CacheRead: $0.30/M
	// Cost = 1000 * 3e-6 + 200 * 15e-6 + 500 * 0.3e-6 = 0.003 + 0.003 + 0.00015 = 0.00615
	if !almostEqual(m.RetailCostUSD, 0.00615, 1e-6) {
		t.Errorf("expected RetailCostUSD = 0.00615, got %f", m.RetailCostUSD)
	}

	if m.SessionRetailUSD != m.RetailCostUSD {
		t.Errorf("expected SessionRetailUSD == RetailCostUSD for first call, got %f vs %f", m.SessionRetailUSD, m.RetailCostUSD)
	}
}

func TestCalculateRetailCost_Models(t *testing.T) {
	t2026 := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	t2027 := time.Date(2027, time.January, 2, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		model    string
		in       int
		out      int
		cr       int
		now      time.Time
		expected float64
	}{
		{
			name:     "claude-opus-4-6-thinking",
			model:    "claude-opus-4-6-thinking",
			in:       1000000,
			out:      1000000,
			cr:       1000000,
			now:      t2026,
			expected: 15.00 + 75.00 + 1.50, // 91.50
		},
		{
			name:     "claude-sonnet-4-6",
			model:    "claude-sonnet-4-6",
			in:       1000000,
			out:      1000000,
			cr:       1000000,
			now:      t2026,
			expected: 3.00 + 15.00 + 0.30, // 18.30
		},
		{
			name:     "gemini-3.1-pro-high",
			model:    "gemini-3.1-pro-high",
			in:       1000000,
			out:      1000000,
			cr:       1000000,
			now:      t2026,
			expected: 2.00 + 12.00 + 0.50, // 14.50
		},
		{
			name:     "gemini-3.1-pro-low",
			model:    "gemini-3.1-pro-low",
			in:       1000000,
			out:      1000000,
			cr:       1000000,
			now:      t2026,
			expected: 2.00 + 12.00 + 0.50, // 14.50
		},
		{
			name:     "gemini-3.8-flash in 2026",
			model:    "gemini-3.8-flash-high",
			in:       1000000,
			out:      1000000,
			cr:       1000000,
			now:      t2026,
			expected: 1.35 + 6.75 + 0.3375, // 8.4375
		},
		{
			name:     "gemini-3.8-flash in 2027",
			model:    "gemini-3.8-flash-high",
			in:       1000000,
			out:      1000000,
			cr:       1000000,
			now:      t2027,
			expected: 2.70 + 13.50 + 0.675, // 16.875
		},
		{
			name:     "gemini-3.7-flash-medium in 2026",
			model:    "gemini-3.7-flash-medium",
			in:       1000000,
			out:      1000000,
			cr:       1000000,
			now:      t2026,
			expected: 1.35 + 6.75 + 0.3375,
		},
		{
			name:     "gemini-3.6-flash-low in 2027",
			model:    "gemini-3.6-flash-low",
			in:       1000000,
			out:      1000000,
			cr:       1000000,
			now:      t2027,
			expected: 2.70 + 13.50 + 0.675,
		},
		{
			name:     "gpt-oss-120b-medium",
			model:    "gpt-oss-120b-medium",
			in:       1000000,
			out:      1000000,
			cr:       1000000,
			now:      t2026,
			expected: 0.15 + 0.60 + 0.0375, // 0.7875
		},
		{
			name:     "claude-haiku-4-5",
			model:    "claude-haiku-4-5",
			in:       1000000,
			out:      1000000,
			cr:       1000000,
			now:      t2026,
			expected: 0.80 + 4.00 + 0.08, // 4.88
		},
		{
			name:     "claude-3-5-haiku-20241022",
			model:    "claude-3-5-haiku-20241022",
			in:       1000000,
			out:      1000000,
			cr:       1000000,
			now:      t2026,
			expected: 0.80 + 4.00 + 0.08, // 4.88
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CalculateRetailCost(tc.model, tc.in, tc.out, tc.cr, tc.now)
			if !almostEqual(got, tc.expected, 1e-4) {
				t.Errorf("CalculateRetailCost(%s) = %f, want %f", tc.model, got, tc.expected)
			}
		})
	}
}

func TestLogObservability(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	m := RequestMetrics{
		Model:            "claude-sonnet-4-6",
		Account:          "dev@example.com",
		ProjectID:        "proj-123",
		SessionID:        "sess-456",
		InputTokens:      14200,
		OutputTokens:     640,
		CacheReadTokens:  8192,
		ThinkingTokens:   256,
		CCRRetrievals:    2,
		Latency:          16600 * time.Millisecond,
		ThroughputTPS:    38.5,
		CacheHitRate:     57.7,
		RetailCostUSD:    0.0520,
		SessionRetailUSD: 0.8420,
	}

	LogObservability(logger, m)

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("failed to decode JSON log record: %v", err)
	}

	// Verify msg string format
	msg, _ := record["msg"].(string)
	wantMsg := "[Antigravity] claude-sonnet-4-6 (dev@example.com) | tokens: 14,200 in (8,192 cached, 57.7% hit), 640 out (256 thinking) | 38.5 TPS | 16.60s | $0.0520 saved ($0.8420 session) | CCR: 2 retrievals"
	if msg != wantMsg {
		t.Errorf("msg mismatch:\n got:  %q\nwant: %q", msg, wantMsg)
	}

	// Verify structured attributes
	if record["gateway"] != "antigravity" {
		t.Errorf("gateway = %v, want antigravity", record["gateway"])
	}
	if record["model"] != "claude-sonnet-4-6" {
		t.Errorf("model = %v, want claude-sonnet-4-6", record["model"])
	}
	if record["account"] != "dev@example.com" {
		t.Errorf("account = %v, want dev@example.com", record["account"])
	}
	if record["project_id"] != "proj-123" {
		t.Errorf("project_id = %v, want proj-123", record["project_id"])
	}
	if record["session_id"] != "sess-456" {
		t.Errorf("session_id = %v, want sess-456", record["session_id"])
	}
	if record["thinking_tokens"] != float64(256) {
		t.Errorf("thinking_tokens = %v, want 256", record["thinking_tokens"])
	}
	if record["ccr_retrievals"] != float64(2) {
		t.Errorf("ccr_retrievals = %v, want 2", record["ccr_retrievals"])
	}
	if record["level_tag"] != "SUCCESS" {
		t.Errorf("level_tag = %v, want SUCCESS", record["level_tag"])
	}
}

func TestLogObservability_NoCCRNoThinkingNoAccount(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	m := RequestMetrics{
		Model:            "gemini-3.8-flash-high",
		SessionID:        "sess-simple",
		InputTokens:      1000,
		OutputTokens:     100,
		CacheReadTokens:  0,
		Latency:          1000 * time.Millisecond,
		ThroughputTPS:    100.0,
		CacheHitRate:     0.0,
		RetailCostUSD:    0.0020,
		SessionRetailUSD: 0.0020,
	}

	LogObservability(logger, m)

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("failed to decode JSON log record: %v", err)
	}

	msg, _ := record["msg"].(string)
	wantMsg := "[Antigravity] gemini-3.8-flash-high | tokens: 1,000 in (0 cached, 0.0% hit), 100 out | 100.0 TPS | 1.00s | $0.0020 saved ($0.0020 session)"
	if msg != wantMsg {
		t.Errorf("msg mismatch:\n got:  %q\nwant: %q", msg, wantMsg)
	}
	if strings.Contains(msg, "CCR:") {
		t.Errorf("expected no CCR tag, got: %s", msg)
	}
	if strings.Contains(msg, "thinking") {
		t.Errorf("expected no thinking tag, got: %s", msg)
	}
}

func TestLogObservability_SingleCCRRetrieval(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	m := RequestMetrics{
		Model:            "claude-sonnet-4-6",
		CCRRetrievals:    1,
		InputTokens:      1000,
		OutputTokens:     100,
		Latency:          2 * time.Second,
		ThroughputTPS:    50.0,
		RetailCostUSD:    0.004,
		SessionRetailUSD: 0.004,
	}

	LogObservability(logger, m)

	var record map[string]any
	if err := json.Unmarshal(buf.Bytes(), &record); err != nil {
		t.Fatalf("failed to decode JSON log record: %v", err)
	}

	msg, _ := record["msg"].(string)
	if !strings.Contains(msg, "| CCR: 1 retrieval") {
		t.Errorf("expected singular CCR retrieval format, got: %s", msg)
	}
}

func TestFormatInt(t *testing.T) {
	tests := []struct {
		input int
		want  string
	}{
		{0, "0"},
		{5, "5"},
		{99, "99"},
		{999, "999"},
		{1000, "1,000"},
		{14200, "14,200"},
		{1234567, "1,234,567"},
		{-1000, "-1,000"},
	}

	for _, tc := range tests {
		got := formatInt(tc.input)
		if got != tc.want {
			t.Errorf("formatInt(%d) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
