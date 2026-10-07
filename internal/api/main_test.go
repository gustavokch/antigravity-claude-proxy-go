package api

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "antigravity-api-test-*")
	if err == nil {
		_ = os.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	}
	code := m.Run()
	if tmpDir != "" {
		_ = os.RemoveAll(tmpDir)
	}
	os.Exit(code)
}

func TestConfigIsolationInTestMain(t *testing.T) {
	cfgDir := os.Getenv("ANTIGRAVITY_CONFIG_DIR")
	if cfgDir == "" {
		t.Fatal("expected ANTIGRAVITY_CONFIG_DIR to be set by TestMain")
	}
	home, _ := os.UserHomeDir()
	if home != "" && cfgDir == filepath.Join(home, ".config", "antigravity-proxy") {
		t.Fatalf("ANTIGRAVITY_CONFIG_DIR points to real host config directory %s", cfgDir)
	}

	// Subprocess test to prove that when ANTIGRAVITY_CONFIG_DIR is set to real host config directory
	// in the parent environment, TestMain unconditionally overrides it with an isolated temp directory.
	if os.Getenv("TEST_SUBPROCESS_CONFIG_OVERRIDE") == "1" {
		realHostDir := filepath.Join(home, ".config", "antigravity-proxy")
		if cfgDir == realHostDir {
			t.Fatalf("TestMain failed to unconditionally override parent ANTIGRAVITY_CONFIG_DIR %s", cfgDir)
		}
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestConfigIsolationInTestMain")
	realHostDir := filepath.Join(home, ".config", "antigravity-proxy")
	cmd.Env = append(os.Environ(), "ANTIGRAVITY_CONFIG_DIR="+realHostDir, "TEST_SUBPROCESS_CONFIG_OVERRIDE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("subprocess failed: %v\nOutput: %s", err, string(out))
	}
}
