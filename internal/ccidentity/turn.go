package ccidentity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// systemReminderPrefix opens the text blocks Claude Code appends to a user
// message to carry context rather than anything the user typed.
const systemReminderPrefix = "<system-reminder>"

// TurnOf reads which user prompt a /v1/messages body belongs to, so the prompt
// fields on the wire move the way the real client's do.
//
// The real client mints a new cc_prompt_id and advances cc_turn_index once per
// user prompt, and repeats both across that prompt's tool follow-up requests
// (.reference/claude-code-headers-20261020-multiprompt.jsonl: three prompts in
// one process gave turn indexes 1, 2, 3, and the third prompt's follow-up
// repeated 3 and its id). The body already says where a request sits in that
// sequence, so no caller has to keep a counter: it is the number of user
// messages that carry a prompt, and it is the same for every request that
// replays the same conversation. A retry, a cache-bump replay and a failover
// attempt therefore present the identity of the request they repeat.
//
// A user message is a prompt unless every block in it is a tool_result or a
// text block that is empty or a <system-reminder>. Those are the messages a
// client sends to feed a tool's output back, and Claude Code adds reminder
// blocks to them. A tool_result alongside real text still counts: a person
// typed it.
//
// The prompt id is derived from the number AND from a digest of the latest
// prompt message (see digestPrompt), not from the number alone. A client that
// compacts its history sends a shorter conversation, so the number goes back
// down; the digest keeps the new prompt from reusing an id an earlier prompt
// already presented, which the real client never does.
//
// A body that does not parse, or has no prompt, is the first prompt. ApplyBody
// reports the parse failure itself.
//
// TurnOf also keeps the content of the latest assistant message, the reply a
// ReplyLog maps to the request id sent as cc_prev_req, the first text of the
// first prompt, which the cc_version suffix hashes, and a digest of the whole
// first prompt, which names the conversation to ReplyScope.
func TurnOf(body []byte) Turn {
	var parsed struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return Turn{PromptNumber: 1}
	}

	prompts := 0
	var latest, firstContent, reply json.RawMessage
	var first string
	for _, message := range parsed.Messages {
		switch {
		case message.Role == "assistant":
			reply = message.Content
		case message.Role == "user":
			text, isPrompt := promptOf(message.Content)
			if !isPrompt {
				continue
			}
			if prompts == 0 {
				first = text
				firstContent = message.Content
			}
			prompts++
			latest = message.Content
		}
	}
	if prompts == 0 {
		return Turn{PromptNumber: 1, latestReply: reply}
	}
	turn := Turn{PromptNumber: prompts, promptDigest: digestPrompt(latest), firstPrompt: first, latestReply: reply}
	turn.firstDigest = turn.promptDigest
	if prompts > 1 {
		turn.firstDigest = digestPrompt(firstContent)
	}
	return turn
}

// digestPrompt hashes a prompt message's content in a form that does not change
// when the client re-encodes the same prompt. The hash seeds cc_prompt_id, which
// the real client repeats across a prompt's tool follow-ups, so everything a
// client may vary between those requests has to drop out of it:
//
//   - cache_control, at any depth. A client that moves its cache breakpoint to
//     the latest message sends the prompt with the marker on its own request and
//     without it on the follow-up.
//   - Injected context. A client that adds a <system-reminder> block to the
//     latest message for one request does not repeat it once the message is
//     older, so the empty and <system-reminder> text blocks that promptOf does
//     not count as typed are left out, as they are when counting prompts.
//   - Object key order and whitespace, by decoding and encoding again.
//   - Whether the text is a string or a text block. A string content is the one
//     text block that holds it.
//
// Numbers keep their literal, so two values a float64 cannot tell apart stay
// apart. Images, documents, tool results and the text a person typed all stay.
func digestPrompt(content json.RawMessage) string {
	canonical := canonicalPrompt(content)
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

// canonicalPrompt returns the form digestPrompt hashes. Content that does not
// decode is returned as it came: promptOf has accepted everything that reaches
// here, so this keeps the function total rather than covering a known case.
func canonicalPrompt(content json.RawMessage) []byte {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return content
	}
	if text, ok := value.(string); ok {
		value = []any{map[string]any{"type": "text", "text": text}}
	}
	if blocks, ok := value.([]any); ok {
		value = withoutInjectedText(blocks)
	}
	dropCacheControl(value)
	canonical, err := json.Marshal(value)
	if err != nil {
		return content
	}
	return canonical
}

// withoutInjectedText returns blocks without the text blocks promptOf does not
// count as typed: empty text and <system-reminder> blocks. Everything else
// stays, including text in a tool_result and every attachment.
func withoutInjectedText(blocks []any) []any {
	typed := make([]any, 0, len(blocks))
	for _, block := range blocks {
		if object, ok := block.(map[string]any); ok && object["type"] == "text" {
			if text, _ := object["text"].(string); !isPromptText(text) {
				continue
			}
		}
		typed = append(typed, block)
	}
	return typed
}

// dropCacheControl deletes every cache_control member under value.
func dropCacheControl(value any) {
	switch v := value.(type) {
	case map[string]any:
		delete(v, "cache_control")
		for _, member := range v {
			dropCacheControl(member)
		}
	case []any:
		for _, element := range v {
			dropCacheControl(element)
		}
	}
}

// promptOf reports whether a user message's content holds something a person
// sent, as opposed to tool output or injected reminders, and returns the first
// text of it.
//
// The text is what the real client hashes into the cc_version suffix: the
// first text block of its first user message that is not injected context. In
// the request body that context arrives as <system-reminder> blocks in the same
// message, so they are skipped. A string content is the text itself.
//
// A list of blocks is decoded into each block's type and text alone, so a
// tool_result's output and an image's data are scanned but never decoded:
// TurnOf runs on every request, and those are most of a long conversation.
func promptOf(content json.RawMessage) (text string, isPrompt bool) {
	content = bytes.TrimSpace(content)
	if len(content) == 0 {
		return "", false
	}
	switch content[0] {
	case '"':
		if json.Unmarshal(content, &text) != nil {
			return "", false
		}
		return text, isPromptText(text)
	case '[':
		var blocks []struct {
			Type any `json:"type"`
			Text any `json:"text"`
		}
		if json.Unmarshal(content, &blocks) != nil {
			return "", false
		}
		var rule promptRule
		for _, block := range blocks {
			if !rule.add(block.Type, block.Text) {
				return "", false
			}
		}
		return rule.result()
	}
	return "", false
}

// HasPromptContent reports whether a user message's decoded content (a string
// or a []any of blocks) holds something a person sent: the rule promptOf
// applies. Text that is empty or starts with <system-reminder> and tool
// results do not count; an image or a document does.
func HasPromptContent(content any) bool {
	_, isPrompt := promptContent(content)
	return isPrompt
}

// promptContent is the rule behind HasPromptContent, over decoded content. A
// block that is no object makes the content no prompt, as a typed decode of it
// would fail; a null block is one with neither type nor text.
func promptContent(content any) (text string, isPrompt bool) {
	switch content := content.(type) {
	case string:
		return content, isPromptText(content)
	case []any:
		var rule promptRule
		for _, raw := range content {
			var block map[string]any
			if raw != nil {
				var ok bool
				if block, ok = raw.(map[string]any); !ok {
					return "", false
				}
			}
			if !rule.add(block["type"], block["text"]) {
				return "", false
			}
		}
		return rule.result()
	}
	return "", false
}

// promptRule is the per-block rule promptOf and promptContent share, applied
// to a message's blocks in order.
type promptRule struct {
	text     string // the first typed text
	found    bool   // a text block holds typed text
	attached bool   // a block is neither text nor a tool_result
}

// add applies the rule to one block's type and text members, each as decoded
// into an any: nil when absent or null. It reports false when either is
// present and not a string, which makes the whole content no prompt.
func (r *promptRule) add(kind, text any) bool {
	kindString, kindOK := stringOrNull(kind)
	textString, textOK := stringOrNull(text)
	if !kindOK || !textOK {
		return false
	}
	switch kindString {
	case "tool_result":
	case "text":
		if isPromptText(textString) {
			if !r.found {
				r.text = textString
			}
			r.found = true
		}
	default:
		// An image or a document is something a person attached.
		r.attached = true
	}
	return true
}

func (r promptRule) result() (text string, isPrompt bool) {
	return r.text, r.found || r.attached
}

// stringOrNull reads a decoded member as a string; nil reads as "", a value of
// another type as not ok.
func stringOrNull(value any) (string, bool) {
	if value == nil {
		return "", true
	}
	text, ok := value.(string)
	return text, ok
}

func isPromptText(text string) bool {
	text = strings.TrimSpace(text)
	return text != "" && !strings.HasPrefix(text, systemReminderPrefix)
}

// promptNumber is PromptNumber with the zero Turn read as the first prompt.
func (t Turn) promptNumber() int {
	if t.PromptNumber < 1 {
		return 1
	}
	return t.PromptNumber
}
