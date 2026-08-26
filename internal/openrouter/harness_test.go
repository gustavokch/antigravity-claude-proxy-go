package openrouter

import "testing"

func TestIsHarnessGateError(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "gate error",
			body: `{"type":"error","error":{"type":"permission_error","message":"thinkingmachines/inkling:free is only available on agentic harnesses. Try plugging it into a coding agent or productivity app listed on https://openrouter.ai/apps","error_type":"permission_denied"}}`,
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
