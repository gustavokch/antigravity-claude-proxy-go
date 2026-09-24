// Package corpus captures Claude Code classifier requests together with the
// verdict that was actually returned for them, as append-only JSONL on the
// operator's own machine. The rows are the labelled data a local decision
// model is fine-tuned on; see
// docs/superpowers/specs/2026-09-23-classifier-corpus-and-laya-design.md.
package corpus

import (
	"regexp"
	"strconv"
	"strings"
)

// Verdict is a parsed classifier answer. Severity is -1 when the response
// carried no parseable <severity> tag: an unreadable answer is itself a
// finding and must be recorded rather than dropped.
type Verdict struct {
	Raw      string `json:"verdict_raw"`
	Severity int    `json:"severity"`
	Category string `json:"category"`
	Thinking string `json:"thinking"`
}

// (?s) lets . match newlines: stage 2 <thinking> blocks are multi-line.
var (
	severityPattern = regexp.MustCompile(`(?s)<severity>\s*(-?\d+)\s*</severity>`)
	// unclosedSeverityPattern recovers the severity when the teacher stopped
	// after the digits (`<severity>10` with no closing tag), which gateway
	// models do at their token limit. Consulted only when severityPattern
	// finds nothing, so a well-formed tag always wins.
	unclosedSeverityPattern = regexp.MustCompile(`<severity>\s*(-?\d+)`)
	categoryPattern         = regexp.MustCompile(`(?s)<category>(.*?)</category>`)
	thinkingPattern         = regexp.MustCompile(`(?s)<thinking>(.*?)</thinking>`)
)

// ParseVerdict extracts the typed fields from a verdict string. It never
// fails: an unrecognized shape yields Severity -1 with Raw preserved.
//
// Severity and category are read with every <thinking> span removed. A
// rationale can quote a tag, and the first quoted tag would otherwise win over
// the final verdict that follows the rationale.
func ParseVerdict(text string) Verdict {
	verdict := Verdict{Raw: text, Severity: -1}
	answer := thinkingPattern.ReplaceAllString(text, "")
	if match := severityPattern.FindStringSubmatch(answer); match != nil {
		if value, err := strconv.Atoi(match[1]); err == nil {
			verdict.Severity = value
		}
	} else if match := unclosedSeverityPattern.FindStringSubmatch(answer); match != nil {
		if value, err := strconv.Atoi(match[1]); err == nil {
			verdict.Severity = value
		}
	}
	if match := categoryPattern.FindStringSubmatch(answer); match != nil {
		verdict.Category = strings.TrimSpace(match[1])
	}
	if match := thinkingPattern.FindStringSubmatch(text); match != nil {
		verdict.Thinking = strings.TrimSpace(match[1])
	}
	return verdict
}
