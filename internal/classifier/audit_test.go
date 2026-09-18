package classifier

import (
	"testing"
	"time"
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

func TestRecorderAddIsNilSafe(t *testing.T) {
	var recorder *Recorder
	recorder.Add(Event{RuleID: "no panic"})
	if history := recorder.History(); history != nil {
		t.Errorf("expected nil history from a nil recorder, got %+v", history)
	}
}
