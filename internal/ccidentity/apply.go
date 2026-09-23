package ccidentity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ErrNotAnObject is returned when a request body is not a JSON object. The
// caller must treat it as a hard failure rather than passing the original bytes
// through: an unnormalised body is exactly the fingerprint this package exists
// to remove, so silently forwarding it would be worse than refusing.
var ErrNotAnObject = errors.New("ccidentity: request body is not a JSON object")

// buildSuffix is the third component of cc_version, e.g. the "5c2" in
// 2.1.280.5c2. The capture shows it differing BETWEEN processes and stable
// within one, so it is generated once per process. Its derivation is unknown;
// any stable 3-hex value reproduces the observed variation.
var buildSuffix = func() string {
	value := make([]byte, 2)
	_, _ = rand.Read(value)
	return hex.EncodeToString(value)[:3]
}()

// ApplyHeaders rewrites h into the captured Claude Code header set.
//
// Order matters: every name in the omit list is deleted BEFORE the captured set
// is assigned. Assigning alone would leave a foreign client's value in place for
// any header the capture does not mention, which is the leak the omit list
// exists to close.
//
// Names are assigned into the map directly rather than through Header.Set,
// because Set canonicalises and the captured name is X-Stainless-OS, not
// X-Stainless-Os. Over the HTTP/1.1 transport the capture used, that difference
// reaches the wire.
func ApplyHeaders(h map[string][]string, p Profile, id Identity, turn Turn) {
	if h == nil {
		return
	}
	for _, name := range p.Omit {
		delete(h, name)
		delete(h, canonicalName(name))
	}
	for _, header := range StaticFor(id) {
		h[header.Name] = []string{header.Value}
	}
	h["anthropic-beta"] = []string{strings.Join(p.Betas, ",")}
	for _, dynamic := range p.Dynamic {
		h[dynamic.Name] = []string{dynamic.Value(id, turn)}
	}
}

// canonicalName is net/http's canonical form, used so an omit entry removes a
// value a caller inserted with Header.Set as well as one assigned by name.
func canonicalName(name string) string {
	parts := strings.Split(strings.ToLower(name), "-")
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, "-")
}

// ApplyBody rewrites a /v1/messages body into the captured Claude Code shape:
// metadata.user_id becomes the captured stringified JSON object, system block 0
// becomes a freshly generated billing header, and top-level fields Claude Code
// does not send are removed.
func ApplyBody(body []byte, p Profile, id Identity, turn Turn) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("ccidentity: parse request body: %w", err)
	}
	if parsed == nil {
		return nil, ErrNotAnObject
	}

	parsed["metadata"] = metadataWithUserID(parsed["metadata"], p, id)
	parsed["system"] = systemWithBillingHeader(parsed["system"], id, turn)

	for _, name := range omittedBodyFields {
		delete(parsed, name)
	}

	encoded, err := json.Marshal(parsed)
	if err != nil {
		return nil, fmt.Errorf("ccidentity: re-encode request body: %w", err)
	}
	return encoded, nil
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
		DeviceID:    deviceID(id),
		AccountUUID: id.AccountUUID,
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
	if id.SessionKey == "" {
		return newUUID()
	}
	sum := sha256.Sum256([]byte("ccidentity-session:" + id.SessionKey))
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

// BillingHeader generates the captured system-block-0 line.
//
// Captured forms, verbatim from .reference/claude-code-headers-20260923.txt:
//
//	x-anthropic-billing-header: cc_version=2.1.280.5c2; cc_entrypoint=sdk-cli; cch=3d0b8; cc_prompt_id=<uuid>; cc_turn_origin=sdk;
//	x-anthropic-billing-header: cc_version=2.1.280.336; cc_entrypoint=sdk-cli; cch=3ab87; cc_prev_req=req_011CfL3J5mtdJ5DhtF1opYTr; cc_prompt_id=<uuid>; cc_turn_origin=sdk;
//
// Three values are per-request or per-process rather than constant, which is why
// this is generated instead of matched:
//
//   - cc_version carries a per-process build suffix.
//   - cch differs on every request. Its derivation is unknown; a fresh 5-hex
//     value reproduces the observed variation.
//   - cc_prev_req appears only on a follow-up turn and carries that turn's
//     request id.
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

	var b strings.Builder
	b.WriteString("x-anthropic-billing-header: cc_version=")
	b.WriteString(version)
	b.WriteString(".")
	b.WriteString(buildSuffix)
	b.WriteString("; cc_entrypoint=")
	b.WriteString(entrypoint)
	b.WriteString("; cch=")
	b.WriteString(randomHex(3)[:5])
	b.WriteString("; ")
	if turn.PrevRequestID != "" {
		b.WriteString("cc_prev_req=")
		b.WriteString(turn.PrevRequestID)
		b.WriteString("; ")
	}
	b.WriteString("cc_prompt_id=")
	b.WriteString(sessionUUID(id))
	b.WriteString("; cc_turn_origin=")
	b.WriteString(origin)
	b.WriteString(";")
	return b.String()
}

// randomHex returns 2*n hex characters.
func randomHex(bytes int) string {
	value := make([]byte, bytes)
	_, _ = rand.Read(value)
	return hex.EncodeToString(value)
}

// systemWithBillingHeader replaces system block 0 with the generated billing
// header, preserving the rest of the system prompt.
//
// A plain string system prompt is promoted to the block array the capture shows,
// because that is the shape Claude Code sends and a string would be a
// distinguishable difference. A block whose type is not "text" is left alone:
// replacing it would destroy content this package cannot reconstruct.
func systemWithBillingHeader(existing any, id Identity, turn Turn) any {
	header := BillingHeader(id, turn)
	switch system := existing.(type) {
	case string:
		return []any{
			map[string]any{"type": "text", "text": header},
			map[string]any{"type": "text", "text": system},
		}
	case []any:
		if len(system) == 0 {
			return []any{map[string]any{"type": "text", "text": header}}
		}
		first, ok := system[0].(map[string]any)
		if !ok {
			return system
		}
		kind, _ := first["type"].(string)
		if kind != "" && kind != "text" {
			return system
		}
		replaced := make([]any, len(system))
		copy(replaced, system)
		replaced[0] = map[string]any{"type": "text", "text": header}
		return replaced
	default:
		// No system field at all: add the block array Claude Code sends rather
		// than omitting the marker entirely.
		return []any{map[string]any{"type": "text", "text": header}}
	}
}
