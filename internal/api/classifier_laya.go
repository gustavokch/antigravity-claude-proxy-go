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
// AnswerConfidence is laya's calibrated max(p). Its "confidence" field is a
// normalized entropy on another scale, so the floor never reads it.
type layaTypedAnswer struct {
	Choice           string   `json:"choice"`
	AnswerConfidence *float64 `json:"answer_confidence"`
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

	var decoded layaResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return nil, fmt.Errorf("laya: response is not JSON: %w", err)
	}
	answer, exists := decoded.Answers[settings.QuestionName]
	if !exists {
		return nil, fmt.Errorf("laya: response carries no answer for question %q", settings.QuestionName)
	}
	severity, known := settings.SeverityMap[answer.Choice]
	if !known {
		return nil, fmt.Errorf("laya: answer label %q is not in the severity map", answer.Choice)
	}
	if slices.Contains(settings.EscalateLabels, answer.Choice) {
		return nil, fmt.Errorf("%w: laya chose %s", errClassifierEscalated, answer.Choice)
	}
	if settings.MinConfidence > 0 {
		if answer.AnswerConfidence == nil {
			return nil, fmt.Errorf("%w: laya chose %s with no answer_confidence to check against the floor", errClassifierEscalated, answer.Choice)
		}
		if *answer.AnswerConfidence < settings.MinConfidence {
			return nil, fmt.Errorf("%w: laya chose %s with answer_confidence %.2f, below %.2f",
				errClassifierEscalated, answer.Choice, *answer.AnswerConfidence, settings.MinConfidence)
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
	preparePayload: buildLayaPayload,
	setHeaders: func(req *http.Request, apiKey string) {
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	},
	parseResponse: parseLayaResponse,
}
