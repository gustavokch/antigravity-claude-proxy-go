package headroom

import (
	"regexp"
	"strings"
)

// pytestProgressRe matches bare progress runs such as "..........",
// "....  [ 50%]", or "FF..". Progress lines with file names (e.g.
// "test_a.py ..F [100%]") are caught by the fast-path prefix/suffix checks.
var pytestProgressRe = regexp.MustCompile(`^[.sFxXE]+\s*(\[\s*\d+%\])?$`)

func isPytestProgress(line string) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) == 0 {
		return false
	}
	if strings.HasSuffix(trimmed, "%]") && strings.Contains(trimmed, ".py") {
		return true
	}
	// Short-summary lines start with a status word, not a progress glyph.
	// They can never be progress, so skip the regex entirely.
	if strings.HasPrefix(trimmed, "FAILED ") || strings.HasPrefix(trimmed, "ERROR ") {
		return false
	}
	// A bare "E" is a traceback continuation line (e.g., blank line inside an
	// exception message prefixed by "E   "), not progress.
	if trimmed == "E" {
		return false
	}
	if strings.HasSuffix(trimmed, "]") || strings.HasPrefix(trimmed, ".") || strings.HasPrefix(trimmed, "s") || strings.HasPrefix(trimmed, "F") || strings.HasPrefix(trimmed, "x") || strings.HasPrefix(trimmed, "X") || strings.HasPrefix(trimmed, "E") {
		return pytestProgressRe.MatchString(trimmed)
	}
	return false
}

// crushPytest strips progress/dot lines and PASSED short-summary lines.
// FAILURES/ERRORS sections, tracebacks, E-lines, and banner lines survive
// untouched.
func crushPytest(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		if isPytestProgress(line) {
			return false
		}
		trimmed := strings.TrimLeft(line, " \t\r")
		if strings.HasPrefix(trimmed, "PASSED ") {
			return false
		}
		return true
	})
}

// unittestDotRe matches a pure pass-progress line: dots only, no F/E.
var unittestDotRe = regexp.MustCompile(`^[.s]+$`)

// crushUnittest drops pure-dot progress lines. Lines containing F or E carry
// failure signal and stay, as do tracebacks and the FAILED/OK footer.
func crushUnittest(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		trimmed := strings.TrimSpace(line)
		return !unittestDotRe.MatchString(trimmed)
	})
}

// crushRuff dedupes identical violation lines (stable, first occurrence wins).
func crushRuff(text string) (string, bool) {
	return dedupeLines(text)
}
