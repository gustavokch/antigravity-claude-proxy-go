package ringbuf

import (
	"sync"
)

// Broadcaster manages a bounded ring buffer of recent items and real-time subscribers.
// All operations are safe for concurrent use.
type Broadcaster[T any] struct {
	mu          sync.RWMutex
	capacity    int
	seq         uint64
	entries     []T
	start       int
	count       int
	subscribers map[chan T]struct{}
	setSeq      func(*T, uint64)
}

// NewBroadcaster creates a Broadcaster with a fixed capacity.
// Optional setSeq callback can be provided to assign monotonic sequences to items.
func NewBroadcaster[T any](capacity int, setSeq func(*T, uint64)) *Broadcaster[T] {
	if capacity <= 0 {
		capacity = 200
	}
	return &Broadcaster[T]{
		capacity:    capacity,
		entries:     make([]T, capacity),
		subscribers: make(map[chan T]struct{}),
		setSeq:      setSeq,
	}
}

// Add appends an item to the ring buffer and delivers it to subscribers.
// Non-blocking drop occurs if a subscriber is slow.
func (b *Broadcaster[T]) Add(item T) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	b.seq++
	if b.setSeq != nil {
		b.setSeq(&item, b.seq)
	}

	if b.count < b.capacity {
		b.entries[(b.start+b.count)%b.capacity] = item
		b.count++
	} else {
		b.entries[b.start] = item
		b.start = (b.start + 1) % b.capacity
	}

	for ch := range b.subscribers {
		select {
		case ch <- item:
		default:
		}
	}
}

// GetHistory returns all buffered entries oldest first.
func (b *Broadcaster[T]) GetHistory() []T {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()

	result := make([]T, b.count)
	for i := 0; i < b.count; i++ {
		result[i] = b.entries[(b.start+i)%b.capacity]
	}
	return result
}

// Subscribe registers a new subscriber channel. The returned cancel func unregisters and closes it.
func (b *Broadcaster[T]) Subscribe(bufSize int) (<-chan T, func()) {
	if b == nil {
		closed := make(chan T)
		close(closed)
		return closed, func() {}
	}
	if bufSize <= 0 {
		bufSize = 32
	}
	ch := make(chan T, bufSize)

	b.mu.Lock()
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			if _, exists := b.subscribers[ch]; exists {
				delete(b.subscribers, ch)
				close(ch)
			}
			b.mu.Unlock()
		})
	}

	return ch, cancel
}

// Clear empties the ring buffer.
func (b *Broadcaster[T]) Clear() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.start = 0
	b.count = 0
}
