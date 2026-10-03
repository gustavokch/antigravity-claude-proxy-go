package webui

import (
	"os/exec"
	"testing"
)

// TestClassifierConfigHarness runs the Node tests for the classifier settings
// component in classifier-config.js.
func TestClassifierConfigHarness(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping classifier config harness")
	}
	out, err := exec.Command(node, "tests/classifier-config.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("classifier config harness failed: %v\n%s", err, out)
	}
}
