package format

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DefaultSensitiveWords are client-harness phrases that Cloud Code's content
// rule answers with a false, detail-less 429 RESOURCE_EXHAUSTED when they
// reach systemInstruction: omp's RFC 2119 preamble (omp #11689/#11730,
// .reference/gemini-429-repair-20261111.txt), the Hermes Agent identity
// (CLIProxyAPI #4696) and omp's former system-conventions/system-directive
// tags. The rule scores the whole RFC 2119 line (omp #11730), so its keyword
// enumeration is masked as well as the label.
var DefaultSensitiveWords = []string{
	"RFC 2119",
	"MUST, REQUIRED, SHOULD, RECOMMENDED, MAY, OPTIONAL",
	"Hermes Agent",
	"Nous Research",
	"system-conventions",
	"system_conventions",
	"system-directive",
	"system_directive",
}

// combiningGraphemeJoiner is invisible and keeps each word one grapheme.
// CLIProxyAPI moved Antigravity masking to it after U+200B splits were
// rejected again.
const combiningGraphemeJoiner = "\u034F"

// SystemMask hides configured phrases in client system text before it
// reaches Cloud Code. Port of CLIProxyAPI's Antigravity sensitive-words
// (internal/runtime/executor/helps/cloak_obfuscate.go): matching is
// case-insensitive with the longest phrase first, and every alphanumeric
// subword of a match gets U+034F after its first rune. A nil mask is a no-op.
type SystemMask struct {
	pattern *regexp.Regexp
}

// NewSystemMask returns nil when no phrase can be masked.
func NewSystemMask(words []string) *SystemMask {
	seen := make(map[string]bool, len(words))
	targets := make([]string, 0, len(words))
	for _, raw := range words {
		word := strings.TrimSpace(raw)
		lower := strings.ToLower(word)
		if utf8.RuneCountInString(word) < 2 || strings.Contains(word, combiningGraphemeJoiner) || seen[lower] || maskPhrase(word) == word {
			continue
		}
		seen[lower] = true
		targets = append(targets, word)
	}
	if len(targets) == 0 {
		return nil
	}
	// Go's alternation is leftmost-first, so a longer phrase must precede
	// any phrase it contains.
	sort.SliceStable(targets, func(i, j int) bool {
		ri, rj := utf8.RuneCountInString(targets[i]), utf8.RuneCountInString(targets[j])
		if ri != rj {
			return ri > rj
		}
		return strings.ToLower(targets[i]) < strings.ToLower(targets[j])
	})
	quoted := make([]string, len(targets))
	for i, target := range targets {
		quoted[i] = regexp.QuoteMeta(target)
	}
	return &SystemMask{pattern: regexp.MustCompile("(?i:" + strings.Join(quoted, "|") + ")")}
}

// Apply masks every configured phrase in text.
func (mask *SystemMask) Apply(text string) string {
	if mask == nil || text == "" {
		return text
	}
	return mask.pattern.ReplaceAllStringFunc(text, maskPhrase)
}

func isMaskSubwordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\u034F' || r == '\u200B'
}

// maskSubword reports whether subword now carries a joiner.
func maskSubword(subword string) (string, bool) {
	if strings.Contains(subword, combiningGraphemeJoiner) {
		return subword, true
	}
	_, size := utf8.DecodeRuneInString(subword)
	if size >= len(subword) {
		return subword, false
	}
	return subword[:size] + combiningGraphemeJoiner + subword[size:], true
}

// maskPhrase inserts U+034F after the first rune of every subword of two or
// more runes, keeping punctuation and spaces; a phrase with no such subword
// gets one joiner after its first rune.
func maskPhrase(phrase string) string {
	if phrase == "" || strings.Contains(phrase, combiningGraphemeJoiner) {
		return phrase
	}
	var out strings.Builder
	out.Grow(len(phrase) + 4*len(combiningGraphemeJoiner))
	start := -1
	inserted := false
	flush := func(end int) {
		masked, ok := maskSubword(phrase[start:end])
		out.WriteString(masked)
		inserted = inserted || ok
		start = -1
	}
	for i, r := range phrase {
		if isMaskSubwordRune(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			flush(i)
		}
		out.WriteRune(r)
	}
	if start >= 0 {
		flush(len(phrase))
	}
	if inserted {
		return out.String()
	}
	_, size := utf8.DecodeRuneInString(phrase)
	if size >= len(phrase) {
		return phrase
	}
	return phrase[:size] + combiningGraphemeJoiner + phrase[size:]
}
