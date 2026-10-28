package webui

import (
	"os/exec"
	"testing"
)

// TestZenConfigLoadHarness runs the Node harness proving a Zen save started
// from a direct Settings → Models load keeps the persisted key and allowlist.
func TestZenConfigLoadHarness(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping Zen config load harness")
	}
	out, err := exec.Command(node, "tests/zen-config-load.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("zen config load harness failed: %v\n%s", err, out)
	}
}
