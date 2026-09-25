package kimi

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestIdentityHeaders(t *testing.T) {
	h := IdentityHeaders("abc")
	if got := h.Get("X-Msh-Platform"); got != "kimi_cli" {
		t.Errorf("X-Msh-Platform = %q, want kimi_cli", got)
	}
	if got := h.Get("User-Agent"); got != "KimiCLI/"+CLIVersion {
		t.Errorf("User-Agent = %q, want KimiCLI/%s", got, CLIVersion)
	}
	if got := h.Get("X-Msh-Device-Id"); got != "abc" {
		t.Errorf("X-Msh-Device-Id = %q, want abc", got)
	}
	printable := regexp.MustCompile(`^[\x20-\x7E]+$`)
	for _, key := range []string{
		"User-Agent", "X-Msh-Platform", "X-Msh-Version", "X-Msh-Device-Name",
		"X-Msh-Device-Model", "X-Msh-Os-Version", "X-Msh-Device-Id",
	} {
		got := h.Get(key)
		if got == "" {
			t.Errorf("%s is empty", key)
		} else if !printable.MatchString(got) {
			t.Errorf("%s = %q, not printable ASCII", key, got)
		}
	}
}

func TestSanitizeHeaderValue(t *testing.T) {
	if got := sanitizeHeaderValue("h\x01ost\u00e9 "); got != "host" {
		t.Errorf("sanitizeHeaderValue = %q, want host", got)
	}
	if got := sanitizeHeaderValue("\x02"); got != "unknown" {
		t.Errorf("sanitizeHeaderValue = %q, want unknown", got)
	}
}

func TestEnsureDeviceID(t *testing.T) {
	dir := t.TempDir()
	id := EnsureDeviceID(dir)
	if matched, _ := regexp.MatchString(`^[0-9a-f]{32}$`, id); !matched {
		t.Errorf("EnsureDeviceID = %q, want 32 hex chars", id)
	}
	if again := EnsureDeviceID(dir); again != id {
		t.Errorf("second call = %q, want %q", again, id)
	}
	data, err := os.ReadFile(filepath.Join(dir, "kimi-device-id"))
	if err != nil {
		t.Fatalf("device id file missing: %v", err)
	}
	if string(data) != id+"\n" {
		t.Errorf("file contents = %q, want %q", string(data), id+"\n")
	}
}

func TestEnsureDeviceIDUnwritableDir(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(blocker, "sub") // MkdirAll fails: parent is a regular file
	id := EnsureDeviceID(dir)
	if again := EnsureDeviceID(dir); again != id {
		t.Errorf("unwritable dir: two calls = %q and %q, want same", id, again)
	}
	if id == "" {
		t.Error("unwritable dir returned empty id")
	}
}
