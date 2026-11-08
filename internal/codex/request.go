package codex

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// BuildOptions are the per-request inputs of BuildRequest that do not come
// from the client's body.
type BuildOptions struct {
	Model          string      // upstream model id
	Entry          ModelConfig // effort clamping
	PromptCacheKey string
}

// requestBody is the Codex Responses request. A struct rather than a map keeps
// the field order of the official client's request and makes the fields that
// must never be sent (temperature, top_p, metadata, previous_response_id, …)
// impossible to send.
type requestBody struct {
	Model             string        `json:"model"`
	Instructions      string        `json:"instructions"`
	Input             []any         `json:"input"`
	Tools             []any         `json:"tools,omitempty"`
	ToolChoice        any           `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool         `json:"parallel_tool_calls,omitempty"`
	Reasoning         *reasoning    `json:"reasoning,omitempty"`
	Store             bool          `json:"store"`
	Stream            bool          `json:"stream"`
	Include           []string      `json:"include"`
	ServiceTier       string        `json:"service_tier,omitempty"`
	PromptCacheKey    string        `json:"prompt_cache_key,omitempty"`
	Text              *textControls `json:"text,omitempty"`
	MaxOutputTokens   json.Number   `json:"max_output_tokens,omitempty"`
}

type reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type textControls struct {
	Format jsonSchemaFormat `json:"format"`
}

type jsonSchemaFormat struct {
	Type   string `json:"type"`
	Name   string `json:"name"`
	Strict bool   `json:"strict"`
	Schema any    `json:"schema"`
}

type messageItem struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type imagePart struct {
	Type     string `json:"type"`
	ImageURL string `json:"image_url"`
}

type filePart struct {
	Type     string `json:"type"`
	FileData string `json:"file_data"`
	Filename string `json:"filename"`
}

type functionCallItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type functionCallOutputItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output any    `json:"output"` // string, or a list of input_text/input_image parts
}

type reasoningItem struct {
	Type             string `json:"type"`
	Summary          []any  `json:"summary"`
	Content          any    `json:"content"` // always null: the thinking text is not replayed
	EncryptedContent string `json:"encrypted_content"`
}

type functionTool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters"`
	Strict      bool   `json:"strict"`
}

type webSearchTool struct {
	Type         string            `json:"type"`
	Filters      *webSearchFilters `json:"filters,omitempty"`
	UserLocation any               `json:"user_location,omitempty"`
}

type webSearchFilters struct {
	AllowedDomains []any `json:"allowed_domains"`
}

// minOutputTokens is the smallest integer max_output_tokens the backend
// accepts; smaller ones are rejected with invalid_request_error.
const minOutputTokens = 16

// structuredOutputName names a json_schema text format whose client gave it
// no name; the backend requires one.
const structuredOutputName = "structured_output"

// anthropicEfforts are the output_config.effort values passed through as the
// requested reasoning level.
var anthropicEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

// BuildRequest converts an Anthropic Messages body into a Codex Responses
// body. It is a port of CLIProxyAPI's Claude→Codex translator plus the body
// rules its Codex executor applies before sending. toolNames maps the upstream
// names of renamed tools back to the client's names, nil when none was
// renamed.
//
// An error means the client's request cannot be sent: the body is not a JSON
// object, or a user turn's only content was an attachment Codex cannot take
// (sending the turn without it would answer a question the user did not ask).
func BuildRequest(anthropicBody []byte, opts BuildOptions) (body []byte, toolNames map[string]string, err error) {
	dec := json.NewDecoder(bytes.NewReader(anthropicBody))
	dec.UseNumber()
	var req map[string]any
	if err := dec.Decode(&req); err != nil {
		return nil, nil, fmt.Errorf("codex: decode request body: %w", err)
	}
	if req == nil {
		return nil, nil, errors.New("codex: request body is not a JSON object")
	}

	clientTools, _ := req["tools"].([]any)
	names := shortNames(clientTools)

	b := inputBuilder{names: names, items: make([]any, 0, len(asSlice(req["messages"]))+1)}
	if texts := systemTexts(req["system"]); len(texts) > 0 {
		parts := make([]any, 0, len(texts))
		for _, t := range texts {
			parts = append(parts, textPart{Type: "input_text", Text: t})
		}
		b.items = append(b.items, messageItem{Type: "message", Role: "developer", Content: parts})
	}
	for i, raw := range asSlice(req["messages"]) {
		if msg, ok := raw.(map[string]any); ok {
			b.message(i, msg)
		}
	}
	b.items = append(b.items, b.pendingReminders...)
	if b.unsupported != "" {
		return nil, nil, fmt.Errorf("codex: messages[%d]: %s content is not supported by the Codex backend", b.unsupportedAt, b.unsupported)
	}

	out := requestBody{
		Model:          opts.Model,
		Input:          b.items,
		Store:          false,
		Stream:         true,
		Include:        []string{"reasoning.encrypted_content"},
		ServiceTier:    serviceTier(req),
		PromptCacheKey: opts.PromptCacheKey,
		Text:           textFormat(req),
	}

	tools, webSearchNames := convertTools(clientTools, names)
	if len(tools) > 0 {
		out.Tools = tools
		out.ToolChoice = toolChoice(req["tool_choice"], names, webSearchNames)
		parallel := true
		if tc, ok := req["tool_choice"].(map[string]any); ok {
			if disable, ok := tc["disable_parallel_tool_use"].(bool); ok {
				parallel = !disable
			}
		}
		out.ParallelToolCalls = &parallel
	}
	if hasWebSearchTool(clientTools) {
		out.Include = append(out.Include, "web_search_call.action.sources")
	}

	if effort, summary := reasoningSettings(req, opts.Entry); effort != "" || summary != "" {
		out.Reasoning = &reasoning{Effort: effort, Summary: summary}
	}
	out.MaxOutputTokens = outputBudget(req)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, nil, fmt.Errorf("codex: encode request body: %w", err)
	}

	for client, upstream := range names {
		if client != upstream {
			if toolNames == nil {
				toolNames = make(map[string]string)
			}
			toolNames[upstream] = client
		}
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), toolNames, nil
}

// PromptCacheKey derives the Codex prompt_cache_key for a Claude Code session
// and agent: a UUIDv5 in the OID namespace, so one session keeps one key across
// restarts and two agents of a session (the main loop and a subagent) do not
// share a cache line. It is empty without a session.
func PromptCacheKey(model, sessionID, agentID string) string {
	if sessionID == "" {
		return ""
	}
	return uuidV5(namespaceOID, "antigravity-proxy:codex:claude-code\x00"+model+"\x00"+sessionID+":agent:"+agentID)
}

// namespaceOID is RFC 4122's name space for ISO OIDs.
var namespaceOID = [16]byte{0x6b, 0xa7, 0xb8, 0x12, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}

func uuidV5(namespace [16]byte, name string) string {
	h := sha1.New()
	h.Write(namespace[:])
	h.Write([]byte(name))
	var u [16]byte
	copy(u[:], h.Sum(nil))
	u[6] = u[6]&0x0f | 0x50
	u[8] = u[8]&0x3f | 0x80
	x := hex.EncodeToString(u[:])
	return x[0:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}

// inputBuilder turns Anthropic messages into Responses input items.
type inputBuilder struct {
	items []any
	names map[string]string // client tool name -> upstream name

	// pendingToolUseIDs are the tool_use ids of the last assistant turn,
	// used to order the tool results of the turn that answers it.
	pendingToolUseIDs []string
	// pendingReminders are role "system" messages that arrived between a
	// tool call and its result. They wait until the results are out, so the
	// function_call_output items follow their calls.
	pendingReminders []any

	// unsupported is the first attachment type that left a user turn with
	// nothing to send, and unsupportedAt that turn's index.
	unsupported   string
	unsupportedAt int
}

func (b *inputBuilder) message(index int, msg map[string]any) {
	role, _ := msg["role"].(string)
	if role == "system" {
		// Claude Code sends its environment block as a trailing system
		// message. Responses has no mid-conversation system role, so it goes
		// in as a user message in the reminder envelope the model knows.
		if text, ok := systemReminder(msg["content"]); ok {
			item := messageItem{Type: "message", Role: "user", Content: []any{textPart{Type: "input_text", Text: text}}}
			if len(b.pendingToolUseIDs) > 0 {
				b.pendingReminders = append(b.pendingReminders, item)
			} else {
				b.items = append(b.items, item)
			}
		}
		return
	}
	if role != "assistant" {
		role = "user"
	}

	pending := b.pendingToolUseIDs
	b.pendingToolUseIDs = nil
	t := turn{b: b, role: role}
	switch content := msg["content"].(type) {
	case string:
		t.text(content)
		t.flush()
		b.flushReminders()
	case []any:
		blocks := content
		if role == "user" {
			blocks = alignToolResults(blocks, pending)
		}
		for _, raw := range blocks {
			if block, ok := raw.(map[string]any); ok {
				t.block(block)
			}
		}
		t.flush()
		b.flushReminders()
	}
	if role == "user" && t.dropped != "" && t.sendable == 0 && b.unsupported == "" {
		b.unsupported, b.unsupportedAt = t.dropped, index
	}
}

func (b *inputBuilder) flushReminders() {
	b.items = append(b.items, b.pendingReminders...)
	b.pendingReminders = nil
}

// turn accumulates the content parts of one message. Parts collect into one
// message item; a reasoning item, a function call or a function call output
// closes the open message first, so the items keep the blocks' order.
type turn struct {
	b     *inputBuilder
	role  string
	parts []any

	// sendable counts what the turn sends: non-empty text, attachments and
	// tool results. dropped is the first attachment type it could not send.
	sendable, buffered int
	dropped            string
}

func (t *turn) flush() {
	if len(t.parts) == 0 {
		return
	}
	t.sendable += t.buffered
	t.buffered = 0
	t.b.items = append(t.b.items, messageItem{Type: "message", Role: t.role, Content: t.parts})
	t.parts = nil
}

func (t *turn) text(s string) {
	partType := "input_text"
	if t.role == "assistant" {
		partType = "output_text"
	}
	t.parts = append(t.parts, textPart{Type: partType, Text: s})
	if s != "" {
		t.buffered++
	}
}

func (t *turn) drop(blockType string) {
	if t.role == "user" && t.dropped == "" {
		t.dropped = blockType
	}
}

func (t *turn) block(block map[string]any) {
	b := t.b
	blockType, _ := block["type"].(string)
	switch blockType {
	case "text":
		b.flushReminders()
		s, _ := block["text"].(string)
		t.text(s)
	case "image":
		b.flushReminders()
		if u, ok := imageURL(block["source"]); ok {
			t.parts = append(t.parts, imagePart{Type: "input_image", ImageURL: u})
			t.buffered++
		} else {
			t.drop(blockType)
		}
	case "document":
		b.flushReminders()
		if data, ok := pdfDataURL(block["source"]); ok {
			t.parts = append(t.parts, filePart{Type: "input_file", FileData: data, Filename: "document.pdf"})
			t.buffered++
		} else {
			t.drop(blockType)
		}
	case "thinking":
		if t.role != "assistant" {
			return
		}
		sig, _ := block["signature"].(string)
		sig = strings.TrimSpace(sig)
		if !isCodexSignature(sig) {
			// A thinking block signed by another backend (Anthropic, Gemini)
			// cannot be replayed here; the block is dropped, as Codex would
			// reject the request otherwise.
			return
		}
		t.flush()
		b.items = append(b.items, reasoningItem{Type: "reasoning", Summary: []any{}, EncryptedContent: sig})
	case "tool_use":
		t.flush()
		id, _ := block["id"].(string)
		if id != "" {
			b.pendingToolUseIDs = append(b.pendingToolUseIDs, id)
		}
		name, _ := block["name"].(string)
		args := "{}"
		if input, ok := block["input"]; ok && input != nil {
			if encoded, err := marshal(input); err == nil {
				args = string(encoded)
			}
		}
		b.items = append(b.items, functionCallItem{
			Type:      "function_call",
			CallID:    shortenCallID(id),
			Name:      upstreamName(b.names, name),
			Arguments: args,
		})
	case "tool_result":
		t.flush()
		id, _ := block["tool_use_id"].(string)
		isError, _ := block["is_error"].(bool)
		b.items = append(b.items, functionCallOutputItem{
			Type:   "function_call_output",
			CallID: shortenCallID(id),
			Output: toolResultOutput(block["content"], isError),
		})
		t.sendable++
	case "redacted_thinking", "server_tool_use", "web_search_tool_result":
		// No Codex input form: the first is another backend's opaque
		// reasoning, the other two record a search the backend ran itself
		// and cannot resume.
	}
}

// codexSignatureMax bounds the signature size the shape check decodes.
const codexSignatureMax = 32 << 20

// isCodexSignature reports whether a thinking signature is a Codex
// encrypted_content envelope, by CLIProxyAPI's shape check
// (signature/gpt_validation.go): a Fernet token, which is base64url text
// starting with "gAAAA" that decodes to version byte 0x80, an 8-byte
// timestamp, a 16-byte IV, a ciphertext of whole AES blocks and a 32-byte HMAC.
// The check proves the shape, not that the backend can decrypt it.
func isCodexSignature(sig string) bool {
	if !strings.HasPrefix(sig, "gAAAA") || len(sig) > codexSignatureMax || strings.ContainsAny(sig, "\r\n") {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(sig, "="))
	if err != nil || len(decoded) < 1+8+16+16+32 || decoded[0] != 0x80 {
		return false
	}
	ciphertext := len(decoded) - 1 - 8 - 16 - 32
	return ciphertext%16 == 0
}

// alignToolResults orders a user turn's tool_result blocks like the tool_use
// blocks they answer, keeping every other block where it is. Without a
// one-to-one match the blocks are returned unchanged.
func alignToolResults(blocks []any, toolUseIDs []string) []any {
	if len(toolUseIDs) == 0 {
		return blocks
	}
	var slots []int
	byID := make(map[string]any, len(toolUseIDs))
	for i, raw := range blocks {
		block, _ := raw.(map[string]any)
		if block["type"] != "tool_result" {
			continue
		}
		id, _ := block["tool_use_id"].(string)
		if id == "" || byID[id] != nil {
			return blocks
		}
		byID[id] = raw
		slots = append(slots, i)
	}
	if len(slots) != len(toolUseIDs) {
		return blocks
	}
	out := append([]any(nil), blocks...)
	for i, id := range toolUseIDs {
		result, ok := byID[id]
		if !ok {
			return blocks
		}
		// A tool_use id repeated in the turn would place one result twice.
		delete(byID, id)
		out[slots[i]] = result
	}
	return out
}

// toolResultOutput is the output of a function_call_output: a list of
// input_text and input_image parts when the result carries an image, else its
// text. A result with neither (a tool_reference list, say) is passed as its
// JSON text so the model still sees what the tool returned. An error result is
// prefixed "Error: ", as on the Zen wire.
func toolResultOutput(content any, isError bool) any {
	prefix := func(s string) string {
		if isError && s != "" {
			return "Error: " + s
		}
		return s
	}
	blocks, ok := content.([]any)
	if !ok {
		s, _ := content.(string)
		return prefix(s)
	}
	var texts []string
	var parts []any
	hasImage := false
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		switch block["type"] {
		case "text":
			s, _ := block["text"].(string)
			texts = append(texts, s)
			parts = append(parts, textPart{Type: "input_text", Text: s})
		case "image":
			if u, ok := imageURL(block["source"]); ok {
				parts = append(parts, imagePart{Type: "input_image", ImageURL: u})
				hasImage = true
			}
		}
	}
	switch {
	case hasImage:
		if isError {
			parts = prefixFirstText(parts)
		}
		return parts
	case len(texts) > 0:
		return prefix(strings.Join(texts, "\n"))
	case len(blocks) > 0:
		if encoded, err := marshal(blocks); err == nil {
			return prefix(string(encoded))
		}
	}
	return ""
}

// prefixFirstText marks a multi-part error result: the first text part gets
// the "Error: " prefix, or a part saying "Error:" leads when there is none.
func prefixFirstText(parts []any) []any {
	for i, p := range parts {
		if tp, ok := p.(textPart); ok {
			tp.Text = "Error: " + tp.Text
			parts[i] = tp
			return parts
		}
	}
	return append([]any{textPart{Type: "input_text", Text: "Error:"}}, parts...)
}

// imageURL returns an Anthropic image source as an input_image URL: base64
// data as a data URL, or an http(s) URL as is.
func imageURL(src any) (string, bool) {
	s, _ := src.(map[string]any)
	if s == nil {
		return "", false
	}
	data := str(s["data"])
	if data == "" {
		data = str(s["base64"])
	}
	if data != "" {
		mediaType := str(s["media_type"])
		if mediaType == "" {
			mediaType = str(s["mime_type"])
		}
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		return "data:" + mediaType + ";base64," + data, true
	}
	if str(s["type"]) == "url" {
		raw := strings.TrimSpace(str(s["url"]))
		if u, err := url.Parse(raw); err == nil && u.Host != "" &&
			(strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")) {
			return raw, true
		}
	}
	return "", false
}

// pdfDataURL returns a base64 PDF document source as a data URL. Codex takes
// no other document form.
func pdfDataURL(src any) (string, bool) {
	s, _ := src.(map[string]any)
	if str(s["type"]) != "base64" || !strings.EqualFold(strings.TrimSpace(str(s["media_type"])), "application/pdf") {
		return "", false
	}
	data := str(s["data"])
	if data == "" {
		data = str(s["base64"])
	}
	if data == "" {
		return "", false
	}
	return "data:application/pdf;base64," + data, true
}

// billingHeaderPrefix starts the attribution block Claude Code puts in its
// system prompt: per-request billing data, not instructions, and different on
// every request, which would break the prompt cache.
const billingHeaderPrefix = "x-anthropic-billing-header:"

// systemTexts returns the text of a system prompt, string or text blocks,
// without empty texts and the billing header block.
func systemTexts(system any) []string {
	var texts []string
	add := func(s string) {
		if s != "" && !strings.HasPrefix(strings.TrimLeft(s, " \t\r\n"), billingHeaderPrefix) {
			texts = append(texts, s)
		}
	}
	switch v := system.(type) {
	case string:
		add(v)
	case []any:
		for _, raw := range v {
			block, _ := raw.(map[string]any)
			if block["type"] == "text" {
				add(str(block["text"]))
			}
		}
	}
	return texts
}

// systemReminder wraps the text of a mid-conversation system message in a
// <system-reminder> envelope.
func systemReminder(content any) (string, bool) {
	text := strings.Join(systemTexts(content), "\n")
	if strings.TrimSpace(text) == "" {
		return "", false
	}
	return "<system-reminder>\n" + text + "\n</system-reminder>", true
}

// convertTools maps the client's tools to Codex tools: web search server tools
// to the built-in web_search, everything else to a non-strict function tool.
// Server tools without an input_schema become functions with an empty object
// schema. The second result holds the client names of the web search tools.
//
// Tools marked defer_loading are sent like any other: this builder does not
// implement tool search, so a deferred tool left out could never be called.
func convertTools(tools []any, names map[string]string) ([]any, map[string]bool) {
	var out []any
	var webSearch map[string]bool
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if isWebSearchType(str(tool["type"])) {
			if name := str(tool["name"]); name != "" {
				if webSearch == nil {
					webSearch = make(map[string]bool)
				}
				webSearch[name] = true
			}
			out = append(out, webSearchFromClaude(tool))
			continue
		}
		name := str(tool["name"])
		if name == "" {
			continue
		}
		out = append(out, functionTool{
			Type:        "function",
			Name:        upstreamName(names, name),
			Description: str(tool["description"]),
			Parameters:  toolParameters(tool["input_schema"]),
			Strict:      false,
		})
	}
	return out, webSearch
}

func hasWebSearchTool(tools []any) bool {
	for _, raw := range tools {
		if tool, ok := raw.(map[string]any); ok && isWebSearchType(str(tool["type"])) {
			return true
		}
	}
	return false
}

func isWebSearchType(t string) bool {
	return t == "web_search_20250305" || t == "web_search_20260209"
}

func webSearchFromClaude(tool map[string]any) webSearchTool {
	out := webSearchTool{Type: "web_search"}
	if domains, ok := tool["allowed_domains"].([]any); ok {
		out.Filters = &webSearchFilters{AllowedDomains: domains}
	}
	if loc, ok := tool["user_location"].(map[string]any); ok {
		out.UserLocation = loc
	}
	return out
}

// toolChoice maps an Anthropic tool_choice: auto and none pass through, any
// becomes required, and a named tool becomes that function (or web_search).
// Anything else is auto.
func toolChoice(v any, names map[string]string, webSearch map[string]bool) any {
	choiceType := ""
	var choice map[string]any
	switch t := v.(type) {
	case string:
		choiceType = t
	case map[string]any:
		choice = t
		choiceType = str(t["type"])
	}
	switch choiceType {
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		name := str(choice["name"])
		if webSearch[name] {
			return map[string]any{"type": "web_search"}
		}
		if name = upstreamName(names, name); name != "" {
			return map[string]any{"type": "function", "name": name}
		}
	}
	return "auto"
}

// nameLimit is the longest tool name and call_id the backend accepts.
const nameLimit = 64

// gatewayToolName matches MCP gateway tool ids of the form
// `[mcp_]<server words>-<hex id…>_<tool>`, e.g.
// `mcp_github-12345678-9abc-def0-1234-56789abcdef0-12345678_add-issue-comment`.
var gatewayToolName = regexp.MustCompile(`^(?:mcp_)?([a-z][a-z0-9-]*?)-[0-9a-f]{6,}(?:-[0-9a-f]+)*_(.+)$`)

// shortenName fits one tool name into nameLimit bytes. An MCP name keeps its
// "mcp__" prefix and the tool's own name, then tries the server's words plus
// the tool name, and only then truncates: plain truncation would leave every
// tool of a long-named server with the same 64 bytes.
func shortenName(name string) string {
	if len(name) <= nameLimit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		if i := strings.LastIndex(name, "__"); i > 0 {
			rest := name[i+2:]
			candidate := "mcp__" + rest
			if len(candidate) <= nameLimit {
				return candidate
			}
			if m := gatewayToolName.FindStringSubmatch(rest); m != nil {
				if readable := "mcp__" + m[1] + "_" + m[2]; len(readable) <= nameLimit {
					return readable
				}
			}
			return candidate[:nameLimit]
		}
	}
	return name[:nameLimit]
}

// shortNames maps every declared tool name to its upstream name, unique within
// the request: a shortened name that collides gets "_1", "_2", … in place of
// its tail.
func shortNames(tools []any) map[string]string {
	names := make(map[string]string, len(tools))
	used := make(map[string]bool, len(tools))
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name := str(tool["name"])
		if name == "" || names[name] != "" {
			continue
		}
		candidate := shortenName(name)
		for i := 1; used[candidate]; i++ {
			suffix := "_" + strconv.Itoa(i)
			base := shortenName(name)
			if len(base) > nameLimit-len(suffix) {
				base = base[:nameLimit-len(suffix)]
			}
			candidate = base + suffix
		}
		used[candidate] = true
		names[name] = candidate
	}
	return names
}

// upstreamName is the name a client tool goes upstream under. A name no
// declared tool carries (a call in the history of a tool since removed) is
// shortened on its own.
func upstreamName(names map[string]string, name string) string {
	if n, ok := names[name]; ok {
		return n
	}
	return shortenName(name)
}

// shortenCallID fits a tool_use id into nameLimit bytes, keeping a stable,
// low-collision mapping: the id's head, "_", and 16 hex digits of its SHA-256.
// The same id shortens the same way in the call and in its result.
func shortenCallID(id string) string {
	if len(id) <= nameLimit {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	suffix := "_" + hex.EncodeToString(sum[:8])
	return id[:nameLimit-len(suffix)] + suffix
}

// reasoningSettings returns reasoning.effort and reasoning.summary.
//
// The effort is output_config.effort when it is an Anthropic level, else the
// thinking budget mapped by CLIProxyAPI's thresholds, else the model's default
// level, else medium; the level is then clamped to the model's published ones.
// Disabled thinking sends no effort and leaves the level to the backend.
//
// The summary is requested when thinking is enabled or adaptive and the client
// did not ask for the thinking to be omitted: Codex returns no reasoning text
// unless asked, and without it the client's thinking blocks would be empty.
func reasoningSettings(req map[string]any, entry ModelConfig) (effort, summary string) {
	thinking, _ := req["thinking"].(map[string]any)
	thinkingType := strings.ToLower(strings.TrimSpace(str(thinking["type"])))
	if thinkingType == "disabled" {
		return "", ""
	}
	if (thinkingType == "enabled" || thinkingType == "adaptive") &&
		!strings.EqualFold(strings.TrimSpace(str(thinking["display"])), "omitted") {
		summary = "auto"
	}

	if oc, ok := req["output_config"].(map[string]any); ok {
		if e := strings.ToLower(strings.TrimSpace(str(oc["effort"]))); anthropicEfforts[e] {
			effort = e
		}
	}
	if effort == "" {
		if budget, ok := thinking["budget_tokens"].(json.Number); ok {
			if n, err := budget.Int64(); err == nil && n > 0 {
				effort = budgetEffort(n)
			}
		}
	}
	if effort == "" {
		effort = strings.ToLower(strings.TrimSpace(entry.DefaultEffort))
	}
	if effort == "" {
		effort = "medium"
	}
	return clampEffort(effort, entry.Efforts), summary
}

// budgetEffort maps a thinking budget to a level (CLIProxyAPI
// thinking.ConvertBudgetToLevel).
func budgetEffort(budget int64) string {
	switch {
	case budget <= 512:
		return "minimal"
	case budget <= 1024:
		return "low"
	case budget <= 8192:
		return "medium"
	case budget <= 24576:
		return "high"
	}
	return "xhigh"
}

// serviceTier is "priority" when the client asked for the fast tier, as
// service_tier fast or priority, or speed fast.
func serviceTier(req map[string]any) string {
	switch strings.ToLower(strings.TrimSpace(str(req["service_tier"]))) {
	case "fast", "priority":
		return "priority"
	}
	if str(req["speed"]) == "fast" {
		return "priority"
	}
	return ""
}

// textFormat maps output_config.format of type json_schema to text.format.
// strict defaults to true but falls back to false when a property is missing
// from its object's required list: strict mode requires every property to be
// listed and rejects the request otherwise.
func textFormat(req map[string]any) *textControls {
	oc, _ := req["output_config"].(map[string]any)
	format, _ := oc["format"].(map[string]any)
	schema, _ := format["schema"].(map[string]any)
	if str(format["type"]) != "json_schema" || schema == nil {
		return nil
	}
	name := str(format["name"])
	if name == "" {
		name = structuredOutputName
	}
	strict := true
	if s, ok := format["strict"].(bool); ok && !s {
		strict = false
	}
	if strict && missesRequired(schema) {
		strict = false
	}
	return &textControls{Format: jsonSchemaFormat{Type: "json_schema", Name: name, Strict: strict, Schema: schema}}
}

// missesRequired reports whether any object in a schema declares a property
// its required list leaves out.
func missesRequired(v any) bool {
	switch schema := v.(type) {
	case []any:
		for _, item := range schema {
			if missesRequired(item) {
				return true
			}
		}
	case map[string]any:
		if props, ok := schema["properties"].(map[string]any); ok {
			required := make(map[string]bool)
			for _, r := range asSlice(schema["required"]) {
				if s, ok := r.(string); ok {
					required[s] = true
				}
			}
			for name := range props {
				if !required[name] {
					return true
				}
			}
		}
		for _, key := range schemaMapKeywords {
			if sub, ok := schema[key].(map[string]any); ok {
				for _, s := range sub {
					if missesRequired(s) {
						return true
					}
				}
			}
		}
		for _, key := range schemaValueKeywords {
			if missesRequired(schema[key]) {
				return true
			}
		}
	}
	return false
}

// outputBudget is max_tokens (max_output_tokens as a fallback) as
// max_output_tokens, an integer below minOutputTokens raised to it. A budget
// that is not a positive integer is passed for the backend to judge.
func outputBudget(req map[string]any) json.Number {
	v, ok := req["max_tokens"]
	if !ok {
		v = req["max_output_tokens"]
	}
	n, ok := v.(json.Number)
	if !ok {
		return ""
	}
	if f, err := n.Float64(); err == nil && f >= 1 && f < minOutputTokens && f == math.Trunc(f) {
		return json.Number(strconv.Itoa(minOutputTokens))
	}
	return n
}

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
