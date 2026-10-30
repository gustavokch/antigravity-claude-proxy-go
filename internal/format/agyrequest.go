package format

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"antigravity-go-proxy/internal/sessionid"
)

// The streamGenerateContent body as agy 1.4.11 sends it
// (.reference/agy-1.4.11-generation-20261030.txt).

const (
	apiProviderGemini    = "API_PROVIDER_GOOGLE_GEMINI"
	apiProviderAnthropic = "API_PROVIDER_ANTHROPIC_VERTEX"
)

// CloudCodeRequest is the streamGenerateContent envelope. The field order is
// agy's JSON key order.
type CloudCodeRequest struct {
	Project     string          `json:"project"`
	RequestID   string          `json:"requestId"`
	Request     GenerateRequest `json:"request"`
	Model       string          `json:"model"`
	UserAgent   string          `json:"userAgent"`
	RequestType string          `json:"requestType"`
}

// GenerateRequest is the inner request object. It marshals the keys agy sends
// in agy's order, then any other key sorted.
type GenerateRequest map[string]any

// agy sends no toolConfig, so its slot is not known; it sits after tools as in
// the aiplatform GenerateContentRequest message.
var generateRequestKeyOrder = []string{"contents", "systemInstruction", "tools", "toolConfig", "labels", "generationConfig", "sessionId"}

func (request GenerateRequest) MarshalJSON() ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	buffer.WriteByte('{')
	write := func(key string, value any) error {
		if buffer.Len() > 1 {
			buffer.WriteByte(',')
		}
		if err := encoder.Encode(key); err != nil {
			return err
		}
		buffer.Truncate(buffer.Len() - 1) // Encode appends a newline.
		buffer.WriteByte(':')
		if err := encoder.Encode(value); err != nil {
			return fmt.Errorf("marshal request.%s: %w", key, err)
		}
		buffer.Truncate(buffer.Len() - 1)
		return nil
	}
	for _, key := range generateRequestKeyOrder {
		if value, ok := request[key]; ok {
			if err := write(key, value); err != nil {
				return nil, err
			}
		}
	}
	var rest []string
	for key := range request {
		if !slices.Contains(generateRequestKeyOrder, key) {
			rest = append(rest, key)
		}
	}
	slices.Sort(rest)
	for _, key := range rest {
		if err := write(key, request[key]); err != nil {
			return nil, err
		}
	}
	buffer.WriteByte('}')
	return buffer.Bytes(), nil
}

// conversation is agy's identity for one model call: the cascade (agy's
// conversation), its trajectory, and the call's place in it.
type conversation struct {
	cascadeID     string
	trajectoryID  string
	modelCalls    int // model calls before this one: request_id "<trajectory>-<k>"
	lastStepIndex int // trajectory steps before this call, minus one
}

// deriveConversation maps an Anthropic request onto agy's conversation ids.
// The ids are fixed per account, client session and first user message, so
// every turn of one client conversation carries the same cascade and
// trajectory, as agy's do across processes; the counters come from the history.
func deriveConversation(request map[string]any, accountEmail string) conversation {
	messages := asSlice(request["messages"])
	seed := accountEmail + "\x00" + clientSessionID(request) + "\x00" + firstUserText(messages)
	calls, lastStep := countSteps(messages)
	return conversation{
		cascadeID:     derivedUUID("cascade", seed),
		trajectoryID:  derivedUUID("trajectory", seed),
		modelCalls:    calls,
		lastStepIndex: lastStep,
	}
}

// requestID is agy's "agent/<cascade>/<unix ms>/<trajectory>/<step>"; the step
// is the index this call's model response takes.
func (c conversation) requestID(now time.Time) string {
	return "agent/" + c.cascadeID + "/" + strconv.FormatInt(now.UnixMilli(), 10) + "/" + c.trajectoryID + "/" + strconv.Itoa(c.lastStepIndex+1)
}

func clientSessionID(request map[string]any) string {
	metadata := asMap(request["metadata"])
	if id := sessionid.FromValue(metadata["session_id"]); id != "" {
		return id
	}
	return sessionid.FromValue(metadata["user_id"])
}

// firstUserText is the text of the first message, without block metadata:
// clients move cache_control markers between turns, and the text alone stays
// the same for the whole conversation.
func firstUserText(messages []any) string {
	if len(messages) == 0 {
		return ""
	}
	first := asMap(messages[0])
	if text, ok := first["content"].(string); ok {
		return text
	}
	var builder strings.Builder
	for _, rawBlock := range asSlice(first["content"]) {
		block := asMap(rawBlock)
		builder.WriteString(stringValue(block["type"]))
		builder.WriteByte(0)
		builder.WriteString(stringValue(block["text"]))
		builder.WriteByte(0)
	}
	return builder.String()
}

// countSteps returns the model calls and the last step index agy would report
// before the next call. In agy's trajectory a user prompt, a model response
// and a tool result are one step each, and one more step follows every final
// answer before the next prompt: prompts land on steps 0, 3, 6, and a tool call
// in the first turn puts its result on step 2. Each tool_result block counts
// as its own step [inference: agy's captures had one tool call per turn].
func countSteps(messages []any) (calls, lastStepIndex int) {
	steps := 0
	afterAnswer := false
	for _, rawMessage := range messages {
		message := asMap(rawMessage)
		switch stringValue(message["role"]) {
		case "assistant":
			calls++
			steps++
			afterAnswer = !messageHasBlock(message, "tool_use", "functionCall")
		case "user":
			results := 0
			for _, rawBlock := range asSlice(message["content"]) {
				if asMap(rawBlock)["type"] == "tool_result" {
					results++
				}
			}
			if results > 0 {
				steps += results
			} else {
				if afterAnswer {
					steps++
				}
				steps++
			}
			afterAnswer = false
		}
	}
	return calls, max(steps-1, 0)
}

// derivedUUID is a version 4 shaped UUID fixed by kind and seed.
func derivedUUID(kind, seed string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + seed))
	sum[6] = sum[6]&0x0f | 0x40
	sum[8] = sum[8]&0x3f | 0x80
	var encoded [32]byte
	hex.Encode(encoded[:], sum[:16])
	return string(encoded[0:8]) + "-" + string(encoded[8:12]) + "-" + string(encoded[12:16]) + "-" + string(encoded[16:20]) + "-" + string(encoded[20:32])
}

// labels is agy's generation label map. It needs the model's enum and
// provider from the live catalog; without them the map is left out rather
// than sent with values agy would never send.
func (builder *Builder) labels(c conversation, options ModelOptions) map[string]string {
	if options.Enum == "" || options.APIProvider == "" {
		return nil
	}
	claude, nonGemini := builder.Conversations.Mark(c.cascadeID,
		options.APIProvider == apiProviderAnthropic, options.APIProvider != apiProviderGemini)
	return map[string]string{
		"cascade_id":               c.cascadeID,
		"last_step_index":          strconv.Itoa(c.lastStepIndex),
		"model_enum":               options.Enum,
		"request_id":               c.trajectoryID + "-" + strconv.Itoa(c.modelCalls),
		"root_cascade_id":          c.cascadeID,
		"trajectory_id":            c.trajectoryID,
		"used_claude":              strconv.FormatBool(claude),
		"used_claude_conservative": strconv.FormatBool(claude),
		"used_non_gemini_model":    strconv.FormatBool(nonGemini),
	}
}

const (
	conversationTTL        = 24 * time.Hour
	conversationStoreLimit = 8192
)

// ConversationStore keeps the used_claude / used_non_gemini_model labels
// sticky per cascade: agy keeps them "true" for the rest of a conversation
// once it used such a model, also on later Gemini calls.
type ConversationStore struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]conversationFlags
}

type conversationFlags struct {
	claude, nonGemini bool
	seen              time.Time
}

func NewConversationStore() *ConversationStore {
	return &ConversationStore{now: time.Now, entries: make(map[string]conversationFlags)}
}

// Mark records this call's model kind for the cascade and returns the sticky
// values to send.
func (store *ConversationStore) Mark(cascadeID string, claude, nonGemini bool) (bool, bool) {
	if store == nil {
		return claude, nonGemini
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	now := store.now()
	entry, ok := store.entries[cascadeID]
	if ok && now.Sub(entry.seen) > conversationTTL {
		entry = conversationFlags{}
	}
	if !ok && len(store.entries) >= conversationStoreLimit {
		for id, old := range store.entries {
			if now.Sub(old.seen) > conversationTTL {
				delete(store.entries, id)
			}
		}
		if len(store.entries) >= conversationStoreLimit {
			clear(store.entries)
		}
	}
	entry.claude = entry.claude || claude
	entry.nonGemini = entry.nonGemini || nonGemini
	entry.seen = now
	store.entries[cascadeID] = entry
	return entry.claude, entry.nonGemini
}
