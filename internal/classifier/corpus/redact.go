package corpus

import (
	"path/filepath"
	"strings"
)

// usableHome returns home in the form redaction matches, or "" when
// redacting it would corrupt rows: a home of "/" would turn every path
// separator into "~".
func usableHome(home string) string {
	if home == "" {
		return ""
	}
	home = filepath.Clean(home)
	if home == "/" || home == "." {
		return ""
	}
	return home
}

// redactHome replaces each occurrence of home that stands as a whole path
// prefix with "~". An occurrence counts only when the byte before it cannot
// be part of a path and the byte after it ends the home component, so
// /home/a matches in "cat /home/a/x" and "cd /home/a" but not inside
// /home/abc or /mnt/home/a.
func redactHome(text, home string) string {
	if home == "" || !strings.Contains(text, home) {
		return text
	}
	var builder strings.Builder
	builder.Grow(len(text))
	copied := 0
	for from := 0; from < len(text); {
		index := strings.Index(text[from:], home)
		if index < 0 {
			break
		}
		start := from + index
		end := start + len(home)
		if startsPath(text, start) && endsHome(text, end) {
			builder.WriteString(text[copied:start])
			builder.WriteByte('~')
			copied = end
			from = end
			continue
		}
		from = start + 1
	}
	builder.WriteString(text[copied:])
	return builder.String()
}

// startsPath reports whether a path can begin at start. Rows hold JSON text,
// so a JSON escape such as \n or \t also ends the previous token even though
// its letter is a path byte.
func startsPath(text string, start int) bool {
	if start == 0 || !isPathByte(text[start-1]) {
		return true
	}
	return start >= 2 && text[start-2] == '\\'
}

// endsHome reports whether the home component ends at end.
func endsHome(text string, end int) bool {
	return end == len(text) || text[end] == '/' || !isPathByte(text[end])
}

// isPathByte reports whether b can sit inside a path name. Bytes of
// multi-byte UTF-8 characters count, so a home never matches as the prefix of
// a longer non-ASCII directory name.
func isPathByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '.' || b == '_' || b == '-' || b == '/' || b >= 0x80:
		return true
	}
	return false
}
