package claudecode

import (
	"testing"
	"time"
)

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
	m := RequestMetrics{
		Model:               "claude-sonnet-5",
		SessionID:           "sess-test",
		InputTokens:         1000,
		OutputTokens:        200,
		CacheReadTokens:     500,
		CacheCreationTokens: 0,
		Latency:             2 * time.Second,
	}

	m.ComputeFinalMetrics(tracker)

	if m.ThroughputTPS != 100.0 { // 200 tokens / 2s = 100 TPS
		t.Errorf("expected 100 TPS, got %f", m.ThroughputTPS)
	}

	// Total prompt = 1000 + 500 = 1500. Hit rate = 500/1500 = 33.33%
	if m.CacheHitRate < 33.0 || m.CacheHitRate > 34.0 {
		t.Errorf("expected ~33.3%% cache hit rate, got %f", m.CacheHitRate)
	}

	if m.CallCost <= 0 {
		t.Errorf("expected positive CallCost, got %f", m.CallCost)
	}

	if m.SessionCost != m.CallCost {
		t.Errorf("expected SessionCost == CallCost for first call, got %f vs %f", m.SessionCost, m.CallCost)
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
		{1234, "1,234"},
		{12345, "12,345"},
		{123456, "123,456"},
		{1234567, "1,234,567"},
		{-1, "-1"},
		{-999, "-999"},
		{-1000, "-1,000"},
		{-1234567, "-1,234,567"},
		{-1 << 63, "-9,223,372,036,854,775,808"},
	}

	for _, tc := range tests {
		got := formatInt(tc.input)
		if got != tc.want {
			t.Errorf("formatInt(%d) = %q, want %q", tc.input, got, tc.want)
		}
	}
}
