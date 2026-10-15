package api

import (
	"encoding/json"
	"fmt"

	"antigravity-go-proxy/internal/classifier"
	"antigravity-go-proxy/internal/classifier/corpus"
)

// blockAllowVerdict is the only block-prefilter verdict a jev backend ever
// returns. Claude Code reads the answer with /<block>(yes|no)\b(<\/block>)?/
// (Claude Code 2.1.274, the Xit parser) and treats "no" as "Allowed by fast
// classifier". A model that cannot explain itself must not issue "yes", so
// every other outcome is an escalation and the teacher decides.
const blockAllowVerdict = "<block>no</block>"

type jevNoulQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

type jevBlockRequest struct {
	Model     string                     `json:"model"`
	State     map[string]string          `json:"state"`
	Questions map[string]jevNoulQuestion `json:"questions"`
}

// buildJevBlockPayload asks Jev one yes/no question about the graded action.
// The block-prefilter body carries the same transcript layout as Stage 1, so
// the action comes out the same way, and an over-long one goes to the teacher
// whole for the same reason (the state holds the action alone).
func buildJevBlockPayload(call classifierCall) ([]byte, error) {
	settings := call.backend.LayaSettings()
	block := call.backend.JevBlockSettings()

	action, _, err := corpus.ExtractAction(call.rawBody, 0)
	if err != nil {
		return nil, err
	}
	if runes := []rune(action); len(runes) > settings.StateChars {
		return nil, fmt.Errorf("%w: action is %d characters, over the %d a jev backend grades",
			errClassifierEscalated, len(runes), settings.StateChars)
	}
	return json.Marshal(jevBlockRequest{
		Model: settings.Model,
		State: map[string]string{"action": action},
		Questions: map[string]jevNoulQuestion{
			block.QuestionName: {Type: "noul", Instructions: block.Instructions, Criteria: block.Criteria},
		},
	})
}

type jevNoulAnswer struct {
	Noul *float64 `json:"noul"`
}

type jevBlockResponse struct {
	Answers map[string]jevNoulAnswer `json:"answers"`
}

// parseJevBlockResponse turns a noul probability into the allow verdict, or
// escalates. A probability at or above the threshold, a missing one, and one
// outside [0, 1] all escalate: only a clear "no" ends the check.
func parseJevBlockResponse(respBody []byte, call classifierCall) ([]byte, error) {
	block := call.backend.JevBlockSettings()

	var decoded jevBlockResponse
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return nil, fmt.Errorf("jev: response is not JSON: %w", err)
	}
	answer, exists := decoded.Answers[block.QuestionName]
	if !exists || answer.Noul == nil {
		return nil, fmt.Errorf("jev: response carries no noul answer for question %q", block.QuestionName)
	}
	probability := *answer.Noul
	if probability < 0 || probability > 1 {
		return nil, fmt.Errorf("jev: noul %.2f is outside [0, 1]", probability)
	}
	if probability >= block.Threshold {
		return nil, fmt.Errorf("%w: jev scored the action %.2f, at or above the %.2f block line",
			errClassifierEscalated, probability, block.Threshold)
	}
	return classifier.StubWithText(call.model, blockAllowVerdict)
}
