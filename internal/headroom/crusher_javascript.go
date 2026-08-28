package headroom

import "strings"

// crushJest strips passing suites ("PASS ") and green checkmark lines. It
// covers vitest, which emits the same ✓/✕ and Tests: summary shapes.
func crushJest(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "✓") || strings.HasPrefix(trimmed, "√") {
			return false
		}
		if strings.HasPrefix(trimmed, "PASS ") {
			return false
		}
		return true
	})
}

// crushMocha strips passing checkmark items; keeps the numbered failure
// items, error frames, and the passing/failing footer.
func crushMocha(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		trimmed := strings.TrimLeft(line, " \t")
		return !strings.HasPrefix(trimmed, "✓") && !strings.HasPrefix(trimmed, "✔")
	})
}

// crushTSC dedupes identical compiler diagnostics (repeating import errors).
func crushTSC(text string) (string, bool) {
	return dedupeLines(text)
}

// crushESLint dedupes identical rule violations; errors, unique warnings, and
// the ✖ summary survive.
func crushESLint(text string) (string, bool) {
	return dedupeLines(text)
}
