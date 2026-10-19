package webui

import (
	"os/exec"
	"testing"
)

// TestRouteMimicHarness runs the Node tests for the shell's route diagram in
// route-mimic.js.
func TestRouteMimicHarness(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping route mimic harness")
	}
	out, err := exec.Command(node, "tests/route-mimic.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("route mimic harness failed: %v\n%s", err, out)
	}
}
