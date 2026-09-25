package webui

import (
	"os/exec"
	"testing"
)

// TestKimiPollRaceHarness runs the deterministic Node harness covering the
// Kimi OAuth poll/cancel races in models.js and add-account-modal.js.
func TestKimiPollRaceHarness(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping Kimi poll race harness")
	}
	out, err := exec.Command(node, "tests/kimi-poll-race.test.mjs").CombinedOutput()
	if err != nil {
		t.Fatalf("kimi poll race harness failed: %v\n%s", err, out)
	}
}
