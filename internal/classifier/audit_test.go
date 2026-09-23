package classifier

import (
	"sync"
	"testing"
	"time"

	"antigravity-go-proxy/internal/config"
)

func TestRecorderHistoryIsCappedAndOrdered(t *testing.T) {
	recorder := NewRecorder(3)
	for i := 0; i < 5; i++ {
		recorder.Add(Event{RuleID: string(rune('a' + i)), Timestamp: time.Now()})
	}

	history := recorder.History()
	if len(history) != 3 {
		t.Fatalf("expected the ring to cap at 3, got %d", len(history))
	}
	if history[0].RuleID != "c" || history[2].RuleID != "e" {
		t.Errorf("expected oldest-first c,d,e, got %q..%q", history[0].RuleID, history[2].RuleID)
	}
}

func TestRecorderDeliversToSubscribers(t *testing.T) {
	recorder := NewRecorder(10)
	events, cancel := recorder.Subscribe(4)

	recorder.Add(Event{RuleID: "stage1-local", Status: "rerouted", LatencyMs: 42})

	select {
	case got := <-events:
		if got.RuleID != "stage1-local" || got.Status != "rerouted" || got.LatencyMs != 42 {
			t.Errorf("unexpected event %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber never received the event")
	}

	cancel()
	recorder.Add(Event{RuleID: "after-cancel"})
	if _, open := <-events; open {
		t.Error("cancel must close the subscriber channel")
	}
}

func TestRecorderDoesNotBlockOnSlowSubscriber(t *testing.T) {
	recorder := NewRecorder(10)
	_, cancel := recorder.Subscribe(1)
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			recorder.Add(Event{RuleID: "flood"})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Add blocked on a subscriber that never drains")
	}
}

func TestRecorderAddDoesNotRaceWithCancel(t *testing.T) {
	for i := 0; i < 2000; i++ {
		recorder := NewRecorder(1)
		_, cancel := recorder.Subscribe(1)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); cancel() }()
		go func() { defer wg.Done(); recorder.Add(Event{RuleID: "x"}) }()
		wg.Wait()
	}
}

func TestRecorderAddIsNilSafe(t *testing.T) {
	var recorder *Recorder
	recorder.Add(Event{RuleID: "no panic"})
	if history := recorder.History(); history != nil {
		t.Errorf("expected nil history from a nil recorder, got %+v", history)
	}
}

func TestRecorderAddAssignsMonotonicSeq(t *testing.T) {
	recorder := NewRecorder(10)
	recorder.Add(Event{RuleID: "a"})
	recorder.Add(Event{RuleID: "b"})
	recorder.Add(Event{RuleID: "c"})
	history := recorder.History()
	for i, event := range history {
		if event.Seq != uint64(i+1) {
			t.Fatalf("expected Seq %d, got %d", i+1, event.Seq)
		}
	}
}

func TestEventJSONWireFormat(t *testing.T) {
	stamp := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	event := Event{
		Seq:       1,
		Timestamp: stamp,
		RuleID:    "r1",
		RuleName:  "Test Rule",
		Action:    config.RuleActionReroute,
		Backend:   "local",
		Status:    EventStatusRerouted,
		LatencyMs: 42,
		Model:     "claude-sonnet-5",
		Detail:    "ok",
	}

	data, err := encodeCompactJSON(event)
	if err != nil {
		t.Fatalf("failed to encode event: %v", err)
	}

	expected := `{"seq":1,"timestamp":"2026-09-18T12:00:00Z","ruleId":"r1","ruleName":"Test Rule","action":"reroute","backend":"local","status":"rerouted","latencyMs":42,"model":"claude-sonnet-5","detail":"ok"}`
	if string(data) != expected {
		t.Errorf("wire format mismatch:\ngot:  %s\nwant: %s", string(data), expected)
	}
}
