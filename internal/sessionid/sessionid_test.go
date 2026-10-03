package sessionid

import "testing"

func TestParseNested(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain", "sess-1", "sess-1"},
		{"nested session_id", `{"device_id":"d","session_id":"n-123"}`, "n-123"},
		{"empty session_id falls back to user_id", `{"session_id":"","user_id":"u-9"}`, "u-9"},
		{"invalid json braces returned raw", `{not json}`, `{not json}`},
		{"whitespace trimmed", "  s  ", "s"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseNested(tc.in); got != tc.want {
				t.Fatalf("ParseNested(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestFromValue(t *testing.T) {
	if got := FromValue(map[string]any{"session_id": "m-1"}); got != "m-1" {
		t.Fatalf("map session_id: got %q", got)
	}
	if got := FromValue(nil); got != "" {
		t.Fatalf("nil: got %q", got)
	}
}
