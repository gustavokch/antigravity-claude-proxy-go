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
// prompt message, not from the number alone. A client that compacts its history
// sends a shorter conversation, so the number goes back down; the digest keeps
// the new prompt from reusing an id an earlier prompt already presented, which
// the real client never does.
//
// A body that does not parse, or has no prompt, is the first prompt. ApplyBody
// reports the parse failure itself.
//
// TurnOf also keeps the content of the latest assistant message, the reply a
// ReplyLog maps to the request id sent as cc_prev_req, and the first text of the
// first prompt, which the cc_version suffix hashes.
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
	var latest, reply json.RawMessage
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
			}
			prompts++
			latest = message.Content
		}
	}
	if prompts == 0 {
		return Turn{PromptNumber: 1, latestReply: reply}
	}
	digest := sha256.Sum256(latest)
	return Turn{PromptNumber: prompts, promptDigest: hex.EncodeToString(digest[:]), firstPrompt: first, latestReply: reply}
}

// promptOf reports whether a user message's content holds something a person
// sent, as opposed to tool output or injected reminders, and returns the first
// text of it.
//
// The text is what the real client hashes into the cc_version suffix: the
// first text block of its first user message that is not injected context. In
// the request body that context arrives as <system-reminder> blocks in the same
// message, so they are skipped. A string content is the text itself.
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
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(content, &blocks) != nil {
			return "", false
		}
		found := false
		for _, block := range blocks {
			switch block.Type {
			case "tool_result":
			case "text":
				if isPromptText(block.Text) {
					if !found {
						text = block.Text
					}
					found = true
				}
			default:
				// An image or a document is something a person attached.
				isPrompt = true
			}
		}
		return text, isPrompt || found
	}
	return "", false
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
