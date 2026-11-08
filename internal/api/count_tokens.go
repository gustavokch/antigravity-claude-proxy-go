package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"antigravity-go-proxy/internal/ccidentity"
	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/kimi"
	"antigravity-go-proxy/internal/openrouter"
)

// countUnsupportedFor is how long a gateway that answered count_tokens with
// 404, 405 or 501 is estimated without being asked.
const countUnsupportedFor = time.Hour

// countBackoffDefault is how long counts back off after a count_tokens 429
// that names no Retry-After.
const countBackoffDefault = 10 * time.Second

// countForwardTimeout bounds a count_tokens forward, from the request to the
// decoded answer. An estimate answers a count well enough, so a gateway that
// takes the count and stays silent is given up on long before the transports'
// response-header timeout.
const countForwardTimeout = 10 * time.Second

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
	model := stringFrom(body["model"])
	if model == "" {
		model = defaultRequestModel
	}
	model = server.resolveModelMapping(model)
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

// countCall is one count_tokens forward: its context (the client's, bounded
// by the count deadline), the client's request, the gateway routeGateway
// chose, the body to send (the client's bytes with the gateway's model id
// written in) and the client's session. A send uses ctx, never the client
// request's own context.
type countCall struct {
	ctx     context.Context
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
	config.GatewayOpenRouter: (*Server).countOpenRouter,
	config.GatewayKimi:       (*Server).countKimi,
	config.GatewayCustom:     (*Server).countCustomEndpoint,
}

// forwardCount asks the gateway route names for the count. ok is false when
// the gateway does not count tokens, is marked, or the call failed in any
// way; the caller then estimates, so a count never fails the client for an
// upstream's sake. The forward, answer included, gets countForwardTimeout. A
// 404, 405 or 501 marks the gateway (a custom endpoint: that endpoint) for
// countUnsupportedFor, and a 429 marks it for countBackoff: counts back off,
// and nothing generation uses changes.
func (server *Server) forwardCount(request *http.Request, cfg config.Config, route gatewayRoute, session string, rawBody []byte) (int, bool) {
	send, ok := countForwards[route.id]
	if !ok {
		return 0, false
	}
	key := countMarkKey(route)
	if server.countMarks.marked(key, server.nowTime()) {
		return 0, false
	}
	body, err := countBody(rawBody, route.target)
	if err != nil {
		server.logger.Debug("count_tokens body not forwardable; estimating", "gateway", key, "error", err)
		return 0, false
	}
	timeout := server.countTimeout
	if timeout <= 0 {
		timeout = countForwardTimeout
	}
	ctx, cancel := context.WithTimeout(request.Context(), timeout)
	defer cancel()
	resp, err := send(server, countCall{ctx: ctx, request: request, cfg: cfg, route: route, body: body, session: session})
	if err != nil {
		server.logger.Debug("count_tokens forward failed; estimating", "gateway", key, "error", err)
		return 0, false
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		server.countMarks.mark(key, server.nowTime(), countUnsupportedFor)
		server.logger.Info("count_tokens not served upstream; estimating for an hour", "gateway", key, "status", resp.StatusCode)
		return 0, false
	case http.StatusTooManyRequests:
		backoff := countBackoff(resp.Header)
		server.countMarks.mark(key, server.nowTime(), backoff)
		server.logger.Info("count_tokens rate limited upstream; estimating", "gateway", key, "backoff", backoff)
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

// countBackoff is how long counts back off after a count_tokens 429: its
// Retry-After (seconds or an HTTP date), countBackoffDefault when it names
// none, and never longer than countUnsupportedFor.
func countBackoff(header http.Header) time.Duration {
	retryAfter := time.Duration(claudecode.ExtractRateLimits(header).RetryAfter) * time.Second
	if retryAfter <= 0 {
		return countBackoffDefault
	}
	return min(retryAfter, countUnsupportedFor)
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

// countMarks remembers the gateways not to ask for counts, and until when:
// those that do not count tokens and those that answered a count 429. Keys
// come from the configuration, so the map stays small. The zero value is
// ready.
type countMarks struct {
	mu    sync.Mutex
	until map[string]time.Time
}

// marked reports whether key is marked at now; an expired mark is dropped.
func (marks *countMarks) marked(key string, now time.Time) bool {
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

// mark marks key from now for d.
func (marks *countMarks) mark(key string, now time.Time, d time.Duration) {
	marks.mu.Lock()
	defer marks.mu.Unlock()
	if marks.until == nil {
		marks.until = make(map[string]time.Time)
	}
	marks.until[key] = now.Add(d)
}

// countClaudeCode sends a count through the Claude Code pool with the count
// identity (claudecode.Client.CountTokens).
//
// Counts arrive in bursts of about 34 from one /context, so nothing is held
// across the call: SelectAccount and Acquire take the pool's locks briefly,
// and the token is not refreshed here (a refresh writes config.json under its
// lock; an expired token answers 401 and the count is estimated). Only a 429
// carrying the unified (subscription) rate-limit headers changes pool state,
// as it would on /v1/messages: the subscription is exhausted for generation
// too. Any other count 429 limits counting alone, and forwardCount backs
// counts off (countBackoff). The account is released when forwardCount closes
// the body.
func (server *Server) countClaudeCode(call countCall) (*http.Response, error) {
	pool, client := server.getOrCreateCCPool(call.cfg.ClaudeCode)
	acc, err := pool.SelectAccount(call.session, nil)
	if err != nil {
		return nil, fmt.Errorf("select Claude Code account: %w", err)
	}
	pool.Acquire(acc.ID)
	identity, normalize := call.cfg.ClaudeCode.Identity.WireIdentity(acc.AccountUUID, call.session)
	resp, err := client.CountTokens(call.ctx, claudecode.CountTokensRequest{
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
		if rl := claudecode.ExtractRateLimits(resp.Header); rl.Unified != nil {
			pool.RecordRateLimit(acc.ID, rl, 10*time.Second)
			server.noteClaudeCodeRateLimits(acc.ID, rl)
		}
	}
	resp.Body = &poolReleasingBody{ReadCloser: resp.Body, releaseFn: func() { pool.Release(acc.ID) }}
	return resp, nil
}

// countOpenRouter asks OpenRouter's Anthropic surface for the count, with the
// headers its messages forward sends (setOpenRouterHeaders).
func (server *Server) countOpenRouter(call countCall) (*http.Response, error) {
	target := openrouter.NormalizeBaseURL(call.cfg.OpenRouter.BaseURL) + "/v1/messages/count_tokens"
	upReq, err := http.NewRequestWithContext(call.ctx, http.MethodPost, target, bytes.NewReader(call.body))
	if err != nil {
		return nil, fmt.Errorf("build OpenRouter count_tokens request: %w", err)
	}
	server.setOpenRouterHeaders(upReq, call.request.Header, call.cfg.OpenRouter)
	return openRouterUpstreamClient().Do(upReq)
}

// countKimi asks the Kimi gateway for the count with the credential, and for a
// Kimi Code login the KimiCLI identity headers, its messages forward sends.
func (server *Server) countKimi(call countCall) (*http.Response, error) {
	cred, err := server.resolveKimiCredential(call.ctx, call.cfg.Kimi)
	if err != nil {
		return nil, err
	}
	var identity http.Header
	if cred.oauth {
		identity = server.kimiIdentityHeaders()
	}
	return kimi.CountTokens(call.ctx, cred.baseURL, cred.token, call.body, call.request.Header, identity)
}

// errNoCountRoute is a custom endpoint whose URL names no Anthropic messages
// route, so it has no count_tokens route to ask.
var errNoCountRoute = errors.New("custom endpoint URL names no Anthropic messages route")

var (
	// customCountClient and customIdentityCountClient carry custom-endpoint
	// counts on the transports the messages forward uses for that endpoint.
	customCountClient         = &http.Client{Transport: customEndpointTransport}
	customIdentityCountClient = &http.Client{Transport: customEndpointIdentityTransport}
)

// countCustomEndpoint asks an Anthropic-format custom endpoint for the count.
// An endpoint the messages forward normalizes (customEndpointIdentity) gets
// the identity Claude Code sends on counts, over the same HTTP/1.1 transport;
// any other gets its key and the client's Anthropic version and betas, as its
// messages forward does.
func (server *Server) countCustomEndpoint(call countCall) (*http.Response, error) {
	endpoint := call.route.endpoint
	target, ok := customCountURL(endpoint.URL)
	if !ok {
		return nil, errNoCountRoute
	}
	identity, normalize := customEndpointIdentity(endpoint, call.session)
	if normalize {
		target.RawQuery = withCapturedBetaQuery(target.RawQuery)
	}
	upReq, err := http.NewRequestWithContext(call.ctx, http.MethodPost, target.String(), bytes.NewReader(call.body))
	if err != nil {
		return nil, fmt.Errorf("build custom endpoint count_tokens request: %w", err)
	}
	if normalize {
		ccidentity.ApplyHeaders(upReq.Header, ccidentity.CountTokensProfile(), identity, ccidentity.Turn{})
		return customIdentityCountClient.Do(upReq)
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "application/json")
	setCustomEndpointKey(upReq.Header, endpoint.APIKey)
	setClientAnthropicHeaders(upReq.Header, call.request.Header, true)
	return customCountClient.Do(upReq)
}

// setCustomEndpointKey sends a custom endpoint's own key in both header forms
// Anthropic-format servers accept. An endpoint without a key gets no
// credential at all, never the client's.
func setCustomEndpointKey(h http.Header, apiKey string) {
	if apiKey == "" {
		h.Del("Authorization")
		h.Del("x-api-key")
		return
	}
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("x-api-key", apiKey)
}

// setClientAnthropicHeaders forwards the client's anthropic-version and
// anthropic-beta to a custom endpoint that is not normalized. defaultVersion
// sets 2023-06-01 when the client sent none; Messages API calls need one.
func setClientAnthropicHeaders(h, client http.Header, defaultVersion bool) {
	if version := client.Get("anthropic-version"); version != "" {
		h.Set("anthropic-version", version)
	} else if defaultVersion {
		h.Set("anthropic-version", "2023-06-01")
	}
	if beta := client.Get("anthropic-beta"); beta != "" {
		h.Set("anthropic-beta", beta)
	}
}

// customCountURL is the count_tokens URL of an Anthropic-format endpoint: its
// messages URL with /messages replaced by /messages/count_tokens. A URL that
// leaves the path to the request (no path, or one ending in /v1) gets
// /v1/messages/count_tokens, as resolveCustomEndpointURL builds a messages URL.
// ok is false for any other URL: it names no messages route.
func customCountURL(endpointURL string) (*url.URL, bool) {
	target, err := resolveCustomEndpointURL(endpointURL, "/v1/messages/count_tokens")
	if err != nil {
		return nil, false
	}
	path := strings.TrimRight(target.Path, "/")
	switch {
	case strings.HasSuffix(path, "/messages/count_tokens"):
		target.Path = path
	case strings.HasSuffix(path, "/messages"):
		target.Path = path + "/count_tokens"
	default:
		return nil, false
	}
	return target, true
}
