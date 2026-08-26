package openrouter

import (
	"net/http"
	"strings"
)

// OpenRouter attributes API-key requests to an app via self-declared headers
// and gates certain models (typically free tiers) to agentic harnesses —
// requests without attribution get a 403 permission_error.
const (
	// SpoofAppRefererHeader declares the calling app's URL for OpenRouter attribution.
	SpoofAppRefererHeader = "HTTP-Referer"
	// SpoofAppRefererLegacyHeader provides standard HTTP referer header compatibility.
	SpoofAppRefererLegacyHeader = "Referer"
	// SpoofAppTitleHeader declares the calling app's display name.
	SpoofAppTitleHeader = "X-OpenRouter-Title"
	// SpoofAppTitleLegacyHeader declares the legacy title header.
	SpoofAppTitleLegacyHeader = "X-Title"
	// SpoofAppCategoriesHeader declares the calling app's categories.
	SpoofAppCategoriesHeader = "X-OpenRouter-Categories"

	// DefaultSpoofAppTitle is the app identity used when no config override exists.
	DefaultSpoofAppTitle = "Claude Code"
	// DefaultSpoofAppCategories self-identifies as a CLI coding agent.
	DefaultSpoofAppCategories = "cli-agent"
	// DefaultSpoofAppReferer is the attribution URL used when no config override exists.
	DefaultSpoofAppReferer = "https://claude.ai/code"
)

// ApplySpoofHeaders injects OpenRouter app attribution headers into an HTTP request.
// If title, categories, or referer are empty or whitespace, the corresponding default value is used.
func ApplySpoofHeaders(req *http.Request, title, categories, referer string) {
	if req == nil {
		return
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = DefaultSpoofAppTitle
	}
	categories = strings.TrimSpace(categories)
	if categories == "" {
		categories = DefaultSpoofAppCategories
	}
	referer = strings.TrimSpace(referer)
	if referer == "" {
		referer = DefaultSpoofAppReferer
	}

	req.Header.Set(SpoofAppRefererHeader, referer)
	req.Header.Set(SpoofAppRefererLegacyHeader, referer)
	req.Header.Set(SpoofAppTitleHeader, title)
	req.Header.Set(SpoofAppTitleLegacyHeader, title)
	req.Header.Set(SpoofAppCategoriesHeader, categories)
}

// IsHarnessGateError reports whether an upstream error body is OpenRouter's
// "model is only available on agentic harnesses" permission error.
func IsHarnessGateError(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "agentic harness")
}
