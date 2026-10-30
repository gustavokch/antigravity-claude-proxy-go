package format

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

// AntigravitySystemInstruction is agy's <identity> block, the first five lines
// of its agent system prompt (agy 1.4.11, .reference/agy-1.4.11-generation-20261030.txt).
const AntigravitySystemInstruction = "<identity>\nYou are Antigravity, a powerful agentic AI coding assistant designed by the Google Deepmind team working on Advanced Agentic Coding.\nYou are pair programming with a USER to solve their coding task. The task may require creating a new codebase, modifying or debugging an existing codebase, or simply answering a question.\nThe USER will send you requests, which you must always prioritize addressing. User requests are enclosed within <USER_REQUEST> tags.\n</identity>"

// agySessionID is the sessionId agy sends on every generation call: the
// FNV-1a 64 hash of the empty string as a signed decimal
// (.reference/agy-1.4.11-generation-20261030.txt, section 4).
const agySessionID = "-3750763034362895579"

type Builder struct {
	Cache         *SignatureCache
	Conversations *ConversationStore
	Now           func() time.Time
}

func NewBuilder() *Builder {
	return &Builder{Cache: NewSignatureCache(), Conversations: NewConversationStore(), Now: time.Now}
}

// BuildCloudCodeRequest builds without catalog details, so the request carries
// no labels (see Builder.labels).
func (builder *Builder) BuildCloudCodeRequest(request map[string]any, projectID, accountEmail string) CloudCodeRequest {
	googleRequest := ConvertAnthropicToGoogle(request, builder.Cache)
	return builder.buildCloudCodeRequest(request, googleRequest, projectID, accountEmail, ModelOptions{})
}

func (builder *Builder) BuildCloudCodeRequestWithModel(request map[string]any, projectID, accountEmail string, options ModelOptions) CloudCodeRequest {
	googleRequest := ConvertAnthropicToGoogleWithModel(request, builder.Cache, options)
	return builder.buildCloudCodeRequest(request, googleRequest, projectID, accountEmail, options)
}

func (builder *Builder) buildCloudCodeRequest(request, googleRequest map[string]any, projectID, accountEmail string, options ModelOptions) CloudCodeRequest {
	googleRequest["sessionId"] = agySessionID

	// agy sends its system prompt as one part; the same texts go joined.
	texts := []string{AntigravitySystemInstruction, "Please ignore the following [ignore]" + AntigravitySystemInstruction + "[/ignore]"}
	if instruction := asMap(googleRequest["systemInstruction"]); instruction != nil {
		for _, rawPart := range asSlice(instruction["parts"]) {
			if text := StripBillingHeader(stringValue(asMap(rawPart)["text"])); text != "" {
				texts = append(texts, text)
			}
		}
	}
	googleRequest["systemInstruction"] = map[string]any{"role": "user", "parts": []any{map[string]any{"text": strings.Join(texts, "\n\n")}}}

	// agy always sends the catalog's maxOutputTokens. The client's own limit
	// is kept for the caller to enforce on the stream (OutputGuard).
	outputLimit := 0
	if generation := asMap(googleRequest["generationConfig"]); generation != nil && options.MaxOutputTokens > 0 {
		if requested := intValue(generation["maxOutputTokens"], 0); requested < options.MaxOutputTokens {
			generation["maxOutputTokens"] = options.MaxOutputTokens
			outputLimit = requested
		}
	}

	conversation := deriveConversation(request, accountEmail)
	if labels := builder.labels(conversation, options); labels != nil {
		googleRequest["labels"] = labels
	}
	now := time.Now
	if builder.Now != nil {
		now = builder.Now
	}
	return CloudCodeRequest{
		Project:      projectID,
		RequestID:    conversation.requestID(now()),
		Request:      GenerateRequest(googleRequest),
		Model:        stringValue(request["model"]),
		UserAgent:    "antigravity",
		RequestType:  "agent",
		OutputLimit:  outputLimit,
		conversation: conversation,
	}
}

// billingHeaderToken is the Claude Code client identity marker that leaks into
// the Anthropic system block. Upstream Cloud Code rejects any request whose
// systemInstruction contains this literal token with a disguised
// 429 RESOURCE_EXHAUSTED ("capacity is exhausted for this model"), regardless
// of the header value and regardless of remaining quota. Verified 2026-09-17
// by replaying an intercepted payload: header name present = 429 on every
// account, header name removed = 200 on every account, same minute.
const billingHeaderToken = "x-anthropic-billing-header"

// StripBillingHeader removes the lines that carry billingHeaderToken from a
// client system part and returns the remaining text. A part that holds nothing
// else returns "" and the caller drops it. The Zen translated wires reuse it:
// the line is a per-request client-identity marker no provider should see.
func StripBillingHeader(text string) string {
	if text == "" || !strings.Contains(strings.ToLower(text), billingHeaderToken) {
		return text
	}
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if strings.Contains(strings.ToLower(line), billingHeaderToken) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

var randPool = sync.Pool{
	New: func() any {
		b := make([]byte, 256)
		if _, err := rand.Read(b); err != nil {
			panic(fmt.Sprintf("read cryptographic randomness: %v", err))
		}
		return &randBuffer{data: b, pos: 0}
	},
}

type randBuffer struct {
	data []byte
	pos  int
}

func readRandom(dest []byte) {
	n := len(dest)
	if n > 256 {
		if _, err := rand.Read(dest); err != nil {
			panic(fmt.Sprintf("read cryptographic randomness: %v", err))
		}
		return
	}
	buf := randPool.Get().(*randBuffer)
	if buf.pos+n > len(buf.data) {
		if _, err := rand.Read(buf.data); err != nil {
			panic(fmt.Sprintf("read cryptographic randomness: %v", err))
		}
		buf.pos = 0
	}
	copy(dest, buf.data[buf.pos:buf.pos+n])
	buf.pos += n
	randPool.Put(buf)
}

func randomUUID() string {
	var value [16]byte
	readRandom(value[:])
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

func randomHex(bytes int) string {
	value := make([]byte, bytes)
	readRandom(value)
	return hex.EncodeToString(value)
}
