package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
)

const classifierStage1Footer = "\nStage 1 does NOT apply user intent or ALLOW exceptions — stage 2 will handle those.\nRespond with <severity>N</severity> ONLY. Grade HARM ONLY — do NOT reduce for user intent. No other text.\n"

const classifierBlockFooter = "\nErr on the side of blocking. Your ENTIRE response MUST begin with <block>. Do NOT output any analysis, reasoning, or commentary before <block>.\n"

const classifierTestModel = "test-classifier-model"

func classifierShapedBody(t *testing.T, model, footer string) []byte {
	t.Helper()
	return classifierShapedBodyWithExtras(t, model, footer, nil)
}

// classifierShapedBodyWithExtras builds a classifier-shaped request and
// merges extras into the top level, so tests can add fields such as
// "stream" without duplicating the fixture.
func classifierShapedBodyWithExtras(t *testing.T, model, footer string, extras map[string]any) []byte {
	t.Helper()
	body := map[string]any{
		"model": model,
		"system": []map[string]any{
			{"type": "text", "text": "x-anthropic-billing-header: ..."},
			{"type": "text", "text": "You are a security monitor for autonomous AI coding agents. (rest of prompt)"},
		},
		"messages": []map[string]any{
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": "<transcript>"},
					{"type": "text", "text": `{"Bash":"ls"}`},
					{"type": "text", "text": "</transcript>"},
					{"type": "text", "text": footer},
				},
			},
		},
	}
	for key, value := range extras {
		body[key] = value
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal classifier body: %v", err)
	}
	return raw
}

func ordinaryBody(t *testing.T, model string) []byte {
	t.Helper()
	body := map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": "please fix the bug"}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal ordinary body: %v", err)
	}
	return raw
}

// dispatchRecordingBackend stands in for the account-backed upstream and
// records whether a request was ever dispatched to it.
type dispatchRecordingBackend struct {
	hit bool
}

func (b *dispatchRecordingBackend) FetchAvailableModels(context.Context) (cloudcode.Response, error) {
	return cloudcode.Response{}, nil
}

func (b *dispatchRecordingBackend) StreamGenerateContent(context.Context, map[string]any, func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	b.hit = true
	return cloudcode.Response{}, nil
}

// newAccountBackedTestServer builds a Server whose only route is the
// account-backed dispatcher — the one path whose capacity the classifier
// fallback is allowed to reason about. accountManager is wired with zero
// available accounts to represent quota exhaustion.
func newAccountBackedTestServer(t *testing.T) (*Server, *dispatchRecordingBackend) {
	t.Helper()
	config.SetForTest(config.DefaultConfig())
	backend := &dispatchRecordingBackend{}
	manager, err := accounts.New(accounts.Options{
		Accounts: []*accounts.Account{},
	})
	if err != nil {
		t.Fatalf("accounts.New: %v", err)
	}
	server := &Server{
		backend:        backend,
		builder:        proxyformat.NewBuilder(),
		logger:         slog.Default(),
		now:            time.Now,
		accountManager: manager,
	}
	return server, backend
}

// newCustomEndpointTestServer routes model at a custom endpoint. Custom
// endpoints carry their own credentials and never consume account capacity,
// so classifier fallback must not gate them.
func newCustomEndpointTestServer(model, backendURL string) *Server {
	cfg := config.DefaultConfig()
	cfg.CustomEndpoints = map[string]config.EndpointConfig{
		model: {URL: backendURL},
	}
	config.SetForTest(cfg)
	return &Server{logger: slog.Default(), now: time.Now}
}

func postClassifierMessages(t *testing.T, server *Server, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.messages(rec, req)
	return rec
}

func TestMessages_ClassifierFallback_StubsWhenNoCapacity(t *testing.T) {
	t.Setenv("ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK", "1")
	server, backend := newAccountBackedTestServer(t)
	rec := postClassifierMessages(t, server, classifierShapedBody(t, classifierTestModel, classifierStage1Footer))

	if backend.hit {
		t.Fatal("backend was dispatched to; classifier fallback should have stubbed the response instead")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var stub struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &stub); err != nil {
		t.Fatalf("stub response is not valid JSON: %v; body: %s", err, rec.Body.String())
	}
	if len(stub.Content) != 1 || stub.Content[0].Text != "<severity>0</severity>" {
		t.Fatalf("stub content = %+v, want a single block with <severity>0</severity>", stub.Content)
	}
}

func TestMessages_ClassifierFallback_FastFailsUnsupportedVariant(t *testing.T) {
	t.Setenv("ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK", "1")
	server, backend := newAccountBackedTestServer(t)
	rec := postClassifierMessages(t, server, classifierShapedBody(t, classifierTestModel, classifierBlockFooter))

	if backend.hit {
		t.Fatal("backend was dispatched to; unsupported classifier variant should fast-fail instead")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (a 429 invites the caller's own backoff, which is the stall this feature removes); body: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("fast-fail body is not valid JSON: %v; body: %s", err, rec.Body.String())
	}
	if payload.Error.Type != "invalid_request_error" {
		t.Fatalf("error.type = %q, want invalid_request_error (non-retryable)", payload.Error.Type)
	}
}

func TestMessages_ClassifierFallback_NonClassifierRequestDispatchesNormally(t *testing.T) {
	t.Setenv("ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK", "1")
	server, backend := newAccountBackedTestServer(t)
	rec := postClassifierMessages(t, server, ordinaryBody(t, classifierTestModel))

	if !backend.hit {
		t.Fatalf("backend was never dispatched to for a non-classifier request; status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMessages_ClassifierFallback_FlagOffDispatchesNormally(t *testing.T) {
	t.Setenv("ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK", "")
	server, backend := newAccountBackedTestServer(t)
	rec := postClassifierMessages(t, server, classifierShapedBody(t, classifierTestModel, classifierStage1Footer))

	if !backend.hit {
		t.Fatalf("flag off: classifier-shaped request must dispatch normally (byte-identical to today); status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMessages_ClassifierFallback_CapacityAvailableDispatchesNormally(t *testing.T) {
	t.Setenv("ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK", "1")
	manager, err := accounts.New(accounts.Options{
		Accounts: []*accounts.Account{{Email: "a@b.com", Enabled: true}},
	})
	if err != nil {
		t.Fatalf("accounts.New: %v", err)
	}

	server, backend := newAccountBackedTestServer(t)
	server.accountManager = manager
	rec := postClassifierMessages(t, server, classifierShapedBody(t, classifierTestModel, classifierStage1Footer))

	if !backend.hit {
		t.Fatalf("capacity available: classifier request must dispatch normally, not be stubbed; status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMessages_ClassifierFallback_RateLimitedAccountStubs(t *testing.T) {
	t.Setenv("ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK", "1")
	acc := &accounts.Account{Email: "exhausted@b.com", Enabled: true}
	manager, err := accounts.New(accounts.Options{
		Accounts: []*accounts.Account{acc},
	})
	if err != nil {
		t.Fatalf("accounts.New: %v", err)
	}
	manager.MarkRateLimited(acc, classifierTestModel, time.Hour)

	server, backend := newAccountBackedTestServer(t)
	server.accountManager = manager
	rec := postClassifierMessages(t, server, classifierShapedBody(t, classifierTestModel, classifierStage1Footer))

	if backend.hit {
		t.Fatal("backend was dispatched to; rate-limited account should have stubbed the response instead")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

func TestMessages_ClassifierFallback_NilAccountManagerDispatchesNormally(t *testing.T) {
	t.Setenv("ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK", "1")
	config.SetForTest(config.DefaultConfig())
	backend := &dispatchRecordingBackend{}
	server := &Server{
		backend: backend,
		builder: proxyformat.NewBuilder(),
		logger:  slog.Default(),
		now:     time.Now,
	}
	rec := postClassifierMessages(t, server, classifierShapedBody(t, classifierTestModel, classifierStage1Footer))

	if !backend.hit {
		t.Fatalf("nil accountManager: request must dispatch normally, not be stubbed; status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMessages_ClassifierFallback_StreamingRequestIsNeverStubbed(t *testing.T) {
	t.Setenv("ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK", "1")
	server, backend := newAccountBackedTestServer(t)
	body := classifierShapedBodyWithExtras(t, classifierTestModel, classifierStage1Footer, map[string]any{"stream": true})
	rec := postClassifierMessages(t, server, body)

	if !backend.hit {
		t.Fatalf("a streaming caller awaits text/event-stream; the JSON stub would hang it, so the request must dispatch; status=%d content-type=%q body=%s",
			rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
}

func TestMessages_ClassifierFallback_CustomEndpointRequestIsNeverStubbed(t *testing.T) {
	t.Setenv("ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK", "1")
	backendHit := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendHit = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer backend.Close()

	server := newCustomEndpointTestServer(classifierTestModel, backend.URL)
	rec := postClassifierMessages(t, server, classifierShapedBody(t, classifierTestModel, classifierStage1Footer))

	if !backendHit {
		t.Fatalf("custom endpoints do not consume account capacity; the classifier request must dispatch, not be stubbed; status=%d body=%s", rec.Code, rec.Body.String())
	}
}
