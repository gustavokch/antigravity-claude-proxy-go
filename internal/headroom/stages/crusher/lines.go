package crusher

import "strings"

// nextLine returns the first line in s (without \n) and the remainder of s.
func nextLine(s string) (line, rest string) {
	idx := strings.IndexByte(s, '\n')
	if idx >= 0 {
		return s[:idx], s[idx+1:]
	}
	return s, ""
}

// hasCommitLine reports whether head contains a `commit <40-hex>` line.
func hasCommitLine(head string) bool {
	var line string
	for len(head) > 0 {
		line, head = nextLine(head)
		if !strings.HasPrefix(line, "commit ") {
			continue
		}
		hash := strings.TrimSpace(strings.TrimPrefix(line, "commit "))
		if len(hash) == 40 && isLowerHex(hash) {
			return true
		}
	}
	return false
}

// cargoVerbs are the Cargo status-line verbs that crushCargoBuild strips.
// Only the subset distinctive enough to identify Cargo output is listed;
// crushCargoBuild strips a wider set once this signature is chosen.
var cargoVerbs = []string{"Compiling ", "Checking ", "Updating "}

// hasCargoVerbLine reports whether head contains an indented Cargo status
// line. The leading space is required: Cargo always indents these lines, and
// demanding the indent keeps unindented prose from matching.
func hasCargoVerbLine(head string) bool {
	var line string
	for len(head) > 0 {
		line, head = nextLine(head)
		if len(line) == 0 || (line[0] != ' ' && line[0] != '\t') {
			continue
		}
		trimmed := strings.TrimLeft(line, " \t")
		for _, verb := range cargoVerbs {
			if strings.HasPrefix(trimmed, verb) {
				return true
			}
		}
	}
	return false
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// filterLines rebuilds text from the lines keep returns true for, preserving
// the original line endings. Returns (input, false) when nothing changed so
// callers can skip the mutation and byte accounting.
func filterLines(text string, keep func(line string) bool) (string, bool) {
	var b strings.Builder
	b.Grow(len(text))
	changed := false
	start := 0
	for i := 0; i <= len(text); i++ {
		if i < len(text) && text[i] != '\n' {
			continue
		}
		line := text[start:i]
		if keep(line) {
			b.WriteString(line)
			if i < len(text) {
				b.WriteByte('\n')
			}
		} else {
			changed = true
		}
		start = i + 1
	}
	if !changed {
		return text, false
	}
	return b.String(), true
}

// dedupeLines keeps the first occurrence of every line, preserving order.
// Idempotent by construction.
func dedupeLines(text string) (string, bool) {
	seen := make(map[string]struct{})
	return filterLines(text, func(line string) bool {
		if _, dup := seen[line]; dup && strings.TrimSpace(line) != "" {
			return false
		}
		seen[line] = struct{}{}
		return true
	})
}
