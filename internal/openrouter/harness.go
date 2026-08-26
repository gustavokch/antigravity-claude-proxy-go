package openrouter

import "strings"

// OpenRouter attributes API-key requests to an app via self-declared headers
// and gates certain models (typically free tiers) to agentic harnesses —
// requests without attribution get a 403 permission_error.
const (
	// SpoofAppTitleHeader declares the calling app's display name.
	SpoofAppTitleHeader = "X-OpenRouter-Title"
	// SpoofAppCategoriesHeader declares the calling app's categories.
	SpoofAppCategoriesHeader = "X-OpenRouter-Categories"

	// DefaultSpoofAppTitle is the app identity used when no config override exists.
	DefaultSpoofAppTitle = "Claude Code"
	// DefaultSpoofAppCategories self-identifies as a CLI coding agent.
	DefaultSpoofAppCategories = "cli-agent"
)

// IsHarnessGateError reports whether an upstream error body is OpenRouter's
// "model is only available on agentic harnesses" permission error.
func IsHarnessGateError(body []byte) bool {
	return strings.Contains(strings.ToLower(string(body)), "only available on agentic harnesses")
}
