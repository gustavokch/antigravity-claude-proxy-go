package headroom

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

func generateBenchmarkTranscript(numMessages int) map[string]any {
	msgs := make([]any, 0, numMessages)
	for i := 0; i < numMessages; i += 2 {
		tuID := fmt.Sprintf("call_%d", i)
		toolName := "Read"
		if i%4 == 0 {
			toolName = "Glob"
		} else if i%6 == 0 {
			toolName = "Bash"
		}
		msgs = append(msgs, map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{
					"type": "tool_use",
					"id":   tuID,
					"name": toolName,
					"input": map[string]any{
						"file_path": fmt.Sprintf("/path/to/file_%d.go", i),
					},
				},
			},
		})

		var content string
		if toolName == "Read" {
			var sb strings.Builder
			for line := 1; line <= 100; line++ {
				sb.WriteString(fmt.Sprintf("  %d\tfunc line%d() { println(%d) }\n", line, line, line))
			}
			content = sb.String()
		} else if toolName == "Bash" {
			content = generateBenchmarkLog(100)
		} else {
			content = generateBenchmarkJSON(20)
		}

		msgs = append(msgs, map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": tuID,
					"content":     content,
				},
			},
		})
	}
	return map[string]any{
		"model":      "claude-3-5-sonnet",
		"max_tokens": float64(8192),
		"messages":   msgs,
	}
}

func BenchmarkToolInspector_Build(b *testing.B) {
	req := generateBenchmarkTranscript(40)
	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		inspector := NewToolInspector(req)
		if inspector.VerbatimCount() == 0 {
			b.Fatalf("expected verbatim items")
		}
	}
}
