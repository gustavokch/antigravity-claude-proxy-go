package claudecode

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/httpwire"
	"antigravity-go-proxy/internal/transport"
)

// TestNewClientLeavesOpenStreamsUncapped pins the invariant that broke. A total
// http.Client.Timeout covers dial, headers and the full body read, so it ends a
// live SSE stream mid-generation: the client then sees a stream that ends
// without message_stop, and ccCopyStream swallows the read error, so nothing is
// logged. The bound belongs on the transport, at the response headers.
func TestNewClientLeavesOpenStreamsUncapped(t *testing.T) {
	client := NewClient("https://api.anthropic.com", nil)

	if client.httpClient.Timeout != 0 {
		t.Errorf("http.Client.Timeout = %v, want 0: a total timeout covers the body read and cuts open streams", client.httpClient.Timeout)
	}
	tr, ok := client.httpClient.Transport.(*httpwire.Transport)
	if !ok {
		t.Fatalf("expected an *httpwire.Transport so the header bound is reachable, got %T", client.httpClient.Transport)
	}
	if tr.ResponseHeaderTimeout != transport.ResponseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %v, want %v: a silent upstream must still end", tr.ResponseHeaderTimeout, transport.ResponseHeaderTimeout)
	}
}

// TestOpenStreamSurvivesTheHeaderBound shows that the bound stops at the headers:
// an upstream that flushes its headers at once and then keeps writing is read to
// completion. The bound is shortened here so the test is fast; the shipped value
// is the same field, on a transport this test builds for itself. The package
// transport is shared, so mutating it would shorten the bound for every other
// test in the package that runs alongside this one.
func TestOpenStreamSurvivesTheHeaderBound(t *testing.T) {
	const events = 20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		for i := 0; i < events; i++ {
			fmt.Fprintf(w, "event: ping\ndata: {\"n\":%d}\n\n", i)
			flusher.Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer srv.Close()

	tr := transport.Upstream()
	tr.ResponseHeaderTimeout = 50 * time.Millisecond
	client := NewClient(srv.URL, &http.Client{Transport: tr})

	resp, err := client.SendMessage(context.Background(), MessageRequest{
		Token: "sk-ant-oat-test",
		Body:  []byte(`{"model":"claude-sonnet-4","stream":true}`),
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	got := 0
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data:") {
			got++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Errorf("body read failed after %d of %d events: %v", got, events, err)
	}
	if got != events {
		t.Errorf("read %d of %d events", got, events)
	}
}
