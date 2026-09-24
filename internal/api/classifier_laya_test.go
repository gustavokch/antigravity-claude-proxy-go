package api

import (
	"encoding/json"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/classifier"
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
