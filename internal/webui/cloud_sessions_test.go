package webui

import (
	"os/exec"
	"testing"
)

// TestCloudSessionsHarness runs the Node tests for the Settings > Cloud tab
// component: polling, the enable toggle and the CA download.
func TestCloudSessionsHarness(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping cloud sessions harness")
	}
	out, err := exec.Command(node, "tests/cloud-sessions.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("cloud sessions harness failed: %v\n%s", err, out)
	}
}
