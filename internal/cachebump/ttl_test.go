package cachebump

import (
	"testing"
	"time"
)

func TestDetectTTL_DefaultFiveMinutes(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
	if got := DetectTTL([]byte(body), extendedCacheTTLBeta); got != 5*time.Minute {
		t.Errorf("expected 5m, got %v", got)
	}
}

func TestDetectTTL_EphemeralNoTTLIsFiveMinutes(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"messages":[]}`
	if got := DetectTTL([]byte(body), extendedCacheTTLBeta); got != 5*time.Minute {
		t.Errorf("expected 5m, got %v", got)
	}
}

func TestDetectTTL_OneHourInMessages(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`
	if got := DetectTTL([]byte(body), extendedCacheTTLBeta); got != time.Hour {
		t.Errorf("expected 1h, got %v", got)
	}
}

func TestDetectTTL_OneHourInSystem(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[]}`
	if got := DetectTTL([]byte(body), extendedCacheTTLBeta); got != time.Hour {
		t.Errorf("expected 1h, got %v", got)
	}
}

func TestDetectTTL_OneHourInTools(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"tools":[{"name":"f","input_schema":{},"cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[]}`
	if got := DetectTTL([]byte(body), extendedCacheTTLBeta); got != time.Hour {
		t.Errorf("expected 1h, got %v", got)
	}
}

func TestDetectTTL_InvalidJSONIsFiveMinutes(t *testing.T) {
	if got := DetectTTL([]byte("garbage"), ""); got != 5*time.Minute {
		t.Errorf("expected 5m, got %v", got)
	}
}

func TestDetectTTL_HourMarkerWithoutBetaIsFiveMinutes(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[]}`
	if got := DetectTTL([]byte(body), ""); got != 5*time.Minute {
		t.Errorf("expected 5m, got %v", got)
	}
}

func TestHasCacheControl(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"no markers", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, false},
		{"marker in system", `{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"messages":[]}`, true},
		{"marker in messages", `{"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`, true},
		{"marker in tools", `{"tools":[{"name":"f","cache_control":{"type":"ephemeral"}}],"messages":[]}`, true},
		{"word in text only", `{"messages":[{"role":"user","content":"please cache_control this"}]}`, false},
		{"invalid json", `garbage`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasCacheControl([]byte(tt.body)); got != tt.want {
				t.Errorf("HasCacheControl = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestInspectCacheControl(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantFound    bool
		wantExtended bool
	}{
		{"no markers", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, false, false},
		{"ephemeral only", `{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}]}`, true, false},
		{"extended in messages", `{"messages":[{"role":"user","content":[{"type":"text","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`, true, true},
		{"extended after an ephemeral marker", `{"system":[{"cache_control":{"type":"ephemeral"}}],"tools":[{"cache_control":{"ttl":"1h"}}]}`, true, true},
		// A member name is not the same as a value: the scan must not match
		// the string "cache_control" appearing as message text.
		{"word in text only", `{"messages":[{"role":"user","content":"please cache_control this"}]}`, false, false},
		{"word as a value", `{"messages":[{"role":"user","content":[{"type":"text","text":"cache_control"}]}]}`, false, false},
		{"marker nested under a key named like a value", `{"meta":{"note":"cache_control"},"system":[{"cache_control":{"ttl":"1h"}}]}`, true, true},
		{"unknown ttl string", `{"system":[{"cache_control":{"type":"ephemeral","ttl":"7d"}}]}`, true, false},
		{"invalid json", `garbage`, false, false},
		{"truncated json", `{"system":[{"cache_control":`, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			found, extended := InspectCacheControl([]byte(tt.body))
			if found != tt.wantFound || extended != tt.wantExtended {
				t.Errorf("InspectCacheControl = (%v, %v), want (%v, %v)",
					found, extended, tt.wantFound, tt.wantExtended)
			}
		})
	}
}

func TestNextBumpTime_LeadClampedToFifthOfTTL(t *testing.T) {
	lastSeen := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)

	// 5m TTL: lead = min(60s, 60s) = 60s
	got := NextBumpTime(lastSeen, 5*time.Minute, 60)
	if want := lastSeen.Add(4 * time.Minute); !got.Equal(want) {
		t.Errorf("expected %v, got %v", want, got)
	}

	// 1h TTL: lead = min(60s, 720s) = 60s
	got = NextBumpTime(lastSeen, time.Hour, 60)
	if want := lastSeen.Add(59 * time.Minute); !got.Equal(want) {
		t.Errorf("expected %v, got %v", want, got)
	}

	// Huge lead is clamped to TTL/5.
	got = NextBumpTime(lastSeen, 5*time.Minute, 300)
	if want := lastSeen.Add(4 * time.Minute); !got.Equal(want) {
		t.Errorf("expected lead clamped to 60s, got %v", got)
	}
}
