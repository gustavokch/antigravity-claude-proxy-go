package headroom

import "strings"

// crushGoTest strips verbose-run scaffolding: "=== RUN", "=== PAUSE",
// "=== CONT", and "--- PASS:" / "    --- PASS:" lines. Failures, panics, and
// package summaries stay.
func crushGoTest(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		trimmed := strings.TrimLeft(line, " \t\r")
		if strings.HasPrefix(trimmed, "=== RUN") ||
			strings.HasPrefix(trimmed, "=== PAUSE") ||
			strings.HasPrefix(trimmed, "=== CONT") ||
			strings.HasPrefix(trimmed, "--- PASS:") {
			return false
		}
		return true
	})
}

// crushGolangci dedupes identical diagnostics.
func crushGolangci(text string) (string, bool) {
	return dedupeLines(text)
}

// crushCargoTest strips "test <name> ... ok" lines. FAILED lines, panics, the
// failures: list, and the test result: footer stay.
func crushCargoTest(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		trimmed := strings.TrimRight(line, "\r")
		return !(strings.HasPrefix(trimmed, "test ") && strings.HasSuffix(trimmed, " ... ok"))
	})
}

// crushCargoBuild strips crate compilation noise; warning:/error: diagnostic
// blocks and the Finished line survive. Covers cargo clippy, which shares the
// Compiling/Checking/Updating line shapes.
func crushCargoBuild(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		trimmed := strings.TrimLeft(strings.TrimRight(line, "\r"), " ")
		for _, prefix := range []string{"Compiling ", "Downloaded ", "Downloading ", "Updating ", "Checking ", "Locking ", "Fresh "} {
			if strings.HasPrefix(trimmed, prefix) {
				return false
			}
		}
		return true
	})
}
