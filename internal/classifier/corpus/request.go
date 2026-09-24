package corpus

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// monitorPromptPrefix is the opening of the shared monitor system block,
// duplicated from internal/classifier so this leaf package stays importable
// from the parent without a cycle. Captured verbatim in
// docs/classifier-fallback-notes.md.
const monitorPromptPrefix = "You are a security monitor for autonomous AI coding agents."

// transcriptClose is the literal block that terminates the transcript. The
// entry immediately before it is the action being graded.
const transcriptClose = "</transcript>"

// ErrNoTranscript reports a body whose block layout does not match any
// captured classifier request. Callers fall back rather than guess.
var ErrNoTranscript = errors.New("corpus: request carries no readable transcript")

// Request is everything the corpus keeps from a classifier request body.
type Request struct {
	Action       string
	Context      []string
	SystemSHA256 string
	FooterSHA256 string
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type systemBlock struct {
	Text string `json:"text"`
}

type messageEnvelope struct {
	Content json.RawMessage `json:"content"`
}

type requestEnvelope struct {
	System   []systemBlock     `json:"system"`
	Messages []messageEnvelope `json:"messages"`
}

// ParseRequest reads the action, its preceding context, and the two block
// hashes out of a classifier request body. The system prompt runs to roughly
// 125KB, so it is hashed rather than stored: that is enough to notice
// upstream changing the prompt without paying 125KB per row.
func ParseRequest(rawBody []byte, contextEntries int) (Request, error) {
	var envelope requestEnvelope
	if err := json.Unmarshal(rawBody, &envelope); err != nil {
		return Request{}, err
	}
	if len(envelope.Messages) == 0 {
		return Request{}, ErrNoTranscript
	}

	var blocks []contentBlock
	if err := json.Unmarshal(envelope.Messages[len(envelope.Messages)-1].Content, &blocks); err != nil {
		return Request{}, ErrNoTranscript
	}

	closeIndex := -1
	for i := len(blocks) - 1; i >= 0; i-- {
		if strings.TrimSpace(blocks[i].Text) == transcriptClose {
			closeIndex = i
			break
		}
	}
	// closeIndex must leave room for at least one entry after the opening
	// <transcript> block; index 0 or 1 means the transcript carried nothing.
	if closeIndex < 2 {
		return Request{}, ErrNoTranscript
	}

	actionIndex := closeIndex - 1
	parsed := Request{Action: blocks[actionIndex].Text}

	if contextEntries > 0 {
		start := actionIndex - contextEntries
		// Index 0 is the literal <transcript> block, never a turn.
		if start < 1 {
			start = 1
		}
		for i := start; i < actionIndex; i++ {
			parsed.Context = append(parsed.Context, blocks[i].Text)
		}
	}

	for _, block := range envelope.System {
		if strings.Contains(block.Text, monitorPromptPrefix) {
			parsed.SystemSHA256 = hashText(block.Text)
			break
		}
	}
	if closeIndex+1 < len(blocks) {
		parsed.FooterSHA256 = hashText(blocks[closeIndex+1].Text)
	}

	return parsed, nil
}

// ExtractAction returns just the action and its context. The Laya adapter
// needs no hashes, so it calls this rather than reaching into Request.
func ExtractAction(rawBody []byte, contextEntries int) (string, []string, error) {
	parsed, err := ParseRequest(rawBody, contextEntries)
	if err != nil {
		return "", nil, err
	}
	return parsed.Action, parsed.Context, nil
}

func hashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
