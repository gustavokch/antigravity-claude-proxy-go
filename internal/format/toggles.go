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
func geminiThinkingRecoveryEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(geminiThinkingRecoveryEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
