package smart

import (
	"fmt"
	"strings"
	"testing"
)

func generateBenchmarkJSON(records int) string {
	var sb strings.Builder
	sb.WriteString("[\n")
	for i := 0; i < records; i++ {
		sb.WriteString(fmt.Sprintf(`  {
    "id": %d,
    "uuid": "550e8400-e29b-41d4-a716-%012d",
    "name": "Customer Account %d",
    "email": "user%d@example.com",
    "balance": 12500.50,
    "is_active": true,
    "tags": ["premium", "enterprise", "cloud"]
  }`, i, i, i, i))
		if i < records-1 {
			sb.WriteString(",\n")
		} else {
			sb.WriteString("\n")
		}
	}
	sb.WriteString("]")
	return sb.String()
}

func BenchmarkSmartCrusher_CompactJSON(b *testing.B) {
	data := generateBenchmarkJSON(100)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		out, changed := CompactJSON(data)
		if !changed || len(out) >= len(data) {
			b.Fatalf("expected compaction")
		}
	}
}

func BenchmarkTabularConversion(b *testing.B) {
	var sb strings.Builder
	sb.WriteString("[\n")
	for i := 0; i < 100; i++ {
		sb.WriteString(fmt.Sprintf(`  {"id": %d, "name": "Item %d", "status": "active", "code": "C-%d"}`, i, i, i))
		if i < 99 {
			sb.WriteString(",\n")
		} else {
			sb.WriteString("\n")
		}
	}
	sb.WriteString("]")
	data := sb.String()

	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		out, changed := TryTabularConversion(data, DefaultMinTabularSavings)
		if !changed || len(out) >= len(data) {
			b.Fatalf("expected tabular conversion")
		}
	}
}
