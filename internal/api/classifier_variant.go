package api

import (
	"net/http"

	"antigravity-go-proxy/internal/classifier"
	"antigravity-go-proxy/internal/classifier/corpus"
	"antigravity-go-proxy/internal/config"
)

// builtinJevBackendName is the backend name audit events carry when a variant
// is answered by the built-in Jev default rather than a configured backend.
const builtinJevBackendName = "zen-jev (built-in)"

// variantRoute names the backend that answers a classifier variant before the
// teacher is consulted. ok is false when the variant stays on the legacy path.
//
// Only Stage 1 and block-prefilter are routed. Stage 2 applies user intent and
// needs <thinking>, which a typed-decision model cannot produce.
func variantRoute(cfg config.Config, kind classifier.Kind) (name string, backend config.TargetBackend, ok bool) {
	if kind != classifier.KindStage1Severity && kind != classifier.KindBlockPrefilter {
		return "", config.TargetBackend{}, false
	}
	// passthrough is the collection window: every classifier request must
	// reach the teacher so it yields a label.
	if cfg.Classifier.Action == config.ActionPassthrough {
		return "", config.TargetBackend{}, false
	}
	target := cfg.Classifier.Variants[kind.String()].TargetBackend
	switch target {
	case config.VariantBackendOff:
		return "", config.TargetBackend{}, false
	case "":
		// The built-in default sends each graded action to Zen and on to
		// TypeSafe, so it needs the operator to have enabled Zen and to hold a
		// key. A deployment with neither behaves exactly as before.
		if !cfg.Zen.Enabled || zenAPIKey(cfg.Zen) == "" {
			return "", config.TargetBackend{}, false
		}
		return builtinJevBackendName, config.DefaultJevBackend(cfg.Zen.BaseURL), true
	}
	configured, exists := cfg.Classifier.Backends[target]
	if !exists {
		return "", config.TargetBackend{}, false
	}
	return target, configured, true
}

// answerFromVariantBackend tries the variant's backend and reports whether it
// wrote the response. A failure or an escalation returns false with the audit
// event recorded, and the caller continues into the legacy path, so the
// teacher still grades whatever the backend declined.
func (server *Server) answerFromVariantBackend(
	writer http.ResponseWriter,
	request *http.Request,
	cfg config.Config,
	kind classifier.Kind,
	rawBody []byte,
	model string,
	captureSource *corpus.Source,
) bool {
	name, backend, ok := variantRoute(cfg, kind)
	if !ok {
		return false
	}
	rule := config.Rule{
		ID:            "variant:" + kind.String(),
		Name:          "Variant default: " + kind.String(),
		Enabled:       true,
		Action:        config.RuleActionReroute,
		TargetBackend: name,
	}
	responded, _ := server.applyClassifierRule(writer, request, classifierRequest{
		rule:          &rule,
		backend:       &backend,
		rawBody:       rawBody,
		model:         model,
		kind:          kind,
		captureSource: captureSource,
	})
	return responded
}
