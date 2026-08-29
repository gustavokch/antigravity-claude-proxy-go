package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/openrouter"
)

var (
	ccPoolMu   sync.Mutex
	ccPoolInst *claudecode.AccountPool
	ccPoolKey  string // tracks config identity to detect changes
)

var ccHTTPClient *claudecode.Client

func getOrCreateCCPool(cfg claudecode.Config) (*claudecode.AccountPool, *claudecode.Client) {
	var s *Server
	return s.getOrCreateCCPool(cfg)
}

func (server *Server) getOrCreateCCPool(cfg claudecode.Config) (*claudecode.AccountPool, *claudecode.Client) {
	ccPoolMu.Lock()
	defer ccPoolMu.Unlock()

	key := cfg.BaseURL
	if ccPoolInst == nil || ccPoolKey != key || len(ccPoolCfg.Accounts) != len(cfg.Accounts) {
		ccPoolInst = claudecode.NewAccountPool(cfg.Accounts)
		ccHTTPClient = claudecode.NewClient(claudecode.NormalizeBaseURL(cfg.BaseURL), nil)
		ccPoolKey = key
		ccPoolCfg = cfg

		if server != nil && server.claudeCodeOAuthMgr != nil {
			oauthMgr := server.claudeCodeOAuthMgr
			ccPoolInst.SetTokenRefresher(func(refreshToken string) (string, string, int, error) {
				resp, err := oauthMgr.RefreshToken(refreshToken)
				if err != nil {
					return "", "", 0, err
				}
				return resp.AccessToken, resp.RefreshToken, resp.ExpiresIn, nil
			})
		}
	}
	if ccHTTPClient == nil {
		ccHTTPClient = claudecode.NewClient(claudecode.NormalizeBaseURL(cfg.BaseURL), nil)
	}
	return ccPoolInst, ccHTTPClient
}

// syncRefreshedAccountToConfig updates config.json with newly refreshed token credentials.
func (server *Server) syncRefreshedAccountToConfig(accID, newToken, newRefreshToken string, expiresAt *time.Time) {
	if accID == "" || newToken == "" {
		return
	}
	cfg := config.Get()
	accountsList := make([]any, 0, len(cfg.ClaudeCode.Accounts))
	updated := false
	for _, a := range cfg.ClaudeCode.Accounts {
		aMap := map[string]any{
			"id":               a.ID,
			"name":             a.Name,
			"token":            a.Token,
			"refreshToken":     a.RefreshToken,
			"expiresAt":        a.ExpiresAt,
			"email":            a.Email,
			"accountUuid":      a.AccountUUID,
			"organizationUuid": a.OrganizationUUID,
			"type":             a.Type,
			"priority":         a.Priority,
			"enabled":          a.Enabled,
			"source":           a.Source,
		}
		if a.ID == accID {
			aMap["token"] = newToken
			if newRefreshToken != "" {
				aMap["refreshToken"] = newRefreshToken
			}
			if expiresAt != nil {
				aMap["expiresAt"] = expiresAt.Format(time.RFC3339)
			}
			updated = true
		}
		accountsList = append(accountsList, aMap)
	}
	if updated {
		ccCfg := map[string]any{
			"enabled":    cfg.ClaudeCode.Enabled,
			"baseUrl":    cfg.ClaudeCode.BaseURL,
			"mode":       cfg.ClaudeCode.Mode,
			"autoImport": cfg.ClaudeCode.AutoImport,
			"accounts":   accountsList,
			"allowlist":  cfg.ClaudeCode.Allowlist,
			"routing":    cfg.ClaudeCode.Routing,
		}
		_, _ = config.Save(map[string]any{"claudecode": ccCfg})
	}
}

var ccPoolCfg claudecode.Config

// matchClaudeCodeModel returns the canonical model ID if the request model
// matches an enabled allowlist entry by ID or alias. Returns "" on no match.
func matchClaudeCodeModel(cfg claudecode.Config, model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return ""
	}
	allowlist := cfg.Allowlist
	if len(allowlist) == 0 {
		allowlist = claudecode.DefaultAllowlist()
	}
	router := claudecode.NewRouter(allowlist)
	if canonical, found := router.ResolveModel(m); found {
		return canonical
	}
	return ""
}

// ccExtractSessionID extracts a stable session key from request headers, then
// from the request body. Claude Code does not send a session header: it carries
// the identifier in metadata.user_id, so the body must be inspected. Mirrors
// openrouter.ExtractSessionID minus the remote-address fallback, which would
// change account stickiness for anonymous clients.
func ccExtractSessionID(r *http.Request, reqBody map[string]any) string {
	if r != nil {
		for _, h := range []string{"x-session-id", "session-id", "anthropic-session-id", "x-conversation-id"} {
			if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
				return v
			}
		}
	}
	if reqBody != nil {
		if meta, ok := reqBody["metadata"].(map[string]any); ok {
			if s, ok := meta["session_id"].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
			if u, ok := meta["user_id"].(string); ok && strings.TrimSpace(u) != "" {
				return strings.TrimSpace(u)
			}
		}
		if s, ok := reqBody["session_id"].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
		if u, ok := reqBody["user_id"].(string); ok && strings.TrimSpace(u) != "" {
			return strings.TrimSpace(u)
		}
	}
	return ""
}

// ccParseBodyMap unmarshals a request body, returning nil on malformed JSON.
func ccParseBodyMap(body []byte) map[string]any {
	if len(body) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	return m
}

type poolReleasingBody struct {
	io.ReadCloser
	releaseOnce sync.Once
	releaseFn   func()
}

func (b *poolReleasingBody) Close() error {
	b.releaseOnce.Do(b.releaseFn)
	return b.ReadCloser.Close()
}

// ccAttempt carries the identity of a single upstream call so usage captured
// later — possibly after the response body has finished streaming — can still
// be attributed to the right model, session and account.
type ccAttempt struct {
	model       string
	sessionID   string
	accountID   string
	accountName string
	startTime   time.Time
	pool        *claudecode.AccountPool
	rateLimits  claudecode.RateLimits
}

// recordClaudeCodeMetrics is the single place a completed Claude Code call
// becomes metrics, logs, pool accounting and dashboard stats. Mirrors
// recordOpenRouterMetrics.
func (server *Server) recordClaudeCodeMetrics(a ccAttempt, in, out, cr, cw int) claudecode.RequestMetrics {
	latency := time.Since(a.startTime)
	metrics := claudecode.RequestMetrics{
		Model:               a.model,
		AccountID:           a.accountID,
		AccountName:         a.accountName,
		SessionID:           a.sessionID,
		InputTokens:         in,
		OutputTokens:        out,
		CacheReadTokens:     cr,
		CacheCreationTokens: cw,
		Latency:             latency,
	}
	metrics.ComputeFinalMetrics(claudecode.DefaultSessionTracker)
	if server.logger != nil {
		claudecode.LogObservability(server.logger, metrics)
	}
	if a.pool != nil {
		a.pool.RecordSuccess(a.accountID, int64(in+out), metrics.CallCost, a.rateLimits)
	}
	if server.tracker != nil {
		server.tracker.TrackRequest(a.model, latency, in, out, cr)
	}
	return metrics
}

// ccIsSSEResponse reports whether the upstream answered with an event stream.
func ccIsSSEResponse(h http.Header) bool {
	return strings.Contains(strings.ToLower(h.Get("Content-Type")), "text/event-stream")
}

// ccInstrumentResponse attaches usage capture to a successful upstream
// response and replaces resp.Body with the instrumented reader. Usage lives in
// the RESPONSE, never in the request, so metrics are emitted once the body is
// consumed. SSE bodies are intercepted line by line, JSON bodies are buffered
// and parsed — the same split the OpenRouter gateway makes between its stream
// and unary paths.
//
// The usage parsers are shared with the OpenRouter gateway because they are
// wire-format generic (they read Anthropic and OpenAI shapes alike).
func (server *Server) ccInstrumentResponse(resp *http.Response, a ccAttempt) {
	if ccIsSSEResponse(resp.Header) {
		resp.Body = openrouter.NewSSEInterceptor(resp.Body, func(in, out, cr, cw int) {
			server.recordClaudeCodeMetrics(a, in, out, cr, cw)
		})
		return
	}

	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		if server.logger != nil {
			server.logger.Warn("claudecode response read failed", "account", a.accountID, "err", err)
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return
	}
	in, out, cr, cw := openrouter.ParseUsageFromJSON(body)
	server.recordClaudeCodeMetrics(a, in, out, cr, cw)
	resp.Body = io.NopCloser(bytes.NewReader(body))
}

// ccCopyStream forwards the upstream body, flushing per chunk so SSE events
// reach the client as they arrive instead of at end of stream.
func ccCopyStream(writer http.ResponseWriter, body io.Reader) {
	flusher, hasFlusher := writer.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := writer.Write(buf[:n]); werr != nil {
				return
			}
			if hasFlusher {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// forwardToClaudeCode sends a /v1/messages request through the Claude Code
// account pool with sticky-session affinity and 429-triggered failover.
func (server *Server) forwardToClaudeCode(
	writer http.ResponseWriter,
	request *http.Request,
	ccCfg claudecode.Config,
	reqBody []byte,
	model string,
) {
	pool, client := server.getOrCreateCCPool(ccCfg)
	sessionKey := ccExtractSessionID(request, ccParseBodyMap(reqBody))

	if server.isCCREnabled() {
		var reqMap map[string]any
		if err := json.Unmarshal(reqBody, &reqMap); err == nil {
			sender := func(ctx context.Context, bodyBytes []byte) (*http.Response, error) {
				const maxAttempts = 3
				excluded := make(map[string]bool)

				for attempt := 0; attempt < maxAttempts; attempt++ {
					acc, err := pool.SelectAccount(sessionKey, excluded)
					if err != nil {
						return nil, fmt.Errorf("no Claude Code accounts available: %w", err)
					}

					_ = pool.RefreshTokenIfNeeded(acc)
					if refreshedAcc, ok := pool.GetAccount(acc.ID); ok {
						server.syncRefreshedAccountToConfig(acc.ID, refreshedAcc.Token, refreshedAcc.RefreshToken, refreshedAcc.ExpiresAt)
					}

					pool.Acquire(acc.ID)
					startTime := time.Now()

					resp, err := client.SendMessage(ctx, acc.Token, bodyBytes, request.Header)
					if err != nil {
						pool.Release(acc.ID)
						pool.RecordFailure(acc.ID, false, 10*time.Second)
						if server.logger != nil {
							server.logger.Warn("claudecode request failed", "account", acc.ID, "err", err)
						}
						excluded[acc.ID] = true
						continue
					}

					// If 401 Unauthorized and account has refresh token, attempt token refresh and retry once
					if resp.StatusCode == http.StatusUnauthorized && acc.RefreshToken != "" {
						if refreshErr := pool.RefreshAccountToken(acc.ID); refreshErr == nil {
							if refreshedAcc, ok := pool.GetAccount(acc.ID); ok {
								server.syncRefreshedAccountToConfig(acc.ID, refreshedAcc.Token, refreshedAcc.RefreshToken, refreshedAcc.ExpiresAt)
								_ = resp.Body.Close()
								retryResp, retryErr := client.SendMessage(ctx, refreshedAcc.Token, bodyBytes, request.Header)
								if retryErr == nil {
									resp = retryResp
								}
							}
						}
					}

					rl := claudecode.ExtractRateLimits(resp.Header)

					if resp.StatusCode == http.StatusTooManyRequests {
						_, _ = io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						pool.Release(acc.ID)
						pool.RecordRateLimit(acc.ID, rl, 10*time.Second)
						if server.logger != nil {
							server.logger.Warn("claudecode 429, failing over", "account", acc.ID)
						}
						excluded[acc.ID] = true
						continue
					}

					if resp.StatusCode < 400 {
						server.ccInstrumentResponse(resp, ccAttempt{
							model:       model,
							sessionID:   sessionKey,
							accountID:   acc.ID,
							accountName: acc.Name,
							startTime:   startTime,
							pool:        pool,
							rateLimits:  rl,
						})
					} else if resp.StatusCode >= 500 {
						pool.RecordFailure(acc.ID, true, 30*time.Second)
					} else {
						pool.RecordFailure(acc.ID, false, 0)
					}

					accID := acc.ID
					wrappedBody := &poolReleasingBody{
						ReadCloser: resp.Body,
						releaseFn: func() {
							pool.Release(accID)
						},
					}
					resp.Body = wrappedBody
					return resp, nil
				}

				return nil, errors.New("all Claude Code accounts rate-limited or unavailable")
			}

			opts := server.defaultCCROptions(sender)
			isStreaming, _ := reqMap["stream"].(bool)
			if isStreaming {
				_ = ProxyAnthropicStreamWithCCR(request.Context(), writer, reqMap, opts)
			} else {
				_ = ProxyAnthropicJSONWithCCR(request.Context(), writer, reqMap, opts)
			}
			return
		}
	}

	const maxAttempts = 3
	excluded := make(map[string]bool)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		acc, err := pool.SelectAccount(sessionKey, excluded)
		if err != nil {
			writeAPIError(writer, http.StatusServiceUnavailable, "overloaded_error", "No Claude Code accounts available: "+err.Error())
			return
		}

		_ = pool.RefreshTokenIfNeeded(acc)
		if refreshedAcc, ok := pool.GetAccount(acc.ID); ok {
			server.syncRefreshedAccountToConfig(acc.ID, refreshedAcc.Token, refreshedAcc.RefreshToken, refreshedAcc.ExpiresAt)
		}

		pool.Acquire(acc.ID)
		startTime := time.Now()

		resp, err := client.SendMessage(request.Context(), acc.Token, reqBody, request.Header)
		if err != nil {
			pool.Release(acc.ID)
			pool.RecordFailure(acc.ID, false, 10*time.Second)
			if server.logger != nil {
				server.logger.Warn("claudecode request failed", "account", acc.ID, "err", err)
			}
			excluded[acc.ID] = true
			continue
		}

		// If 401 Unauthorized and account has refresh token, attempt token refresh and retry once
		if resp.StatusCode == http.StatusUnauthorized && acc.RefreshToken != "" {
			if refreshErr := pool.RefreshAccountToken(acc.ID); refreshErr == nil {
				if refreshedAcc, ok := pool.GetAccount(acc.ID); ok {
					server.syncRefreshedAccountToConfig(acc.ID, refreshedAcc.Token, refreshedAcc.RefreshToken, refreshedAcc.ExpiresAt)
					_ = resp.Body.Close()
					retryResp, retryErr := client.SendMessage(request.Context(), refreshedAcc.Token, reqBody, request.Header)
					if retryErr == nil {
						resp = retryResp
					}
				}
			}
		}

		rl := claudecode.ExtractRateLimits(resp.Header)

		if resp.StatusCode == http.StatusTooManyRequests {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			pool.Release(acc.ID)
			pool.RecordRateLimit(acc.ID, rl, 10*time.Second)
			if server.logger != nil {
				server.logger.Warn("claudecode 429, failing over", "account", acc.ID)
			}
			excluded[acc.ID] = true
			continue
		}

		// Success — instrument usage capture before the body is consumed.
		if resp.StatusCode < 400 {
			server.ccInstrumentResponse(resp, ccAttempt{
				model:       model,
				sessionID:   sessionKey,
				accountID:   acc.ID,
				accountName: acc.Name,
				startTime:   startTime,
				pool:        pool,
				rateLimits:  rl,
			})
		} else if resp.StatusCode >= 500 {
			pool.RecordFailure(acc.ID, true, 30*time.Second)
		} else {
			pool.RecordFailure(acc.ID, false, 0)
		}

		defer resp.Body.Close()
		defer pool.Release(acc.ID)

		ccCopyResponseHeaders(writer.Header(), resp.Header)
		writer.WriteHeader(resp.StatusCode)

		ccCopyStream(writer, resp.Body)

		return
	}

	writeAPIError(writer, http.StatusServiceUnavailable, "overloaded_error", "All Claude Code accounts rate-limited or unavailable")
}

// ccCopyResponseHeaders forwards relevant upstream headers to the downstream response.
func ccCopyResponseHeaders(dst, src http.Header) {
	for _, k := range []string{
		"Content-Type",
		"X-Request-Id",
		"Request-Id",
		"Anthropic-Ratelimit-Input-Tokens-Limit",
		"Anthropic-Ratelimit-Input-Tokens-Remaining",
		"Anthropic-Ratelimit-Input-Tokens-Reset",
		"Anthropic-Ratelimit-Output-Tokens-Limit",
		"Anthropic-Ratelimit-Output-Tokens-Remaining",
		"Anthropic-Ratelimit-Output-Tokens-Reset",
		"Anthropic-Ratelimit-Requests-Limit",
		"Anthropic-Ratelimit-Requests-Remaining",
		"Anthropic-Ratelimit-Requests-Reset",
	} {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}
