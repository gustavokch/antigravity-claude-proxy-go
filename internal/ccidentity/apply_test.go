package ccidentity

import (
	"encoding/json"
	"net/http"
	"regexp"
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
// .reference/claude-code-headers-20260923.txt. It is duplicated here on purpose:
// a test that reads the artifact at run time would pass even if both the
// artifact and the implementation were changed together, and this test's job is
// to fail when the implementation drifts from the recorded bytes.
var capturedHeaders = map[string]string{
	"Accept":                                    "application/json",
	"Content-Type":                              "application/json",
	"X-Stainless-Arch":                          "arm64",
	"X-Stainless-Lang":                          "js",
	"X-Stainless-OS":                            "Linux",
	"X-Stainless-Package-Version":               "0.112.1",
	"X-Stainless-Retry-Count":                   "0",
	"X-Stainless-Runtime":                       "node",
	"X-Stainless-Runtime-Version":               "v26.3.0",
	"X-Stainless-Timeout":                       "600",
	"anthropic-dangerous-direct-browser-access": "true",
	"anthropic-version":                         "2023-06-01",
	"x-app":                                     "cli",
	"x-claude-code-request-class":               "main",
}

func TestApplyHeadersSendsTheCapturedSet(t *testing.T) {
	h := http.Header{}
	ApplyHeaders(h, DefaultProfile(), testIdentity(), Turn{})

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
	if payload["account_uuid"] != testIdentity().AccountUUID {
		t.Errorf("account_uuid = %q, want the identity's", payload["account_uuid"])
	}
	if !uuidRE.MatchString(payload["session_id"]) {
		t.Errorf("session_id = %q, want a UUID", payload["session_id"])
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
	if len(parsed.System) != 2 {
		t.Fatalf("system has %d blocks, want 2", len(parsed.System))
	}
	if !strings.HasPrefix(parsed.System[0].Text, "x-anthropic-billing-header: ") {
		t.Errorf("system[0] = %q, want the billing header", parsed.System[0].Text)
	}
	if parsed.System[1].Text != "second" {
		t.Errorf("system[1] = %q; later blocks must be untouched", parsed.System[1].Text)
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

func TestApplyBodyEmptyBodyIsPassedThrough(t *testing.T) {
	out, err := ApplyBody(nil, DefaultProfile(), testIdentity(), Turn{})
	if err != nil || out != nil {
		t.Errorf("ApplyBody(nil) = %v, %v; want nil, nil", out, err)
	}
}

var billingRE = regexp.MustCompile(`^x-anthropic-billing-header: cc_version=2\.1\.280\.[0-9a-f]{3}; cc_entrypoint=(sdk-cli|cli); cch=[0-9a-f]{5}; cc_prompt_id=[0-9a-f-]{36}; cc_turn_origin=(sdk|cli);$`)

func TestBillingHeaderMatchesCapturedShape(t *testing.T) {
	got := BillingHeader(testIdentity(), Turn{})
	if !billingRE.MatchString(got) {
		t.Errorf("billing header = %q, does not match the captured shape", got)
	}
}

func TestBillingHeaderCCHIsFreshPerRequest(t *testing.T) {
	id := testIdentity()
	first := BillingHeader(id, Turn{})
	second := BillingHeader(id, Turn{})
	if first == second {
		t.Error("two requests produced identical billing headers; cch must be fresh")
	}
	// Everything except cch is stable within one session.
	strip := func(s string) string {
		return regexp.MustCompile(`cch=[0-9a-f]{5}`).ReplaceAllString(s, "cch=X")
	}
	if strip(first) != strip(second) {
		t.Errorf("only cch should vary within a session:\n %q\n %q", first, second)
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
}

func TestBillingHeaderEntrypointAndOriginFollowTheIdentity(t *testing.T) {
	id := testIdentity()
	id.Entrypoint = EntrypointInteractive
	id.TurnOrigin = "cli"
	got := BillingHeader(id, Turn{})
	if !strings.Contains(got, "cc_entrypoint=cli;") {
		t.Errorf("billing header = %q, want the configured entrypoint", got)
	}
	if !strings.Contains(got, "cc_turn_origin=cli;") {
		t.Errorf("billing header = %q, want the configured turn origin", got)
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

func TestUserAgentConstructionForOtherEntrypointsIsInferred(t *testing.T) {
	// No capture confirms this string; the test pins the construction so a
	// change is deliberate rather than accidental.
	got := messagesUserAgent(Identity{Entrypoint: EntrypointInteractive})
	if got != "claude-cli/2.1.280 (external, cli)" {
		t.Errorf("User-Agent = %q, want the constructed form", got)
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
	if len(p.Betas) != 13 {
		t.Fatalf("Betas has %d entries, want the captured 13", len(p.Betas))
	}
	if p.Betas[0] != "claude-code-20250219" || p.Betas[1] != "oauth-2025-04-20" {
		t.Errorf("first betas = %q, %q; the capture puts claude-code-20250219 first", p.Betas[0], p.Betas[1])
	}
}
