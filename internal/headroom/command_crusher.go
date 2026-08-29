package headroom

import (
	"context"
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

type CommandCrusherStage struct{}

func (s *CommandCrusherStage) Name() string { return "command_crusher" }

// CrushCommandOutput detects a known tool-output signature and compresses it.
// Pure and deterministic (invariant I1); input returned unchanged when no
// signature matches or the filter finds nothing to strip.
func CrushCommandOutput(text string) (string, bool) {
	switch detectSignature(text) {
	case sigPytest:
		return crushPytest(text)
	case sigUnittest:
		return crushUnittest(text)
	case sigRuff:
		return crushRuff(text)
	case sigJest:
		return crushJest(text)
	case sigMocha:
		return crushMocha(text)
	case sigTSC:
		return crushTSC(text)
	case sigESLint:
		return crushESLint(text)
	case sigGoTest:
		return crushGoTest(text)
	case sigGolangci:
		return crushGolangci(text)
	case sigCargoTest:
		return crushCargoTest(text)
	case sigCargoBuild:
		return crushCargoBuild(text)
	case sigGitStatus:
		return crushGitStatus(text)
	case sigGitLog:
		return crushGitLog(text)
	}
	return text, false
}

func (s *CommandCrusherStage) Execute(ctx context.Context, reqCtx *RequestContext, cfg *Config) error {
	if !cfg.CommandCrusher {
		return nil
	}
	errOrds := ErrorOrdinals(reqCtx.Request)
	// from=0: history included. Position independence keeps the provider
	// prompt cache warm across turns (invariant I1).
	WalkToolResultText(reqCtx.Request, 0, func(_, ord int, get func() string, set func(string)) {
		if errOrds[ord] {
			return // spec §4: is_error payloads pass through unchanged
		}
		if SkipVerbatim(reqCtx, cfg, ord) {
			return
		}
		before := get()
		if after, changed := CrushCommandOutput(before); changed {
			set(after)
			reqCtx.RecordRewrite(before, after)
		}
	})
	return nil
}

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
