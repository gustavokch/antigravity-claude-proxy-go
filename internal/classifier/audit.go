package classifier

import (
	"sync"
	"time"
)

// Event is one interception decision, as shown in the WebUI audit stream.
// Status is one of "rerouted", "stubbed", "passthrough", or "error".
type Event struct {
	Seq       uint64    `json:"seq"`
	Timestamp time.Time `json:"timestamp"`
	RuleID    string    `json:"ruleId"`
	RuleName  string    `json:"ruleName"`
	Action    string    `json:"action"`
	Backend   string    `json:"backend,omitempty"`
	Status    string    `json:"status"`
	LatencyMs int64     `json:"latencyMs"`
	Model     string    `json:"model,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

// Recorder is a bounded ring of recent events plus a fan-out to live
// subscribers. Every method is nil-safe so callers need no guard: the proxy
// must keep serving even if the recorder was never wired up.
type Recorder struct {
	mu          sync.RWMutex
	capacity    int
	seq         uint64
	events      []Event
	subscribers map[chan Event]struct{}
}

func NewRecorder(capacity int) *Recorder {
	if capacity <= 0 {
		capacity = 200
	}
	return &Recorder{
		capacity:    capacity,
		events:      make([]Event, 0, capacity),
		subscribers: make(map[chan Event]struct{}),
	}
}

// Add records an event and fans it out. Delivery is non-blocking: a
// subscriber whose buffer is full drops the event rather than stalling a
// request that is sitting in the user's permission prompt.
func (recorder *Recorder) Add(event Event) {
	if recorder == nil {
		return
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}

	recorder.mu.Lock()
	recorder.seq++
	event.Seq = recorder.seq
	recorder.events = append(recorder.events, event)
	if len(recorder.events) > recorder.capacity {
		recorder.events = recorder.events[len(recorder.events)-recorder.capacity:]
	}
	for subscriber := range recorder.subscribers {
		select {
		case subscriber <- event:
		default:
		}
	}
	recorder.mu.Unlock()
}

// History returns the buffered events oldest-first.
func (recorder *Recorder) History() []Event {
	if recorder == nil {
		return nil
	}
	recorder.mu.RLock()
	defer recorder.mu.RUnlock()
	out := make([]Event, len(recorder.events))
	copy(out, recorder.events)
	return out
}

// Subscribe registers a live listener. The returned func unregisters and
// closes the channel; it is safe to call more than once.
func (recorder *Recorder) Subscribe(bufSize int) (<-chan Event, func()) {
	if recorder == nil {
		closed := make(chan Event)
		close(closed)
		return closed, func() {}
	}
	if bufSize <= 0 {
		bufSize = 32
	}
	channel := make(chan Event, bufSize)

	recorder.mu.Lock()
	recorder.subscribers[channel] = struct{}{}
	recorder.mu.Unlock()

	var once sync.Once
	return channel, func() {
		once.Do(func() {
			recorder.mu.Lock()
			delete(recorder.subscribers, channel)
			close(channel)
			recorder.mu.Unlock()
		})
	}
}
