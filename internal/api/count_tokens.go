package api

import (
	"encoding/json"
	"io"
	"net/http"

	"antigravity-go-proxy/internal/config"
)

// countTokens answers POST /v1/messages/count_tokens with {"input_tokens": N}.
//
// A count takes the /v1/messages route: model mapping, then the gateway
// precedence, which decides the model id the count is for. Headroom and the
// classifier do not run: a count measures the request as the client wrote it.
// The count is estimated from what the conversation really cost so far
// (internal/tokencount). Only a body the proxy cannot read is refused.
func (server *Server) countTokens(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBody)
	rawBody, err := io.ReadAll(request.Body)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Failed to read request body: "+err.Error())
		return
	}
	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Invalid JSON request body: "+err.Error())
		return
	}
	if _, ok := body["messages"].([]any); !ok {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "messages is required and must be an array")
		return
	}
	model := server.resolveModelMapping(stringFrom(body["model"]))
	session := ccExtractSessionID(request, body)
	if route, routed := server.routeGateway(request.Context(), config.Get(), model); routed {
		model = route.target
	}
	tokens := server.tokens.Estimate(session, model, body)
	server.logger.Debug("count_tokens estimate", "session_id", session, "model", model, "input_tokens", tokens)
	writeJSON(writer, http.StatusOK, map[string]any{"input_tokens": tokens})
}

// observeTokens gives the estimator the real input count of a completed
// request: its uncached input plus cache reads and writes. An answer that
// reported no usage teaches nothing, and a bare test Server has no estimator.
func (server *Server) observeTokens(session, model string, request map[string]any, realInput int) {
	if server.tokens == nil || request == nil || realInput <= 0 {
		return
	}
	server.tokens.Observe(session, model, request, realInput)
}
