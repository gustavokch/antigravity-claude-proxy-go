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
		t.Fatal("Dynamic is empty; the captured profile has two dynamic headers")
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
	if len(p.Dynamic) != 2 {
		t.Fatalf("Dynamic has %d entries, want the captured 2", len(p.Dynamic))
	}
	if p.Dynamic[0].Name != "X-Claude-Code-Session-Id" || p.Dynamic[1].Name != "x-client-request-id" {
		t.Errorf("Dynamic names = %q, %q", p.Dynamic[0].Name, p.Dynamic[1].Name)
	}
	// The dynamic values are still computed per request: the session header is
	// derived from the identity and the request id is fresh each time.
	id := Identity{SessionKey: "session-a"}
	if got := p.Dynamic[1].Value(id, Turn{}); got == p.Dynamic[1].Value(id, Turn{}) {
		t.Error("x-client-request-id repeated; a shared profile must not freeze its values")
	}
	if p.Dynamic[0].Value(id, Turn{}) != p.Dynamic[0].Value(id, Turn{}) {
		t.Error("X-Claude-Code-Session-Id is unstable for one identity")
	}
}
