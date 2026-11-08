package tokencount

import (
	"encoding/json"
	"math"
	"unicode/utf8"
)

// measurement is what an estimate is made of: characters that the family's
// ratio turns into tokens, and image tokens that are priced per image.
type measurement struct {
	chars       int
	imageTokens int
}

// tokens converts a measurement at the given chars-per-token ratio.
func (m measurement) tokens(ratio float64) int {
	return int(math.Ceil(float64(m.chars)/ratio)) + m.imageTokens
}

// measureRequest measures a decoded Anthropic Messages body: the system text,
// every message, and each tool definition as compact JSON.
func measureRequest(pricing imagePricing, request map[string]any) measurement {
	var m measurement
	m.addSystem(request["system"])
	messages, _ := request["messages"].([]any)
	m.addMessages(pricing, messages)
	tools, _ := request["tools"].([]any)
	for _, tool := range tools {
		m.chars += jsonChars(tool)
	}
	return m
}

func (m *measurement) addSystem(system any) {
	switch system := system.(type) {
	case string:
		m.chars += utf8.RuneCountInString(system)
	case []any:
		for _, block := range system {
			if block, ok := block.(map[string]any); ok && block["type"] == "text" {
				m.addString(block["text"])
			}
		}
	}
}

func (m *measurement) addMessages(pricing imagePricing, messages []any) {
	for _, message := range messages {
		if message, ok := message.(map[string]any); ok {
			m.addContent(pricing, message["content"])
		}
	}
}

// addContent measures a message's content or a tool_result's content: a
// string, or a list of blocks. Thinking signatures, redacted thinking and
// cache_control markers are not text the model reads as tokens of the
// conversation, so they are left out.
func (m *measurement) addContent(pricing imagePricing, content any) {
	switch content := content.(type) {
	case string:
		m.chars += utf8.RuneCountInString(content)
	case []any:
		for _, block := range content {
			block, ok := block.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "text":
				m.addString(block["text"])
			case "thinking":
				m.addString(block["thinking"])
			case "tool_use":
				if input, ok := block["input"]; ok {
					m.chars += jsonChars(input)
				}
			case "tool_result":
				m.addContent(pricing, block["content"])
			case "image":
				m.imageTokens += pricing.sourceTokens(block["source"])
			}
		}
	}
}

func (m *measurement) addString(value any) {
	if text, ok := value.(string); ok {
		m.chars += utf8.RuneCountInString(text)
	}
}

// runeCounter is an io.Writer that only counts the runes written to it.
type runeCounter int

func (counter *runeCounter) Write(p []byte) (int, error) {
	*counter += runeCounter(utf8.RuneCount(p))
	return len(p), nil
}

// jsonChars is the length of value as compact JSON, counted without building
// the string. HTML escaping is off: with it, every < > & in a tool input full
// of code would count as six characters.
func jsonChars(value any) int {
	var counter runeCounter
	encoder := json.NewEncoder(&counter)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return 0
	}
	return int(counter) - 1 // Encode ends the value with a newline
}
