package webui

import (
	"os/exec"
	"testing"
)

// TestImportCCDefaultsHarness runs the Node test verifying that Claude 5.5
// defaults imported in the WebUI carry contextLength and maxOutputTokens.
func TestImportCCDefaultsHarness(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping importCCDefaults harness")
	}
	out, err := exec.Command(node, "tests/import-cc-defaults.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("importCCDefaults harness failed: %v\n%s", err, out)
	}
}
