package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"antigravity-go-proxy/internal/classifier"
	"antigravity-go-proxy/internal/classifier/corpus"
	"antigravity-go-proxy/internal/config"
)

// layaQuestion is one typed question in laya-serve's /v1/systemone protocol.
type layaQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type layaRequest struct {
	Model     string                  `json:"model,omitempty"`
	State     map[string]string       `json:"state"`
	Questions map[string]layaQuestion `json:"questions"`
}

// buildLayaPayload turns a Stage 1 classifier request into a laya
// typed-decision request. The classifier body runs to roughly 125KB while
// laya's English checkpoint holds about 320 tokens of state, so only the
// graded action is sent; an unreadable transcript is an error, never a guess.
func buildLayaPayload(call classifierCall) ([]byte, error) {
	// Both refusals happen before any network call, so neither waits on the
	// backend timeout.
	switch call.kind {
	case classifier.KindStage1Severity:
	case classifier.KindStage2Severity:
		// Stage 2 applies user intent, which the action-only state laya sees
		// does not carry, and reconsiders an action Stage 1 graded high. A
		// laya Stage 1 answer never is, so this request follows a teacher
		// verdict, which a capped laya allow must not overrule.
		return nil, fmt.Errorf("%w: stage 2 goes to the teacher", errClassifierEscalated)
	default:
		// KindBlockPrefilter's response format was never captured, so it must
		// not be guessed; KindNone is not a classifier request at all.
		return nil, classifier.ErrUnsupportedKind
	}
	settings := call.backend.LayaSettings()

	action, _, err := corpus.ExtractAction(call.rawBody, 0)
	if err != nil {
		return nil, err
	}
	// Truncate from the left: the graded action sits at the tail, so the head
	// is what can be lost without losing the thing being judged.
	if runes := []rune(action); len(runes) > settings.StateChars {
		action = string(runes[len(runes)-settings.StateChars:])
	}

	payload := layaRequest{
		Model: settings.Model,
		State: map[string]string{"action": action},
		Questions: map[string]layaQuestion{
			settings.QuestionName: {
				Type:         "choice",
				Instructions: settings.Instructions,
				Criteria:     settings.Criteria,
			},
		},
	}
	return json.Marshal(payload)
}

// layaSettingsFor is a nil-safe accessor used by the adapter functions.
func layaSettingsFor(backend *config.TargetBackend) config.LayaSettings {
	if backend == nil {
		return config.TargetBackend{}.LayaSettings()
	}
	return backend.LayaSettings()
}

// layaTypedAnswer is one typed answer in a /v1/systemone response.
// AnswerConfidence is laya's calibrated max(p). laya's "confidence" field is
// a normalized entropy on another scale, so a laya backend's floor never
// reads it. Jev has no answer_confidence; its "confidence" is the certainty
// TypeSafe documents for gating, derived from the probabilities.
type layaTypedAnswer struct {
	Choice           string   `json:"choice"`
	AnswerConfidence *float64 `json:"answer_confidence"`
	Confidence       *float64 `json:"confidence"`
}

// floorConfidence is the value layaMinConfidence compares for this backend,
// with the response field it came from, which the escalation detail names.
func (answer layaTypedAnswer) floorConfidence(backend *config.TargetBackend) (*float64, string) {
	if backend != nil && backend.Format == config.BackendFormatJev {
		return answer.Confidence, "confidence"
	}
	return answer.AnswerConfidence, "answer_confidence"
}

// systemOneName names the backend behind an error. The audit feed shows
// err.Error(), and a Jev failure must not read as a laya one.
func systemOneName(backend *config.TargetBackend) string {
	if backend != nil && backend.Format == config.BackendFormatJev {
		return "jev"
	}
	return "laya"
}

type layaResponse struct {
	Answers map[string]layaTypedAnswer `json:"answers"`
}

// parseLayaResponse maps a laya label to a Stage 1 severity verdict, or
// escalates: a label in EscalateLabels, or an answer below MinConfidence,
// goes to built-in handling instead. Severity is clamped by MaxSeverity,
// which defaults below the 50 allow/block boundary.
func parseLayaResponse(respBody []byte, call classifierCall) ([]byte, error) {
	// buildLayaPayload stops every other kind before the call.
	if call.kind != classifier.KindStage1Severity {
		return nil, classifier.ErrUnsupportedKind
	}
	settings := layaSettingsFor(call.backend)
	name := systemOneName(call.backend)

	var decoded layaResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return nil, fmt.Errorf("%s: response is not JSON: %w", name, err)
	}
	answer, exists := decoded.Answers[settings.QuestionName]
	if !exists {
		return nil, fmt.Errorf("%s: response carries no answer for question %q", name, settings.QuestionName)
	}
	severity, known := settings.SeverityMap[answer.Choice]
	if !known {
		return nil, fmt.Errorf("%s: answer label %q is not in the severity map", name, answer.Choice)
	}
	if slices.Contains(settings.EscalateLabels, answer.Choice) {
		return nil, fmt.Errorf("%w: %s chose %s", errClassifierEscalated, name, answer.Choice)
	}
	if settings.MinConfidence > 0 {
		confidence, field := answer.floorConfidence(call.backend)
		if confidence == nil {
			return nil, fmt.Errorf("%w: %s chose %s with no %s to check against the floor", errClassifierEscalated, name, answer.Choice, field)
		}
		if *confidence < settings.MinConfidence {
			return nil, fmt.Errorf("%w: %s chose %s with %s %.2f, below %.2f",
				errClassifierEscalated, name, answer.Choice, field, *confidence, settings.MinConfidence)
		}
	}
	if severity > settings.MaxSeverity {
		severity = settings.MaxSeverity
	}
	if severity < 0 {
		severity = 0
	}
	return classifier.StubWithText(call.model, fmt.Sprintf("<severity>%d</severity>", severity))
}

var layaFormatAdapter = backendFormatAdapter{
	defaultTimeout: defaultLayaBackendTimeout,
	preparePayload: buildLayaPayload,
	setHeaders: func(req *http.Request, apiKey string) {
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	},
	parseResponse: parseLayaResponse,
}
