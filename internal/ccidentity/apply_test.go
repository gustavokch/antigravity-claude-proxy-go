package ccidentity

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// hdrGet reads a header without canonicalising the lookup key.
//
// http.Header.Get canonicalises its argument, so it cannot find the names this
// package stores on purpose: the captured wire names are X-Stainless-OS and the
// lowercase anthropic-* family, and canonicalising either would change what
// reaches the wire.
func hdrGet(h http.Header, name string) string {
	if values, ok := h[name]; ok && len(values) > 0 {
		return values[0]
	}
	return ""
}

func testIdentity() Identity {
	return Identity{
		AccountUUID: "11111111-2222-3333-4444-555555555555",
		SessionKey:  "session-abc",
	}
}

// capturedHeaders is the constant header set read out of
// .reference/claude-code-headers-20261020.txt. It is duplicated here on purpose:
// a test that reads the artifact at run time would pass even if both the
// artifact and the implementation were changed together, and this test's job is
// to fail when the implementation drifts from the recorded bytes.
var capturedHeaders = map[string]string{
	"Accept":                                    "application/json",
	"Content-Type":                              "application/json",
	"X-Stainless-Arch":                          "arm64",
	"X-Stainless-Lang":                          "js",
	"X-Stainless-OS":                            "Linux",
	"X-Stainless-Package-Version":               "0.128.0",
	"X-Stainless-Retry-Count":                   "0",
	"X-Stainless-Runtime":                       "node",
	"X-Stainless-Runtime-Version":               "v26.12.1",
	"X-Stainless-Timeout":                       "600",
	"anthropic-dangerous-direct-browser-access": "true",
	"anthropic-version":                         "2023-06-01",
	"x-app":                                     "cli",
	"x-claude-code-request-class":               "main",
}

func TestApplyHeadersSendsTheCapturedSet(t *testing.T) {
	h := http.Header{}
	// The capture ran on a Linux arm64 container; the OS and arch default to the
	// host, so the test names the capture's environment rather than assuming it.
	id := testIdentity()
	id.StainlessOS, id.StainlessArch = "Linux", "arm64"
	ApplyHeaders(h, DefaultProfile(), id, Turn{})

	for name, want := range capturedHeaders {
		if got := hdrGet(h, name); got != want {
			t.Errorf("%s = %q, want captured %q", name, got, want)
		}
	}
	if got := hdrGet(h, "User-Agent"); got != MessagesUserAgent {
		t.Errorf("User-Agent = %q, want %q", got, MessagesUserAgent)
	}
	wantBeta := strings.Join(Betas, ",")
	if got := hdrGet(h, "anthropic-beta"); got != wantBeta {
		t.Errorf("anthropic-beta = %q, want captured order %q", got, wantBeta)
	}
}

// The OS and arch the proxy claims are those of the machine it runs on: the
// TCP/IP fingerprint of the connection says which OS sent it, so a default that
// claims another one is a contradiction no header can repair.
func TestStainlessOSAndArchDefaultToTheHost(t *testing.T) {
	h := http.Header{}
	ApplyHeaders(h, DefaultProfile(), testIdentity(), Turn{})
	if got, want := hdrGet(h, "X-Stainless-OS"), HostOS(runtime.GOOS); got != want {
		t.Errorf("X-Stainless-OS = %q, want the host's %q", got, want)
	}
	if got, want := hdrGet(h, "X-Stainless-Arch"), HostArch(runtime.GOARCH); got != want {
		t.Errorf("X-Stainless-Arch = %q, want the host's %q", got, want)
	}
}

func TestStainlessOSAndArchOverridesWin(t *testing.T) {
	h := http.Header{}
	id := testIdentity()
	id.StainlessOS, id.StainlessArch = "Windows", "x64"
	ApplyHeaders(h, DefaultProfile(), id, Turn{})
	if got := hdrGet(h, "X-Stainless-OS"); got != "Windows" {
		t.Errorf("X-Stainless-OS = %q, want the override", got)
	}
	if got := hdrGet(h, "X-Stainless-Arch"); got != "x64" {
		t.Errorf("X-Stainless-Arch = %q, want the override", got)
	}
}

func TestApplyHeadersPreservesCapturedNameCase(t *testing.T) {
	h := http.Header{}
	ApplyHeaders(h, DefaultProfile(), testIdentity(), Turn{})
	if _, ok := h["X-Stainless-OS"]; !ok {
		t.Fatalf("X-Stainless-OS missing; Header.Set canonicalisation would produce %q", "X-Stainless-Os")
	}
	if _, ok := h["X-Stainless-Os"]; ok {
		t.Error("X-Stainless-Os present; the captured name is X-Stainless-OS")
	}
}

func TestApplyHeadersDeletesForeignClientValues(t *testing.T) {
	h := http.Header{}
	h.Set("User-Agent", "cursor/1.2.3")
	h.Set("x-api-key", "sk-ant-api-key-that-must-not-leak")
	h.Set("X-Stainless-Lang", "js")
	h.Set("HTTP-Referer", "https://cursor.com")
	h.Set("X-Title", "Cursor")
	h.Set("Authorization", "Bearer foreign")
	h.Set("X-Forwarded-For", "10.0.0.1")
	h.Set("x-session-id", "cursor-session")

	ApplyHeaders(h, DefaultProfile(), testIdentity(), Turn{})

	for _, name := range []string{
		"x-api-key", "HTTP-Referer", "X-Title", "X-Forwarded-For", "x-session-id",
	} {
		if got := hdrGet(h, name); got != "" {
			t.Errorf("%s survived the rewrite as %q; omit list must delete it", name, got)
		}
	}
	if got := hdrGet(h, "User-Agent"); got != MessagesUserAgent {
		t.Errorf("User-Agent = %q, want the captured value", got)
	}
}

func TestApplyHeadersLeavesTransportOwnedHeadersToGo(t *testing.T) {
	// Accept-Encoding is deliberately not spoofed: setting it stops net/http
	// adding gzip and its transparent decompression, which the SSE path needs.
	h := http.Header{}
	h.Set("Accept-Encoding", "br")
	ApplyHeaders(h, DefaultProfile(), testIdentity(), Turn{})
	if got := hdrGet(h, "Accept-Encoding"); got != "" {
		t.Errorf("Accept-Encoding = %q; want it cleared so Go manages it", got)
	}
}

func TestSessionIDStablePerSessionAndRequestIDFresh(t *testing.T) {
	id := testIdentity()
	first := http.Header{}
	second := http.Header{}
	ApplyHeaders(first, DefaultProfile(), id, Turn{})
	ApplyHeaders(second, DefaultProfile(), id, Turn{})

	if hdrGet(first, "X-Claude-Code-Session-Id") != hdrGet(second, "X-Claude-Code-Session-Id") {
		t.Error("session id changed between two requests of one session")
	}
	if hdrGet(first, "x-client-request-id") == hdrGet(second, "x-client-request-id") {
		t.Error("x-client-request-id repeated; the capture shows it fresh per request")
	}

	other := http.Header{}
	ApplyHeaders(other, DefaultProfile(), Identity{SessionKey: "different"}, Turn{})
	if hdrGet(other, "X-Claude-Code-Session-Id") == hdrGet(first, "X-Claude-Code-Session-Id") {
		t.Error("session id identical across two different session keys")
	}
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func TestSessionIDIsUUIDShaped(t *testing.T) {
	h := http.Header{}
	ApplyHeaders(h, DefaultProfile(), testIdentity(), Turn{})
	if got := hdrGet(h, "X-Claude-Code-Session-Id"); !uuidRE.MatchString(got) {
		t.Errorf("session id %q is not UUID-shaped; the capture shows a UUID", got)
	}
}

func TestApplyBodyMetadataUserIDIsTheCapturedJSONObject(t *testing.T) {
	body := []byte(`{"model":"m","messages":[]}`)
	out, err := ApplyBody(body, DefaultProfile(), testIdentity(), Turn{})
	if err != nil {
		t.Fatalf("ApplyBody: %v", err)
	}
	var parsed struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if !strings.HasPrefix(parsed.Metadata.UserID, `{"device_id":"`) {
		t.Fatalf("user_id = %q, want the captured stringified JSON object", parsed.Metadata.UserID)
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(parsed.Metadata.UserID), &payload); err != nil {
		t.Fatalf("user_id is not valid JSON: %v", err)
	}
	if len(payload["device_id"]) != 64 {
		t.Errorf("device_id length = %d, want 64 hex characters as captured", len(payload["device_id"]))
	}
	// Claude Code sends account_uuid EMPTY. Observed in 6 of 6 captured
	// requests across two independent OAuth credentials:
	// .reference/claude-code-headers-20260923.jsonl (3) and
	// .reference/claude-code-headers-20260923-acct2.jsonl (3). Emitting the
	// real account UUID here would be a field no real client populates.
	if payload["account_uuid"] != "" {
		t.Errorf("account_uuid = %q, want empty as captured", payload["account_uuid"])
	}
	if !uuidRE.MatchString(payload["session_id"]) {
		t.Errorf("session_id = %q, want a UUID", payload["session_id"])
	}
}

// Emptying account_uuid must not cost per-account stickiness: device_id is
// derived from the account, so two accounts still present different devices.
func TestApplyBodyDeviceIDStillDiffersPerAccountWithEmptyAccountUUID(t *testing.T) {
	first := testIdentity()
	first.AccountUUID = "11111111-1111-4111-8111-111111111111"
	second := testIdentity()
	second.AccountUUID = "22222222-2222-4222-8222-222222222222"

	read := func(id Identity) (device, account string) {
		out, err := ApplyBody([]byte(`{}`), DefaultProfile(), id, Turn{})
		if err != nil {
			t.Fatalf("ApplyBody: %v", err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(out, &parsed); err != nil {
			t.Fatalf("re-parse: %v", err)
		}
		userID, _ := parsed["metadata"].(map[string]any)["user_id"].(string)
		var payload map[string]string
		if err := json.Unmarshal([]byte(userID), &payload); err != nil {
			t.Fatalf("user_id is not valid JSON: %v", err)
		}
		return payload["device_id"], payload["account_uuid"]
	}

	firstDevice, firstAccount := read(first)
	secondDevice, secondAccount := read(second)

	if firstAccount != "" || secondAccount != "" {
		t.Errorf("account_uuid = %q and %q, want both empty", firstAccount, secondAccount)
	}
	if firstDevice == secondDevice {
		t.Errorf("device_id is %q for both accounts; per-account identity was lost", firstDevice)
	}
}

func TestApplyBodyMetadataUserIDFieldOrderMatchesCapture(t *testing.T) {
	out, err := ApplyBody([]byte(`{}`), DefaultProfile(), testIdentity(), Turn{})
	if err != nil {
		t.Fatalf("ApplyBody: %v", err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(out, &parsed)
	userID, _ := parsed["metadata"].(map[string]any)["user_id"].(string)
	device, account, session := strings.Index(userID, "device_id"), strings.Index(userID, "account_uuid"), strings.Index(userID, "session_id")
	if !(device < account && account < session) {
		t.Errorf("user_id key order = %q, want device_id, account_uuid, session_id", userID)
	}
}

func TestApplyBodyDeviceIDStablePerIdentity(t *testing.T) {
	// Only metadata can be compared: the body also carries a fresh cch per call,
	// so two whole bodies are never equal.
	id := testIdentity()
	userID := func(body []byte) string {
		var parsed struct {
			Metadata struct {
				UserID string `json:"user_id"`
			} `json:"metadata"`
		}
		_ = json.Unmarshal(body, &parsed)
		return parsed.Metadata.UserID
	}
	first, _ := ApplyBody([]byte(`{}`), DefaultProfile(), id, Turn{})
	second, _ := ApplyBody([]byte(`{}`), DefaultProfile(), id, Turn{})
	if userID(first) != userID(second) {
		t.Errorf("metadata differs across turns of one session:\n %s\n %s", userID(first), userID(second))
	}
}

func TestApplyBodyPreservesOtherMetadataKeys(t *testing.T) {
	body := []byte(`{"metadata":{"custom":"keep-me"}}`)
	out, err := ApplyBody(body, DefaultProfile(), testIdentity(), Turn{})
	if err != nil {
		t.Fatalf("ApplyBody: %v", err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(out, &parsed)
	if parsed["metadata"].(map[string]any)["custom"] != "keep-me" {
		t.Error("an unrelated metadata key was dropped")
	}
}

func TestApplyBodyDropsFieldsClaudeCodeDoesNotSend(t *testing.T) {
	body := []byte(`{"model":"m","temperature":0.7,"top_p":0.9,"top_k":40,"stop_sequences":["x"],"tool_choice":{"type":"auto"},"service_tier":"auto"}`)
	out, err := ApplyBody(body, DefaultProfile(), testIdentity(), Turn{})
	if err != nil {
		t.Fatalf("ApplyBody: %v", err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(out, &parsed)
	for _, name := range omittedBodyFields {
		if _, present := parsed[name]; present {
			t.Errorf("%s survived; the capture shows Claude Code does not send it", name)
		}
	}
	if parsed["model"] != "m" {
		t.Error("model was dropped")
	}
}

func TestApplyBodySystemBlockZeroIsTheBillingHeader(t *testing.T) {
	id := testIdentity()
	body := []byte(`{"system":[{"type":"text","text":"original"},{"type":"text","text":"second"}]}`)
	out, err := ApplyBody(body, DefaultProfile(), id, Turn{})
	if err != nil {
		t.Fatalf("ApplyBody: %v", err)
	}
	var parsed struct {
		System []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"system"`
	}
	_ = json.Unmarshal(out, &parsed)
	if len(parsed.System) != 3 {
		t.Fatalf("system has %d blocks, want 3: the marker plus both original blocks", len(parsed.System))
	}
	if !strings.HasPrefix(parsed.System[0].Text, "x-anthropic-billing-header: ") {
		t.Errorf("system[0] = %q, want the billing header", parsed.System[0].Text)
	}
	// The caller's prompt is content this package cannot reconstruct. Claude
	// Code's own block 0 is the marker and its prompt follows, so prepending is
	// both the captured shape and the non-destructive one.
	if parsed.System[1].Text != "original" {
		t.Errorf("system[1] = %q; the caller's first block must survive", parsed.System[1].Text)
	}
	if parsed.System[2].Text != "second" {
		t.Errorf("system[2] = %q; later blocks must be untouched", parsed.System[2].Text)
	}
}

func TestApplyBodyReplacesAnExistingBillingHeader(t *testing.T) {
	// A caller that already is Claude Code sends a marker of its own. Prepending
	// a second one would put two on the wire, which no client does.
	body := []byte(`{"system":[` +
		`{"type":"text","text":"x-anthropic-billing-header: cc_version=1.0.0.abc; cc_entrypoint=sdk-cli; cch=11111; cc_prompt_id=x; cc_turn_origin=sdk;"},` +
		`{"type":"text","text":"real prompt"}]}`)
	out, err := ApplyBody(body, DefaultProfile(), testIdentity(), Turn{})
	if err != nil {
		t.Fatalf("ApplyBody: %v", err)
	}
	var parsed struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
	}
	_ = json.Unmarshal(out, &parsed)
	if len(parsed.System) != 2 {
		t.Fatalf("system has %d blocks, want 2: the caller's marker replaced, not doubled", len(parsed.System))
	}
	if strings.Contains(parsed.System[0].Text, "cc_version=1.0.0.abc") {
		t.Error("the caller's own billing header survived; it must be replaced")
	}
	if parsed.System[1].Text != "real prompt" {
		t.Errorf("system[1] = %q, want the caller's prompt", parsed.System[1].Text)
	}
}

func TestApplyBodyStringSystemIsPromotedToBlocks(t *testing.T) {
	out, err := ApplyBody([]byte(`{"system":"be helpful"}`), DefaultProfile(), testIdentity(), Turn{})
	if err != nil {
		t.Fatalf("ApplyBody: %v", err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(out, &parsed)
	blocks, ok := parsed["system"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("system = %#v, want a two-block array as captured", parsed["system"])
	}
}

func TestApplyBodyRejectsMalformedJSON(t *testing.T) {
	if _, err := ApplyBody([]byte(`{"model":`), DefaultProfile(), testIdentity(), Turn{}); err == nil {
		t.Error("malformed JSON returned no error; forwarding unnormalised bytes is worse than failing")
	}
	if _, err := ApplyBody([]byte(`[1,2]`), DefaultProfile(), testIdentity(), Turn{}); err == nil {
		t.Error("non-object body returned no error")
	}
}

// A Message Threads body cannot be normalized. A continuation carries only the
// messages after thread.previous_message_id (every continuation in
// .reference/claude-code-count-tokens-20261107.jsonl), so TurnOf would read a
// later prompt as the first one, and the captured beta list has no
// message-threads beta for the field to go upstream with. The create form still
// holds the whole conversation, but it carries the same field.
func TestApplyBodyRefusesAMessageThread(t *testing.T) {
	for name, thread := range map[string]string{
		"create":   `{"type":"create"}`,
		"continue": `{"type":"continue","previous_message_id":"msg_01"}`,
	} {
		body := []byte(`{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":"hi"}],"thread":` + thread + `}`)
		if _, err := ApplyBody(body, DefaultProfile(), testIdentity(), TurnOf(body)); !errors.Is(err, ErrMessageThread) {
			t.Errorf("%s: err = %v, want ErrMessageThread", name, err)
		}
	}
}

// Claude Code adds "thread" only when it threads (2.1.311 builds the body with
// ...thread!==null&&{thread:...}), so a null thread asks for nothing and the real
// client never sends one. It is dropped rather than refused: a client whose SDK
// writes every optional field could never resend without it.
func TestApplyBodyDropsANullThread(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":"hi"}],"thread":null}`)
	out, err := ApplyBody(body, DefaultProfile(), testIdentity(), TurnOf(body))
	if err != nil {
		t.Fatalf("ApplyBody: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatalf("normalized body is not JSON: %v", err)
	}
	if _, ok := fields["thread"]; ok {
		t.Errorf("normalized body = %s, want no thread", out)
	}
}

// Claude Code 2.1.311 gives up threading for the session, and resends the turn
// with the whole conversation, on a 400 whose message carries both its
// message-threads beta and "anthropic-beta". The beta named is the client's own,
// so a later dated beta still matches.
func TestMessageThreadRefusalNamesTheClientsBeta(t *testing.T) {
	got := MessageThreadRefusal(http.Header{"Anthropic-Beta": {"claude-code-20250219,message-threads-2026-08-12"}})
	if !strings.Contains(got, "message-threads-2026-08-12") || !strings.Contains(got, "anthropic-beta") {
		t.Errorf("refusal = %q, want it to name message-threads-2026-08-12 and anthropic-beta", got)
	}
	if got := MessageThreadRefusal(http.Header{"Anthropic-Beta": {"claude-code-20250219"}}); strings.Contains(got, "message-threads") {
		t.Errorf("refusal = %q names a beta the client did not send", got)
	}
}

func TestApplyBodyEmptyBodyIsPassedThrough(t *testing.T) {
	out, err := ApplyBody(nil, DefaultProfile(), testIdentity(), Turn{})
	if err != nil || out != nil {
		t.Errorf("ApplyBody(nil) = %v, %v; want nil, nil", out, err)
	}
}

var billingRE = regexp.MustCompile(`^x-anthropic-billing-header: cc_version=` + regexp.QuoteMeta(ClientVersion) + `\.[0-9a-f]{3}; cc_entrypoint=(sdk-cli|cli); cch=[0-9a-f]{5}; cc_prompt_id=[0-9a-f-]{36}; cc_turn_origin=(sdk|cli); cc_prompt_index=0; cc_turn_index=1;$`)

func TestBillingHeaderMatchesCapturedShape(t *testing.T) {
	got := BillingHeader(testIdentity(), Turn{})
	if !billingRE.MatchString(got) {
		t.Errorf("billing header = %q, does not match the captured shape", got)
	}
}

// promptIDRE pulls cc_prompt_id out of a billing header.
var promptIDRE = regexp.MustCompile(`cc_prompt_id=([0-9a-f-]{36})`)

// TestBillingHeaderPromptIDDiffersFromSessionID pins a relation the captures
// prove, which no single-field check can catch.
//
// In all six captured requests across two independent OAuth accounts,
// X-Claude-Code-Session-Id and cc_prompt_id are different UUIDs, and both are
// stable within one process:
//
//	5c1c7cec-… / 7cd5023f-…   ca6f5fe3-… / 9f414346-…   (rows 2, 5 and 6)
//	ccbbdab8-… / b0f9a831-…   2ca893cf-… / fc5d5bcf-…   (acct2, same rows)
//
// A request where the two are equal is one no real Claude Code emits, which is
// exactly the kind of tell this package exists to remove.
func TestBillingHeaderPromptIDDiffersFromSessionID(t *testing.T) {
	id := testIdentity()

	match := promptIDRE.FindStringSubmatch(BillingHeader(id, Turn{}))
	if match == nil {
		t.Fatalf("billing header carries no cc_prompt_id: %q", BillingHeader(id, Turn{}))
	}
	promptID := match[1]

	if promptID == sessionUUID(id) {
		t.Errorf("cc_prompt_id == session UUID (%s); the captures show them always different", promptID)
	}

	h := http.Header{}
	ApplyHeaders(h, DefaultProfile(), id, Turn{})
	if header := hdrGet(h, "X-Claude-Code-Session-Id"); header == promptID {
		t.Errorf("cc_prompt_id == X-Claude-Code-Session-Id (%s); 6 of 6 captured requests differ", header)
	}

	// Stable for one identity, as rows 5 and 6 of each capture show.
	again := promptIDRE.FindStringSubmatch(BillingHeader(id, Turn{}))[1]
	if again != promptID {
		t.Errorf("cc_prompt_id changed within one session: %s then %s", promptID, again)
	}
}

// TestPromptIDHeaderEqualsTheBillingPromptID pins a relation 2.1.304 added:
// x-claude-code-prompt-id carries the same UUID as cc_prompt_id in system[0].
// All 7 captured messages requests agree. Each field is a valid UUID on its
// own, so only a test that reads both can see them drift apart.
func TestPromptIDHeaderEqualsTheBillingPromptID(t *testing.T) {
	id := testIdentity()
	h := http.Header{}
	ApplyHeaders(h, DefaultProfile(), id, Turn{})

	header := hdrGet(h, "x-claude-code-prompt-id")
	if !uuidRE.MatchString(header) {
		t.Fatalf("x-claude-code-prompt-id = %q, want a UUID as captured", header)
	}
	match := promptIDRE.FindStringSubmatch(BillingHeader(id, Turn{}))
	if match == nil {
		t.Fatalf("billing header carries no cc_prompt_id: %q", BillingHeader(id, Turn{}))
	}
	if header != match[1] {
		t.Errorf("x-claude-code-prompt-id = %s, cc_prompt_id = %s; the capture shows one value in both", header, match[1])
	}
	if header == hdrGet(h, "X-Claude-Code-Session-Id") {
		t.Errorf("x-claude-code-prompt-id equals the session id (%s); the capture keeps them distinct", header)
	}
}

func TestBillingHeaderCarriesPrevRequestIDOnFollowUpTurns(t *testing.T) {
	got := BillingHeader(testIdentity(), Turn{PrevRequestID: "req_011CfL3J5mtdJ5DhtF1opYTr"})
	if !strings.Contains(got, "cc_prev_req=req_011CfL3J5mtdJ5DhtF1opYTr; cc_prompt_id=") {
		t.Errorf("billing header = %q; cc_prev_req must sit between cch and cc_prompt_id as captured", got)
	}
	if strings.Contains(BillingHeader(testIdentity(), Turn{}), "cc_prev_req") {
		t.Error("cc_prev_req present on a first turn; the capture only shows it on a follow-up")
	}
	// The client writes the field only for a value matching its own guard.
	injected := BillingHeader(testIdentity(), Turn{PrevRequestID: "req_011; cc_turn_index=9"})
	if strings.Contains(injected, "cc_prev_req") || strings.Contains(injected, "cc_turn_index=9") {
		t.Errorf("billing header = %q; a request id the client's guard rejects must not be written", injected)
	}
}

func TestBillingHeaderEntrypointAndOriginFollowTheIdentity(t *testing.T) {
	id := testIdentity()
	id.Entrypoint = EntrypointInteractive
	id.TurnOrigin = TurnOriginHuman
	got := BillingHeader(id, Turn{})
	if !strings.Contains(got, "cc_entrypoint=cli;") {
		t.Errorf("billing header = %q, want the configured entrypoint", got)
	}
	if !strings.Contains(got, "cc_turn_origin=human;") {
		t.Errorf("billing header = %q, want the configured turn origin", got)
	}
}

// The binary adds 1 to cc_prompt_index only for a turn whose origin is human,
// so a human identity counts prompts in both fields and any other origin keeps
// the index at 0.
func TestBillingHeaderPromptIndexCountsOnlyHumanTurns(t *testing.T) {
	human := testIdentity()
	human.TurnOrigin = TurnOriginHuman
	if got := BillingHeader(human, Turn{PromptNumber: 3}); !strings.Contains(got, "cc_prompt_index=3; cc_turn_index=3;") {
		t.Errorf("human billing header = %q, want prompt and turn index 3", got)
	}
	if got := BillingHeader(testIdentity(), Turn{PromptNumber: 3}); !strings.Contains(got, "cc_prompt_index=0; cc_turn_index=3;") {
		t.Errorf("sdk billing header = %q, want prompt index 0 and turn index 3", got)
	}
}

func TestUserAgentDefaultsToTheCapturedLiteral(t *testing.T) {
	id := Identity{}
	if got := messagesUserAgent(id); got != MessagesUserAgent {
		t.Errorf("User-Agent = %q, want the captured literal %q", got, MessagesUserAgent)
	}
	id.Entrypoint = EntrypointSDKCLI
	if got := messagesUserAgent(id); got != MessagesUserAgent {
		t.Errorf("User-Agent = %q for sdk-cli, want the captured literal", got)
	}
}

func TestUserAgentForTheInteractiveEntrypointIsTheCapturedOne(t *testing.T) {
	// .reference/claude-code-count-tokens-20261107.jsonl records this string
	// on an interactive session's messages and counts.
	got := messagesUserAgent(Identity{Entrypoint: EntrypointInteractive})
	if got != "claude-cli/2.1.322 (external, cli)" {
		t.Errorf("User-Agent = %q, want the captured interactive form", got)
	}
}

func TestOverridesWinWhenSet(t *testing.T) {
	id := Identity{
		UserAgent:               "custom-ua",
		StainlessOS:             "macOS",
		StainlessRuntimeVersion: "v99.0.0",
	}
	h := http.Header{}
	ApplyHeaders(h, DefaultProfile(), id, Turn{})
	if hdrGet(h, "User-Agent") != "custom-ua" {
		t.Errorf("User-Agent = %q, want the override", hdrGet(h, "User-Agent"))
	}
	if hdrGet(h, "X-Stainless-OS") != "macOS" {
		t.Errorf("X-Stainless-OS = %q, want the override", hdrGet(h, "X-Stainless-OS"))
	}
	if hdrGet(h, "X-Stainless-Runtime-Version") != "v99.0.0" {
		t.Errorf("X-Stainless-Runtime-Version = %q, want the override", hdrGet(h, "X-Stainless-Runtime-Version"))
	}
}

func TestDefaultProfilePinsTheCapturedPathAndBetas(t *testing.T) {
	p := DefaultProfile()
	if p.Path != "/v1/messages?beta=true" {
		t.Errorf("Path = %q, want the captured path with its query string", p.Path)
	}
	// 2.1.322 adds tool-provenance-2026-10-26 to the 2.1.311 list
	// (.reference/claude-code-headers capture of 2026-11-10; see
	// Betas in defaults.go, which lists it in the captured order).
	if len(p.Betas) != 15 {
		t.Fatalf("Betas has %d entries, want the captured 15", len(p.Betas))
	}
	if p.Betas[0] != "claude-code-20250219" || p.Betas[1] != "oauth-2025-04-20" {
		t.Errorf("first betas = %q, %q; the capture puts claude-code-20250219 first", p.Betas[0], p.Betas[1])
	}
	if !slices.Contains(p.Betas, advisorBetaPrecedes) {
		t.Errorf("Betas lost %q, the beta Claude Code sends right after the advisor beta; re-derive the advisor position (ADR-0004 §6)", advisorBetaPrecedes)
	}
}

// TestWithClientBetas pins ADR-0004 §6: the client's advisor beta goes right
// before advisorBetaPrecedes, where Claude Code 2.1.311 puts it, and only when
// the client named it exactly.
func TestWithClientBetas(t *testing.T) {
	captured := []string{"claude-code-20250219", "mid-conversation-tool-changes-2026-07-01", advisorBetaPrecedes, "cache-diagnosis-2026-04-07"}
	withAdvisor := []string{"claude-code-20250219", "mid-conversation-tool-changes-2026-07-01", AdvisorToolBeta, advisorBetaPrecedes, "cache-diagnosis-2026-04-07"}
	cases := []struct {
		name   string
		client map[string][]string
		want   []string
	}{
		{"in a list with a foreign beta", map[string][]string{"Anthropic-Beta": {"some-harness-beta, " + AdvisorToolBeta}}, withAdvisor},
		{"on its own header line", map[string][]string{"Anthropic-Beta": {"some-harness-beta", AdvisorToolBeta}}, withAdvisor},
		{"under a non-canonical key", map[string][]string{"anthropic-beta": {AdvisorToolBeta}}, withAdvisor},
		{"a longer name is not the beta", map[string][]string{"Anthropic-Beta": {AdvisorToolBeta + "-preview"}}, captured},
		{"named in another header", map[string][]string{"X-Beta": {AdvisorToolBeta}}, captured},
		{"no client headers", nil, captured},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Profile{Betas: captured}.WithClientBetas(tc.client).Betas
			if !slices.Equal(got, tc.want) {
				t.Errorf("Betas = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWithClientBetasAppendsWithoutItsSuccessor covers a list without
// advisorBetaPrecedes: the beta goes last rather than being dropped.
func TestWithClientBetasAppendsWithoutItsSuccessor(t *testing.T) {
	p := Profile{Betas: []string{"claude-code-20250219", "oauth-2025-04-20"}}
	got := p.WithClientBetas(map[string][]string{"Anthropic-Beta": {AdvisorToolBeta}}).Betas
	if want := []string{"claude-code-20250219", "oauth-2025-04-20", AdvisorToolBeta}; !slices.Equal(got, want) {
		t.Errorf("Betas = %q, want %q", got, want)
	}
}

// TestWithClientBetasDoesNotRepeatACapturedAdvisorBeta covers a recapture whose
// list already carries the beta: appending it again would send it twice.
func TestWithClientBetasDoesNotRepeatACapturedAdvisorBeta(t *testing.T) {
	p := Profile{Betas: []string{"claude-code-20250219", AdvisorToolBeta, "oauth-2025-04-20"}}
	got := p.WithClientBetas(map[string][]string{"Anthropic-Beta": {AdvisorToolBeta}}).Betas
	if !slices.Equal(got, p.Betas) {
		t.Errorf("Betas = %q, want the captured list unchanged %q", got, p.Betas)
	}
}
