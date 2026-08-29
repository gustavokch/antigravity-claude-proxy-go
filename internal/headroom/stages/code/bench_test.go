package code

import (
	"fmt"
	"strings"
	"testing"
)

func generateBenchmarkLog(lines int) string {
	var sb strings.Builder
	sb.WriteString("=== Build started ===\n")
	for i := 0; i < lines; i++ {
		if i >= 10 && i < 10+lines/2 {
			sb.WriteString("  [downloading] package dependencies in progress...\n")
		} else {
			sb.WriteString(fmt.Sprintf("Step %d: processing target module %d   \n\n\n", i, i))
		}
	}
	sb.WriteString("=== Build finished successfully ===\n")
	return sb.String()
}

func BenchmarkCodeCompressor_PruneText(b *testing.B) {
	data := generateBenchmarkLog(500)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		out := PruneText(data)
		if len(out) >= len(data) {
			b.Fatalf("expected pruning")
		}
	}
}
