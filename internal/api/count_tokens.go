package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"antigravity-go-proxy/internal/ccidentity"
	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
)

// countUnsupportedFor is how long a gateway that answered count_tokens with
// 404, 405 or 501 is estimated without being asked.
const countUnsupportedFor = time.Hour

// maxCountResponse caps a count answer; Claude Code's is about 20 bytes.
const maxCountResponse = 64 << 10

// countTokens answers POST /v1/messages/count_tokens with {"input_tokens": N}.
//
// A count takes the /v1/messages route: model mapping, then the gateway
// precedence. Headroom and the classifier do not run: a count measures the
// request as the client wrote it. A gateway with a count endpoint is asked
// (forwardCount). Zen and Cloud Code have none, and a gateway that cannot
// answer is no error for the client, so those counts are estimated from what
// the conversation really cost so far (internal/tokencount). Only a body the
// proxy cannot read is refused.
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
	cfg := config.Get()
	if route, routed := server.routeGateway(request.Context(), cfg, model); routed {
		model = route.target
		if tokens, ok := server.forwardCount(request, cfg, route, session, rawBody); ok {
			writeJSON(writer, http.StatusOK, map[string]any{"input_tokens": tokens})
			return
		}
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

// countCall is one count_tokens forward: the client's request, the gateway
// routeGateway chose, the body to send (the client's bytes with the gateway's
// model id written in) and the client's session.
type countCall struct {
	request *http.Request
	cfg     config.Config
	route   gatewayRoute
	body    []byte
	session string
}

// countForwards maps the gateways that count tokens themselves onto the call
// that asks them. Zen has no count endpoint and the account-backed path is not
// a gateway: both are estimated.
var countForwards = map[config.GatewayID]func(*Server, countCall) (*http.Response, error){
	config.GatewayClaudeCode: (*Server).countClaudeCode,
}

// forwardCount asks the gateway route names for the count. ok is false when
// the gateway does not count tokens, is marked as not counting, or the call
// failed in any way; the caller then estimates, so a count never fails the
// client for an upstream's sake. A 404, 405 or 501 marks the gateway (a custom
// endpoint: that endpoint) for countUnsupportedFor.
func (server *Server) forwardCount(request *http.Request, cfg config.Config, route gatewayRoute, session string, rawBody []byte) (int, bool) {
	send, ok := countForwards[route.id]
	if !ok {
		return 0, false
	}
	key := countMarkKey(route)
	if server.countMarks.unsupported(key, server.nowTime()) {
		return 0, false
	}
	body, err := countBody(rawBody, route.target)
	if err != nil {
		server.logger.Debug("count_tokens body not forwardable; estimating", "gateway", key, "error", err)
		return 0, false
	}
	resp, err := send(server, countCall{request: request, cfg: cfg, route: route, body: body, session: session})
	if err != nil {
		server.logger.Debug("count_tokens forward failed; estimating", "gateway", key, "error", err)
		return 0, false
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		server.countMarks.mark(key, server.nowTime())
		server.logger.Info("count_tokens not served upstream; estimating for an hour", "gateway", key, "status", resp.StatusCode)
		return 0, false
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		server.logger.Debug("count_tokens upstream error; estimating", "gateway", key, "status", resp.StatusCode)
		return 0, false
	}
	var counted struct {
		InputTokens *int `json:"input_tokens"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCountResponse)).Decode(&counted); err != nil || counted.InputTokens == nil || *counted.InputTokens < 0 {
		server.logger.Debug("count_tokens upstream answered no count; estimating", "gateway", key)
		return 0, false
	}
	return *counted.InputTokens, true
}

// countBody is the client's count body with the gateway's model id written
// into the client's own bytes; every other byte goes upstream as the client
// sent it.
func countBody(rawBody []byte, model string) ([]byte, error) {
	quoted, err := ccidentity.MarshalJS(model)
	if err != nil {
		return nil, fmt.Errorf("encode count model: %w", err)
	}
	return ccidentity.SetObjectMembers(rawBody, ccidentity.ObjectMember{Name: "model", Value: quoted})
}

// countMarkKey names what a mark covers: a custom endpoint by its URL, since
// one endpoint can count and another not; any other gateway by its id.
func countMarkKey(route gatewayRoute) string {
	if route.id == config.GatewayCustom {
		return string(config.GatewayCustom) + ":" + route.endpoint.URL
	}
	return string(route.id)
}

// countMarks remembers the gateways that do not count tokens, and until when.
// Keys come from the configuration, so the map stays small. The zero value is
// ready.
type countMarks struct {
	mu    sync.Mutex
	until map[string]time.Time
}

// unsupported reports whether key is marked at now; an expired mark is dropped.
func (marks *countMarks) unsupported(key string, now time.Time) bool {
	marks.mu.Lock()
	defer marks.mu.Unlock()
	until, ok := marks.until[key]
	if !ok {
		return false
	}
	if now.Before(until) {
		return true
	}
	delete(marks.until, key)
	return false
}

func (marks *countMarks) mark(key string, now time.Time) {
	marks.mu.Lock()
	defer marks.mu.Unlock()
	if marks.until == nil {
		marks.until = make(map[string]time.Time)
	}
	marks.until[key] = now.Add(countUnsupportedFor)
}

// countClaudeCode sends a count through the Claude Code pool with the count
// identity (claudecode.Client.CountTokens).
//
// Counts arrive in bursts of about 34 from one /context, so nothing is held
// across the call: SelectAccount and Acquire take the pool's locks briefly,
// and the token is not refreshed here (a refresh writes config.json under its
// lock; an expired token answers 401 and the count is estimated). A count
// answer carries no unified rate-limit headers, so only a 429 changes pool
// state, as it would on /v1/messages. The account is released when
// forwardCount closes the body.
func (server *Server) countClaudeCode(call countCall) (*http.Response, error) {
	pool, client := server.getOrCreateCCPool(call.cfg.ClaudeCode)
	acc, err := pool.SelectAccount(call.session, nil)
	if err != nil {
		return nil, fmt.Errorf("select Claude Code account: %w", err)
	}
	pool.Acquire(acc.ID)
	identity, normalize := call.cfg.ClaudeCode.Identity.WireIdentity(acc.AccountUUID, call.session)
	resp, err := client.CountTokens(call.request.Context(), claudecode.CountTokensRequest{
		Token:         acc.Token,
		Body:          call.body,
		ClientHeaders: call.request.Header,
		Normalize:     normalize,
		Identity:      identity,
	})
	if err != nil {
		pool.Release(acc.ID)
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		rl := claudecode.ExtractRateLimits(resp.Header)
		pool.RecordRateLimit(acc.ID, rl, 10*time.Second)
		server.noteClaudeCodeRateLimits(acc.ID, rl)
	}
	resp.Body = &poolReleasingBody{ReadCloser: resp.Body, releaseFn: func() { pool.Release(acc.ID) }}
	return resp, nil
}
