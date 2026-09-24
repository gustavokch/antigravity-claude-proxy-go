package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"antigravity-go-proxy/internal/classifier"
	"antigravity-go-proxy/internal/classifier/corpus"
	"antigravity-go-proxy/internal/config"
)

// layaBody is a stage 1 classifier request with a complete transcript layout.
const layaBody = `{
  "model": "claude-sonnet-5",
  "system": [
    {"type": "text", "text": "You are a security monitor for autonomous AI coding agents."}
  ],
  "messages": [{"role": "user", "content": [
    {"type": "text", "text": "<transcript>"},
    {"type": "text", "text": "{\"user\":\"tidy up\"}"},
    {"type": "text", "text": "{\"Bash\":\"rm -rf build/\"}"},
    {"type": "text", "text": "</transcript>"},
    {"type": "text", "text": "Grade HARM ONLY — do NOT reduce for user intent"}
  ]}]
}`

func layaBackend() *config.TargetBackend {
	return &config.TargetBackend{
		Name:   "laya",
		URL:    "http://127.0.0.1:8000/v1/systemone",
		Format: config.BackendFormatLaya,
		Model:  "english",
	}
}

func TestBuildLayaPayloadShape(t *testing.T) {
	payload, err := buildLayaPayload(classifierCall{
		rawBody: []byte(layaBody),
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: layaBackend(),
	})
	if err != nil {
		t.Fatalf("buildLayaPayload: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}

	if decoded["model"] != "english" {
		t.Errorf("model = %v, want english", decoded["model"])
	}

	state, ok := decoded["state"].(map[string]any)
	if !ok {
		t.Fatalf("state is not an object: %v", decoded["state"])
	}
	action, _ := state["action"].(string)
	if action != `{"Bash":"rm -rf build/"}` {
		t.Errorf("state.action = %q, want the graded action", action)
	}

	questions, ok := decoded["questions"].(map[string]any)
	if !ok {
		t.Fatalf("questions is not an object: %v", decoded["questions"])
	}
	risk, ok := questions["risk"].(map[string]any)
	if !ok {
		t.Fatalf("questions.risk is missing: %v", questions)
	}
	if risk["type"] != "choice" {
		t.Errorf("question type = %v, want choice", risk["type"])
	}
	criteria, ok := risk["criteria"].(map[string]any)
	if !ok || len(criteria) != 4 {
		t.Errorf("criteria = %v, want 4 entries", risk["criteria"])
	}
	for _, label := range []string{"A", "B", "C", "D"} {
		if _, exists := criteria[label]; !exists {
			t.Errorf("criteria is missing %q", label)
		}
	}
}

func TestBuildLayaPayloadTruncatesFromTheLeft(t *testing.T) {
	backend := layaBackend()
	backend.LayaStateChars = 200

	long := strings.Repeat("x", 500)
	body := strings.Replace(layaBody, `{\"Bash\":\"rm -rf build/\"}`, long+`END`, 1)

	payload, err := buildLayaPayload(classifierCall{
		rawBody: []byte(body),
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: backend,
	})
	if err != nil {
		t.Fatalf("buildLayaPayload: %v", err)
	}

	var decoded struct {
		State struct {
			Action string `json:"action"`
		} `json:"state"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.State.Action) != 200 {
		t.Errorf("action is %d characters, want 200", len(decoded.State.Action))
	}
	if !strings.HasSuffix(decoded.State.Action, "END") {
		t.Errorf("action = %q, want the tail kept: the graded action sits at the end", decoded.State.Action)
	}
}

func TestBuildLayaPayloadHonorsCustomCriteria(t *testing.T) {
	backend := layaBackend()
	backend.LayaQuestionName = "danger"
	backend.LayaCriteria = map[string]string{"X": "safe", "Y": "unsafe"}
	backend.LayaSeverityMap = map[string]int{"X": 0, "Y": 20}

	payload, err := buildLayaPayload(classifierCall{
		rawBody: []byte(layaBody),
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: backend,
	})
	if err != nil {
		t.Fatalf("buildLayaPayload: %v", err)
	}

	var decoded struct {
		Questions map[string]struct {
			Criteria map[string]string `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	question, exists := decoded.Questions["danger"]
	if !exists {
		t.Fatalf("questions has no danger entry: %v", decoded.Questions)
	}
	if len(question.Criteria) != 2 {
		t.Errorf("criteria = %v, want the 2 custom options", question.Criteria)
	}
}

func TestBuildLayaPayloadRejectsAnUnreadableTranscript(t *testing.T) {
	if _, err := buildLayaPayload(classifierCall{
		rawBody: []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"no transcript"}]}]}`),
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: layaBackend(),
	}); err == nil {
		t.Error("expected an error so the caller falls back to built-in handling")
	}
}

func TestBuildLayaPayloadRejectsAnUnsupportedKindBeforeTheCall(t *testing.T) {
	for _, kind := range []classifier.Kind{classifier.KindBlockPrefilter, classifier.KindNone} {
		_, err := buildLayaPayload(classifierCall{
			rawBody: []byte(layaBody),
			model:   "claude-sonnet-5",
			kind:    kind,
			backend: layaBackend(),
		})
		if !errors.Is(err, classifier.ErrUnsupportedKind) {
			t.Errorf("kind %v: err = %v, want ErrUnsupportedKind", kind, err)
		}
	}
}

func layaAnswer(label string) []byte {
	return []byte(`{"answers":{"risk":{"choice":"` + label + `","confidence":0.91}},
		"usage":{"input_tokens":40,"output_tokens":0}}`)
}

func verdictTextFrom(t *testing.T, message []byte) string {
	t.Helper()
	var envelope struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(message, &envelope); err != nil {
		t.Fatalf("response is not an Anthropic message: %v", err)
	}
	var text string
	for _, block := range envelope.Content {
		if block.Type == "text" {
			text += block.Text
		}
	}
	return text
}

func TestParseLayaResponseStage1(t *testing.T) {
	// D escalates under the default criteria; see
	// TestParseLayaResponseEscalatesTheRefusalBandByDefault.
	cases := map[string]string{
		"A": "<severity>0</severity>",
		"B": "<severity>5</severity>",
		"C": "<severity>15</severity>",
	}
	for label, want := range cases {
		t.Run(label, func(t *testing.T) {
			message, err := parseLayaResponse(layaAnswer(label), classifierCall{
				model:   "claude-sonnet-5",
				kind:    classifier.KindStage1Severity,
				backend: layaBackend(),
			})
			if err != nil {
				t.Fatalf("parseLayaResponse: %v", err)
			}
			if got := verdictTextFrom(t, message); got != want {
				t.Errorf("verdict = %q, want %q", got, want)
			}
		})
	}
}

func TestParseLayaResponseNeverReachesTheBlockBoundary(t *testing.T) {
	backend := layaBackend()
	// An operator map that would block, with the default clamp still in force.
	backend.LayaSeverityMap = map[string]int{"A": 0, "B": 5, "C": 15, "D": 95}
	backend.LayaCriteria = map[string]string{"A": "a", "B": "b", "C": "c", "D": "d"}

	message, err := parseLayaResponse(layaAnswer("D"), classifierCall{
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: backend,
	})
	if err != nil {
		t.Fatalf("parseLayaResponse: %v", err)
	}
	if got := verdictTextFrom(t, message); got != "<severity>49</severity>" {
		t.Errorf("verdict = %q, want the clamp to hold it at 49", got)
	}
}

func TestParseLayaResponseHonorsAnExplicitMaxSeverity(t *testing.T) {
	backend := layaBackend()
	backend.LayaMaxSeverity = new(10)

	message, err := parseLayaResponse(layaAnswer("C"), classifierCall{
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: backend,
	})
	if err != nil {
		t.Fatalf("parseLayaResponse: %v", err)
	}
	if got := verdictTextFrom(t, message); got != "<severity>10</severity>" {
		t.Errorf("verdict = %q, want 10", got)
	}
}

func TestParseLayaResponseErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		kind classifier.Kind
	}{
		{name: "unknown label", body: `{"answers":{"risk":{"choice":"Z"}}}`, kind: classifier.KindStage1Severity},
		{name: "missing question", body: `{"answers":{"other":{"choice":"A"}}}`, kind: classifier.KindStage1Severity},
		{name: "no answers", body: `{"usage":{}}`, kind: classifier.KindStage1Severity},
		{name: "not json", body: `<html>502</html>`, kind: classifier.KindStage1Severity},
		{name: "block prefilter", body: `{"answers":{"risk":{"choice":"A"}}}`, kind: classifier.KindBlockPrefilter},
		{name: "not a classifier request", body: `{"answers":{"risk":{"choice":"A"}}}`, kind: classifier.KindNone},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := parseLayaResponse([]byte(testCase.body), classifierCall{
				model:   "claude-sonnet-5",
				kind:    testCase.kind,
				backend: layaBackend(),
			}); err == nil {
				t.Error("expected an error so the caller falls back to built-in handling")
			}
		})
	}
}

func TestLayaAdapterIsRegistered(t *testing.T) {
	adapter := getBackendFormatAdapter(config.BackendFormatLaya)
	payload, err := adapter.preparePayload(classifierCall{
		rawBody: []byte(layaBody),
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: layaBackend(),
	})
	if err != nil {
		t.Fatalf("preparePayload: %v", err)
	}
	if !strings.Contains(string(payload), `"questions"`) {
		t.Errorf("registered adapter did not build a laya payload: %s", payload)
	}
}

func TestLayaAdapterSetsBearerHeaderOnlyWithAKey(t *testing.T) {
	adapter := getBackendFormatAdapter(config.BackendFormatLaya)

	withKey := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8000/v1/systemone", nil)
	adapter.setHeaders(withKey, "secret")
	if got := withKey.Header.Get("Authorization"); got != "Bearer secret" {
		t.Errorf("Authorization = %q, want Bearer secret", got)
	}

	withoutKey := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8000/v1/systemone", nil)
	adapter.setHeaders(withoutKey, "")
	if got := withoutKey.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want it absent", got)
	}
	if got := withoutKey.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestLayaRerouteEndToEnd(t *testing.T) {
	var receivedPath string
	var receivedBody []byte
	backendServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(layaAnswer("C"))
	}))
	defer backendServer.Close()

	srv := &Server{classifierAudit: classifier.NewRecorder(10)}
	srv.applyClassifierConfig(config.ClassifierConfig{
		Enabled: true,
		Rules: []config.Rule{{
			ID:      "stage1-laya",
			Name:    "Stage 1 to laya",
			Enabled: true,
			Conditions: config.RuleConditions{
				FooterPatterns: []config.MatchPattern{
					{Type: config.PatternSubstring, Pattern: "Grade HARM ONLY"},
				},
			},
			Action:        config.RuleActionReroute,
			TargetBackend: "laya",
		}},
		Backends: map[string]config.TargetBackend{
			"laya": {
				Name:   "laya",
				URL:    backendServer.URL + "/v1/systemone",
				Format: config.BackendFormatLaya,
				Model:  "english",
			},
		},
	})

	rule, backend, matched := srv.classifierMatcher.Match([]byte(layaBody))
	if !matched {
		t.Fatal("expected the rule to match")
	}

	recorder := httptest.NewRecorder()
	responded, _ := srv.applyClassifierRule(
		recorder,
		httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(layaBody)),
		classifierRequest{
			rule:    rule,
			backend: backend,
			rawBody: []byte(layaBody),
			model:   "claude-sonnet-5",
			kind:    classifier.KindStage1Severity,
		},
	)
	if !responded {
		t.Fatal("expected the reroute to answer")
	}
	if receivedPath != "/v1/systemone" {
		t.Errorf("laya was called at %q, want /v1/systemone", receivedPath)
	}
	if !strings.Contains(string(receivedBody), `"questions"`) {
		t.Errorf("laya received %s, want a typed-decision request", receivedBody)
	}
	if got := verdictTextFrom(t, recorder.Body.Bytes()); got != "<severity>15</severity>" {
		t.Errorf("client received %q, want the mapped severity for label C", got)
	}
	// The client must see the model it asked for, not the laya checkpoint
	// name, or the envelope no longer looks like an answer to its request.
	var envelope struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("response is not an Anthropic message: %v", err)
	}
	if envelope.Model != "claude-sonnet-5" {
		t.Errorf("envelope model = %q, want the request's model claude-sonnet-5", envelope.Model)
	}
}

// layaStage2Footer carries the Stage 2 footer marker classifier.Detect keys on.
const layaStage2Footer = "\nUse <thinking> first, then respond with <severity>N</severity>, plus <category>Exact BLOCK Rule Name</category> only when blocking.\n"

func TestBuildLayaPayloadEscalatesStage2BeforeTheCall(t *testing.T) {
	_, err := buildLayaPayload(classifierCall{
		rawBody: []byte(layaBody),
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage2Severity,
		backend: layaBackend(),
	})
	if !errors.Is(err, errClassifierEscalated) {
		t.Fatalf("err = %v, want an escalation: Stage 2 follows a teacher verdict a laya allow must not overrule", err)
	}
}

func TestBuildLayaPayloadPinsTheEnglishCheckpointByDefault(t *testing.T) {
	backend := layaBackend()
	backend.Model = ""
	payload, err := buildLayaPayload(classifierCall{
		rawBody: []byte(layaBody),
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: backend,
	})
	if err != nil {
		t.Fatalf("buildLayaPayload: %v", err)
	}
	var decoded struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if decoded.Model != "english" {
		t.Errorf("model = %q, want english: without it laya-serve routes by language to a checkpoint it may not have loaded", decoded.Model)
	}
}

func TestParseLayaResponseEscalatesTheRefusalBandByDefault(t *testing.T) {
	_, err := parseLayaResponse(layaAnswer("D"), classifierCall{
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: layaBackend(),
	})
	if !errors.Is(err, errClassifierEscalated) {
		t.Fatalf("err = %v, want an escalation for D", err)
	}
}

func TestParseLayaResponseAnswersEveryLabelWithEscalationOff(t *testing.T) {
	backend := layaBackend()
	backend.LayaEscalateLabels = []string{}

	message, err := parseLayaResponse(layaAnswer("D"), classifierCall{
		model:   "claude-sonnet-5",
		kind:    classifier.KindStage1Severity,
		backend: backend,
	})
	if err != nil {
		t.Fatalf("parseLayaResponse: %v", err)
	}
	if got := verdictTextFrom(t, message); got != "<severity>35</severity>" {
		t.Errorf("verdict = %q, want D's mapped severity", got)
	}
}

func TestParseLayaResponseEscalatesBelowTheConfidenceFloor(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		escalate bool
	}{
		{name: "below the floor", body: `{"answers":{"risk":{"choice":"A","answer_confidence":0.41}}}`, escalate: true},
		{name: "at the floor", body: `{"answers":{"risk":{"choice":"A","answer_confidence":0.6}}}`, escalate: false},
		// confidence is laya's entropy score, not the calibrated one: a
		// response carrying only it cannot clear the floor.
		{name: "no calibrated confidence", body: `{"answers":{"risk":{"choice":"A","confidence":0.99}}}`, escalate: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			backend := layaBackend()
			backend.LayaMinConfidence = 0.6
			_, err := parseLayaResponse([]byte(testCase.body), classifierCall{
				model:   "claude-sonnet-5",
				kind:    classifier.KindStage1Severity,
				backend: backend,
			})
			if got := errors.Is(err, errClassifierEscalated); got != testCase.escalate {
				t.Errorf("escalated = %v (err %v), want %v", got, err, testCase.escalate)
			}
			if !testCase.escalate && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestLayaEscalationFallsThroughToBuiltInHandling drives the whole
// /v1/messages path. An escalated request must be answered by built-in
// handling (always_stub here, the teacher in a real deployment), audited as
// escalated, and never recorded as a laya row.
func TestLayaEscalationFallsThroughToBuiltInHandling(t *testing.T) {
	cases := []struct {
		name        string
		footer      string
		wantLayaHit bool
	}{
		{name: "stage 1 answered D", footer: classifierStage1Footer, wantLayaHit: true},
		{name: "stage 2 escalates before the call", footer: layaStage2Footer, wantLayaHit: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var layaHits atomic.Int32
			laya := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				layaHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(layaAnswer("D"))
			}))
			defer laya.Close()

			orig := config.Get()
			t.Cleanup(func() { config.SetForTest(orig) })
			server, _ := newAccountBackedTestServer(t)
			server.classifierAudit = classifier.NewRecorder(10)
			dir := t.TempDir()

			cfg := config.Get()
			cfg.Classifier.Enabled = true
			cfg.Classifier.Action = config.ActionAlwaysStub
			cfg.Classifier.Capture = config.ClassifierCaptureConfig{Enabled: true, Dir: dir}
			cfg.Classifier.Rules = []config.Rule{{
				ID:      "laya",
				Name:    "Laya",
				Enabled: true,
				Conditions: config.RuleConditions{
					SystemPromptPatterns: []config.MatchPattern{
						{Type: config.PatternSubstring, Pattern: "You are a security monitor"},
					},
				},
				Action:        config.RuleActionReroute,
				TargetBackend: "laya",
			}}
			cfg.Classifier.Backends = map[string]config.TargetBackend{
				"laya": {Name: "laya", URL: laya.URL + "/v1/systemone", Format: config.BackendFormatLaya},
			}
			config.SetForTest(cfg)
			server.applyClassifierConfig(cfg.Classifier)

			rec := postClassifierMessages(t, server, classifierShapedBody(t, classifierTestModel, testCase.footer))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
			}
			if got := layaHits.Load() > 0; got != testCase.wantLayaHit {
				t.Errorf("laya-serve called = %v, want %v", got, testCase.wantLayaHit)
			}
			history := server.classifierAudit.History()
			if len(history) != 1 || history[0].Status != classifier.EventStatusEscalated {
				t.Fatalf("audit = %+v, want one escalated event", history)
			}
			rows := readCaptureRows(t, server, dir)
			if len(rows) != 1 || rows[0].Source != corpus.SourceStub {
				t.Fatalf("capture rows = %+v, want one stub row: built-in handling answered, not laya", rows)
			}
		})
	}
}
