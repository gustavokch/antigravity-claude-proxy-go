package headroom

import (
	"regexp"
	"strings"
)

// pytestProgressRe matches progress lines: "test_a.py ..F [100%]", bare
// dot runs "..........", "....  [ 50%]", and "test_b.py  [100%]".
var pytestProgressRe = regexp.MustCompile(`^\S*\.py\s+[.sFxXeE]*\s*\[\s*\d+%\]$|^[.sFxX]+\s*(\[\s*\d+%\])?$`)

func isPytestProgress(line string) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) == 0 {
		return false
	}
	if strings.HasSuffix(trimmed, "%]") && strings.Contains(trimmed, ".py") {
		return true
	}
	if strings.HasSuffix(trimmed, "]") || strings.HasPrefix(trimmed, ".") || strings.HasPrefix(trimmed, "s") || strings.HasPrefix(trimmed, "F") || strings.HasPrefix(trimmed, "x") || strings.HasPrefix(trimmed, "X") {
		return pytestProgressRe.MatchString(line)
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
		if strings.HasPrefix(line, "PASSED ") {
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
		return !unittestDotRe.MatchString(line)
	})
}

// crushRuff dedupes identical violation lines (stable, first occurrence wins).
func crushRuff(text string) (string, bool) {
	return dedupeLines(text)
}
