package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/cloudcode"
	proxyformat "antigravity-go-proxy/internal/format"
)

// limitUpstream streams head, waits for release, then streams tail and ends:
// upstream generating past the client's max_tokens. Like
// cloudcode.Client.DoSSE, it stops reading at the first consume error or when
// its context ends.
type limitUpstream struct {
	head, tail [][]byte
	release    chan struct{}
	ended      chan struct{} // closed when the agent stream call returns
	// Set before ended is closed.
	readToEOF, cancelled bool
}

func (upstream *limitUpstream) LoadCodeAssist(context.Context) (cloudcode.Response, error) {
	return cloudcode.Response{StatusCode: http.StatusOK, Body: []byte(`{"cloudaicompanionProject":"aicode-consumers"}`)}, nil
}

func (upstream *limitUpstream) FetchAvailableModels(context.Context, string) (cloudcode.Response, error) {
	return cloudcode.Response{StatusCode: http.StatusOK, Body: []byte(`{
		"agentModelSorts":[{"groups":[{"modelIds":["gemini-3.8-flash-low"]}]}],
		"models":{"gemini-3.8-flash-low":{"displayName":"Gemini 3.8 Flash (Low)","supportsThinking":true,"thinkingLevel":1,
			"maxTokens":1048576,"maxOutputTokens":65536,"model":"MODEL_PLACEHOLDER_M320","apiProvider":"API_PROVIDER_GOOGLE_GEMINI"}}
	}`)}, nil
}

func (upstream *limitUpstream) StreamGenerateContent(ctx context.Context, payload any, _ cloudcode.RequestOptions, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	if built, ok := payload.(proxyformat.CloudCodeRequest); !ok || built.RequestType != "agent" {
		return cloudcode.Response{StatusCode: http.StatusOK}, nil
	}
	defer close(upstream.ended)
	send := func(events [][]byte) error {
		for _, data := range events {
			if err := consume(cloudcode.SSEEvent{Data: data}); err != nil {
				return err
			}
		}
		return nil
	}
	if err := send(upstream.head); err != nil {
		return cloudcode.Response{StatusCode: http.StatusOK}, err
	}
	select {
	case <-upstream.release:
	case <-ctx.Done():
		upstream.cancelled = true
		return cloudcode.Response{StatusCode: http.StatusOK}, ctx.Err()
	}
	if err := send(upstream.tail); err != nil {
		return cloudcode.Response{StatusCode: http.StatusOK}, err
	}
	upstream.cancelled = ctx.Err() != nil
	upstream.readToEOF = !upstream.cancelled
	return cloudcode.Response{StatusCode: http.StatusOK}, nil
}

func limitEvent(candidates int, finish string) []byte {
	candidate := map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": "word "}}}}
	if finish != "" {
		candidate["finishReason"] = finish
	}
	encoded, _ := json.Marshal(map[string]any{"response": map[string]any{
		"candidates":    []any{candidate},
		"usageMetadata": map[string]any{"promptTokenCount": 12, "candidatesTokenCount": candidates},
	}})
	return encoded
}

// At the client's max_tokens, /v1/messages answers at once, streaming or not,
// with stop_reason max_tokens, while upstream keeps generating; upstream is
// read to its end and never cancelled, also after net/http cancels the
// request context when the handler returns.
func TestMessagesAnswerAtTheClientLimitWhileUpstreamIsReadToTheEnd(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(map[bool]string{true: "stream", false: "unary"}[stream], func(t *testing.T) {
			t.Parallel()
			upstream := &limitUpstream{
				head:    [][]byte{limitEvent(30, ""), limitEvent(120, "")},
				tail:    [][]byte{limitEvent(150, ""), limitEvent(160, "STOP")},
				release: make(chan struct{}), ended: make(chan struct{}),
			}
			manager, err := accounts.New(accounts.Options{Accounts: []*accounts.Account{{Email: "limit@example.com", Enabled: true}}})
			if err != nil {
				t.Fatal(err)
			}
			dispatcher, err := accounts.NewDispatcher(accounts.DispatcherOptions{
				Manager: manager, Resolver: &agyFakeResolver{},
				NewClient: func(string) accounts.CloudClient { return upstream },
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(dispatcher.Close)
			server := &Server{backend: dispatcher, builder: proxyformat.NewBuilder(), logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now}

			body, _ := json.Marshal(map[string]any{"model": "gemini-3.8-flash-low", "stream": stream, "max_tokens": 100,
				"messages": []any{map[string]any{"role": "user", "content": "hello"}}})
			requestCtx, endRequest := context.WithCancel(context.Background())
			defer endRequest()
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body))).WithContext(requestCtx)
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			server.messages(recorder, request)
			endRequest() // net/http cancels the request context once the handler returns

			select {
			case <-upstream.ended:
				t.Fatalf("the client got its answer only after upstream ended (read to EOF %v, cancelled %v)", upstream.readToEOF, upstream.cancelled)
			default:
			}
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"stop_reason":"max_tokens"`) {
				t.Fatalf("answer %d: %s", recorder.Code, recorder.Body.String())
			}
			if stream && !strings.Contains(recorder.Body.String(), "event: message_stop") {
				t.Fatalf("the stream did not end with message_stop: %s", recorder.Body.String())
			}

			close(upstream.release)
			select {
			case <-upstream.ended:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream never ended")
			}
			if !upstream.readToEOF || upstream.cancelled {
				t.Fatalf("upstream read to EOF %v, cancelled %v; want read to the end, never cancelled", upstream.readToEOF, upstream.cancelled)
			}
		})
	}
}
