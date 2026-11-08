package api

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"strconv"

	"antigravity-go-proxy/internal/ccidentity"
	"antigravity-go-proxy/internal/codex"
	"antigravity-go-proxy/internal/config"
)

// gatewayRequest carries what one /v1/messages forward needs. The forward of
// the gateway routeGateway picked writes the response.
type gatewayRequest struct {
	writer  http.ResponseWriter
	request *http.Request
	cfg     config.Config
	body    map[string]any
	rawBody []byte
	mutated bool
	model   string
}

// gatewayRoute is the gateway that claims a model and what its matcher found:
// target is the model id that gateway is sent, and kimi, zen, codex and
// endpoint hold the matched allowlist entry or endpoint its forward reads.
type gatewayRoute struct {
	id       config.GatewayID
	target   string
	kimi     config.KimiModelConfig
	zen      config.ZenModelConfig
	codex    codex.ModelConfig
	endpoint config.EndpointConfig
}

// gateway is one gateway this build can dispatch to. match decides whether the
// gateway claims a model and writes nothing, so count_tokens asks the same
// question /v1/messages does; forward sends a /v1/messages request to the
// gateway match claimed it for.
type gateway struct {
	match   func(server *Server, ctx context.Context, cfg config.Config, model string) (gatewayRoute, bool)
	forward func(server *Server, g *gatewayRequest, route gatewayRoute)
}

// gateways maps every gateway this build can dispatch to onto its matcher and
// forward. cloudcode is deliberately absent: it is the terminal account-backed
// path, not a gateway. TestGateways_CoverEveryKnownGateway pins that this table
// covers exactly config.KnownGatewayIDs() minus cloudcode.
var gateways = map[config.GatewayID]gateway{
	config.GatewayKimi:       {(*Server).matchKimiGateway, (*Server).forwardKimiGateway},
	config.GatewayZen:        {(*Server).matchZenGateway, (*Server).forwardZenGateway},
	config.GatewayClaudeCode: {(*Server).matchClaudeCodeGateway, (*Server).forwardClaudeCodeGateway},
	config.GatewayCodex:      {(*Server).matchCodexGateway, (*Server).forwardCodexGateway},
	config.GatewayOpenRouter: {(*Server).matchOpenRouterGateway, (*Server).forwardOpenRouterGateway},
	config.GatewayCustom:     {(*Server).matchCustomEndpointGateway, (*Server).forwardCustomEndpointGateway},
}

// routeGateway walks the configured precedence and returns the first gateway
// that claims model. ok is false when cloudcode is reached or every listed
// gateway declines: the request then belongs to the account-backed path.
// Nothing is sent or written.
//
// Each matcher re-checks its own enabled flag, its own allowlist and (for Zen)
// its own key resolution; the walk is responsible for order only. An operator
// who hoists `cloudcode` ahead of a gateway is choosing to have the
// classifier-fallback gate consulted first: the walk stops at cloudcode and the
// caller falls through to the account-backed path (and its gate).
func (server *Server) routeGateway(ctx context.Context, cfg config.Config, model string) (gatewayRoute, bool) {
	for _, id := range cfg.GatewayOrder.Effective(model) {
		if id == config.GatewayCloudCode {
			return gatewayRoute{}, false
		}
		gw, ok := gateways[id]
		if !ok {
			continue
		}
		if route, ok := gw.match(server, ctx, cfg, model); ok {
			route.id = id
			return route, true
		}
	}
	return gatewayRoute{}, false
}

// dispatchAlternateBackend hands a /v1/messages request to the gateway
// routeGateway picks and returns true once that gateway wrote the response. It
// returns false, having written nothing, when the request belongs to the
// account-backed path.
func (server *Server) dispatchAlternateBackend(g *gatewayRequest) bool {
	route, ok := server.routeGateway(g.request.Context(), g.cfg, g.model)
	if !ok {
		return false
	}
	gateways[route.id].forward(server, g, route)
	return true
}

func (server *Server) matchKimiGateway(_ context.Context, cfg config.Config, model string) (gatewayRoute, bool) {
	if !cfg.Kimi.Enabled {
		return gatewayRoute{}, false
	}
	entry, ok := matchKimiModelEntry(cfg.Kimi, model)
	if !ok {
		return gatewayRoute{}, false
	}
	return gatewayRoute{target: stripKimi1mSuffix(entry.ID), kimi: entry}, true
}

func (server *Server) forwardKimiGateway(g *gatewayRequest, route gatewayRoute) {
	g.body["model"] = route.target
	reqBody, err := json.Marshal(g.body)
	if err != nil {
		writeAPIError(g.writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal Kimi request: "+err.Error())
		return
	}
	reqBody = applyMaxTokensPolicy(reqBody, g.body, route.kimi.MaxOutputTokens, 0)
	server.forwardToKimi(g.writer, g.request, g.cfg.Kimi, reqBody, route.target)
}

func (server *Server) matchZenGateway(_ context.Context, cfg config.Config, model string) (gatewayRoute, bool) {
	if !cfg.Zen.Enabled {
		return gatewayRoute{}, false
	}
	entry, ok := matchZenModelEntry(cfg.Zen, model)
	if !ok {
		return gatewayRoute{}, false
	}
	return gatewayRoute{target: zenTargetModel(entry), zen: entry}, true
}

func (server *Server) forwardZenGateway(g *gatewayRequest, route gatewayRoute) {
	g.body["model"] = route.target
	reqBody, err := json.Marshal(g.body)
	if err != nil {
		writeAPIError(g.writer, http.StatusBadRequest, "invalid_request_error",
			"Failed to marshal Zen request: "+err.Error())
		return
	}
	server.forwardToZen(g.writer, g.request, g.cfg.Zen, reqBody, g.body, route.target, route.zen)
}

func (server *Server) matchClaudeCodeGateway(_ context.Context, cfg config.Config, model string) (gatewayRoute, bool) {
	if !cfg.ClaudeCode.Enabled {
		return gatewayRoute{}, false
	}
	target := matchClaudeCodeModel(cfg.ClaudeCode, model)
	return gatewayRoute{target: target}, target != ""
}

func (server *Server) forwardClaudeCodeGateway(g *gatewayRequest, route gatewayRoute) {
	g.body["model"] = route.target
	// The Anthropic gateway requires max_tokens; derive it from the
	// allowlist entry (defaults carry each model's max output) when
	// the client omitted it. Client values are clamped down, never up.
	ccBody, err := claudeCodeGatewayBody(g, route.target, claudeCodeEntryMaxOutput(g.cfg.ClaudeCode, route.target))
	if err != nil {
		writeAPIError(g.writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal ClaudeCode request: "+err.Error())
		return
	}
	server.forwardToClaudeCode(g.writer, g.request, g.cfg.ClaudeCode, ccBody, route.target)
}

func (server *Server) matchCodexGateway(_ context.Context, cfg config.Config, model string) (gatewayRoute, bool) {
	if !cfg.Codex.Enabled {
		return gatewayRoute{}, false
	}
	entry, ok := codex.ResolveModel(codexAllowlist(cfg.Codex), model)
	if !ok {
		return gatewayRoute{}, false
	}
	return gatewayRoute{target: entry.ID, codex: entry}, true
}

// forwardCodexGateway serves a model on the Codex allowlist from the ChatGPT
// account pool. The Anthropic body is translated into a Codex Responses body
// here, once, so every account the request fails over to gets the same bytes.
// Its prompt cache key names the session and the Claude Code agent, so a
// subagent's turns do not evict the main loop's cache.
func (server *Server) forwardCodexGateway(g *gatewayRequest, route gatewayRoute) {
	entry := route.codex
	// Client values are clamped to the entry's output limit (the built-in
	// entry's when the configured one sets none), never raised.
	anthropicBody := g.rawBody
	maxTokens, setMaxTokens := maxTokensValue(g.body, entry.MaxOutputTokens, codexDefaultMaxOutput(entry.ID))
	if setMaxTokens {
		g.body["max_tokens"] = maxTokens
	}
	if setMaxTokens || g.mutated {
		var err error
		if anthropicBody, err = json.Marshal(g.body); err != nil {
			writeAPIError(g.writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal Codex request: "+err.Error())
			return
		}
	}
	sessionKey := ccExtractSessionID(g.request, g.body)
	cacheKey := codex.PromptCacheKey(entry.ID, sessionKey, codexAgentID(g.request))
	body, toolNames, err := codex.BuildRequest(anthropicBody, codex.BuildOptions{
		Model:          entry.ID,
		Entry:          entry,
		PromptCacheKey: cacheKey,
	})
	if err != nil {
		writeAPIError(g.writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	stream, _ := g.body["stream"].(bool)
	server.forwardToCodex(g.writer, g.request, g.cfg.Codex, codexCall{
		model:          entry.ID,
		body:           body,
		toolNames:      toolNames,
		sessionKey:     sessionKey,
		promptCacheKey: cacheKey,
		serviceTier:    codexServiceTier(body),
		stream:         stream,
	})
}

// claudeCodeGatewayBody is the body the pooled Claude Code gateway sends for g,
// with model and max_tokens decided by the gateway.
//
// When nothing rewrote the request, those two values are written into the
// client's own bytes and everything else is forwarded as the client sent it.
// Encoding the decoded map instead would sort every nested object and turn "<",
// ">" and "&" into \u003c, \u003e and \u0026: a JavaScript client sends none of
// that, and every <system-reminder> block would show it. A request the proxy did
// rewrite exists only as the decoded map, so it is encoded from there, still
// without HTML escaping.
func claudeCodeGatewayBody(g *gatewayRequest, model string, derivedLimit int) ([]byte, error) {
	maxTokens, setMaxTokens := maxTokensValue(g.body, 0, derivedLimit)
	setDisplay := thinkingNeedsDisplay(g.body)
	if !g.mutated {
		quotedModel, err := ccidentity.MarshalJS(model)
		if err != nil {
			return nil, err
		}
		members := []ccidentity.ObjectMember{{Name: "model", Value: quotedModel}}
		if setMaxTokens {
			members = append(members, ccidentity.ObjectMember{Name: "max_tokens", Value: strconv.AppendInt(nil, int64(maxTokens), 10)})
		}
		if setDisplay {
			thinking, err := rawThinking(g.rawBody)
			if err != nil {
				return nil, err
			}
			members = append(members, ccidentity.ObjectMember{Name: "thinking", Value: withSummarizedDisplay(thinking)})
		}
		return ccidentity.SetObjectMembers(g.rawBody, members...)
	}
	body := g.body
	if setMaxTokens || setDisplay {
		body = maps.Clone(g.body)
	}
	if setMaxTokens {
		body["max_tokens"] = maxTokens
	}
	if setDisplay {
		thinking := maps.Clone(g.body["thinking"].(map[string]any))
		thinking["display"] = displaySummarized
		body["thinking"] = thinking
	}
	return ccidentity.MarshalJS(body)
}

// displaySummarized is the thinking.display value Claude Code always sends.
const displaySummarized = "summarized"

// thinkingNeedsDisplay reports whether the request turns thinking on and names
// no display. Newer Claude models read a missing display as "omitted", which
// returns thinking_delta events with empty text, so the gateway names the value
// Claude Code itself sends. An explicit display, and a disabled thinking
// object, are left as the client wrote them.
func thinkingNeedsDisplay(body map[string]any) bool {
	thinking, ok := body["thinking"].(map[string]any)
	if !ok {
		return false
	}
	if _, named := thinking["display"]; named {
		return false
	}
	return thinking["type"] != "disabled"
}

// rawThinking returns the client's own bytes of the top-level thinking object.
func rawThinking(rawBody []byte) ([]byte, error) {
	var top struct {
		Thinking json.RawMessage `json:"thinking"`
	}
	if err := json.Unmarshal(rawBody, &top); err != nil {
		return nil, err
	}
	return top.Thinking, nil
}

// withSummarizedDisplay returns the JSON object raw with display "summarized"
// added as its last member. raw must be an object, so its final byte is '}'.
// Every other byte is kept.
func withSummarizedDisplay(raw []byte) []byte {
	head := raw[:len(raw)-1]
	out := make([]byte, 0, len(raw)+len(`,"display":"summarized"`))
	out = append(out, head...)
	if len(bytes.TrimSpace(head[1:])) > 0 {
		out = append(out, ',')
	}
	out = append(out, `"display":"`+displaySummarized+`"}`...)
	return out
}

func (server *Server) matchOpenRouterGateway(_ context.Context, cfg config.Config, model string) (gatewayRoute, bool) {
	if !cfg.OpenRouter.Enabled {
		return gatewayRoute{}, false
	}
	for _, item := range cfg.OpenRouter.Allowlist {
		if item.Enabled && (item.ID == model || (item.Alias != "" && item.Alias == model)) {
			return gatewayRoute{target: item.ID}, true
		}
	}
	return gatewayRoute{}, false
}

func (server *Server) forwardOpenRouterGateway(g *gatewayRequest, route gatewayRoute) {
	g.body["model"] = route.target
	reqBody, err := json.Marshal(g.body)
	if err != nil {
		writeAPIError(g.writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal request: "+err.Error())
		return
	}
	server.forwardToOpenRouter(g.writer, g.request, g.cfg.OpenRouter, reqBody, g.body)
}

func (server *Server) matchCustomEndpointGateway(ctx context.Context, cfg config.Config, model string) (gatewayRoute, bool) {
	endpoint, ok := server.resolveCustomEndpoint(ctx, cfg, model)
	if !ok {
		return gatewayRoute{}, false
	}
	return gatewayRoute{target: model, endpoint: endpoint}, true
}

func (server *Server) forwardCustomEndpointGateway(g *gatewayRequest, route gatewayRoute) {
	// Nothing rewrote the request: forward the client's exact bytes.
	reqBody := g.rawBody
	if g.mutated {
		var err error
		if reqBody, err = json.Marshal(g.body); err != nil {
			writeAPIError(g.writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal request: "+err.Error())
			return
		}
	}
	server.forwardToCustomEndpoint(g.writer, g.request, route.endpoint, g.model, reqBody)
}
