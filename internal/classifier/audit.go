package classifier

import (
	"time"

	"antigravity-go-proxy/internal/ringbuf"
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
	b *ringbuf.Broadcaster[Event]
}

func NewRecorder(capacity int) *Recorder {
	if capacity <= 0 {
		capacity = 200
	}
	return &Recorder{
		b: ringbuf.NewBroadcaster[Event](capacity, func(event *Event, seq uint64) {
			event.Seq = seq
		}),
	}
}

// Add records an event and fans it out. Delivery is non-blocking: a
// subscriber whose buffer is full drops the event rather than stalling a
// request that is sitting in the user's permission prompt.
func (recorder *Recorder) Add(event Event) {
	if recorder == nil || recorder.b == nil {
		return
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	recorder.b.Add(event)
}

// History returns the buffered events oldest-first.
func (recorder *Recorder) History() []Event {
	if recorder == nil || recorder.b == nil {
		return nil
	}
	return recorder.b.GetHistory()
}

// Subscribe registers a live listener. The returned func unregisters and
// closes the channel; it is safe to call more than once.
func (recorder *Recorder) Subscribe(bufSize int) (<-chan Event, func()) {
	if recorder == nil || recorder.b == nil {
		closed := make(chan Event)
		close(closed)
		return closed, func() {}
	}
	return recorder.b.Subscribe(bufSize)
}
