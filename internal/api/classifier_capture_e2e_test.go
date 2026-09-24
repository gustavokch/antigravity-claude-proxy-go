package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"antigravity-go-proxy/internal/classifier"
	"antigravity-go-proxy/internal/classifier/corpus"
	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
)

// verdictBackend stands in for the account-backed upstream and answers every
// request with one Cloud Code event carrying verdict as its text.
type verdictBackend struct {
	verdict string
	hit     bool
}

func (b *verdictBackend) FetchAvailableModels(context.Context) (cloudcode.Response, error) {
	return cloudcode.Response{}, nil
}

func (b *verdictBackend) StreamGenerateContent(_ context.Context, _ map[string]any, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	b.hit = true
	event, err := json.Marshal(map[string]any{
		"response": map[string]any{
			"candidates": []map[string]any{{
				"content":      map[string]any{"parts": []map[string]any{{"text": b.verdict}}},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]any{"promptTokenCount": 5, "candidatesTokenCount": 2},
		},
	})
	if err != nil {
		return cloudcode.Response{}, err
	}
	if err := consume(cloudcode.SSEEvent{Data: event}); err != nil {
		return cloudcode.Response{StatusCode: http.StatusOK}, err
	}
	return cloudcode.Response{StatusCode: http.StatusOK}, nil
}

// anthropicVerdictServer answers every request with an Anthropic message whose
// only text block is verdict, and records the model it was asked for.
func anthropicVerdictServer(t *testing.T, verdict string) (*httptest.Server, *string) {
	t.Helper()
	receivedModel := new(string)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &decoded)
		*receivedModel = decoded.Model
		message, _ := json.Marshal(map[string]any{
			"id":          "msg_capture",
			"type":        "message",
			"role":        "assistant",
			"model":       decoded.Model,
			"content":     []map[string]any{{"type": "text", "text": verdict}},
			"stop_reason": "end_turn",
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(message)
	}))
	t.Cleanup(server.Close)
	return server, receivedModel
}

// TestClassifierCaptureUpstreamRowEndToEnd drives the real handler in the
// recommended collection setup: capture on, classifier interception off and
// the fallback unset. An upstream answer is the only training-eligible label,
// so this pins that it yields exactly one upstream row with the parsed
// severity.
func TestClassifierCaptureUpstreamRowEndToEnd(t *testing.T) {
	t.Setenv("ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK", "")
	orig := config.Get()
	t.Cleanup(func() { config.SetForTest(orig) })
	dir := t.TempDir()
	server, _ := newAccountBackedTestServer(t)
	backend := &verdictBackend{verdict: "<severity>12</severity>"}
	server.backend = backend

	cfg := config.Get()
	cfg.Classifier.Enabled = false
	cfg.Classifier.Capture = config.ClassifierCaptureConfig{Enabled: true, Dir: dir}
	config.SetForTest(cfg)
	server.applyClassifierConfig(cfg.Classifier)

	rec := postClassifierMessages(t, server, classifierShapedBody(t, classifierTestModel, classifierStage1Footer))
	if !backend.hit {
		t.Fatalf("the upstream backend was never called; status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	rows := readCaptureRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want exactly 1", len(rows))
	}
	row := rows[0]
	if row.Source != corpus.SourceUpstream {
		t.Errorf("Source = %q, want upstream", row.Source)
	}
	if row.Severity != 12 {
		t.Errorf("Severity = %d, want 12; verdict_raw = %q", row.Severity, row.VerdictRaw)
	}
	if row.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200", row.Status)
	}
	if row.Kind != classifier.KindStage1Severity.String() {
		t.Errorf("Kind = %q, want stage1-severity", row.Kind)
	}
	if row.Model != classifierTestModel {
		t.Errorf("Model = %q, want %q", row.Model, classifierTestModel)
	}
	if row.Action != `{"Bash":"ls"}` {
		t.Errorf("Action = %q", row.Action)
	}
}

// TestClassifierCaptureGatewayRowEndToEnd pins that a request a gateway
// answered is never labeled upstream: the gateway's model graded it, not the
// teacher. Kimi stands in for every gateway because it rewrites an alias to
// its own model ID and can be pointed at a local server. The row must record
// that rewritten model, not the alias the client sent.
func TestClassifierCaptureGatewayRowEndToEnd(t *testing.T) {
	t.Setenv("ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK", "")
	orig := config.Get()
	t.Cleanup(func() { config.SetForTest(orig) })
	dir := t.TempDir()
	kimiServer, receivedModel := anthropicVerdictServer(t, "<severity>7</severity>")
	server, backend := newAccountBackedTestServer(t)

	cfg := config.Get()
	cfg.Classifier.Enabled = false
	cfg.Classifier.Capture = config.ClassifierCaptureConfig{Enabled: true, Dir: dir}
	cfg.Kimi = config.KimiConfig{
		Enabled: true,
		BaseURL: kimiServer.URL,
		APIKey:  "sk-kimi-test",
		Allowlist: []config.KimiModelConfig{
			{ID: "kimi-k2-thinking", Alias: "k2-classifier", Enabled: true},
		},
	}
	config.SetForTest(cfg)
	server.applyClassifierConfig(cfg.Classifier)

	rec := postClassifierMessages(t, server, classifierShapedBody(t, "k2-classifier", classifierStage1Footer))
	if backend.hit {
		t.Fatal("the account-backed upstream was called; the Kimi gateway should have answered")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if *receivedModel != "kimi-k2-thinking" {
		t.Fatalf("Kimi received model %q, want the rewritten kimi-k2-thinking", *receivedModel)
	}

	rows := readCaptureRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want exactly 1", len(rows))
	}
	row := rows[0]
	if row.Source != corpus.SourceGateway {
		t.Errorf("Source = %q, want gateway: an upstream label would feed a substitute model's verdict to the fine-tune", row.Source)
	}
	if row.Model != "kimi-k2-thinking" {
		t.Errorf("Model = %q, want the post-gateway model kimi-k2-thinking", row.Model)
	}
	if row.Severity != 7 {
		t.Errorf("Severity = %d, want 7; verdict_raw = %q", row.Severity, row.VerdictRaw)
	}
}

// TestClassifierCaptureRuleRerouteRowEndToEnd pins the rule label through the
// real handler: a reroute to a non-Laya backend is graded by the operator's
// own model, so its row is labeled rule, not upstream.
func TestClassifierCaptureRuleRerouteRowEndToEnd(t *testing.T) {
	t.Setenv("ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK", "")
	orig := config.Get()
	t.Cleanup(func() { config.SetForTest(orig) })
	dir := t.TempDir()
	sidecar, _ := anthropicVerdictServer(t, "<severity>4</severity>")
	server, backend := newAccountBackedTestServer(t)
	server.classifierAudit = classifier.NewRecorder(10)

	cfg := config.Get()
	cfg.Classifier.Enabled = true
	cfg.Classifier.Capture = config.ClassifierCaptureConfig{Enabled: true, Dir: dir}
	cfg.Classifier.Rules = []config.Rule{{
		ID:      "stage1-sidecar",
		Name:    "Stage 1 to sidecar",
		Enabled: true,
		Conditions: config.RuleConditions{
			FooterPatterns: []config.MatchPattern{
				{Type: config.PatternSubstring, Pattern: "Grade HARM ONLY"},
			},
		},
		Action:        config.RuleActionReroute,
		TargetBackend: "sidecar",
	}}
	cfg.Classifier.Backends = map[string]config.TargetBackend{
		"sidecar": {
			Name:   "sidecar",
			URL:    sidecar.URL + "/v1/messages",
			Format: config.BackendFormatAnthropic,
			Model:  "sidecar-model",
		},
	}
	config.SetForTest(cfg)
	server.applyClassifierConfig(cfg.Classifier)

	rec := postClassifierMessages(t, server, classifierShapedBody(t, classifierTestModel, classifierStage1Footer))
	if backend.hit {
		t.Fatal("the account-backed upstream was called; the reroute rule should have answered")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}

	rows := readCaptureRows(t, dir)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want exactly 1", len(rows))
	}
	if rows[0].Source != corpus.SourceRule {
		t.Errorf("Source = %q, want rule", rows[0].Source)
	}
	if rows[0].Severity != 4 {
		t.Errorf("Severity = %d, want 4; verdict_raw = %q", rows[0].Severity, rows[0].VerdictRaw)
	}
}

// TestClassifierCaptureFailFastRowIsLabeledStub pins that the fail-fast 400
// the proxy writes itself, when a variant has no canned verdict, is not
// labeled upstream: no teacher graded it.
func TestClassifierCaptureFailFastRowIsLabeledStub(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, cfg *config.Config)
	}{
		{
			name: "always-stub",
			setup: func(t *testing.T, cfg *config.Config) {
				t.Setenv("ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK", "")
				cfg.Classifier.Enabled = true
				cfg.Classifier.Action = config.ActionAlwaysStub
			},
		},
		{
			name: "fallback on exhaustion",
			setup: func(t *testing.T, cfg *config.Config) {
				t.Setenv("ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK", "1")
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			orig := config.Get()
			t.Cleanup(func() { config.SetForTest(orig) })
			dir := t.TempDir()
			server, backend := newAccountBackedTestServer(t)

			cfg := config.Get()
			cfg.Classifier.Capture = config.ClassifierCaptureConfig{Enabled: true, Dir: dir}
			testCase.setup(t, &cfg)
			config.SetForTest(cfg)
			server.applyClassifierConfig(cfg.Classifier)

			// block-prefilter has no canned verdict, so both paths fail fast.
			rec := postClassifierMessages(t, server, classifierShapedBody(t, classifierTestModel, classifierBlockFooter))
			if backend.hit {
				t.Fatal("the upstream backend was called; the request should have failed fast")
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
			}

			rows := readCaptureRows(t, dir)
			if len(rows) != 1 {
				t.Fatalf("got %d rows, want exactly 1", len(rows))
			}
			if rows[0].Source != corpus.SourceStub {
				t.Errorf("Source = %q, want stub", rows[0].Source)
			}
			if rows[0].Status != http.StatusBadRequest {
				t.Errorf("Status = %d, want 400", rows[0].Status)
			}
		})
	}
}

// TestClassifierCaptureConfigSwapIsRaceFree pins that a settings save can
// swap the recorder while classifier requests are in flight. It proves
// nothing without -race: run it with go test -race.
func TestClassifierCaptureConfigSwapIsRaceFree(t *testing.T) {
	t.Setenv("ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK", "")
	orig := config.Get()
	t.Cleanup(func() { config.SetForTest(orig) })
	dir := t.TempDir()
	server, _ := newAccountBackedTestServer(t)
	server.backend = &verdictBackend{verdict: "<severity>12</severity>"}

	cfg := config.Get()
	cfg.Classifier.Enabled = false
	cfg.Classifier.Capture = config.ClassifierCaptureConfig{Enabled: true, Dir: dir}
	config.SetForTest(cfg)
	server.applyClassifierConfig(cfg.Classifier)

	body := classifierShapedBody(t, classifierTestModel, classifierStage1Footer)
	const rounds = 50
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			swapped := cfg.Classifier
			swapped.Capture.Enabled = i%2 == 0
			server.applyClassifierConfig(swapped)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			postClassifierMessages(t, server, body)
		}
	}()
	wg.Wait()
}
