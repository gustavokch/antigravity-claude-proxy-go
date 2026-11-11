package openrouter

import (
	"net/http"
	"testing"
)

func TestDefaultSpoofConstants(t *testing.T) {
	if DefaultSpoofAppReferer != "https://claude.ai/code" {
		t.Errorf("DefaultSpoofAppReferer = %q, want %q", DefaultSpoofAppReferer, "https://claude.ai/code")
	}
	if DefaultSpoofAppTitle != "Claude Code" {
		t.Errorf("DefaultSpoofAppTitle = %q, want %q", DefaultSpoofAppTitle, "Claude Code")
	}
	if DefaultSpoofAppCategories != "cli-agent" {
		t.Errorf("DefaultSpoofAppCategories = %q, want %q", DefaultSpoofAppCategories, "cli-agent")
	}
}

func TestApplySpoofHeaders(t *testing.T) {
	t.Run("default values", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", nil)
		ApplySpoofHeaders(req, "", "", "")

		if got := req.Header.Get(SpoofAppRefererHeader); got != DefaultSpoofAppReferer {
			t.Errorf("HTTP-Referer = %q, want %q", got, DefaultSpoofAppReferer)
		}
		if got := req.Header.Get(SpoofAppRefererLegacyHeader); got != DefaultSpoofAppReferer {
			t.Errorf("Referer = %q, want %q", got, DefaultSpoofAppReferer)
		}
		if got := req.Header.Get(SpoofAppTitleHeader); got != DefaultSpoofAppTitle {
			t.Errorf("X-OpenRouter-Title = %q, want %q", got, DefaultSpoofAppTitle)
		}
		if got := req.Header.Get(SpoofAppTitleLegacyHeader); got != DefaultSpoofAppTitle {
			t.Errorf("X-Title = %q, want %q", got, DefaultSpoofAppTitle)
		}
		if got := req.Header.Get(SpoofAppCategoriesHeader); got != DefaultSpoofAppCategories {
			t.Errorf("X-OpenRouter-Categories = %q, want %q", got, DefaultSpoofAppCategories)
		}
	})

	t.Run("whitespace-only values fall back to defaults", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", nil)
		ApplySpoofHeaders(req, "   ", " \t\n ", "  ")

		if got := req.Header.Get(SpoofAppRefererHeader); got != DefaultSpoofAppReferer {
			t.Errorf("HTTP-Referer = %q, want %q", got, DefaultSpoofAppReferer)
		}
		if got := req.Header.Get(SpoofAppRefererLegacyHeader); got != DefaultSpoofAppReferer {
			t.Errorf("Referer = %q, want %q", got, DefaultSpoofAppReferer)
		}
		if got := req.Header.Get(SpoofAppTitleHeader); got != DefaultSpoofAppTitle {
			t.Errorf("X-OpenRouter-Title = %q, want %q", got, DefaultSpoofAppTitle)
		}
		if got := req.Header.Get(SpoofAppTitleLegacyHeader); got != DefaultSpoofAppTitle {
			t.Errorf("X-Title = %q, want %q", got, DefaultSpoofAppTitle)
		}
		if got := req.Header.Get(SpoofAppCategoriesHeader); got != DefaultSpoofAppCategories {
			t.Errorf("X-OpenRouter-Categories = %q, want %q", got, DefaultSpoofAppCategories)
		}
	})

	t.Run("custom values", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", nil)
		ApplySpoofHeaders(req, "My App", "agentic-coding", "https://example.com/app")

		if got := req.Header.Get(SpoofAppRefererHeader); got != "https://example.com/app" {
			t.Errorf("HTTP-Referer = %q, want %q", got, "https://example.com/app")
		}
		if got := req.Header.Get(SpoofAppRefererLegacyHeader); got != "https://example.com/app" {
			t.Errorf("Referer = %q, want %q", got, "https://example.com/app")
		}
		if got := req.Header.Get(SpoofAppTitleHeader); got != "My App" {
			t.Errorf("X-OpenRouter-Title = %q, want %q", got, "My App")
		}
		if got := req.Header.Get(SpoofAppTitleLegacyHeader); got != "My App" {
			t.Errorf("X-Title = %q, want %q", got, "My App")
		}
		if got := req.Header.Get(SpoofAppCategoriesHeader); got != "agentic-coding" {
			t.Errorf("X-OpenRouter-Categories = %q, want %q", got, "agentic-coding")
		}
	})

	t.Run("nil request does not panic", func(t *testing.T) {
		ApplySpoofHeaders(nil, "title", "cat", "ref")
	})
}

func TestIsHarnessGateError(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "exact gate error",
			body: `{"type":"error","error":{"type":"permission_error","message":"thinkingmachines/inkling:free is only available on agentic harnesses. Try plugging it into a coding agent or productivity app listed on https://openrouter.ai/apps","error_type":"permission_denied"}}`,
			want: true,
		},
		{
			name: "alternate phrasing with agentic harness and openrouter.ai/apps",
			body: `{"error":{"message":"Requires an agentic harness. See openrouter.ai/apps"}}`,
			want: true,
		},
		{
			name: "phrasing without url",
			body: `{"error":{"message":"This model requires an agentic harness."}}`,
			want: true,
		},
		{
			name: "only available to agentic harnesses",
			body: `{"error":{"message":"Only available to agentic harnesses"}}`,
			want: true,
		},
		{name: "generic 403", body: `{"type":"error","error":{"type":"permission_error","message":"insufficient credits"}}`, want: false},
		{name: "not found", body: `{"type":"error","error":{"type":"not_found_error","message":"model not found"}}`, want: false},
		{name: "empty", body: ``, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsHarnessGateError([]byte(tc.body)); got != tc.want {
				t.Errorf("IsHarnessGateError(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// toolCompatibility404 is OpenRouter's live answer (2026-11-11) to a forced
// tool_choice with provider.require_parameters on
// thinkingmachines/inkling-2-small:free, whose only endpoint lists no
// tool_choice support.
const toolCompatibility404 = `{"type":"error","error":{"type":"not_found_error","message":"No endpoints found that support the provided 'tool_choice' value. To learn more about provider routing, visit: https://openrouter.ai/docs/guides/routing/provider-selection","error_type":"not_found"},"request_id":"gen-1794429803-FG4hFa2urv6hxO7wYh6V","metadata":{"routing_funnel":[{"step":"Initial Endpoints","endpoint_count":1}],"failed_routing_step":"Filter by Tool Compatibility"}}`

func TestIsToolCompatibilityRejection(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       string
		want       bool
	}{
		{"404 tool compatibility", http.StatusNotFound, toolCompatibility404, true},
		{"404 no endpoints for model", http.StatusNotFound,
			`{"error":{"message":"No endpoints found for minimax/minimax-m3."},"metadata":{"routing_funnel":[{"step":"Initial Endpoints","endpoint_count":0}]}}`, false},
		{"404 failed after passing tool compatibility", http.StatusNotFound,
			`{"error":{"message":"No endpoints found matching your data policy."},"metadata":{"routing_funnel":[{"step":"Initial Endpoints","endpoint_count":3},{"step":"Filter by Tool Compatibility","endpoint_count":2}],"failed_routing_step":"Filter by Data Policy"}}`, false},
		{"502 with the same body", http.StatusBadGateway, toolCompatibility404, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsToolCompatibilityRejection(tc.statusCode, []byte(tc.body)); got != tc.want {
				t.Errorf("IsToolCompatibilityRejection(%d, %s) = %v, want %v", tc.statusCode, tc.name, got, tc.want)
			}
		})
	}
}

func TestIsOpenRouterTransientError(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		body       string
		want       bool
	}{
		{
			name:       "404 no endpoints found for model",
			statusCode: http.StatusNotFound,
			body:       `{"type":"error","error":{"type":"not_found_error","message":"No endpoints found for minimax/minimax-m3.","error_type":"not_found"},"request_id":"gen-123","metadata":{"routing_funnel":[{"step":"Initial Endpoints","endpoint_count":0}]}}`,
			want:       true,
		},
		{
			name:       "404 plain model not found",
			statusCode: http.StatusNotFound,
			body:       `{"error":{"message":"Model not found: bad-model-id"}}`,
			want:       false,
		},
		{
			name:       "404 tool compatibility rejection",
			statusCode: http.StatusNotFound,
			body:       toolCompatibility404,
			want:       false,
		},
		{
			name:       "429 rate limited",
			statusCode: http.StatusTooManyRequests,
			body:       `{"error":{"message":"rate limit exceeded"}}`,
			want:       true,
		},
		{
			name:       "500 internal server error",
			statusCode: http.StatusInternalServerError,
			body:       `{"error":{"message":"internal error"}}`,
			want:       true,
		},
		{
			name:       "502 bad gateway",
			statusCode: http.StatusBadGateway,
			body:       `{"error":{"message":"bad gateway"}}`,
			want:       true,
		},
		{
			name:       "503 service unavailable",
			statusCode: http.StatusServiceUnavailable,
			body:       `{"error":{"message":"service unavailable"}}`,
			want:       true,
		},
		{
			name:       "504 gateway timeout",
			statusCode: http.StatusGatewayTimeout,
			body:       `{"error":{"message":"gateway timeout"}}`,
			want:       true,
		},
		{
			name:       "529 capacity overloaded",
			statusCode: 529,
			body:       `{"error":{"message":"overloaded"}}`,
			want:       true,
		},
		{
			name:       "400 invalid request syntax",
			statusCode: http.StatusBadRequest,
			body:       `{"error":{"message":"messages must be an array"}}`,
			want:       false,
		},
		{
			name:       "401 unauthorized",
			statusCode: http.StatusUnauthorized,
			body:       `{"error":{"message":"invalid api key"}}`,
			want:       false,
		},
		{
			name:       "403 permission error without transient phrase",
			statusCode: http.StatusForbidden,
			body:       `{"error":{"message":"insufficient credits"}}`,
			want:       false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsOpenRouterTransientError(tc.statusCode, []byte(tc.body)); got != tc.want {
				t.Errorf("IsOpenRouterTransientError(%d, %s) = %v, want %v", tc.statusCode, tc.name, got, tc.want)
			}
		})
	}
}
