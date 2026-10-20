package ccidentity

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrNotAnObject is returned when a request body is not a JSON object. The
// caller must treat it as a hard failure rather than passing the original bytes
// through: an unnormalised body is exactly the fingerprint this package exists
// to remove, so silently forwarding it would be worse than refusing.
var ErrNotAnObject = errors.New("ccidentity: request body is not a JSON object")

// ApplyHeaders rewrites h into the captured Claude Code header set.
//
// Deletion happens before assignment, and it is case-insensitive, because
// net/http canonicalises the names it parses: a client's `x-app` arrives stored
// as `X-App`, and `anthropic-beta` as `Anthropic-Beta`. Assigning the captured
// spelling without first removing the canonical one leaves BOTH keys in the map,
// and the transport writes both — so the upstream would read the client's value
// under the canonical key and the captured one would be inert. That is a silent
// failure, not a visible one: the request still succeeds.
//
// The same canonicalisation is why names are assigned into the map directly
// rather than through Header.Set. The captured wire names are X-Stainless-OS and
// the lowercase anthropic-* family; Set would store X-Stainless-Os and
// Anthropic-Beta instead, which over the captured HTTP/1.1 transport reaches the
// wire.
func ApplyHeaders(h map[string][]string, p Profile, id Identity, turn Turn) {
	if h == nil {
		return
	}

	static := StaticFor(id)

	// Names this call will write, compared case-insensitively.
	claimed := make(map[string]bool, len(p.Omit)+len(static)+len(p.Dynamic)+1)
	for _, name := range p.Omit {
		claimed[strings.ToLower(name)] = true
	}
	for _, header := range static {
		claimed[strings.ToLower(header.Name)] = true
	}
	claimed[strings.ToLower(OAuthBetaKey)] = true
	for _, dynamic := range p.Dynamic {
		claimed[strings.ToLower(dynamic.Name)] = true
	}

	for key := range h {
		if claimed[strings.ToLower(key)] {
			delete(h, key)
		}
	}

	for _, header := range static {
		h[header.Name] = []string{header.Value}
	}
	h[OAuthBetaKey] = []string{strings.Join(p.Betas, ",")}
	for _, dynamic := range p.Dynamic {
		h[dynamic.Name] = []string{dynamic.Value(id, turn)}
	}
}

// OAuthBetaKey is the captured spelling of the beta header. It is lowercase on
// the wire, which is why this package addresses it by name rather than through
// the canonicalising http.Header helpers.
const OAuthBetaKey = "anthropic-beta"

// ApplyBody rewrites a /v1/messages body into the captured Claude Code shape:
// metadata.user_id becomes the captured stringified JSON object, system block 0
// becomes a freshly generated billing header, and top-level fields Claude Code
// does not send are removed.
//
// The output is written the way the real client writes it, because the cch in
// the billing header is a hash of these exact bytes (see signCCH):
//
//   - Top-level keys follow bodyFieldOrder, the captured order, so "model" is
//     the first key, as the hash's "first model field" rule assumes. Keys the
//     capture does not show follow in the caller's order.
//   - Values this function does not rewrite are the caller's own bytes,
//     compacted, so nested key order and string escapes are not re-encoded.
//   - Values it writes are encoded without HTML escaping, as JSON.stringify
//     does: Go's default turns "<" into \u003c, which no JavaScript client sends.
func ApplyBody(body []byte, p Profile, id Identity, turn Turn) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	fields, err := objectFields(body)
	if err != nil {
		return nil, err
	}
	for _, name := range omittedBodyFields {
		delete(fields.values, name)
	}

	var existingMetadata any
	if raw, ok := fields.values["metadata"]; ok {
		_ = json.Unmarshal(raw, &existingMetadata)
	}
	metadata, err := marshalJS(metadataWithUserID(existingMetadata, p, id))
	if err != nil {
		return nil, fmt.Errorf("ccidentity: encode metadata: %w", err)
	}
	fields.values["metadata"] = metadata

	system, signed, err := systemWithBillingHeader(fields.values["system"], id, turn)
	if err != nil {
		return nil, err
	}
	fields.values["system"] = system

	out := bytes.NewBuffer(make([]byte, 0, len(body)+512))
	placeholderAt := -1
	out.WriteByte('{')
	write := func(key string) error {
		value, ok := fields.values[key]
		if !ok {
			return nil
		}
		delete(fields.values, key)
		if out.Len() > 1 {
			out.WriteByte(',')
		}
		encodedKey, err := marshalJS(key)
		if err != nil {
			return err
		}
		out.Write(encodedKey)
		out.WriteByte(':')
		start := out.Len()
		if err := json.Compact(out, value); err != nil {
			return err
		}
		if key == "system" && signed {
			// Block 0 is the billing header, so the first placeholder in
			// the array is the one to sign.
			if at := bytes.Index(out.Bytes()[start:], []byte(cchPlaceholder)); at >= 0 {
				placeholderAt = start + at
			}
		}
		return nil
	}
	for _, key := range bodyFieldOrder {
		if err := write(key); err != nil {
			return nil, fmt.Errorf("ccidentity: re-encode request body: %w", err)
		}
	}
	for _, key := range fields.order {
		if err := write(key); err != nil {
			return nil, fmt.Errorf("ccidentity: re-encode request body: %w", err)
		}
	}
	out.WriteByte('}')

	encoded := out.Bytes()
	if placeholderAt >= 0 {
		signCCH(encoded, placeholderAt)
	}
	return encoded, nil
}

// bodyFieldOrder is the top-level key order of a captured Claude Code 2.1.304
// /v1/messages body (testdata/claude-code-2.1.304-bodies.jsonl.gz).
var bodyFieldOrder = []string{
	"model",
	"messages",
	"system",
	"tools",
	"metadata",
	"max_tokens",
	"thinking",
	"context_management",
	"output_config",
	"diagnostics",
	"stream",
}

// bodyFields is a JSON object's top-level members, in the order they arrived.
type bodyFields struct {
	order  []string
	values map[string]json.RawMessage
}

// objectFields splits body into its top-level members without decoding them.
// A key that appears twice keeps its first position and its last value, as
// encoding/json's own map decoding keeps the last value.
func objectFields(body []byte) (bodyFields, error) {
	parseError := func(err error) (bodyFields, error) {
		return bodyFields{}, fmt.Errorf("ccidentity: parse request body: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil {
		return parseError(err)
	}
	if token != json.Delim('{') {
		return bodyFields{}, ErrNotAnObject
	}
	fields := bodyFields{values: map[string]json.RawMessage{}}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return parseError(err)
		}
		key, _ := token.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return parseError(err)
		}
		if _, seen := fields.values[key]; !seen {
			fields.order = append(fields.order, key)
		}
		fields.values[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return parseError(err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return parseError(errors.New("data after the top-level object"))
	}
	return fields, nil
}

// marshalJS encodes v the way JSON.stringify does for the values this package
// writes: no HTML escaping and no trailing newline.
func marshalJS(v any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// omittedBodyFields are top-level fields Claude Code does not send in the
// captured traffic but that OpenAI-shimmed clients routinely produce. Leaving
// them in place would advertise the real caller.
//
// Deliberately NOT synthesised: the capture shows Claude Code sending
// context_management, diagnostics and output_config, and this package cannot
// invent their contents. Their absence is a known difference from the capture,
// not an oversight.
var omittedBodyFields = []string{
	"temperature",
	"top_p",
	"top_k",
	"stop_sequences",
	"tool_choice",
	"service_tier",
}

// metadataUserID is the captured metadata.user_id payload. It exists as a struct
// rather than a map so the field order matches the capture: encoding/json emits
// struct fields in declaration order and map keys sorted, and the captured order
// is device_id, account_uuid, session_id.
type metadataUserID struct {
	DeviceID    string `json:"device_id"`
	AccountUUID string `json:"account_uuid"`
	SessionID   string `json:"session_id"`
}

// metadataWithUserID returns the metadata object with user_id set to the
// captured stringified JSON form, preserving any other keys the caller sent.
func metadataWithUserID(existing any, p Profile, id Identity) map[string]any {
	metadata, ok := existing.(map[string]any)
	if !ok {
		metadata = map[string]any{}
	}
	payload := metadataUserID{
		DeviceID: deviceID(id),
		// Always empty, as captured. Claude Code sent account_uuid empty in 6
		// of 6 requests across two independent OAuth credentials
		// (.reference/claude-code-headers-20260923.jsonl and
		// ...-20260923-acct2.jsonl), so populating it would be a value no real
		// client emits. Per-account identity is not lost: deviceID derives from
		// id.AccountUUID, so two accounts still present different devices.
		AccountUUID: "",
		SessionID:   sessionUUID(id),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// Unreachable for this struct: three strings cannot fail to encode.
		encoded = []byte("{}")
	}
	metadata["user_id"] = string(encoded)
	return metadata
}

// deviceID is the 64-hex device identifier in metadata.user_id. It is derived
// from the account when there is one and from the session otherwise, so it is
// stable across a conversation and does not change between turns.
func deviceID(id Identity) string {
	seed := id.DeviceID
	if seed == "" {
		seed = id.AccountUUID
	}
	if seed == "" {
		seed = id.SessionKey
	}
	sum := sha256.Sum256([]byte("ccidentity-device:" + seed))
	return hex.EncodeToString(sum[:])
}

// sessionUUID is the session identifier, shaped as a UUID because the capture
// shows one. Claude Code's session key is used as the derivation seed, so the
// same session key always yields the same UUID and the identity stays consistent
// with the pool's sticky routing.
func sessionUUID(id Identity) string {
	return derivedUUID("ccidentity-session:", id.SessionKey)
}

// NewSessionKey returns a random session key for a request that arrived with
// none.
//
// derivedUUID answers an empty seed with a fresh random UUID on EVERY call, and
// ApplyHeaders and ApplyBody are separate calls. For an unkeyed request the
// prompt id in x-claude-code-prompt-id and the one in system[0] therefore came
// out as two different UUIDs, and so did X-Claude-Code-Session-Id and
// metadata.user_id.session_id. The capture shows one value in each pair, in
// every request (.reference/claude-code-headers-20261020*.jsonl), so the
// disagreement was a tell. The live gate found it: scripts/verify-claude-code-
// identity.sh sends no session key and diffed the two prompt ids as unequal.
//
// A caller that builds an Identity from a possibly empty key sets this once per
// request, before either apply, so every derived value agrees across the header
// set and the body. It is random, not constant, because a constant would give
// every unkeyed request the same identity.
func NewSessionKey() string {
	return newUUID()
}

// promptUUID is the cc_prompt_id in the billing header and the value of
// x-claude-code-prompt-id. It is derived from the session key under a DIFFERENT
// salt from sessionUUID, because the captures show the two are never the same
// value.
//
// X-Claude-Code-Session-Id and cc_prompt_id differ in every captured request
// across two independent OAuth accounts, and the session id is stable for a
// whole process. Deriving them from one salt made every normalised request
// carry one UUID in both slots — a correlation no real client produces.
//
// The real client mints a new cc_prompt_id for every user prompt and repeats it
// only across that prompt's tool follow-up requests
// (.reference/claude-code-headers-20261020-multiprompt.jsonl: three prompts in
// one process gave three ids, the third repeated on its follow-up). The turn
// carries what moves the id: the prompt's number and a digest of the prompt
// itself. Both come from the request body (see TurnOf), so the id is stable
// across one prompt's follow-ups and changes with the next prompt, with nothing
// stored between requests.
//
// Their true relation is not knowable from the capture, since metadata's own
// session_id is redacted to <uuid> there. Distinctness is what the capture
// proves, and distinctness is all this reproduces.
func promptUUID(id Identity, turn Turn) string {
	if id.SessionKey == "" {
		return derivedUUID("ccidentity-prompt:", "")
	}
	seed := id.SessionKey + "\x00" + strconv.Itoa(turn.promptNumber()) + "\x00" + turn.promptDigest
	return derivedUUID("ccidentity-prompt:", seed)
}

// derivedUUID turns a salt and a seed into a stable version-5-shaped UUID. An
// empty seed yields a random version-4 UUID instead, because there is nothing to
// derive from and a constant would collide across every session.
func derivedUUID(salt, seed string) string {
	if seed == "" {
		return newUUID()
	}
	sum := sha256.Sum256([]byte(salt + seed))
	var value [16]byte
	copy(value[:], sum[:16])
	value[6] = value[6]&0x0f | 0x50 // version 5, name-based
	value[8] = value[8]&0x3f | 0x80
	return formatUUID(value)
}

// newUUID returns a random RFC 4122 version 4 UUID.
func newUUID() string {
	var value [16]byte
	_, _ = rand.Read(value[:])
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return formatUUID(value)
}

func formatUUID(value [16]byte) string {
	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] +
		"-" + encoded[16:20] + "-" + encoded[20:32]
}

// BillingHeader generates the captured system-block-0 line, with cch still the
// placeholder 00000. ApplyBody replaces it with the hash of the final body.
//
// Captured forms, verbatim from .reference/claude-code-headers-20261020.txt:
//
//	x-anthropic-billing-header: cc_version=2.1.304.456; cc_entrypoint=sdk-cli; cch=6cba6; cc_prompt_id=<uuid>; cc_turn_origin=sdk; cc_prompt_index=0; cc_turn_index=1;
//	x-anthropic-billing-header: cc_version=2.1.304.d31; cc_entrypoint=sdk-cli; cch=bfd5f; cc_prev_req=req_011CgDkvRyJGtnMF7Y4UgAR7; cc_prompt_id=<uuid>; cc_turn_origin=sdk; cc_prompt_index=0; cc_turn_index=1;
//
// The values that change are derived the way the 2.1.304 binary derives them:
//
//   - The cc_version suffix is a hash of the conversation's first prompt; see
//     versionSuffix.
//   - cch is a hash of the whole request body; see signCCH.
//   - cc_prev_req names the request that produced the reply the latest
//     assistant message echoes, so it is absent on a conversation's first
//     request. ReplyLog supplies it; see there for how the client derives it.
//     It is written only when it matches the client's own guard,
//     requestIDPattern, because the value comes from an upstream response
//     header and is spliced into this line.
//   - cc_turn_index counts user prompts from 1: three prompts sent one at a
//     time in one process gave 1, 2, 3, and the tool follow-up of the third
//     repeated 3 (.reference/claude-code-headers-20261020-multiprompt.jsonl).
//     It is the turn's PromptNumber, which TurnOf reads from the request body.
//   - cc_prompt_index counts the turns whose origin is "human" and stays 0
//     otherwise: the binary adds 1 to it only for a human turn. Every turn of
//     one identity has the same origin, so it is PromptNumber for a human
//     origin and 0 for any other.
func BillingHeader(id Identity, turn Turn) string {
	version := id.ClientVersion
	if version == "" {
		version = ClientVersion
	}
	entrypoint := id.Entrypoint
	if entrypoint == "" {
		entrypoint = EntrypointSDKCLI
	}
	origin := id.TurnOrigin
	if origin == "" {
		origin = TurnOriginSDK
	}
	promptIndex := 0
	if origin == TurnOriginHuman {
		promptIndex = turn.promptNumber()
	}

	var b strings.Builder
	b.WriteString("x-anthropic-billing-header: cc_version=")
	b.WriteString(version)
	b.WriteString(".")
	b.WriteString(versionSuffix(turn.firstPrompt, version))
	b.WriteString("; cc_entrypoint=")
	b.WriteString(entrypoint)
	b.WriteString("; ")
	b.WriteString(cchPlaceholder)
	b.WriteString("; ")
	if requestIDPattern.MatchString(turn.PrevRequestID) {
		b.WriteString("cc_prev_req=")
		b.WriteString(turn.PrevRequestID)
		b.WriteString("; ")
	}
	b.WriteString("cc_prompt_id=")
	b.WriteString(promptUUID(id, turn))
	b.WriteString("; cc_turn_origin=")
	b.WriteString(origin)
	b.WriteString("; cc_prompt_index=")
	b.WriteString(strconv.Itoa(promptIndex))
	b.WriteString("; cc_turn_index=")
	b.WriteString(strconv.Itoa(turn.promptNumber()))
	b.WriteString(";")
	return b.String()
}

// textBlock is a system text block in the captured key order.
type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// systemWithBillingHeader puts the generated billing header at system block 0,
// preserving the rest of the system prompt byte for byte. It reports whether
// the header was placed, which is when the body's cch must be signed.
//
// A plain string system prompt is promoted to the block array the capture shows,
// because that is the shape Claude Code sends and a string would be a
// distinguishable difference.
//
// The marker is PREPENDED, not written over block 0. Claude Code's own block 0
// is this marker and its real prompt follows, so prepending is both the captured
// shape and the only non-destructive option: overwriting would silently delete
// the caller's whole system prompt whenever it arrived as a single text block,
// while the identical prompt sent as a plain string survived. A block 0 that is
// already a billing header is replaced instead, so a caller that really is
// Claude Code does not end up sending two.
//
// A block 0 that is not an object, or whose type is not "text", leaves the
// system prompt alone: replacing it would destroy content this package cannot
// reconstruct.
func systemWithBillingHeader(existing json.RawMessage, id Identity, turn Turn) (json.RawMessage, bool, error) {
	marker, err := marshalJS(textBlock{Type: "text", Text: BillingHeader(id, turn)})
	if err != nil {
		return nil, false, fmt.Errorf("ccidentity: encode billing header: %w", err)
	}
	array := func(blocks ...[]byte) json.RawMessage {
		return append(append([]byte{'['}, bytes.Join(blocks, []byte{','})...), ']')
	}

	existing = bytes.TrimSpace(existing)
	if len(existing) == 0 {
		// No system field at all: add the block array Claude Code sends rather
		// than omitting the marker entirely.
		return array(marker), true, nil
	}
	switch existing[0] {
	case '"':
		// The caller's own string bytes, so its escaping is kept.
		promoted := append(append([]byte(`{"type":"text","text":`), existing...), '}')
		return array(marker, promoted), true, nil
	case '[':
		var blocks []json.RawMessage
		if err := json.Unmarshal(existing, &blocks); err != nil {
			return nil, false, fmt.Errorf("ccidentity: parse system: %w", err)
		}
		if len(blocks) == 0 {
			return array(marker), true, nil
		}
		var first map[string]any
		if json.Unmarshal(blocks[0], &first) != nil || first == nil {
			return existing, false, nil
		}
		kind, _ := first["type"].(string)
		if kind != "" && kind != "text" {
			return existing, false, nil
		}
		if text, _ := first["text"].(string); strings.HasPrefix(text, billingHeaderPrefix) {
			blocks = blocks[1:]
		}
		raw := make([][]byte, 0, len(blocks)+1)
		raw = append(raw, marker)
		for _, block := range blocks {
			raw = append(raw, block)
		}
		return array(raw...), true, nil
	default:
		return array(marker), true, nil
	}
}

// billingHeaderPrefix identifies a block that already carries the marker, so it
// is replaced rather than duplicated.
const billingHeaderPrefix = "x-anthropic-billing-header: "
