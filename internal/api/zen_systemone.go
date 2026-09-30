package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/zen"
)

// systemone serves POST /v1/systemone by forwarding the Jev typed-decision
// request to the Zen gateway unchanged: the body already speaks the systemone
// contract ({model, state, questions}), which has no faithful Anthropic
// Messages mapping, so the proxy only resolves the Zen key, claims the
// OpenCode harness identity, and rewrites the transport.
//
// The wire allowlist that guards POST /v1/messages is deliberately not
// consulted: systemone is a separate route, and a jev-* id stays
// non-forwardable for /v1/messages because it cannot serve that wire.
func (server *Server) systemone(writer http.ResponseWriter, request *http.Request) {
	cfg := config.Get().Zen
	if !cfg.Enabled {
		writeAPIError(writer, http.StatusNotFound, "not_found_error",
			"Endpoint "+request.Method+" "+request.URL.Path+" not found (Zen gateway is disabled)")
		return
	}
	key := zenAPIKey(cfg)
	if key == "" {
		writeAPIError(writer, http.StatusUnauthorized, "authentication_error",
			"Zen API key not configured (zen.apiKey or OPENCODE_API_KEY)")
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBody)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeAPIError(writer, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body too large")
			return
		}
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Failed to read request body: "+err.Error())
		return
	}
	// Read the model only for the free-tier gate warning; the body itself is
	// forwarded byte-identically.
	var probe struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &probe)

	if server.logger != nil {
		server.logger.Info("zen systemone forward", "model", probe.Model)
	}
	zen.ForwardSystemOne(writer, request, cfg.BaseURL, key, body, func(resp *http.Response) error {
		zen.ObserveFreeTierGate(resp, probe.Model)
		return nil
	})
}
