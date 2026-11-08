package webui

import (
	"os/exec"
	"testing"
)

// TestCodexGatewayHarness runs the Node tests for the Codex login flow,
// account actions and allowlist editor.
func TestCodexGatewayHarness(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping codex gateway harness")
	}
	out, err := exec.Command(node, "tests/codex-gateway.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("codex gateway harness failed: %v\n%s", err, out)
	}
}
