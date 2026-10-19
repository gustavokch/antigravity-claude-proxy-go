package webui

import (
	"os/exec"
	"testing"
)

// TestAccountExportHarness runs the Node tests for the credential export
// confirmation in account-manager.js.
func TestAccountExportHarness(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping account export harness")
	}
	out, err := exec.Command(node, "tests/account-export.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("account export harness failed: %v\n%s", err, out)
	}
}
