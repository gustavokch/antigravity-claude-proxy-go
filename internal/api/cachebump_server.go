package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"antigravity-go-proxy/internal/cachebump"
	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/kimi"
	"antigravity-go-proxy/internal/openrouter"
)

// cacheBumpTickingInterval is how often the scheduler looks for due bumps.
const cacheBumpTickingInterval = 10 * time.Second

// getCacheBump returns the server's cache-bump store and scheduler, creating
// them on first use with the capacities from the current config.
func (server *Server) getCacheBump() (*cachebump.Store, *cachebump.Scheduler) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.cacheBumpStore == nil {
		cfg := config.Get().CacheBump
		maxBytes := cfg.MaxBodyMB << 20
		store := cachebump.NewStoreWithLimits(24*time.Hour, cfg.MaxSessions, maxBytes)
		sched := cachebump.NewScheduler(store, server.cacheBumpSender(), cachebump.SchedulerConfig{
			LeadSeconds:        cfg.LeadSeconds,
			MaxBumpsPerSession: cfg.MaxBumpsPerSession,
			MaxIdleMinutes:     cfg.MaxIdleMinutes,
		})
		sched.OnEvent = server.logCacheBumpEvent
		server.cacheBumpStore = store
		server.cacheBumpSched = sched
	}
	return server.cacheBumpStore, server.cacheBumpSched
}

// StartCacheBumpScheduler drives the bump loop until ctx is done, beside
// StartClaudeCodeBackgroundWorker. A tick is skipped while the feature is
// disabled so flipping the WebUI switch stops bumps without deleting
// recorded sessions.
func (server *Server) StartCacheBumpScheduler(ctx context.Context) {
	_, sched := server.getCacheBump()
	ticker := time.NewTicker(cacheBumpTickingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !config.Get().CacheBump.Enabled {
				continue
			}
			sched.Tick(ctx)
		}
	}
}

// cacheBumpHeaderKey carries the X-Cache-Bump value through the request
// context after the header itself is stripped (it must never reach upstream).
type cacheBumpHeaderKey struct{}

// CacheBumpHeader is the per-request override header: on|off.
const CacheBumpHeader = "X-Cache-Bump"

// consumeCacheBumpHeader removes the X-Cache-Bump header from the request and
// preserves its value in the context for the recorder, which runs after the
// header must already be gone from the wire.
func (server *Server) consumeCacheBumpHeader(request *http.Request) *http.Request {
	val := request.Header.Get(CacheBumpHeader)
	if val == "" {
		return request
	}
	request.Header.Del(CacheBumpHeader)
	return request.WithContext(context.WithValue(request.Context(), cacheBumpHeaderKey{}, val))
}

// cacheBumpHeaderValue reads the per-request override, whether it arrived as
// a header (direct forwarder calls) or via the context (messages handler).
func cacheBumpHeaderValue(request *http.Request) string {
	if request == nil {
		return ""
	}
	if val := request.Header.Get(CacheBumpHeader); val != "" {
		return val
	}
	if val, ok := request.Context().Value(cacheBumpHeaderKey{}).(string); ok {
		return val
	}
	return ""
}

// maybeRecordCacheBump records a completed turn so its cache entry can be
// bumped later. It runs on the success path only, and records nothing unless
// the feature is enabled for this route and request (config plus the
// X-Cache-Bump header), the session is identifiable, and the body actually
// carries cache_control markers — a body without markers can never hold a
// cache entry, so bumping it would only ever pay a write.
func (server *Server) maybeRecordCacheBump(route cachebump.Route, request *http.Request, reqBody []byte, sessionKey, model, accountID, endpointID string, floor int) {
	cfg := config.Get().CacheBump
	if !cfg.EnabledFor(string(route), cacheBumpHeaderValue(request)) {
		return
	}
	if sessionKey == "" {
		return
	}
	marker, _ := cachebump.InspectCacheControl(reqBody)
	if !marker {
		return
	}

	replay, err := cachebump.BuildReplayBody(reqBody, floor)
	if err != nil {
		if server.logger != nil {
			server.logger.Warn("cachebump: build replay body failed", "session", sessionKey, "err", err)
		}
		return
	}

	now := time.Now()
	// The 1h TTL is inert without the extended-cache-ttl beta, so compute it
	// from the same beta the forward path sent, not from the body alone.
	ttl := cachebump.DetectTTL(reqBody, request.Header.Get("anthropic-beta"))
	store, _ := server.getCacheBump()
	store.Upsert(cachebump.Record{
		Key:        cachebump.RecordKey(route, sessionKey),
		SessionID:  sessionKey,
		Route:      route,
		Model:      model,
		AccountID:  accountID,
		EndpointID: endpointID,
		Body:       replay,
		Headers:    cachebump.AllowlistHeaders(request.Header),
		TTL:        ttl,
		LastSeen:   now,
		NextBump:   cachebump.NextBumpTime(now, ttl, cfg.LeadSeconds),
	})
}

// ccMaybeRecordCacheBump records a completed Claude Code turn. The replay
// floor is 1: Anthropic rejects max_tokens < 1 and accepts it.
func (server *Server) ccMaybeRecordCacheBump(request *http.Request, reqBody []byte, sessionKey, model, accountID string) {
	server.maybeRecordCacheBump(cachebump.RouteClaudeCode, request, reqBody, sessionKey, model, accountID, "", 1)
}

// cacheBumpSender dispatches a bump to the sender for its route.
func (server *Server) cacheBumpSender() cachebump.Sender {
	return func(ctx context.Context, rec cachebump.Record) (cachebump.BumpResult, error) {
		switch rec.Route {
		case cachebump.RouteClaudeCode:
			return server.sendClaudeCodeBump(ctx, rec)
		case cachebump.RouteKimi:
			return server.sendKimiBump(ctx, rec)
		case cachebump.RouteCustom:
			return server.sendCustomBump(ctx, rec)
		}
		return cachebump.BumpResult{}, cachebump.ErrAccountUnavailable
	}
}

// sendClaudeCodeBump replays a bump pinned to the recorded account: the
// cache entry belongs to that account, and SelectAccount could hand back a
// different one. A disabled, cooling or missing account stops the record
// instead of failing over.
func (server *Server) sendClaudeCodeBump(ctx context.Context, rec cachebump.Record) (cachebump.BumpResult, error) {
	pool, client := server.getOrCreateCCPool(config.Get().ClaudeCode)

	acc, ok := pool.GetAccount(rec.AccountID)
	if !ok || !acc.Enabled || acc.CooldownUntil.After(time.Now()) {
		return cachebump.BumpResult{}, cachebump.ErrAccountUnavailable
	}

	// A failed refresh would send a stale token, earn a 401 and stop the
	// record for good ("upstream_rejected"). Treat it as an unavailable
	// account instead, which is the recoverable stop.
	if err := pool.RefreshTokenIfNeeded(acc); err != nil {
		return cachebump.BumpResult{}, cachebump.ErrAccountUnavailable
	}
	if refreshed, ok := pool.GetAccount(acc.ID); ok {
		server.syncRefreshedAccountToConfig(acc.ID, refreshed.Token, refreshed.RefreshToken, refreshed.ExpiresAt)
		acc = refreshed
	}

	pool.Acquire(acc.ID)
	defer pool.Release(acc.ID)

	resp, err := client.SendMessage(ctx, acc.Token, rec.Body, rec.Headers)
	if err != nil {
		return cachebump.BumpResult{}, err
	}
	defer resp.Body.Close()

	// A bump burns the same window a real turn does, so its outcome has to
	// reach the pool: otherwise bump traffic silently drains an account the
	// selector still believes is healthy.
	rl := claudecode.ExtractRateLimits(resp.Header)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		pool.RecordRateLimit(acc.ID, rl, 10*time.Second)
		return cachebump.BumpResult{}, &cachebump.UpstreamError{Status: resp.StatusCode}
	case resp.StatusCode >= 500:
		pool.RecordFailure(acc.ID, true, 30*time.Second)
		return cachebump.BumpResult{}, &cachebump.UpstreamError{Status: resp.StatusCode}
	case resp.StatusCode >= 400:
		pool.RecordFailure(acc.ID, false, 0)
		return cachebump.BumpResult{}, &cachebump.UpstreamError{Status: resp.StatusCode}
	}
	pool.UpdateAccountRateLimits(acc.ID, rl)

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return cachebump.BumpResult{}, err
	}
	_, _, cacheRead, cacheCreation := openrouter.ParseUsageFromJSON(body)
	return cachebump.BumpResult{CacheReadTokens: cacheRead, CacheCreationTokens: cacheCreation}, nil
}

// sendKimiBump replays a bump against the currently configured Kimi gateway.
func (server *Server) sendKimiBump(ctx context.Context, rec cachebump.Record) (cachebump.BumpResult, error) {
	kimiCfg := config.Get().Kimi
	if kimiCfg.BaseURL == "" {
		return cachebump.BumpResult{}, cachebump.ErrAccountUnavailable
	}
	return postBumpRequest(ctx, rec, kimi.NormalizeBaseURL(kimiCfg.BaseURL)+"/v1/messages", func(hdr http.Header) {
		hdr.Set("Authorization", "Bearer "+kimiCfg.APIKey)
	})
}

// sendCustomBump replays a bump against the recorded custom endpoint.
func (server *Server) sendCustomBump(ctx context.Context, rec cachebump.Record) (cachebump.BumpResult, error) {
	endpoint, ok := config.Get().CustomEndpoints[rec.EndpointID]
	if !ok || endpoint.URL == "" {
		return cachebump.BumpResult{}, cachebump.ErrAccountUnavailable
	}
	target := endpoint.URL
	if !strings.HasSuffix(target, "/v1/messages") {
		target = strings.TrimSuffix(target, "/") + "/v1/messages"
	}
	return postBumpRequest(ctx, rec, target, func(hdr http.Header) {
		if endpoint.APIKey != "" {
			hdr.Set("x-api-key", endpoint.APIKey)
		}
	})
}

// postBumpRequest sends one bump replay with plain HTTP: the shared shape of
// the Kimi and custom-endpoint senders. Auth is applied by the caller.
func postBumpRequest(ctx context.Context, rec cachebump.Record, targetURL string, applyAuth func(http.Header)) (cachebump.BumpResult, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(rec.Body))
	if err != nil {
		return cachebump.BumpResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	applyAuth(httpReq.Header)
	for name, vals := range rec.Headers {
		for _, v := range vals {
			httpReq.Header.Add(name, v)
		}
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return cachebump.BumpResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return cachebump.BumpResult{}, &cachebump.UpstreamError{Status: resp.StatusCode}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return cachebump.BumpResult{}, err
	}
	_, _, cacheRead, cacheCreation := openrouter.ParseUsageFromJSON(body)
	return cachebump.BumpResult{CacheReadTokens: cacheRead, CacheCreationTokens: cacheCreation}, nil
}

// logCacheBumpEvent logs one bump outcome at info, keyed for dashboards:
// session, route, model, account, cache_read_tokens, outcome.
func (server *Server) logCacheBumpEvent(rec cachebump.Record, outcome string) {
	if server.logger == nil {
		return
	}
	fields := []any{
		"session", rec.SessionID,
		"route", string(rec.Route),
		"model", rec.Model,
		"cache_read_tokens", rec.LastCacheReadTokens,
		"cache_creation_tokens", rec.LastCacheCreationTokens,
		"outcome", outcome,
	}
	if rec.AccountID != "" {
		fields = append(fields, "account", rec.AccountID)
	}
	server.logger.Info("cachebump", fields...)
}
