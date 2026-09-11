package format

import (
	"os"
	"strings"
)

const geminiThinkingRecoveryEnv = "ANTIGRAVITY_GEMINI_THINKING_RECOVERY"

// geminiThinkingRecoveryEnabled reports whether the legacy synthetic tool-loop
// recovery turns are re-enabled for Gemini thinking models.
//
// Those turns ("[Tool execution completed.]" / "[Continue]") are never stored by
// the client, so the next request puts a real functionCall at the same index and
// Gemini implicit context caching misses every turn. They are off by default.
// Gemini validates thought signatures only for function calls in the current
// turn, and unsigned calls already carry GeminiSkipSignature, so the synthetic
// turns are not needed to pass validation. The switch exists only as an operator
// rollback if a backend rejects that bypass.
//
// scripts/verify-gemini-cache-prefix.sh measures both halves of that claim
// against a live backend. Recorded run, gemini-3.0-flash-high, 2026-09-11:
// turn 2 reused 110529 of 112691 prompt tokens, and turn 3 got a 200 for a
// thought signature the proxy never issued, sitting in history.
func geminiThinkingRecoveryEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(geminiThinkingRecoveryEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
