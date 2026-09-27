package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
)

var (
	ccPoolMu   sync.Mutex
	ccPoolInst *claudecode.AccountPool
	ccPoolKey  string // tracks config identity to detect changes
	// ccPoolStale forces the next getOrCreateCCPool to rebuild while keeping
	// the old pool reachable, so its live state can be carried over.
	ccPoolStale bool
)

// invalidateCCPoolLocked marks the pool for rebuild on next use after a
// config change. ccPoolMu must be held. Unlike dropping ccPoolInst, the old
// pool stays reachable so the rebuild can inherit its unified snapshots and
// retire it.
func invalidateCCPoolLocked() {
	ccPoolStale = true
	ccHTTPClient = nil
}

var ccHTTPClient *claudecode.Client

func getOrCreateCCPool(cfg claudecode.Config) (*claudecode.AccountPool, *claudecode.Client) {
	var s *Server
	return s.getOrCreateCCPool(cfg)
}

func ccAccountsEqual(a, b []claudecode.AccountConfig) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Token != b[i].Token || a[i].RefreshToken != b[i].RefreshToken || a[i].Enabled != b[i].Enabled {
			return false
		}
	}
	return true
}

func (server *Server) getOrCreateCCPool(cfg claudecode.Config) (*claudecode.AccountPool, *claudecode.Client) {
	ccPoolMu.Lock()
	defer ccPoolMu.Unlock()

	key := cfg.BaseURL
	if ccPoolInst == nil || ccPoolStale || ccPoolKey != key || !ccAccountsEqual(ccPoolCfg.Accounts, cfg.Accounts) {
		oldPool := ccPoolInst
		newPool := claudecode.NewAccountPool(cfg.Accounts)

		// Surviving accounts keep their in-memory subscription windows,
		// which are at least as fresh as the stored ones. The old pool is
		// retired before the new one is published, so a save it still had
		// queued cannot overwrite the new account set on disk.
		if oldPool != nil {
			newPool.InheritUnified(oldPool)
			// May block getOrCreateCCPool callers on one atomic file write.
			oldPool.Retire()
		}

		ccPoolInst = newPool
		ccPoolStale = false
		ccHTTPClient = claudecode.NewClient(claudecode.NormalizeBaseURL(cfg.BaseURL), nil)
		ccPoolKey = key
		ccPoolCfg = cfg

		// Pick up the last known subscription limits so a restart (or a
		// pool rebuilt after a config change) does not start blank. The
		// explicit path also enables saving unified snapshot changes.
		// Accounts that inherited a snapshot above are skipped.
		ccPoolInst.SetStoragePath(claudecode.DefaultStoragePath())
		if err := ccPoolInst.RestoreStoredUnified(); err != nil {
			slog.Warn("claudecode: failed to restore unified limit snapshots", "error", err)
		}

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
// from the request body.
//
// The four header names below are the spellings third-party harnesses use.
// Claude Code itself sends none of them: it sends X-Claude-Code-Session-Id,
// present on every captured POST /v1/messages in
// .reference/claude-code-headers-20260923*.jsonl and absent from the captured
// GETs. Reading that name here would change which requests get a session key
// and therefore account stickiness, so it is a behaviour change rather than a
// spelling to add to the list.
//
// The body fallback exists because a harness that sends no session header may
// still carry the identifier in metadata. Mirrors openrouter.ExtractSessionID
// minus the remote-address fallback, which would change account stickiness for
// anonymous clients.
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
	requestID   string // upstream "request-id" response header
}

// recordClaudeCodeMetrics is the single place a completed Claude Code call
// becomes metrics, logs, pool accounting and dashboard stats. Mirrors
// recordOpenRouterMetrics. u is the detailed usage captured from the upstream
// response; its cost comes from claudecode.UsageCost via ComputeFinalMetrics.
func (server *Server) recordClaudeCodeMetrics(a ccAttempt, u claudecode.Usage) claudecode.RequestMetrics {
	latency := time.Since(a.startTime)
	in, out, cr, cw := int(u.Input), int(u.Output), int(u.CacheRead), int(u.CacheCreate)
	metrics := claudecode.RequestMetrics{
		Model:                 a.model,
		AccountID:             a.accountID,
		AccountName:           a.accountName,
		SessionID:             a.sessionID,
		InputTokens:           in,
		OutputTokens:          out,
		CacheReadTokens:       cr,
		CacheCreationTokens:   cw,
		CacheCreation1hTokens: int(u.CacheCreate1h),
		Speed:                 u.Speed,
		Latency:               latency,
	}
	metrics.ComputeFinalMetrics(claudecode.DefaultSessionTracker)
	if server.logger != nil {
		claudecode.LogObservability(server.logger, metrics)
		server.logger.Debug("claudecode usage detail",
			"account", a.accountID,
			"message_id", u.MessageID,
			"served_model", u.Model,
			"request_id", u.RequestID,
			"cache_creation_5m", u.CacheCreate5m,
			"cache_creation_1h", u.CacheCreate1h,
			"speed", u.Speed,
			"iterations", len(u.Iterations),
		)
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
// The capture is Anthropic-specific (claudecode.UsageInterceptor and
// ParseUsageJSON) so it also records the message ID, served model, upstream
// request ID and the 5m/1h cache write split.
func (server *Server) ccInstrumentResponse(resp *http.Response, a ccAttempt) {
	a.requestID = resp.Header.Get("request-id")
	if ccIsSSEResponse(resp.Header) {
		resp.Body = claudecode.NewUsageInterceptor(resp.Body, func(u claudecode.Usage) {
			u.RequestID = a.requestID
			server.recordClaudeCodeMetrics(a, u)
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
	u := claudecode.ParseUsageJSON(body)
	u.RequestID = a.requestID
	server.recordClaudeCodeMetrics(a, u)
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
				var last429Body []byte

				for attempt := 0; attempt < maxAttempts; attempt++ {
					acc, err := pool.SelectAccount(sessionKey, excluded)
					if err != nil {
						if last429Body != nil {
							return nil, fmt.Errorf("upstream rate limit: %s", strings.TrimSpace(string(last429Body)))
						}
						return nil, fmt.Errorf("no Claude Code accounts available: %w", err)
					}

					_ = pool.RefreshTokenIfNeeded(acc)
					if refreshedAcc, ok := pool.GetAccount(acc.ID); ok {
						server.syncRefreshedAccountToConfig(acc.ID, refreshedAcc.Token, refreshedAcc.RefreshToken, refreshedAcc.ExpiresAt)
					}

					pool.Acquire(acc.ID)
					startTime := time.Now()

					identity, normalize := ccCfg.Identity.WireIdentity(acc.AccountUUID, sessionKey)
					resp, err := client.SendMessage(ctx, claudecode.MessageRequest{
						Token:         acc.Token,
						Body:          bodyBytes,
						ClientHeaders: request.Header,
						Normalize:     normalize,
						Identity:      identity,
					})
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
					if resp.StatusCode == http.StatusUnauthorized {
						if acc.RefreshToken != "" {
							if refreshErr := pool.RefreshAccountToken(acc.ID); refreshErr == nil {
								if refreshedAcc, ok := pool.GetAccount(acc.ID); ok {
									server.syncRefreshedAccountToConfig(acc.ID, refreshedAcc.Token, refreshedAcc.RefreshToken, refreshedAcc.ExpiresAt)
									retryResp, retryErr := client.SendMessage(ctx, claudecode.MessageRequest{
										Token:         refreshedAcc.Token,
										Body:          bodyBytes,
										ClientHeaders: request.Header,
										Normalize:     normalize,
										Identity:      identity,
									})
									if retryErr == nil {
										_ = resp.Body.Close()
										resp = retryResp
									}
								}
							}
						}
						if resp.StatusCode == http.StatusUnauthorized {
							_ = resp.Body.Close()
							pool.Release(acc.ID)
							pool.RecordFailure(acc.ID, true, 30*time.Second)
							if server.logger != nil {
								server.logger.Warn("claudecode 401 unauthorized, failing over", "account", acc.ID)
							}
							excluded[acc.ID] = true
							continue
						}
					}

					rl := claudecode.ExtractRateLimits(resp.Header)

					if resp.StatusCode == http.StatusTooManyRequests {
						last429Body, _ = io.ReadAll(io.LimitReader(resp.Body, 8192))
						_ = resp.Body.Close()
						pool.Release(acc.ID)
						pool.RecordRateLimit(acc.ID, rl, 10*time.Second)
						if server.logger != nil {
							server.logger.Warn("claudecode 429, failing over", "account", acc.ID, "body", strings.TrimSpace(string(last429Body)))
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
						server.ccMaybeRecordCacheBump(request, bodyBytes, sessionKey, model, acc.ID)
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

				if last429Body != nil {
					return nil, fmt.Errorf("upstream rate limit: %s", strings.TrimSpace(string(last429Body)))
				}
				return nil, errors.New("all Claude Code accounts rate-limited or unavailable")
			}

			opts := server.defaultCCROptions(sender)
			if ccCfg.ForwardUnifiedHeadersEnabled() {
				opts.ResponseHeaderFilter = ccCopyUnifiedHeaders
			}
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
	var last429Body []byte
	var last429Header http.Header

	for attempt := 0; attempt < maxAttempts; attempt++ {
		acc, err := pool.SelectAccount(sessionKey, excluded)
		if err != nil {
			if last429Body != nil {
				writeCCUpstream429(writer, last429Body, last429Header, ccCfg.ForwardUnifiedHeadersEnabled())
				return
			}
			writeAPIError(writer, http.StatusServiceUnavailable, "overloaded_error", "No Claude Code accounts available: "+err.Error())
			return
		}

		_ = pool.RefreshTokenIfNeeded(acc)
		if refreshedAcc, ok := pool.GetAccount(acc.ID); ok {
			server.syncRefreshedAccountToConfig(acc.ID, refreshedAcc.Token, refreshedAcc.RefreshToken, refreshedAcc.ExpiresAt)
		}

		pool.Acquire(acc.ID)
		startTime := time.Now()

		identity, normalize := ccCfg.Identity.WireIdentity(acc.AccountUUID, sessionKey)
		resp, err := client.SendMessage(request.Context(), claudecode.MessageRequest{
			Token:         acc.Token,
			Body:          reqBody,
			ClientHeaders: request.Header,
			Normalize:     normalize,
			Identity:      identity,
		})
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
		if resp.StatusCode == http.StatusUnauthorized {
			if acc.RefreshToken != "" {
				if refreshErr := pool.RefreshAccountToken(acc.ID); refreshErr == nil {
					if refreshedAcc, ok := pool.GetAccount(acc.ID); ok {
						server.syncRefreshedAccountToConfig(acc.ID, refreshedAcc.Token, refreshedAcc.RefreshToken, refreshedAcc.ExpiresAt)
						retryResp, retryErr := client.SendMessage(request.Context(), claudecode.MessageRequest{
							Token:         refreshedAcc.Token,
							Body:          reqBody,
							ClientHeaders: request.Header,
							Normalize:     normalize,
							Identity:      identity,
						})
						if retryErr == nil {
							_ = resp.Body.Close()
							resp = retryResp
						}
					}
				}
			}
			if resp.StatusCode == http.StatusUnauthorized {
				_ = resp.Body.Close()
				pool.Release(acc.ID)
				pool.RecordFailure(acc.ID, true, 30*time.Second)
				if server.logger != nil {
					server.logger.Warn("claudecode 401 unauthorized, failing over", "account", acc.ID)
				}
				excluded[acc.ID] = true
				continue
			}
		}

		rl := claudecode.ExtractRateLimits(resp.Header)

		if resp.StatusCode == http.StatusTooManyRequests {
			last429Body, _ = io.ReadAll(io.LimitReader(resp.Body, 8192))
			last429Header = resp.Header.Clone()
			_ = resp.Body.Close()
			pool.Release(acc.ID)
			pool.RecordRateLimit(acc.ID, rl, 10*time.Second)
			if server.logger != nil {
				server.logger.Warn("claudecode 429, failing over", "account", acc.ID, "body", strings.TrimSpace(string(last429Body)))
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
			server.ccMaybeRecordCacheBump(request, reqBody, sessionKey, model, acc.ID)
		} else if resp.StatusCode >= 500 {
			pool.RecordFailure(acc.ID, true, 30*time.Second)
		} else {
			pool.RecordFailure(acc.ID, false, 0)
		}

		defer resp.Body.Close()
		defer pool.Release(acc.ID)

		ccCopyResponseHeaders(writer.Header(), resp.Header, ccCfg.ForwardUnifiedHeadersEnabled())
		writer.WriteHeader(resp.StatusCode)

		ccCopyStream(writer, resp.Body)

		return
	}

	if last429Body != nil {
		writeCCUpstream429(writer, last429Body, last429Header, ccCfg.ForwardUnifiedHeadersEnabled())
		return
	}
	writeAPIError(writer, http.StatusServiceUnavailable, "overloaded_error", "All Claude Code accounts rate-limited or unavailable")
}

// writeCCUpstream429 mirrors an upstream rate-limit rejection to the client
// instead of the generic 503, so callers (and humans) see the real cause.
// Retry guidance (Retry-After, Anthropic-Ratelimit-*) is forwarded when the
// upstream supplied it, and the subscription (Anthropic-Ratelimit-Unified-*)
// headers too when forwardUnified is set.
func writeCCUpstream429(w http.ResponseWriter, body []byte, header http.Header, forwardUnified bool) {
	if !json.Valid(body) {
		body = []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"upstream rate limit exceeded"}}`)
	}
	if header != nil {
		for _, k := range []string{
			"Retry-After",
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
			if v := header.Get(k); v != "" {
				w.Header().Set(k, v)
			}
		}
		if forwardUnified {
			ccCopyUnifiedHeaders(w.Header(), header)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(body)
}

// ccCopyResponseHeaders forwards relevant upstream headers to the downstream
// response. forwardUnified adds every Anthropic-Ratelimit-Unified-* header.
func ccCopyResponseHeaders(dst, src http.Header, forwardUnified bool) {
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
	if forwardUnified {
		ccCopyUnifiedHeaders(dst, src)
	}
}

// ccCopyUnifiedHeaders copies every header whose name starts with the
// subscription rate-limit prefix (anthropic-ratelimit-unified-), matched
// case-insensitively, so new unified windows reach clients without a code
// change. All values of a header are copied.
func ccCopyUnifiedHeaders(dst, src http.Header) {
	for k, vs := range src {
		if len(vs) == 0 || !strings.HasPrefix(strings.ToLower(k), claudecode.HeaderUnifiedPrefix) {
			continue
		}
		dst[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
	}
}
