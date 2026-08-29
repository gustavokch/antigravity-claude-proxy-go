package crusher

import (
	"fmt"
	"strings"
	"testing"
)

func generatePytestOutput(tests int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("collected %d items\n\n", tests))
	for i := 0; i < tests/10; i++ {
		fmt.Fprintf(&sb, "test_mod_%d.py .......... [%3d%%]\n", i, (i+1)*100/(tests/10))
	}
	sb.WriteString("=== 200 passed in 12.40s ===\n")
	return sb.String()
}

func BenchmarkCommandCrusher_Pytest100KB(b *testing.B) {
	data := generatePytestOutput(10000) // ~100KB
	for len(data) < 100*1024 {
		data += data[:len(data)/2]
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, changed := CrushCommandOutput(data)
		if !changed || len(out) >= len(data) {
			b.Fatal("expected compression")
		}
	}
}

func BenchmarkCommandCrusher_Fallback100KB(b *testing.B) {
	data := strings.Repeat("just an ordinary log line with no signature\n", 2400) // ~100KB
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, changed := CrushCommandOutput(data); changed {
			b.Fatal("unexpected change")
		}
	}
}
