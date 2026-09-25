package kimi

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// CLIVersion is the proxy's own client-version token, sent in the KimiCLI
// identity headers. oh-my-pi sends its own package version in the same slot.
const CLIVersion = "1.0.0"

var (
	deviceIDMu     sync.Mutex
	fallbackDevIDs = map[string]string{}
)

// IdentityHeaders returns the KimiCLI fingerprint headers. deviceID is a
// persistent 32-hex identifier (see EnsureDeviceID); every value is sanitized
// to printable ASCII.
func IdentityHeaders(deviceID string) http.Header {
	release, version := osInfo()
	hostname, err := os.Hostname()
	if err != nil {
		hostname = ""
	}
	header := http.Header{}
	header.Set("User-Agent", "KimiCLI/"+CLIVersion)
	header.Set("X-Msh-Platform", "kimi_cli")
	header.Set("X-Msh-Version", CLIVersion)
	header.Set("X-Msh-Device-Name", sanitizeHeaderValue(hostname))
	header.Set("X-Msh-Device-Model", sanitizeHeaderValue(deviceModel(release)))
	header.Set("X-Msh-Os-Version", sanitizeHeaderValue(version))
	header.Set("X-Msh-Device-Id", sanitizeHeaderValue(deviceID))
	return header
}

func deviceModel(release string) string {
	label := runtime.GOOS
	switch runtime.GOOS {
	case "darwin":
		label = "macOS"
	case "windows":
		label = "Windows"
	case "linux":
		label = "Linux"
	}
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x64"
	case "386":
		arch = "ia32"
	}
	return joinNonEmpty(label, release, arch)
}

func joinNonEmpty(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}

// sanitizeHeaderValue strips bytes outside 0x20-0x7E, trims whitespace, and
// turns an empty result into "unknown".
func sanitizeHeaderValue(v string) string {
	var b strings.Builder
	for i := range len(v) {
		if v[i] >= 0x20 && v[i] <= 0x7E {
			b.WriteByte(v[i])
		}
	}
	s := strings.TrimSpace(b.String())
	if s == "" {
		return "unknown"
	}
	return s
}

// EnsureDeviceID returns the persistent device id stored in dir/kimi-device-id,
// generating and persisting a new 32-hex one when missing. Persistence is
// best-effort: on failure the id is cached in-process per dir.
func EnsureDeviceID(dir string) string {
	deviceIDMu.Lock()
	defer deviceIDMu.Unlock()

	if id, ok := fallbackDevIDs[dir]; ok {
		return id
	}

	path := filepath.Join(dir, "kimi-device-id")
	if data, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id
		}
	}

	buf := make([]byte, 16)
	_, randErr := rand.Read(buf)
	id := hex.EncodeToString(buf)

	if err := os.MkdirAll(dir, 0o700); err == nil && randErr == nil {
		if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
			fallbackDevIDs[dir] = id
		}
	} else {
		fallbackDevIDs[dir] = id
	}
	return id
}
