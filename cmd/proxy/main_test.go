package main

import (
	"net/http"
	"testing"
	"time"
)

func TestHTTPServerHasNoWriteTimeout(t *testing.T) {
	t.Parallel()
	// WriteTimeout covers the full response write and kills long SSE
	// streams mid-generation; the client then sees a stream ending
	// without message_stop. Streaming responses are bounded by client
	// disconnect (request context), never by a server write deadline.
	srv := newHTTPServer("127.0.0.1:8091", http.NotFoundHandler())
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 (kills long SSE streams)", srv.WriteTimeout)
	}
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 10s", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != 30*time.Second {
		t.Errorf("ReadTimeout = %v, want 30s", srv.ReadTimeout)
	}
	if srv.IdleTimeout != 2*time.Minute {
		t.Errorf("IdleTimeout = %v, want 2m", srv.IdleTimeout)
	}
}
