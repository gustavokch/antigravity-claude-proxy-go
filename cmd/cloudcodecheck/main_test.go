package main

import (
	"testing"

	"antigravity-go-proxy/internal/cloudcode"
)

func TestEndpointOverride(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"daily": cloudcode.DailyEndpoint,
		"prod":  cloudcode.ProdEndpoint,
	} {
		got, err := endpointOverride(name)
		if err != nil || got != want {
			t.Errorf("endpointOverride(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
	for _, name := range []string{"https://evil.example", "http://127.0.0.1:1", "DAILY2", " "} {
		if got, err := endpointOverride(name); err == nil {
			t.Errorf("endpointOverride(%q) = %q, want error (token must not go to arbitrary hosts)", name, got)
		}
	}
}
