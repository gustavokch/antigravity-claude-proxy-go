package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/codex"
	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/responses"
	"antigravity-go-proxy/internal/transport"
)

const (
	// codexMaxAttempts bounds how many accounts one request is tried on.
	codexMaxAttempts = 3
	// codexFailureCooldown rests an account after a server error, a transport
	// error, or a 401 that a fresh token did not fix.
	codexFailureCooldown = 30 * time.Second
	// codexRateLimitCooldown rests an account after a 429 that names neither
	// a usage-limit reset nor a Retry-After.
	codexRateLimitCooldown = time.Minute
	// codexDefaultAgent is the agent a request without X-Claude-Code-Agent-Id
	// belongs to: Claude Code's main loop sends none.
	codexDefaultAgent = "main"
)

// codexCall is one Anthropic request translated for the Codex backend.
type codexCall struct {
	model          string            // canonical upstream id, echoed as message.model
	body           []byte            // the Codex Responses body
	toolNames      map[string]string // upstream tool name -> client tool name
	sessionKey     string            // account stickiness
	promptCacheKey string            // Session-Id header and prompt_cache_key
	serviceTier    string
	stream         bool // the client asked for an event stream
	// withoutReasoning rebuilds body with codex.BuildOptions.DropReasoning;
	// nil once used.
	withoutReasoning func() ([]byte, error)
}

// send builds the upstream request of the call for one account.
func (c codexCall) send(acc codex.Account, identity codex.IdentityConfig) codex.SendRequest {
	return codex.SendRequest{
		Token:       acc.Token,
		AccountID:   acc.AccountID,
		SessionID:   c.promptCacheKey,
		Model:       c.model,
		ServiceTier: c.serviceTier,
		Body:        c.body,
		Identity:    identity,
	}
}

// codexAllowlist is the configured allowlist, or the defaults when it is
// empty.
func codexAllowlist(cfg codex.Config) []codex.ModelConfig {
	if len(cfg.Allowlist) == 0 {
		return codex.DefaultAllowlist()
	}
	return cfg.Allowlist
}

// codexDefaultMaxOutput is the output limit of the built-in entry with the
// given id, or 0 when there is none. It stands in for a configured entry that
// sets no limit, as claudeCodeEntryMaxOutput does for Claude Code.
func codexDefaultMaxOutput(id string) int {
	for _, m := range codex.DefaultAllowlist() {
		if strings.EqualFold(m.ID, id) {
			return m.MaxOutputTokens
		}
	}
	return 0
}

// codexAgentID names the agent a request belongs to. Claude Code tags each
// subagent's requests with X-Claude-Code-Agent-Id, so a subagent gets a prompt
// cache key of its own instead of evicting the main loop's.
func codexAgentID(r *http.Request) string {
	if r != nil {
		if id := strings.TrimSpace(r.Header.Get("X-Claude-Code-Agent-Id")); id != "" {
			return id
		}
	}
	return codexDefaultAgent
}

// codexServiceTier reads the service tier BuildRequest put in the body; the
// routing hint repeats it.
func codexServiceTier(body []byte) string {
	var top struct {
		ServiceTier string `json:"service_tier"`
	}
	_ = json.Unmarshal(body, &top)
	return top.ServiceTier
}

// codexPoolFor returns the Codex account pool and client for cfg. The pool is
// rebuilt when the configured accounts differ from the ones it was built from,
// and the new pool inherits the old one's quota, cooldown and in-progress
// refreshes, so a config save costs no account state. The client is rebuilt
// when the base URL changes.
func (server *Server) codexPoolFor(cfg codex.Config) (*codex.AccountPool, *codex.Client) {
	server.codexMu.Lock()
	defer server.codexMu.Unlock()
	baseURL := codex.NormalizeBaseURL(cfg.BaseURL)
	if server.codexClient == nil || server.codexBaseURL != baseURL {
		server.codexClient = codex.NewClient(baseURL, server.codexUpstream)
		server.codexBaseURL = baseURL
	}
	if server.codexPool == nil || !slices.Equal(server.codexPoolCfg, cfg.Accounts) {
		pool := codex.NewAccountPool(cfg.Accounts)
		pool.SetTokenRefresher(server.refreshCodexToken)
		pool.SetTokenPersister(server.persistCodexTokens)
		pool.Inherit(server.codexPool)
		server.codexPool = pool
		// A copy: the persister edits it in place, and cfg.Accounts shares
		// its backing array with the live config.
		server.codexPoolCfg = slices.Clone(cfg.Accounts)
	}
	return server.codexPool, server.codexClient
}

// refreshCodexToken is the pool's TokenRefresher. A grant the token endpoint
// refused for good is reported as codex.ErrNeedsLogin, so the pool waits for a
// new login instead of spending requests on an account that cannot recover.
func (server *Server) refreshCodexToken(ctx context.Context, refreshToken string) (codex.TokenSet, error) {
	res, err := server.codexOAuthMgr.RefreshToken(ctx, refreshToken)
	if err != nil {
		if errors.Is(err, auth.ErrCodexGrantInvalid) {
			return codex.TokenSet{}, fmt.Errorf("%w: %w", codex.ErrNeedsLogin, err)
		}
		return codex.TokenSet{}, err
	}
	return codex.TokenSet{
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		IDToken:      res.IDToken,
		ExpiresAt:    res.ExpiresAt,
		Email:        res.Email,
		AccountID:    res.AccountID,
		PlanType:     res.PlanType,
	}, nil
}

// persistCodexTokens is the pool's TokenPersister. OpenAI rotates the refresh
// token on every refresh and the old one stops working, so the new set is in
// config.json before anything uses it: a restart that read the old one back
// would find the account signed out. The pool's own record of its accounts is
// updated with it, so the write does not read as a config change that
// rebuilds the pool.
func (server *Server) persistCodexTokens(accountID string, ts codex.TokenSet) error {
	server.codexMu.Lock()
	defer server.codexMu.Unlock()
	accounts := slices.Clone(config.Get().Codex.Accounts)
	i := codexAccountIndex(accounts, accountID)
	if i < 0 {
		return fmt.Errorf("codex account %s is no longer configured", accountID)
	}
	applyCodexTokens(&accounts[i], ts)
	if err := saveCodexSection(map[string]any{"accounts": accounts}); err != nil {
		return err
	}
	if j := codexAccountIndex(server.codexPoolCfg, accountID); j >= 0 {
		applyCodexTokens(&server.codexPoolCfg[j], ts)
	}
	return nil
}

// applyCodexTokens writes a refreshed token set into a stored account. Empty
// fields are ones the refresh did not report, so the stored value stays.
func applyCodexTokens(a *codex.AccountConfig, ts codex.TokenSet) {
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&a.Token, ts.AccessToken)
	set(&a.RefreshToken, ts.RefreshToken)
	set(&a.IDToken, ts.IDToken)
	set(&a.Email, ts.Email)
	set(&a.AccountID, ts.AccountID)
	set(&a.PlanType, ts.PlanType)
	if !ts.ExpiresAt.IsZero() {
		a.ExpiresAt = ts.ExpiresAt.UTC().Format(time.RFC3339)
	}
}

// codexAccountIndex returns the index of the account with the given id, or -1.
func codexAccountIndex(accounts []codex.AccountConfig, id string) int {
	return slices.IndexFunc(accounts, func(a codex.AccountConfig) bool { return a.ID == id })
}

// saveCodexSection merges section into config.json's codex section. Callers
// hold server.codexMu when section carries accounts read from the config, so
// two writers cannot each drop the other's change.
func saveCodexSection(section map[string]any) error {
	_, err := config.Save(map[string]any{"codex": section})
	return err
}

// codexUpstreamReply is a non-2xx Codex reply, read and closed.
type codexUpstreamReply struct {
	status int
	header http.Header
	body   []byte
}

// readCodexReply reads an error reply up to the error-body cap and closes it.
func readCodexReply(resp *http.Response) *codexUpstreamReply {
	body, _, _ := transport.ReadCapped(resp.Body, transport.MaxErrorBody)
	_ = resp.Body.Close()
	return &codexUpstreamReply{status: resp.StatusCode, header: resp.Header, body: body}
}

// peekCodexReply holds back the start of a 2xx stream until its first output
// event. A failure the backend reports before any output comes back as a reply,
// accounted for like the HTTP status it stands for; otherwise the response
// replays what was read. The bufio.Reader may hold bytes past the prefix, so it,
// not resp.Body, continues the stream.
func peekCodexReply(resp *http.Response) (*http.Response, *codexUpstreamReply, error) {
	buffered := bufio.NewReader(resp.Body)
	prefix, failure, err := codex.Peek(buffered, codex.PeekLimit)
	if err != nil {
		_ = resp.Body.Close()
		return nil, nil, err
	}
	if failure != nil {
		_ = resp.Body.Close()
		return nil, &codexUpstreamReply{status: codex.FailureStatus(failure), header: resp.Header, body: failure}, nil
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(prefix), buffered), resp.Body}
	return resp, nil, nil
}

// forwardToCodex serves an Anthropic Messages request from the Codex account
// pool: a sticky account first, then up to codexMaxAttempts accounts in all.
//
// A 401 gets one token refresh and one resend on the same account, then fails
// over. A 429 rests the account until its usage window resets (a
// usage_limit_reached body), or for Retry-After, or a minute, and fails over;
// when no account is left the last 429 reaches the client as a
// rate_limit_error. A server or transport error rests the account and fails
// over. A 400 invalid_encrypted_content is sent once more, on the same account,
// without the conversation's reasoning. Any other 4xx is the request's own fault
// and goes to the client as an Anthropic error envelope. A failure the backend
// reports inside a 2xx stream before any output is handled like the status it
// stands for; once output has started, a failure stays in the stream. A 2xx
// stream is translated into the Anthropic shape the client asked for.
func (server *Server) forwardToCodex(writer http.ResponseWriter, request *http.Request, cfg codex.Config, call codexCall) {
	pool, client := server.codexPoolFor(cfg)
	ctx := request.Context()
	excluded := make(map[string]bool)
	var last429, lastFailure *codexUpstreamReply
	var selectErr error

	for attempt := 0; attempt < codexMaxAttempts; attempt++ {
		selected, err := pool.SelectAccount(call.sessionKey, excluded)
		if err != nil {
			selectErr = err
			break
		}
		acc, err := pool.EnsureFresh(ctx, selected.ID)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			server.warnCodex("codex: account has no usable token, failing over", "account", selected.ID, "error", err)
			excluded[selected.ID] = true
			continue
		}

		pool.Acquire(acc.ID)
		resp, err := client.Send(ctx, call.send(acc, cfg.Identity))
		var reply *codexUpstreamReply
		if err == nil && resp.StatusCode == http.StatusUnauthorized {
			// An access token can be revoked before it expires, and a fresh
			// one fixes that: refresh once and resend on the same account.
			reply = readCodexReply(resp)
			refreshed, refreshErr := pool.RefreshRejected(ctx, acc.ID, acc.Token)
			if refreshErr == nil {
				acc, reply = refreshed, nil
				resp, err = client.Send(ctx, call.send(acc, cfg.Identity))
			} else {
				if ctx.Err() != nil {
					pool.Release(acc.ID)
					return
				}
				server.warnCodex("codex: token refresh after 401 failed", "account", acc.ID, "error", refreshErr)
			}
		}
		if err == nil && reply == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			resp, reply, err = peekCodexReply(resp)
		}
		if err != nil {
			pool.Release(acc.ID)
			if ctx.Err() != nil {
				return
			}
			pool.RecordFailure(acc.ID, codexFailureCooldown, "")
			server.warnCodex("codex: request failed, failing over", "account", acc.ID, "error", err)
			excluded[acc.ID] = true
			continue
		}
		if reply == nil {
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				server.serveCodexResponse(writer, request, pool, acc, call, resp)
				return
			}
			reply = readCodexReply(resp)
		}
		pool.Release(acc.ID)
		switch {
		case reply.status == http.StatusUnauthorized:
			pool.RecordFailure(acc.ID, codexFailureCooldown, "unauthorized")
			server.warnCodex("codex: 401 that a token refresh did not fix, failing over", "account", acc.ID)
		case reply.status == http.StatusTooManyRequests:
			noteCodexRateLimit(pool, acc.ID, reply)
			server.warnCodex("codex: 429, failing over", "account", acc.ID, "body", strings.TrimSpace(string(reply.body)))
			last429 = reply
		case reply.status >= 500:
			pool.RecordFailure(acc.ID, codexFailureCooldown, "server_error")
			server.warnCodex("codex: upstream server error, failing over", "account", acc.ID, "status", reply.status)
			lastFailure = reply
		default:
			if reply.status == http.StatusBadRequest && call.withoutReasoning != nil && codex.IsInvalidEncryptedContent(reply.body) {
				rebuilt, err := call.withoutReasoning()
				call.withoutReasoning = nil
				if err == nil {
					server.warnCodex("codex: backend refused the replayed reasoning; resending without it", "account", acc.ID)
					call.body = rebuilt
					attempt-- // the resend is not a new account; it happens at most once
					continue
				}
			}
			writeCodexUpstreamError(writer, reply)
			return
		}
		excluded[acc.ID] = true
	}

	// Every account resting after a rate limit is a 429 the client can wait
	// out, not an outage: it learns when the first account comes back.
	var coolingUntil time.Time
	cooling := false
	if selectErr != nil && len(excluded) == 0 {
		coolingUntil, cooling = pool.CooldownEnd()
	}
	switch {
	case last429 != nil:
		writeCodexUpstreamError(writer, last429)
	case lastFailure != nil:
		writeCodexUpstreamError(writer, lastFailure)
	case cooling:
		wait := max(time.Until(coolingUntil), time.Second)
		writer.Header().Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
		server.warnCodex("codex: every account is cooling down, replying 429", "until", coolingUntil, "accounts", codexAccountStates(pool))
		writeAPIError(writer, http.StatusTooManyRequests, "rate_limit_error",
			"Codex: every account is rate-limited until "+coolingUntil.UTC().Format(time.RFC3339))
	default:
		// The 503 body alone never reaches the operator: log each account's
		// state so the log shows why no account could serve the request.
		reason := "All Codex accounts are rate-limited, signed out or failing"
		if selectErr != nil && len(excluded) == 0 {
			reason = "No Codex accounts available: " + selectErr.Error()
		}
		if server.logger != nil {
			server.logger.Error("codex: no account could serve the request, replying 503",
				"model", call.model, "reason", reason, "excluded", ccExcludedIDs(excluded), "accounts", codexAccountStates(pool))
		}
		writeAPIError(writer, http.StatusServiceUnavailable, "overloaded_error", reason)
	}
}

// noteCodexRateLimit records what a 429 says about the account: its usage
// windows when the reply carries them, and the cooldown. A spent usage window
// rests the account until the reset the body names; any other 429 until
// Retry-After, or a minute.
func noteCodexRateLimit(pool *codex.AccountPool, accountID string, reply *codexUpstreamReply) {
	now := time.Now()
	if q, ok := codex.ParseQuota(reply.header, now); ok {
		pool.UpdateQuota(accountID, q)
	}
	if until, ok := codex.ParseUsageLimit(reply.body, now); ok {
		pool.RecordCooldown(accountID, until, "usage_limit_reached")
		return
	}
	pool.RecordCooldown(accountID, now.Add(codexRetryAfter(reply.header, now)), "rate_limited")
}

// codexRetryAfter reads Retry-After as seconds or an HTTP date, falling back
// to codexRateLimitCooldown.
func codexRetryAfter(h http.Header, now time.Time) time.Duration {
	value := strings.TrimSpace(h.Get("Retry-After"))
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return codexRateLimitCooldown
}

// serveCodexResponse translates a 2xx Codex event stream, whose start
// peekCodexReply found free of a failure, into the client's Anthropic shape and
// releases the account when done. The usage windows in the response headers
// are recorded first, so a request that later breaks still updates them.
func (server *Server) serveCodexResponse(writer http.ResponseWriter, request *http.Request, pool *codex.AccountPool, acc codex.Account, call codexCall, resp *http.Response) {
	defer pool.Release(acc.ID)
	defer resp.Body.Close()
	pool.RecordSuccess(acc.ID)
	if q, ok := codex.ParseQuota(resp.Header, time.Now()); ok {
		pool.UpdateQuota(acc.ID, q)
	}
	opts := responses.Options{Model: call.model, ToolNames: call.toolNames, ErrorLabel: "Codex"}

	if call.stream {
		header := writer.Header()
		header.Set("Content-Type", "text/event-stream")
		header.Set("Cache-Control", "no-cache")
		writer.WriteHeader(http.StatusOK)
		flusher, _ := writer.(http.Flusher)
		if err := responses.Stream(resp.Body, flushWriter{w: writer, f: flusher}, opts); err != nil {
			server.logStreamBreak(request, err,
				"gateway", "codex", "account", acc.ID, "model", call.model, "session", call.sessionKey)
		}
		return
	}

	upstream, err := responses.Aggregate(resp.Body)
	if err != nil {
		server.warnCodex("codex: upstream stream failed", "account", acc.ID, "model", call.model, "error", err)
		writeAPIError(writer, http.StatusBadGateway, "api_error", "Codex: "+err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, responses.ToAnthropic(upstream, opts))
}

// flushWriter flushes after every write, so each Anthropic event reaches the
// client as the upstream produces it instead of when a buffer fills.
type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

// writeCodexUpstreamError mirrors a Codex error reply to the client as an
// Anthropic error envelope with the upstream's status and message, and its
// Retry-After when it sent one.
func writeCodexUpstreamError(writer http.ResponseWriter, reply *codexUpstreamReply) {
	status, kind := codexErrorKind(reply.status)
	if retry := reply.header.Get("Retry-After"); retry != "" {
		writer.Header().Set("Retry-After", retry)
	}
	writeAPIError(writer, status, kind, "Codex: "+codexErrorMessage(reply))
}

// codexErrorKind maps an upstream status onto the status and Anthropic error
// type the client gets. A status Anthropic has no type for passes as a bad
// gateway.
func codexErrorKind(status int) (int, string) {
	switch {
	case status == http.StatusBadRequest:
		return status, "invalid_request_error"
	case status == http.StatusUnauthorized:
		return status, "authentication_error"
	case status == http.StatusForbidden:
		return status, "permission_error"
	case status == http.StatusNotFound:
		return status, "not_found_error"
	case status == http.StatusRequestEntityTooLarge:
		return status, "request_too_large"
	case status == http.StatusTooManyRequests:
		return status, "rate_limit_error"
	case status == http.StatusServiceUnavailable || status == 529:
		return status, "overloaded_error"
	case status >= 500 && status < 600:
		return status, "api_error"
	case status >= 400 && status < 500:
		return status, "invalid_request_error"
	}
	return http.StatusBadGateway, "api_error"
}

// codexErrorMessage extracts the human-readable reason from a Codex error
// body: error.message, a top-level message or detail, else the body text, else
// the status text.
func codexErrorMessage(reply *codexUpstreamReply) string {
	var doc struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  any             `json:"detail"`
	}
	if json.Unmarshal(reply.body, &doc) == nil {
		var inner struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(doc.Error, &inner) == nil && inner.Message != "" {
			return inner.Message
		}
		var flat string
		if json.Unmarshal(doc.Error, &flat) == nil && flat != "" {
			return flat
		}
		if doc.Message != "" {
			return doc.Message
		}
		if detail, ok := doc.Detail.(string); ok && detail != "" {
			return detail
		}
	}
	if text := strings.TrimSpace(string(reply.body)); text != "" {
		return text
	}
	return fmt.Sprintf("upstream returned %d %s", reply.status, http.StatusText(reply.status))
}

// codexAccountStates renders every pool account as "id=status" for the log.
func codexAccountStates(pool *codex.AccountPool) []string {
	snaps := pool.Snapshots()
	states := make([]string, 0, len(snaps))
	for _, s := range snaps {
		states = append(states, s.ID+"="+s.Status)
	}
	return states
}

func (server *Server) warnCodex(msg string, attrs ...any) {
	if server.logger != nil {
		server.logger.Warn(msg, attrs...)
	}
}
