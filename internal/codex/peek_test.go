package codex

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
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
			prefix, failure, _, err := Peek(bufio.NewReader(strings.NewReader(tc.in)), PeekLimit, 0)
			if err != nil || string(failure) != tc.wantFailure || !strings.Contains(string(prefix), tc.wantPrefixHas) {
				t.Errorf("Peek = %q, %q, %v; want failure %q and a prefix holding %q", prefix, failure, err, tc.wantFailure, tc.wantPrefixHas)
			}
		})
	}
}

// A preamble that outlasts the wait releases the stream: Peek returns what it
// read with no failure, and rest carries on from there, the line Peek was still
// reading included, so no byte is lost or repeated.
func TestPeekReleasesTheStreamOnceTheWaitPasses(t *testing.T) {
	created := "data: {\"type\":\"response.created\"}\n\n"
	cut := "data: {\"type\":\"response.outp"
	tail := "ut_item.added\"}\n\ndata: [DONE]\n\n"
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close() })

	type peeked struct {
		prefix, failure []byte
		rest            io.Reader
		err             error
	}
	done := make(chan peeked, 1)
	go func() {
		prefix, failure, rest, err := Peek(bufio.NewReader(io.MultiReader(strings.NewReader(created+cut), pr)), PeekLimit, 100*time.Millisecond)
		done <- peeked{prefix, failure, rest, err}
	}()
	var got peeked
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Peek held the stream back past its wait")
	}
	if got.err != nil || got.failure != nil || string(got.prefix) != created {
		t.Fatalf("Peek = %q, %q, %v; want the preamble read so far and no failure", got.prefix, got.failure, got.err)
	}

	go func() {
		_, _ = io.WriteString(pw, tail)
		_ = pw.Close()
	}()
	rest, err := io.ReadAll(got.rest)
	if err != nil || string(rest) != cut+tail {
		t.Errorf("rest = %q, %v; want %q", rest, err, cut+tail)
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
