package ringbuf

import (
	"sync"
	"testing"
	"time"
)

type testItem struct {
	ID  string
	Seq uint64
}

func TestBroadcaster_RingBuffer(t *testing.T) {
	b := NewBroadcaster[testItem](3, func(item *testItem, seq uint64) {
		item.Seq = seq
	})

	b.Add(testItem{ID: "1"})
	b.Add(testItem{ID: "2"})
	b.Add(testItem{ID: "3"})

	history := b.GetHistory()
	if len(history) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(history))
	}
	if history[0].ID != "1" || history[2].ID != "3" {
		t.Errorf("unexpected history contents: %+v", history)
	}

	b.Add(testItem{ID: "4"})
	history = b.GetHistory()
	if len(history) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(history))
	}
	if history[0].ID != "2" || history[1].ID != "3" || history[2].ID != "4" {
		t.Errorf("unexpected ring buffer ordering: %+v", history)
	}
}

func TestBroadcaster_ConcurrentAddAndCancel(t *testing.T) {
	for i := 0; i < 2000; i++ {
		b := NewBroadcaster[testItem](1, func(item *testItem, seq uint64) {
			item.Seq = seq
		})
		_, cancel := b.Subscribe(1)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); cancel() }()
		go func() { defer wg.Done(); b.Add(testItem{ID: "x"}) }()
		wg.Wait()
	}
}

func TestBroadcaster_MonotonicSeq(t *testing.T) {
	b := NewBroadcaster[testItem](10, func(item *testItem, seq uint64) {
		item.Seq = seq
	})
	b.Add(testItem{ID: "a"})
	b.Add(testItem{ID: "b"})
	b.Add(testItem{ID: "c"})
	history := b.GetHistory()
	for i, entry := range history {
		if entry.Seq != uint64(i+1) {
			t.Fatalf("expected Seq %d, got %d", i+1, entry.Seq)
		}
	}
}

func TestBroadcaster_SlowSubscriberDrop(t *testing.T) {
	b := NewBroadcaster[testItem](10, nil)
	ch, cancel := b.Subscribe(1)
	defer cancel()

	for i := 0; i < 100; i++ {
		b.Add(testItem{ID: "flood"})
	}

	select {
	case item := <-ch:
		if item.ID != "flood" {
			t.Errorf("unexpected item: %+v", item)
		}
	case <-time.After(time.Second):
		t.Fatal("expected at least one item delivered")
	}
}
