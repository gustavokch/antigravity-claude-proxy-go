package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
)

// sessionKeyBackend records the session key each Cloud Code send carries.
type sessionKeyBackend struct {
	mu   sync.Mutex
	keys []string
}

func (b *sessionKeyBackend) FetchAvailableModels(context.Context) (cloudcode.Response, error) {
	return cloudcode.Response{}, nil
}

func (b *sessionKeyBackend) StreamGenerateContent(ctx context.Context, _ map[string]any, _ func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.keys = append(b.keys, accounts.SessionKey(ctx))
	return cloudcode.Response{}, nil
}

// The account pool pins on the session key, so messages() must hand the
// backend the same id the request metrics use: the Claude Code header first,
// then the session inside metadata.user_id.
func TestMessagesCarriesTheSessionKeyToTheBackend(t *testing.T) {
	config.SetForTest(config.DefaultConfig())
	const headerSession = "8b1f7c62-3c1e-4a51-9a63-1f0c2f3e9d10"
	const bodySession = "5d2e0a4b-7f61-4c3a-8e92-6b0d1c4f2a77"
	tests := []struct {
		name   string
		header string
		body   string
		want   string
	}{
		{
			name:   "Claude Code header",
			header: headerSession,
			body:   `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}]}`,
			want:   headerSession,
		},
		{
			name:   "streaming request",
			header: headerSession,
			body:   `{"model":"gemini-3.8-flash-high","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			want:   headerSession,
		},
		{
			name: "metadata user_id",
			body: `{"model":"gemini-3.8-flash-high","metadata":{"user_id":"{\"device_id\":\"d1\",\"session_id\":\"` + bodySession + `\"}"},"messages":[{"role":"user","content":"hi"}]}`,
			want: bodySession,
		},
		{
			name: "no session",
			body: `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}]}`,
			want: "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &sessionKeyBackend{}
			server := &Server{backend: backend, builder: proxyformat.NewBuilder(), logger: slog.Default(), now: time.Now}
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(test.body))
			req.Header.Set("Content-Type", "application/json")
			if test.header != "" {
				req.Header.Set("X-Claude-Code-Session-Id", test.header)
			}
			rec := httptest.NewRecorder()
			server.messages(rec, req)

			if len(backend.keys) != 1 || backend.keys[0] != test.want {
				t.Fatalf("backend saw session keys %q, want [%q]; status=%d body=%s", backend.keys, test.want, rec.Code, rec.Body.String())
			}
		})
	}
}
