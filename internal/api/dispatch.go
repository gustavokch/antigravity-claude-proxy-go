package api

import (
	"encoding/json"
	"net/http"

	"antigravity-go-proxy/internal/config"
)

// gatewayRequest carries what one alternate-backend dispatch pass needs. A
// gateway that accepts the request writes its response and returns true. A
// gateway that declines must leave every field untouched, so the next gateway
// sees the request exactly as the previous one did.
type gatewayRequest struct {
	writer  http.ResponseWriter
	request *http.Request
	cfg     config.Config
	body    map[string]any
	rawBody []byte
	mutated bool
	model   string
}

// gatewayHandlers maps every gateway this build can dispatch to onto its
// handler. cloudcode is deliberately absent: it is the terminal account-backed
// path, not a gateway. TestGatewayHandlers_CoverEveryKnownGateway pins that this
// table covers exactly config.KnownGatewayIDs() minus cloudcode.
var gatewayHandlers = map[config.GatewayID]func(*Server, *gatewayRequest) bool{
	config.GatewayKimi:       (*Server).tryKimiGateway,
	config.GatewayZen:        (*Server).tryZenGateway,
	config.GatewayClaudeCode: (*Server).tryClaudeCodeGateway,
	config.GatewayOpenRouter: (*Server).tryOpenRouterGateway,
	config.GatewayCustom:     (*Server).tryCustomEndpointGateway,
}

// dispatchAlternateBackend walks the configured precedence and hands the request
// to the first gateway that accepts it. It returns true when a gateway wrote the
// response, and false when no gateway handled the request: cloudcode was
// reached, or every listed gateway declined.
//
// Each handler re-checks its own enabled flag, its own matcher, and (for Zen)
// its own key resolution. The walk is responsible for order only. Keeping those
// conditions inside the handlers is deliberate: they are the exact conditions
// the pre-refactor if-chain used.
//
// An operator who hoists `cloudcode` ahead of a gateway is choosing to have the
// classifier-fallback gate consulted first: the walk stops at cloudcode and the
// caller falls through to the account-backed path (and its gate) below.
func (server *Server) dispatchAlternateBackend(g *gatewayRequest) bool {
	for _, id := range g.cfg.GatewayOrder.Effective(g.model) {
		if id == config.GatewayCloudCode {
			return false
		}
		handler, ok := gatewayHandlers[id]
		if !ok {
			continue
		}
		if handler(server, g) {
			return true
		}
	}
	return false
}

func (server *Server) tryKimiGateway(g *gatewayRequest) bool {
	if !g.cfg.Kimi.Enabled {
		return false
	}
	kimiEntry, ok := matchKimiModelEntry(g.cfg.Kimi, g.model)
	if !ok {
		return false
	}
	targetModel := stripKimi1mSuffix(kimiEntry.ID)
	g.body["model"] = targetModel
	reqBody, err := json.Marshal(g.body)
	if err != nil {
		writeAPIError(g.writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal Kimi request: "+err.Error())
		return true
	}
	reqBody = applyMaxTokensPolicy(reqBody, g.body, kimiEntry.MaxOutputTokens, 0)
	server.forwardToKimi(g.writer, g.request, g.cfg.Kimi, reqBody, targetModel)
	return true
}

func (server *Server) tryZenGateway(g *gatewayRequest) bool {
	if !g.cfg.Zen.Enabled {
		return false
	}
	zenEntry, ok := matchZenModelEntry(g.cfg.Zen, g.model)
	if !ok {
		return false
	}
	target := zenTargetModel(zenEntry)
	g.body["model"] = target
	reqBody, err := json.Marshal(g.body)
	if err != nil {
		writeAPIError(g.writer, http.StatusBadRequest, "invalid_request_error",
			"Failed to marshal Zen request: "+err.Error())
		return true
	}
	server.forwardToZen(g.writer, g.request, g.cfg.Zen, reqBody, g.body, target, zenEntry)
	return true
}

func (server *Server) tryClaudeCodeGateway(g *gatewayRequest) bool {
	if !g.cfg.ClaudeCode.Enabled {
		return false
	}
	ccMatch := matchClaudeCodeModel(g.cfg.ClaudeCode, g.model)
	if ccMatch == "" {
		return false
	}
	g.body["model"] = ccMatch
	ccBody, err := json.Marshal(g.body)
	if err != nil {
		writeAPIError(g.writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal ClaudeCode request: "+err.Error())
		return true
	}
	// The Anthropic gateway requires max_tokens; derive it from the
	// allowlist entry (defaults carry each model's max output) when
	// the client omitted it. Client values are clamped down, never up.
	ccBody = applyMaxTokensPolicy(ccBody, g.body, 0, claudeCodeEntryMaxOutput(g.cfg.ClaudeCode, ccMatch))
	server.forwardToClaudeCode(g.writer, g.request, g.cfg.ClaudeCode, ccBody, ccMatch)
	return true
}

func (server *Server) tryOpenRouterGateway(g *gatewayRequest) bool {
	if !g.cfg.OpenRouter.Enabled {
		return false
	}
	for _, item := range g.cfg.OpenRouter.Allowlist {
		if !item.Enabled {
			continue
		}
		if item.ID == g.model || (item.Alias != "" && item.Alias == g.model) {
			g.body["model"] = item.ID
			reqBody, err := json.Marshal(g.body)
			if err != nil {
				writeAPIError(g.writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal request: "+err.Error())
				return true
			}
			server.forwardToOpenRouter(g.writer, g.request, g.cfg.OpenRouter, reqBody, g.body)
			return true
		}
	}
	return false
}

func (server *Server) tryCustomEndpointGateway(g *gatewayRequest) bool {
	endpoint, exists := g.cfg.CustomEndpoints[g.model]
	if !exists || endpoint.URL == "" {
		return false
	}
	reqBody, err := json.Marshal(g.body)
	if err != nil {
		writeAPIError(g.writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal request: "+err.Error())
		return true
	}
	if !g.mutated {
		// Nothing rewrote the request — forward the client's exact bytes.
		reqBody = g.rawBody
	}
	server.forwardToCustomEndpoint(g.writer, g.request, endpoint, g.model, reqBody)
	return true
}
