package crusher

import "strings"

// crushGitStatus strips git's instructional "(use ..." hints; branch state,
// tracking info, and file lists survive.
func crushGitStatus(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "(use \"") {
			return false
		}
		// The trailing "no changes added to commit" and "nothing added to commit"
		// lines are pure hints.
		if strings.HasPrefix(trimmed, "no changes added to commit") ||
			strings.HasPrefix(trimmed, "nothing added to commit but untracked files present") {
			return false
		}
		return true
	})
}

// crushGitLog strips Date: boilerplate. Commit hashes, authors, subjects, and
// bodies survive (spec: retain author).
func crushGitLog(text string) (string, bool) {
	return filterLines(text, func(line string) bool {
		return !strings.HasPrefix(line, "Date:")
	})
}
