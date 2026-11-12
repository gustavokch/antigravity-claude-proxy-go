package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/cloudcode"
	proxyformat "antigravity-go-proxy/internal/format"
)

type feedbackTransport func(*http.Request) (*http.Response, error)

func (transport feedbackTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestMessagesGemini429Feedback(t *testing.T) {
	for _, scenario := range []string{"bare", "escalated", "stalled retry", "attempt exhaustion"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", scenario, stream), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					account := &accounts.Account{Email: "feedback@example.com", Enabled: true, ProjectID: "synthetic-project"}
					manager, err := accounts.New(accounts.Options{Accounts: []*accounts.Account{account}, Strategy: accounts.StrategySticky})
					if err != nil {
						t.Fatal(err)
					}
					calls := 0
					client := cloudcode.New(cloudcode.Options{AccessToken: "synthetic-token", HTTPClient: &http.Client{Transport: feedbackTransport(func(request *http.Request) (*http.Response, error) {
						status, body := http.StatusOK, `{}`
						header := make(http.Header)
						switch {
						case strings.Contains(request.URL.Path, "fetchAvailableModels"):
							body = `{"agentModelSorts":[{"groups":[{"modelIds":["gemini-3.8-flash-high"]}]}],"models":{"gemini-3.8-flash-high":{"displayName":"Gemini Flash High","supportsThinking":true,"thinkingBudget":24576,"maxOutputTokens":65536}}}`
						case strings.Contains(request.URL.Path, "streamGenerateContent"):
							calls++
							if scenario == "stalled retry" && calls > 1 {
								<-request.Context().Done()
								return nil, request.Context().Err()
							}
							status, body = http.StatusTooManyRequests, `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`
							if scenario == "stalled retry" || scenario == "attempt exhaustion" {
								header.Set("Retry-After", "2")
							}
						}
						return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
					})}})
					dispatcher, err := accounts.NewDispatcher(accounts.DispatcherOptions{Manager: manager, Resolver: &agyFakeResolver{}, ProjectID: account.ProjectID,
						NewClient: func(string) accounts.CloudClient { return client }, Random: func() float64 { return 1 }, SwitchDelay: -1, MaxRetries: 1})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(dispatcher.Close)
					if _, err := dispatcher.FetchAvailableModels(context.Background()); err != nil {
						t.Fatal(err)
					}
					if scenario == "escalated" {
						manager.IncrementFailure(account)
					}
					server := &Server{backend: dispatcher, builder: proxyformat.NewBuilder(), logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now}
					body := fmt.Sprintf(`{"model":"gemini-3.8-flash-high","stream":%v,"messages":[{"role":"user","content":"Reply OK"}]}`, stream)
					request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
					recorder := httptest.NewRecorder()
					start := time.Now()
					server.messages(recorder, request)
					if time.Since(start) > 10*time.Second || recorder.Code != http.StatusTooManyRequests {
						t.Fatalf("elapsed=%s status=%d body=%s", time.Since(start), recorder.Code, recorder.Body.String())
					}
					var envelope struct {
						Type  string `json:"type"`
						Error struct {
							Type    string `json:"type"`
							Message string `json:"message"`
						} `json:"error"`
					}
					if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
						t.Fatal(err)
					}
					if envelope.Type != "error" || envelope.Error.Type != "rate_limit_error" || !strings.Contains(envelope.Error.Message, "gemini-3.8-flash-high") || !strings.Contains(envelope.Error.Message, "retry") {
						t.Fatalf("unhelpful feedback: %+v", envelope)
					}
					seconds, err := strconv.Atoi(recorder.Header().Get("Retry-After"))
					remaining := manager.MinWait("gemini-3.8-flash-high")
					if err != nil || seconds <= 0 || time.Duration(seconds)*time.Second < remaining {
						t.Fatalf("Retry-After=%q remaining=%s", recorder.Header().Get("Retry-After"), remaining)
					}
					if scenario == "bare" && (calls != 1 || seconds != 38) {
						t.Fatalf("bare rejection sends=%d retry=%d", calls, seconds)
					}
					if scenario == "escalated" && seconds != 75 {
						t.Fatalf("escalated retry=%d want 75", seconds)
					}
					if scenario == "attempt exhaustion" && calls != 3 {
						t.Fatalf("attempt sends=%d want 3 (one fast retry plus two selections)", calls)
					}
				})
			})
		}
	}
}

func TestMessagesCommittedStreamPreservesErrorKind(t *testing.T) {
	for _, thinking := range []bool{false, true} {
		t.Run(fmt.Sprintf("thinking=%v", thinking), func(t *testing.T) {
			part := `{"text":"content before rejection"}`
			if thinking {
				part = `{"text":"content before rejection","thought":true}`
			}
			upstream := &fakeUpstream{streamData: [][]byte{[]byte(`{"response":{"candidates":[{"content":{"parts":[` + part + `]}}]}}`)}, streamErr: &accounts.RateLimitError{Model: "gemini-3.8-flash-high", RetryAfter: time.Minute}}
			handler := newTestHandler(t, upstream, "project")
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gemini-3.8-flash-high","stream":true,"messages":[{"role":"user","content":"Reply OK"}]}`))
			request.Header.Set("x-api-key", "local-key")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			body := recorder.Body.String()
			if recorder.Code != http.StatusOK || strings.Count(body, "event: error\n") != 1 || !strings.Contains(body, `"type":"rate_limit_error"`) || !strings.Contains(body, "content before rejection") || strings.Contains(body, "event: message_stop\n") || upstream.streamCalls != 1 {
				t.Fatalf("status=%d sends=%d body=%s", recorder.Code, upstream.streamCalls, body)
			}
		})
	}
}

func TestMessagesEmptyStreamReportsFailure(t *testing.T) {
	handler := newTestHandler(t, &fakeUpstream{}, "project")
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gemini-3.8-flash-high","stream":true,"messages":[{"role":"user","content":"Reply OK"}]}`))
	request.Header.Set("x-api-key", "local-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("status=%d body=%q: %v", recorder.Code, recorder.Body.String(), err)
	}
	if recorder.Code != http.StatusBadGateway || body.Error.Type != "api_error" {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

type disconnectedFeedbackWriter struct {
	header http.Header
	writes int
}

func (writer *disconnectedFeedbackWriter) Header() http.Header { return writer.header }
func (writer *disconnectedFeedbackWriter) WriteHeader(int)     {}
func (writer *disconnectedFeedbackWriter) Write([]byte) (int, error) {
	writer.writes++
	return 0, io.ErrClosedPipe
}
func (writer *disconnectedFeedbackWriter) Flush() {}

func TestMessagesStreamStopsOnWriteFailure(t *testing.T) {
	for _, openAI := range []bool{false, true} {
		t.Run(fmt.Sprintf("openai=%v", openAI), func(t *testing.T) {
			server := newTestServer(t, &fakeUpstream{}, "project")
			writer := &disconnectedFeedbackWriter{header: make(http.Header)}
			consumed := 0
			var deliveryErr error
			send := func(_ context.Context, _ map[string]any, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
				for range 3 {
					consumed++
					deliveryErr = consume(cloudcode.SSEEvent{Data: []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"still writing"}]}}]}}`)})
					if deliveryErr != nil {
						return cloudcode.Response{}, deliveryErr
					}
				}
				return cloudcode.Response{}, nil
			}
			var destination http.ResponseWriter = writer
			if openAI {
				destination = newOpenAIResponseWriter(writer, "gemini-3.8-flash-high", false, "")
			}
			server.streamMessage(destination, httptest.NewRequest(http.MethodPost, "/v1/messages", nil), send, nil, "gemini-3.8-flash-high")
			if !errors.Is(deliveryErr, io.ErrClosedPipe) || consumed != 1 || writer.writes != 1 {
				t.Fatalf("delivery=%v consumed=%d writes=%d", deliveryErr, consumed, writer.writes)
			}
		})
	}
}

func TestOpenAICommittedStreamPreservesErrorKind(t *testing.T) {
	upstream := &fakeUpstream{streamData: [][]byte{[]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"partial output"}]}}]}}`)}, streamErr: &accounts.RateLimitError{Model: "gemini-3.8-flash-high", RetryAfter: time.Minute}}
	handler := newTestHandler(t, upstream, "project")
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gemini-3.8-flash-high","stream":true,"messages":[{"role":"user","content":"Reply OK"}]}`))
	request.Header.Set("x-api-key", "local-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	scanner := bufio.NewScanner(recorder.Body)
	content, errorsSeen, success := "", 0, false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk struct {
			Error *struct {
				Type string `json:"type"`
			} `json:"error"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatal(err)
		}
		if chunk.Error != nil {
			if chunk.Error.Type != "rate_limit_error" {
				t.Fatalf("error kind=%s", chunk.Error.Type)
			}
			errorsSeen++
			continue
		}
		for _, choice := range chunk.Choices {
			content += choice.Delta.Content
			success = success || choice.FinishReason != nil
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if content != "partial output" || errorsSeen != 1 || success || upstream.streamCalls != 1 {
		t.Fatalf("content=%q errors=%d success=%v sends=%d", content, errorsSeen, success, upstream.streamCalls)
	}
}
