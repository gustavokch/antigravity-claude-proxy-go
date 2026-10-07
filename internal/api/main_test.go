package api

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Isolate tests from the host's real config directory so tests never read or write ~/.config/antigravity-proxy
	if os.Getenv("ANTIGRAVITY_CONFIG_DIR") == "" {
		tmpDir, err := os.MkdirTemp("", "antigravity-api-test-*")
		if err == nil {
			_ = os.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
			defer os.RemoveAll(tmpDir)
		}
	}
	os.Exit(m.Run())
}
