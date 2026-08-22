package openrouter

import (
	"math"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSessionTracker_RecordAndGet(t *testing.T) {
	st := NewSessionTracker(1*time.Hour, 100)

	sessionID := "test-session-123"
	s1 := st.Record(sessionID, 1000, 200, 500, 0.005)

	if s1.SessionID != sessionID {
		t.Errorf("expected sessionID %s, got %s", sessionID, s1.SessionID)
	}
	if s1.RequestCount != 1 {
		t.Errorf("expected request count 1, got %d", s1.RequestCount)
	}
	if s1.InputTokens != 1000 {
		t.Errorf("expected input tokens 1000, got %d", s1.InputTokens)
	}
	if s1.OutputTokens != 200 {
		t.Errorf("expected output tokens 200, got %d", s1.OutputTokens)
	}
	if s1.CacheReadTokens != 500 {
		t.Errorf("expected cache read tokens 500, got %d", s1.CacheReadTokens)
	}
	if math.Abs(s1.TotalCost-0.005) > 1e-9 {
		t.Errorf("expected total cost 0.005, got %f", s1.TotalCost)
	}

	// Record second request
	s2 := st.Record(sessionID, 800, 150, 400, 0.003)
	if s2.RequestCount != 2 {
		t.Errorf("expected request count 2, got %d", s2.RequestCount)
	}
	if s2.InputTokens != 1800 {
		t.Errorf("expected input tokens 1800, got %d", s2.InputTokens)
	}
	if s2.OutputTokens != 350 {
		t.Errorf("expected output tokens 350, got %d", s2.OutputTokens)
	}
	if s2.CacheReadTokens != 900 {
		t.Errorf("expected cache read tokens 900, got %d", s2.CacheReadTokens)
	}
	if math.Abs(s2.TotalCost-0.008) > 1e-9 {
		t.Errorf("expected total cost 0.008, got %f", s2.TotalCost)
	}

	// Fetch via Get
	fetched, ok := st.Get(sessionID)
	if !ok {
		t.Fatalf("expected session to exist")
	}
	if fetched.RequestCount != 2 {
		t.Errorf("expected request count 2 in fetched, got %d", fetched.RequestCount)
	}
}

func TestSessionTracker_Concurrent(t *testing.T) {
	st := NewSessionTracker(1*time.Hour, 1000)
	sessionID := "concurrent-session"

	var wg sync.WaitGroup
	workers := 20
	iterations := 50

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				st.Record(sessionID, 10, 5, 2, 0.001)
			}
		}()
	}
	wg.Wait()

	stats, ok := st.Get(sessionID)
	if !ok {
		t.Fatalf("expected session to exist")
	}

	expectedRequests := workers * iterations
	if stats.RequestCount != expectedRequests {
		t.Errorf("expected %d requests, got %d", expectedRequests, stats.RequestCount)
	}
	expectedInput := expectedRequests * 10
	if stats.InputTokens != expectedInput {
		t.Errorf("expected %d input tokens, got %d", expectedInput, stats.InputTokens)
	}
	expectedCost := float64(expectedRequests) * 0.001
	if math.Abs(stats.TotalCost-expectedCost) > 1e-6 {
		t.Errorf("expected total cost %f, got %f", expectedCost, stats.TotalCost)
	}
}

func TestExtractSessionID(t *testing.T) {
	// 1. From header
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("x-session-id", "header-session-123")
	if id := ExtractSessionID(req, nil); id != "header-session-123" {
		t.Errorf("expected header-session-123, got %s", id)
	}

	// 2. From metadata.session_id
	req2 := httptest.NewRequest("POST", "/v1/messages", nil)
	body := map[string]any{
		"metadata": map[string]any{
			"session_id": "meta-session-456",
		},
	}
	if id := ExtractSessionID(req2, body); id != "meta-session-456" {
		t.Errorf("expected meta-session-456, got %s", id)
	}

	// 3. Fallback
	req3 := httptest.NewRequest("POST", "/v1/messages", nil)
	req3.RemoteAddr = "127.0.0.1:12345"
	id := ExtractSessionID(req3, nil)
	if !strings.HasPrefix(id, "session_") {
		t.Errorf("expected fallback session prefix session_, got %s", id)
	}
}
