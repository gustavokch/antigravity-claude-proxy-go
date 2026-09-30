package api

import (
	"errors"
	"net/http"
	"time"

	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/zen"
)

// defaultJevBackendTimeout bounds a Jev call. It sits in front of the user's
// permission prompt like a laya call does. Zen answered in 0.39-0.69s warm and
// 3.4s on the first call of a cold process (2026-09-30), so 5s leaves headroom
// for the cold start without letting a stalled call hold the prompt; a timeout
// only costs a teacher call, because the reroute fails open.
const defaultJevBackendTimeout = 5 * time.Second

// resolveJevKey picks the Zen credential for a Jev backend: its own apiKey,
// then zen.apiKey, then OPENCODE_API_KEY. Sharing zenAPIKey keeps this in step
// with the gateway routes.
func resolveJevKey(backend *config.TargetBackend) (string, error) {
	if backend.APIKey != "" {
		return backend.APIKey, nil
	}
	if key := zenAPIKey(config.Get().Zen); key != "" {
		return key, nil
	}
	return "", errors.New("jev: no Zen key (backend apiKey, zen.apiKey or OPENCODE_API_KEY)")
}

// setJevHeaders authenticates the way the gateway's systemone forward does
// (Bearer and x-api-key) and claims the OpenCode harness identity, as every
// other Zen-bound request does.
func setJevHeaders(req *http.Request, apiKey string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("x-api-key", apiKey)
	zen.ApplyHarnessHeaders(req)
}

// jevFormatAdapter reuses the laya request, severity mapping and escalation:
// Jev answers the same typed-decision question. What differs is the
// credential, the harness identity, the HTTP client (zen.TLSClient honours the
// operator's TLS-disguise opt-in) and the field the confidence floor reads.
var jevFormatAdapter = backendFormatAdapter{
	defaultTimeout: defaultJevBackendTimeout,
	preparePayload: buildLayaPayload,
	setHeaders:     setJevHeaders,
	parseResponse:  parseLayaResponse,
	resolveKey:     resolveJevKey,
	client:         zen.TLSClient,
}
