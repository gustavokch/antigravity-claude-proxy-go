package smart

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// DefaultMinTabularSavings is the minimum savings threshold (30%) required to convert
// uniform JSON object arrays into Markdown pipe tables.
const DefaultMinTabularSavings = 0.30

// TryTabularConversion attempts to convert a top-level JSON array of uniform scalar
// objects into a Markdown pipe-delimited table if the byte savings exceed minSavingsPct.
// It returns (convertedTable, true) on success, or (originalInput, false) if the input
// is not suitable, non-uniform, non-scalar, or fails the savings threshold.
func TryTabularConversion(input string, minSavingsPct float64) (string, bool) {
	trimmed := strings.TrimSpace(input)
	if !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		return input, false
	}

	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()

	var rawArray []any
	if err := decoder.Decode(&rawArray); err != nil {
		return input, false
	}

	if len(rawArray) < 3 {
		return input, false
	}

	firstObj, ok := rawArray[0].(map[string]any)
	if !ok || len(firstObj) == 0 {
		return input, false
	}

	// Extract sorted keys for deterministic column ordering (invariant I1)
	keys := make([]string, 0, len(firstObj))
	for k := range firstObj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Validate uniformity and scalar types across all rows
	for _, rowRaw := range rawArray {
		rowObj, ok := rowRaw.(map[string]any)
		if !ok || len(rowObj) != len(keys) {
			return input, false
		}
		for _, k := range keys {
			val, exists := rowObj[k]
			if !exists {
				return input, false
			}
			if !isScalar(val) {
				return input, false
			}
		}
	}

	// Build markdown table
	var buf bytes.Buffer

	// Header row
	buf.WriteString("| ")
	for i, k := range keys {
		buf.WriteString(escapeCell(k))
		if i < len(keys)-1 {
			buf.WriteString(" | ")
		}
	}
	buf.WriteString(" |\n")

	// Separator row
	buf.WriteString("| ")
	for i := range keys {
		buf.WriteString("---")
		if i < len(keys)-1 {
			buf.WriteString(" | ")
		}
	}
	buf.WriteString(" |\n")

	// Data rows
	for _, rowRaw := range rawArray {
		rowObj := rowRaw.(map[string]any)
		buf.WriteString("| ")
		for i, k := range keys {
			val := rowObj[k]
			buf.WriteString(formatCell(val))
			if i < len(keys)-1 {
				buf.WriteString(" | ")
			}
		}
		buf.WriteString(" |\n")
	}

	result := strings.TrimRight(buf.String(), "\n")
	savings := float64(len(input)-len(result)) / float64(len(input))
	if savings < minSavingsPct {
		return input, false
	}

	return result, true
}

func isScalar(val any) bool {
	if val == nil {
		return true
	}
	switch val.(type) {
	case string, bool, json.Number, float64, int, int64, float32:
		return true
	default:
		return false
	}
}

func formatCell(val any) string {
	if val == nil {
		return "null"
	}
	switch v := val.(type) {
	case string:
		return escapeCell(v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	case json.Number:
		return v.String()
	default:
		return escapeCell(fmt.Sprintf("%v", v))
	}
}

func escapeCell(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\r\n", "\\n")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "")
	return s
}
