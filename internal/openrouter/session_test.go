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

	// 2b. From metadata.user_id with JSON-encoded session string
	req2b := httptest.NewRequest("POST", "/v1/messages", nil)
	body2b := map[string]any{
		"metadata": map[string]any{
			"user_id": `{"device_id":"6a0a24c32704dc7fec147799c3e8161a371cbea05de6ccb8c376847bfa428346","account_uuid":"aabe0580-c4df-475e-8f00-7715f1947755","session_id":"6c03114f-4472-472b-8abc-b64396665e4d"}`,
		},
	}
	if id := ExtractSessionID(req2b, body2b); id != "6c03114f-4472-472b-8abc-b64396665e4d" {
		t.Errorf("expected extracted session_id 6c03114f-4472-472b-8abc-b64396665e4d, got %s", id)
	}

	// 3. Fallback with port stripping (different ports from same client yield same session ID)
	req3a := httptest.NewRequest("POST", "/v1/messages", nil)
	req3a.RemoteAddr = "127.0.0.1:12345"
	req3a.Header.Set("User-Agent", "TestClient/1.0")
	idA := ExtractSessionID(req3a, nil)

	req3b := httptest.NewRequest("POST", "/v1/messages", nil)
	req3b.RemoteAddr = "127.0.0.1:54321"
	req3b.Header.Set("User-Agent", "TestClient/1.0")
	idB := ExtractSessionID(req3b, nil)

	if !strings.HasPrefix(idA, "session_") {
		t.Errorf("expected fallback session prefix session_, got %s", idA)
	}
	if idA != idB {
		t.Errorf("expected same session ID despite different ephemeral ports, got %s vs %s", idA, idB)
	}
}

func TestSessionTracker_CapacityEviction(t *testing.T) {
	// Max capacity = 3, long TTL so nothing expires naturally
	st := NewSessionTracker(24*time.Hour, 3)

	st.Record("sess-1", 100, 50, 0, 0.01)
	time.Sleep(5 * time.Millisecond)
	st.Record("sess-2", 100, 50, 0, 0.01)
	time.Sleep(5 * time.Millisecond)
	st.Record("sess-3", 100, 50, 0, 0.01)

	// Now add 4th session - sess-1 should be evicted as the oldest
	st.Record("sess-4", 100, 50, 0, 0.01)

	if _, ok := st.Get("sess-1"); ok {
		t.Errorf("expected sess-1 to be evicted as oldest")
	}
	if _, ok := st.Get("sess-2"); !ok {
		t.Errorf("expected sess-2 to exist")
	}
	if _, ok := st.Get("sess-3"); !ok {
		t.Errorf("expected sess-3 to exist")
	}
	if _, ok := st.Get("sess-4"); !ok {
		t.Errorf("expected sess-4 to exist")
	}
}
