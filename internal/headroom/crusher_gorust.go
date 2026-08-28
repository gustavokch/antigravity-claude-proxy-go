package headroom

import "strings"

// crushGoTest strips verbose-run scaffolding: "=== RUN" and "--- PASS:" /
// "    --- PASS:" lines. Failures, panics, and package summaries stay.
func crushGoTest(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		if strings.HasPrefix(line, "=== RUN") {
			return false
		}
		trimmed := strings.TrimLeft(line, " \t")
		return !strings.HasPrefix(trimmed, "--- PASS:")
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
		return !(strings.HasPrefix(line, "test ") && strings.HasSuffix(line, " ... ok"))
	})
}

// crushCargoBuild strips crate compilation noise; warning:/error: diagnostic
// blocks and the Finished line survive. Covers cargo clippy, which shares the
// Compiling/Checking/Updating line shapes.
func crushCargoBuild(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		for _, prefix := range []string{"   Compiling ", "   Downloaded ", "  Downloading ", "    Updating ", "   Checking ", "     Locking "} {
			if strings.HasPrefix(line, prefix) {
				return false
			}
		}
		return true
	})
}
