package crusher

import (
	"regexp"
	"strings"
)

var (
	// pytestFooterRe matches the pytest summary banner: "=== 2 passed in 0.01s ===".
	pytestFooterRe = regexp.MustCompile(`=+ .* in [\d.]+s =+`)
	// eslintLineRe matches "  12:5  error  'x' is unused  no-unused-vars".
	eslintLineRe = regexp.MustCompile(`(?m)^\s*\d+:\d+\s+(error|warning)\s+\S.*\s[a-z@/][a-z0-9@/\-]*$`)
	// golangciLineRe matches "main.go:12:3: message (lintername)".
	golangciLineRe = regexp.MustCompile(`(?m)^[\w./\-]+\.go:\d+:\d+: .+ \([a-z][a-z0-9\-]*\)$`)
	// ruffLineRe matches "app.py:4:1: E402 module level import not at top".
	ruffLineRe = regexp.MustCompile(`(?m)^\S+\.py:\d+:\d+: [A-Z]+\d+ `)
)

// signature identifies a tool-output format. Detection order in
// detectSignature is most-specific first.
type signature int

const (
	sigNone signature = iota
	sigGitStatus
	sigGitLog
	sigCargoTest
	sigGoTest
	sigGolangci
	sigPytest
	sigUnittest
	sigJest
	sigMocha
	sigTSC
	sigESLint
	sigRuff
	sigCargoBuild
)

func (s signature) String() string {
	switch s {
	case sigNone:
		return "none"
	case sigGitStatus:
		return "git_status"
	case sigGitLog:
		return "git_log"
	case sigCargoTest:
		return "cargo_test"
	case sigGoTest:
		return "go_test"
	case sigGolangci:
		return "golangci"
	case sigPytest:
		return "pytest"
	case sigUnittest:
		return "unittest"
	case sigJest:
		return "jest"
	case sigMocha:
		return "mocha"
	case sigTSC:
		return "tsc"
	case sigESLint:
		return "eslint"
	case sigRuff:
		return "ruff"
	case sigCargoBuild:
		return "cargo_build"
	default:
		return "unknown"
	}
}

// signatureScanCap bounds detection to the payload prefix; signatures are
// decided by header/footer markers visible early (or a capped full scan for
// footer markers — detectSignature may scan the whole text with Contains,
// which is SIMD-fast, but never with regex).
const signatureScanCap = 4096

func detectSignature(text string) signature {
	head := text
	if len(head) > signatureScanCap {
		head = head[:signatureScanCap]
	}
	tail := text
	if len(tail) > signatureScanCap {
		tail = tail[len(tail)-signatureScanCap:]
	}
	switch {
	case strings.HasPrefix(head, "On branch ") || strings.Contains(head, "\nOn branch "):
		return sigGitStatus
	case hasCommitLine(head):
		return sigGitLog
	case strings.Contains(tail, "test result:"):
		return sigCargoTest
	case strings.Contains(head, "=== RUN") || strings.Contains(head, "--- FAIL:") ||
		strings.Contains(head, "--- PASS:") || strings.HasPrefix(head, "ok  \t") || strings.HasPrefix(head, "FAIL\t") ||
		strings.Contains(tail, "\nok  \t") || strings.Contains(tail, "\nFAIL\t"):
		return sigGoTest
	case golangciLineRe.MatchString(head):
		return sigGolangci
	case (strings.Contains(head, "collected ") && strings.Contains(head, " items")) ||
		pytestFooterRe.MatchString(tail) || strings.Contains(tail, "short test summary info") || strings.Contains(head, "short test summary info"):
		return sigPytest
	case strings.Contains(tail, "FAILED (") || (strings.Contains(tail, "\nRan ") && strings.Contains(tail, " tests")):
		return sigUnittest
	case strings.Contains(head, "PASS ") || strings.Contains(head, "FAIL ") || strings.Contains(tail, "Tests: "):
		return sigJest
	case strings.Contains(tail, " passing") &&
		(strings.Contains(head, "✓") || strings.Contains(head, "✔") || strings.Contains(head, "√")):
		return sigMocha
	case strings.Contains(head, "error TS"):
		return sigTSC
	case eslintLineRe.MatchString(head):
		return sigESLint
	case ruffLineRe.MatchString(head):
		return sigRuff
	case hasCargoVerbLine(head):
		return sigCargoBuild
	}
	return sigNone
}
