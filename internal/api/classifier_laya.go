package api

import (
	"encoding/json"
	"fmt"
	"net/http"

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

// buildLayaPayload turns a classifier request into a laya typed-decision
// request. The classifier body runs to roughly 125KB while laya's English
// checkpoint holds about 320 tokens of state, so only the graded action is
// sent; an unreadable transcript is an error, never a guess.
func buildLayaPayload(call classifierCall) ([]byte, error) {
	// Reject a kind parseLayaResponse cannot render before any network call,
	// so an unsupported request does not wait on the backend timeout.
	if !layaSupportsKind(call.kind) {
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
		Model: call.backend.Model,
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

// layaSupportsKind reports whether parseLayaResponse can render a verdict
// for kind. KindBlockPrefilter's response format was never captured, so it
// must not be guessed; KindNone is not a classifier request at all.
func layaSupportsKind(kind classifier.Kind) bool {
	return kind == classifier.KindStage1Severity || kind == classifier.KindStage2Severity
}

// layaSettingsFor is a nil-safe accessor used by the adapter functions.
func layaSettingsFor(backend *config.TargetBackend) config.LayaSettings {
	if backend == nil {
		return config.TargetBackend{}.LayaSettings()
	}
	return backend.LayaSettings()
}

// layaTypedAnswer is one typed answer in a /v1/systemone response.
type layaTypedAnswer struct {
	Choice     string  `json:"choice"`
	Confidence float64 `json:"confidence"`
}

type layaResponse struct {
	Answers map[string]layaTypedAnswer `json:"answers"`
}

// layaThinkingPhrases give stage 2 a one-line rationale per default label.
// Stage 2 requires a <thinking> block; allow verdicts carry no <category>
// tag. The phrases describe the default criteria only: custom criteria can
// give A-D other meanings, so they get layaFallbackThinking instead.
var layaThinkingPhrases = map[string]string{
	"A": "Read-only inspection; no policy match.",
	"B": "Local write action; no policy match.",
	"C": "Outbound effect; no policy match.",
	"D": "Destructive shape; no policy match.",
}

const layaFallbackThinking = "Routine action, no policy match."

// parseLayaResponse maps a laya label to a severity and renders the verdict
// shape the detected variant requires. Severity is clamped below the 50
// allow/block boundary by MaxSeverity, so a laya answer cannot block.
func parseLayaResponse(respBody []byte, call classifierCall) ([]byte, error) {
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
	if severity > settings.MaxSeverity {
		severity = settings.MaxSeverity
	}
	if severity < 0 {
		severity = 0
	}

	var verdict string
	switch call.kind {
	case classifier.KindStage1Severity:
		verdict = fmt.Sprintf("<severity>%d</severity>", severity)
	case classifier.KindStage2Severity:
		thinking := layaFallbackThinking
		if phrase, ok := layaThinkingPhrases[answer.Choice]; ok && (call.backend == nil || len(call.backend.LayaCriteria) == 0) {
			thinking = phrase
		}
		verdict = fmt.Sprintf("<thinking>%s</thinking><severity>%d</severity>", thinking, severity)
	default:
		// KindBlockPrefilter's response format was never captured, so it must
		// not be guessed; KindNone is not a classifier request at all.
		return nil, classifier.ErrUnsupportedKind
	}

	return classifier.StubWithText(call.model, verdict)
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
