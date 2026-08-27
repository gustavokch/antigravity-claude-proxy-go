package api

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"antigravity-go-proxy/internal/claudecode"
)

var (
	ccPoolMu   sync.Mutex
	ccPoolInst *claudecode.AccountPool
	ccPoolKey  string // tracks config identity to detect changes
)

var ccHTTPClient *claudecode.Client

func getOrCreateCCPool(cfg claudecode.Config) (*claudecode.AccountPool, *claudecode.Client) {
	ccPoolMu.Lock()
	defer ccPoolMu.Unlock()

	key := cfg.BaseURL
	if ccPoolInst == nil || ccPoolKey != key || len(ccPoolCfg.Accounts) != len(cfg.Accounts) {
		ccPoolInst = claudecode.NewAccountPool(cfg.Accounts)
		ccPoolKey = key
		ccPoolCfg = cfg
	}
	if ccHTTPClient == nil {
		ccHTTPClient = claudecode.NewClient(claudecode.NormalizeBaseURL(cfg.BaseURL), nil)
	}
	return ccPoolInst, ccHTTPClient
}

var ccPoolCfg claudecode.Config

// matchClaudeCodeModel returns the canonical model ID if the request model
// matches an enabled allowlist entry by ID or alias. Returns "" on no match.
func matchClaudeCodeModel(cfg claudecode.Config, model string) string {
	if model == "" {
		return ""
	}
	for _, item := range cfg.Allowlist {
		if !item.Enabled {
			continue
		}
		if (item.ID != "" && item.ID == model) || (item.Alias != "" && item.Alias == model) {
			return item.ID
		}
	}
	return ""
}

// ccExtractSessionID extracts a stable session key from request headers.
func ccExtractSessionID(r *http.Request) string {
	for _, h := range []string{"x-session-id", "session-id", "anthropic-session-id", "x-conversation-id"} {
		if v := r.Header.Get(h); v != "" {
			return v
		}
	}
	return ""
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
	pool, client := getOrCreateCCPool(ccCfg)

	sessionKey := ccExtractSessionID(request)

	const maxAttempts = 3
	excluded := make(map[string]bool)

	for attempt := 0; attempt < maxAttempts; attempt++ {
		acc, err := pool.SelectAccount(sessionKey, excluded)
		if err != nil {
			writeAPIError(writer, http.StatusServiceUnavailable, "overloaded_error", "No Claude Code accounts available: "+err.Error())
			return
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

		latency := time.Since(startTime)
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

		// Success — stream response back.
		defer resp.Body.Close()
		defer pool.Release(acc.ID)

		ccCopyResponseHeaders(writer.Header(), resp.Header)
		writer.WriteHeader(resp.StatusCode)

		_, _ = io.Copy(writer, resp.Body)
		if f, ok := writer.(http.Flusher); ok {
			f.Flush()
		}

		if resp.StatusCode < 400 {
			// Record success with cost=0 (streaming, tokens not yet parsed).
			pool.RecordSuccess(acc.ID, 0, 0, rl)
		} else if resp.StatusCode >= 500 {
			pool.RecordFailure(acc.ID, true, 30*time.Second)
		} else {
			pool.RecordFailure(acc.ID, false, 0)
		}

		m := claudecode.RequestMetrics{
			Model:     model,
			SessionID: sessionKey,
			Latency:   latency,
		}
		_ = ccParseRequestTokens(reqBody, &m)
		m.ComputeFinalMetrics(claudecode.DefaultSessionTracker)
		if server.logger != nil {
			claudecode.LogObservability(server.logger, m)
		}

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

// ccParseRequestTokens extracts token usage from request body for observability.
// The request body usually does not include usage; this is a best-effort probe.
func ccParseRequestTokens(body []byte, m *claudecode.RequestMetrics) error {
	if len(body) == 0 {
		return nil
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return err
	}
	if usage, ok := req["usage"].(map[string]any); ok {
		if v, ok := usage["input_tokens"].(float64); ok {
			m.InputTokens = int(v)
		}
		if v, ok := usage["output_tokens"].(float64); ok {
			m.OutputTokens = int(v)
		}
		if v, ok := usage["cache_read_input_tokens"].(float64); ok {
			m.CacheReadTokens = int(v)
		}
		if v, ok := usage["cache_creation_input_tokens"].(float64); ok {
			m.CacheCreationTokens = int(v)
		}
	}
	return nil
}
