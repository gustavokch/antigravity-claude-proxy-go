package ccidentity

import "testing"

// TestDefaultProfileIsBuiltOnce pins that the captured profile is a value, not
// a constructor.
//
// Omit and Betas were already package-level slices, so they were shared before
// this test existed; Dynamic was rebuilt on every call, allocating a slice and
// two closures. internal/claudecode/client.go calls DefaultProfile three times
// per request and internal/api/server.go twice, so the allocation was on the
// hot path for no gain: nothing in the profile varies per call. The values that
// do vary with the identity live in StaticFor, which stays a function.
func TestDefaultProfileIsBuiltOnce(t *testing.T) {
	first, second := DefaultProfile(), DefaultProfile()

	if len(first.Dynamic) == 0 {
		t.Fatal("Dynamic is empty; the captured profile has dynamic headers")
	}
	if &first.Dynamic[0] != &second.Dynamic[0] {
		t.Error("Dynamic is reallocated on every call; the profile is a constant and should be built once")
	}
	if &first.Omit[0] != &second.Omit[0] {
		t.Error("Omit is reallocated on every call")
	}
	if &first.Betas[0] != &second.Betas[0] {
		t.Error("Betas is reallocated on every call")
	}
}

// TestDefaultProfileFieldsAreIntact guards the hoisting: a package-level value
// built at init must carry exactly what the constructor did.
func TestDefaultProfileFieldsAreIntact(t *testing.T) {
	p := DefaultProfile()

	if p.Path != MessagesPath {
		t.Errorf("Path = %q, want %q", p.Path, MessagesPath)
	}
	if len(p.Betas) != len(Betas) {
		t.Errorf("Betas has %d entries, want the captured %d", len(p.Betas), len(Betas))
	}
	if len(p.Omit) != len(omittedHeaders) {
		t.Errorf("Omit has %d entries, want %d", len(p.Omit), len(omittedHeaders))
	}
	dynamic := map[string]DynamicHeader{}
	for _, header := range p.Dynamic {
		dynamic[header.Name] = header
	}
	for _, name := range []string{"X-Claude-Code-Session-Id", "x-claude-code-prompt-id", "x-client-request-id"} {
		if _, ok := dynamic[name]; !ok {
			t.Fatalf("Dynamic has no %q; names present: %v", name, p.Dynamic)
		}
	}
	// The dynamic values are still computed per request: the session and prompt
	// headers are derived from the identity and the request id is fresh each time.
	id := Identity{SessionKey: "session-a"}
	requestID := dynamic["x-client-request-id"]
	if got := requestID.Value(id, Turn{}); got == requestID.Value(id, Turn{}) {
		t.Error("x-client-request-id repeated; a shared profile must not freeze its values")
	}
	for _, name := range []string{"X-Claude-Code-Session-Id", "x-claude-code-prompt-id"} {
		if dynamic[name].Value(id, Turn{}) != dynamic[name].Value(id, Turn{}) {
			t.Errorf("%s is unstable for one identity", name)
		}
	}
}
