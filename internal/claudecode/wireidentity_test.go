package claudecode

import (
	"encoding/json"
	"net/http"
	"regexp"
	"testing"

	"antigravity-go-proxy/internal/ccidentity"
)

var ccPromptIDInBillingHeader = regexp.MustCompile(`cc_prompt_id=([0-9a-f-]{36})`)

// wireValuesOf applies the identity to a header set and a body the way
// SendMessage does, and reads back the two pairs that must agree.
func wireValuesOf(t *testing.T, identity ccidentity.Identity) (headerPrompt, billingPrompt, headerSession, bodySession string) {
	t.Helper()

	profile := ccidentity.DefaultProfile()
	request := []byte(`{"model":"m","messages":[]}`)
	turn := ccidentity.TurnOf(request)
	headers := http.Header{}
	ccidentity.ApplyHeaders(headers, profile, identity, turn)

	out, err := ccidentity.ApplyBody(request, profile, identity, turn)
	if err != nil {
		t.Fatalf("ApplyBody: %v", err)
	}
	var body struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.System) == 0 {
		t.Fatalf("body has no system block: %s", out)
	}
	match := ccPromptIDInBillingHeader.FindStringSubmatch(body.System[0].Text)
	if match == nil {
		t.Fatalf("system[0] carries no cc_prompt_id: %q", body.System[0].Text)
	}
	var user struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(body.Metadata.UserID), &user); err != nil {
		t.Fatalf("decode metadata.user_id %q: %v", body.Metadata.UserID, err)
	}

	return headers["x-claude-code-prompt-id"][0], match[1], headers["X-Claude-Code-Session-Id"][0], user.SessionID
}

// TestWireIdentity_UnkeyedRequestAgreesAcrossHeadersAndBody pins the failure the
// live identity gate found: a request with no session key has nothing to derive
// its UUIDs from, and ApplyHeaders and ApplyBody are separate calls, so each
// drew its own random value. The capture shows one value in each pair, in every
// request.
func TestWireIdentity_UnkeyedRequestAgreesAcrossHeadersAndBody(t *testing.T) {
	for _, key := range []string{"", "   "} {
		identity, ok := IdentityConfig{}.WireIdentity("", key)
		if !ok {
			t.Fatal("WireIdentity reported normalization off for the zero config")
		}
		headerPrompt, billingPrompt, headerSession, bodySession := wireValuesOf(t, identity)
		if headerPrompt != billingPrompt {
			t.Errorf("key %q: x-claude-code-prompt-id = %s, cc_prompt_id = %s; one value in both slots", key, headerPrompt, billingPrompt)
		}
		if headerSession != bodySession {
			t.Errorf("key %q: X-Claude-Code-Session-Id = %s, metadata session_id = %s; one value in both slots", key, headerSession, bodySession)
		}
	}
}

// TestWireIdentity_UnkeyedRequestsAreNotOneIdentity guards the other side of the
// fix: seeding must stay random, because a constant seed would hand every
// unkeyed request the same session id and the same prompt id.
func TestWireIdentity_UnkeyedRequestsAreNotOneIdentity(t *testing.T) {
	first, _ := IdentityConfig{}.WireIdentity("", "")
	second, _ := IdentityConfig{}.WireIdentity("", "")

	_, _, firstSession, _ := wireValuesOf(t, first)
	_, _, secondSession, _ := wireValuesOf(t, second)
	if firstSession == secondSession {
		t.Errorf("two unkeyed requests share session id %s", firstSession)
	}
}

// TestWireIdentity_KeyedRequestKeepsItsKey pins that a real session key still
// drives the derivation, so sticky routing and the wire identity keep agreeing.
func TestWireIdentity_KeyedRequestKeepsItsKey(t *testing.T) {
	first, _ := IdentityConfig{}.WireIdentity("", "session-abc")
	second, _ := IdentityConfig{}.WireIdentity("", "session-abc")

	_, _, firstSession, _ := wireValuesOf(t, first)
	_, _, secondSession, _ := wireValuesOf(t, second)
	if firstSession != secondSession {
		t.Errorf("one session key gave two session ids: %s, %s", firstSession, secondSession)
	}
}
