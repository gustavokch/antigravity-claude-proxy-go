package webui

import (
	"os/exec"
	"testing"
)

// TestZenHarnessIdentityHarness runs the Node tests for the Zen panel's
// harness identity fields in models.js.
func TestZenHarnessIdentityHarness(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping zen harness identity harness")
	}
	out, err := exec.Command(node, "tests/zen-harness-identity.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("zen harness identity harness failed: %v\n%s", err, out)
	}
}
