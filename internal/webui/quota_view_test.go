package webui

import (
	"os/exec"
	"testing"
)

// TestQuotaViewHarness runs the Node tests for the quota-pool and Claude
// usage-window helpers in account-manager.js.
func TestQuotaViewHarness(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping quota view harness")
	}
	out, err := exec.Command(node, "tests/quota-view.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("quota view harness failed: %v\n%s", err, out)
	}
}
