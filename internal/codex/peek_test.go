package codex

import (
	"bufio"
	"net/http"
	"strings"
	"testing"
)

func TestPeek(t *testing.T) {
	for _, tc := range []struct {
		name, in, wantFailure string
		wantPrefixHas         string
	}{
		{"failure before output",
			"data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"type\":\"usage_limit_reached\",\"resets_in_seconds\":60}}}\n\n",
			`{"error":{"resets_in_seconds":60,"type":"usage_limit_reached"}}`, ""},
		{"top-level error event",
			"data: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\",\"sequence_number\":1}\n\n",
			`{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`, ""},
		{"output first",
			"data: {\"type\":\"response.created\"}\n\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0}\n\ndata: {\"type\":\"response.failed\"}\n\n",
			"", "response.output_item.added"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix, failure, err := Peek(bufio.NewReader(strings.NewReader(tc.in)), PeekLimit)
			if err != nil || string(failure) != tc.wantFailure || !strings.Contains(string(prefix), tc.wantPrefixHas) {
				t.Errorf("Peek = %q, %q, %v; want failure %q and a prefix holding %q", prefix, failure, err, tc.wantFailure, tc.wantPrefixHas)
			}
		})
	}
}

func TestFailureStatus(t *testing.T) {
	for body, want := range map[string]int{
		`{"error":{"type":"usage_limit_reached"}}`:           http.StatusTooManyRequests,
		`{"error":{"code":"rate_limit_exceeded"}}`:           http.StatusTooManyRequests,
		`{"error":{"code":"invalid_encrypted_content"}}`:     http.StatusBadRequest,
		`{"error":{"type":"invalid_request_error"}}`:         http.StatusBadRequest,
		`{"error":{"status_code":503,"type":"whatever"}}`:    http.StatusServiceUnavailable,
		`{"error":{"type":"server_error","message":"boom"}}`: http.StatusBadGateway,
		`{"error":{"message":"no details"}}`:                 http.StatusBadGateway,
	} {
		if got := FailureStatus([]byte(body)); got != want {
			t.Errorf("FailureStatus(%s) = %d, want %d", body, got, want)
		}
	}
}
