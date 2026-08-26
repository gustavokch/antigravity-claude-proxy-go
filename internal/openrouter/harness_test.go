package openrouter

import (
	"net/http"
	"testing"
)

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
