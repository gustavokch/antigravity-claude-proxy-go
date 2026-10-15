package api

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/classifier"
	"antigravity-go-proxy/internal/config"
)

// Bodies captured from POST https://opencode.ai/zen/v1/systemone with model
// jev-1.13-free on 2026-10-15, asking the default "block" noul question about
// `ls -la` and `git push origin main --force`.
const (
	jevBlockNoAnswer    = `{"model":"jev-1.13-free","answers":{"block":{"type":"noul","noul":0.03}},"usage":{"input_tokens":346,"output_tokens":20}}`
	jevBlockMaybeAnswer = `{"model":"jev-1.13-free","answers":{"block":{"type":"noul","noul":0.41}},"usage":{"input_tokens":346,"output_tokens":20}}`
	jevBlockYesAnswer   = `{"model":"jev-1.13-free","answers":{"block":{"type":"noul","noul":0.97}},"usage":{"input_tokens":346,"output_tokens":20}}`
)

func TestParseJevBlockResponseThreshold(t *testing.T) {
	backend := jevBackend("u")
	call := classifierCall{rawBody: []byte(layaBody), model: "claude-sonnet-5", kind: classifier.KindBlockPrefilter, backend: backend}
	cases := []struct {
		name         string
		body         string
		threshold    float64
		wantAllow    bool
		wantEscalate bool
	}{
		{name: "below the default line allows", body: `{"answers":{"block":{"noul":0.09}}}`, wantAllow: true},
		{name: "at the default line escalates", body: `{"answers":{"block":{"noul":0.10}}}`, wantEscalate: true},
		{name: "a configured line moves it", body: `{"answers":{"block":{"noul":0.30}}}`, threshold: 0.5, wantAllow: true},
		{name: "no answer for the question is an error", body: `{"answers":{"risk":{"noul":0.01}}}`},
		{name: "a missing noul is an error", body: `{"answers":{"block":{"type":"noul"}}}`},
		{name: "out of range is an error", body: `{"answers":{"block":{"noul":1.4}}}`},
		{name: "not json is an error", body: `<html>502</html>`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			call.backend = &config.TargetBackend{Format: config.BackendFormatJev, JevBlockThreshold: testCase.threshold}
			message, err := parseLayaResponse([]byte(testCase.body), call)
			switch {
			case testCase.wantAllow:
				if err != nil {
					t.Fatalf("err = %v, want an allow", err)
				}
				if got := verdictTextFrom(t, message); got != "<block>no</block>" {
					t.Errorf("verdict = %q, want <block>no</block>", got)
				}
			case testCase.wantEscalate:
				if err == nil || !strings.Contains(err.Error(), errClassifierEscalated.Error()) {
					t.Errorf("err = %v, want an escalation", err)
				}
			default:
				if err == nil {
					t.Errorf("want an error, got verdict %q", verdictTextFrom(t, message))
				}
			}
		})
	}
}

// A laya checkpoint is not offered the block verdict.
func TestLayaBackendStillRefusesBlockPrefilter(t *testing.T) {
	_, err := buildLayaPayload(classifierCall{
		rawBody: []byte(layaBody), model: "claude-sonnet-5", kind: classifier.KindBlockPrefilter,
		backend: &config.TargetBackend{Format: config.BackendFormatLaya},
	})
	if err != classifier.ErrUnsupportedKind {
		t.Errorf("err = %v, want ErrUnsupportedKind", err)
	}
}

func TestBuildJevBlockPayloadAsksOneNoulQuestionAboutTheAction(t *testing.T) {
	call := classifierCall{rawBody: []byte(layaBody), model: "claude-sonnet-5", kind: classifier.KindBlockPrefilter, backend: jevBackend("u")}

	payload, err := buildLayaPayload(call)
	if err != nil {
		t.Fatalf("buildLayaPayload: %v", err)
	}
	var sent struct {
		Model     string            `json:"model"`
		State     map[string]string `json:"state"`
		Questions map[string]struct {
			Type         string            `json:"type"`
			Instructions string            `json:"instructions"`
			Criteria     map[string]string `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(payload, &sent); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	question, exists := sent.Questions["block"]
	if !exists || len(sent.Questions) != 1 {
		t.Fatalf("questions = %v, want exactly the block question", sent.Questions)
	}
	if question.Type != "noul" || question.Instructions == "" || question.Criteria["true"] == "" || question.Criteria["false"] == "" {
		t.Errorf("question = %+v, want a noul with instructions and both criteria", question)
	}
	if sent.Model != "jev-1.13-free" || sent.State["action"] != `{"Bash":"rm -rf build/"}` {
		t.Errorf("model/state = %q/%v, want the free Jev model and the graded action", sent.Model, sent.State)
	}

	long := strings.Replace(layaBody, `{\"Bash\":\"rm -rf build/\"}`, strings.Repeat("x", 1201), 1)
	call.rawBody = []byte(long)
	if _, err := buildLayaPayload(call); !errors.Is(err, errClassifierEscalated) {
		t.Errorf("1201-character action: err = %v, want an escalation, as for Stage 1", err)
	}
}
