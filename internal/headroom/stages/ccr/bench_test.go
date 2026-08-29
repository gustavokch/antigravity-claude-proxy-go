package ccr

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkCCRStore_PutGet(b *testing.B) {
	store := NewCCRStore(64)
	payloads := make([]string, 100)
	for i := 0; i < 100; i++ {
		payloads[i] = fmt.Sprintf("chunk payload data for item %d: %s", i, strings.Repeat("content ", 50))
	}

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		idx := 0
		for pb.Next() {
			p := payloads[idx%len(payloads)]
			id, ok := store.Put(p)
			if ok {
				_, _ = store.Get(id)
			}
			idx++
		}
	})
}
