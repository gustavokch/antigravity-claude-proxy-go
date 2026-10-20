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

// HostOS and HostArch answer what the Stainless SDK would put in the header on
// that machine. The SDK reads Node's process.platform and process.arch and
// normalizes them (detect-platform.ts, normalizePlatform and normalizeArch), so
// each row is a Go name, Node's name for the same system, and the SDK's verdict.
// scripts/test_diff_claude_code_identity.py holds the same hosts under the names
// Python reports for them; the two tables have to agree, or the gate reports a
// drift that is not one.
func TestHostSpellingsMatchTheStainlessSDK(t *testing.T) {
	for _, row := range []struct{ goos, want string }{
		{"darwin", "MacOS"},
		{"linux", "Linux"},
		{"windows", "Windows"}, // Node: win32
		{"freebsd", "FreeBSD"},
		{"openbsd", "OpenBSD"},
		{"android", "Android"},
		{"ios", "iOS"},
		{"netbsd", "Other:netbsd"},
		{"aix", "Other:aix"},
		{"solaris", "Other:sunos"}, // Node: sunos, for illumos too
		{"illumos", "Other:sunos"},
		{"dragonfly", "Other:dragonfly"},
		{"plan9", "Other:plan9"},
	} {
		if got := HostOS(row.goos); got != row.want {
			t.Errorf("HostOS(%q) = %q, want %q", row.goos, got, row.want)
		}
	}
	for _, row := range []struct{ goarch, want string }{
		{"amd64", "x64"},
		{"arm64", "arm64"},
		{"arm", "arm"},
		{"386", "other:ia32"}, // Node never says x32; it says ia32
		{"ppc64", "other:ppc64"},
		{"ppc64le", "other:ppc64"}, // Node does not tell the byte orders apart
		{"mips", "other:mips"},
		{"mipsle", "other:mipsel"},
		{"mips64le", "other:mips64el"},
		{"loong64", "other:loong64"},
		{"riscv64", "other:riscv64"},
		{"s390x", "other:s390x"},
		{"wasm", "other:wasm"},
	} {
		if got := HostArch(row.goarch); got != row.want {
			t.Errorf("HostArch(%q) = %q, want %q", row.goarch, got, row.want)
		}
	}
}

// The two SDK functions are ported unchanged, including the spellings Node never
// produces, so that a Deno or Bun runtime string and the SDK's own rules read
// the same here as there.
func TestSDKNormalizersFollowDetectPlatform(t *testing.T) {
	for in, want := range map[string]string{
		"x32": "x32", "x86_64": "x64", "x64": "x64", "arm": "arm",
		"aarch64": "arm64", "arm64": "arm64", "ia32": "other:ia32", "": "unknown",
		// normalizeArch does not lowercase: only the platform does.
		"ARM64": "other:ARM64",
	} {
		if got := normalizeArch(in); got != want {
			t.Errorf("normalizeArch(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"darwin": "MacOS", "DARWIN": "MacOS", "win32": "Windows", "linux": "Linux",
		"freebsd": "FreeBSD", "openbsd": "OpenBSD", "android": "Android",
		"ios": "iOS", "iOS": "iOS", "xios": "iOS",
		"sunos": "Other:sunos", "SunOS": "Other:sunos", "": "Unknown",
	} {
		if got := normalizePlatform(in); got != want {
			t.Errorf("normalizePlatform(%q) = %q, want %q", in, got, want)
		}
	}
}
