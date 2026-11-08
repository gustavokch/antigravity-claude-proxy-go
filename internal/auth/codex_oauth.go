package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"antigravity-go-proxy/internal/transport"
)

const (
	// CodexClientID is the public OAuth client ID of the Codex CLI.
	CodexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	// CodexAuthorizeURL is the OpenAI OAuth authorization endpoint.
	CodexAuthorizeURL = "https://auth.openai.com/oauth/authorize"

	// CodexTokenURL is the OpenAI OAuth token endpoint.
	CodexTokenURL = "https://auth.openai.com/oauth/token"

	// CodexRedirectURI is the only redirect URI registered for the Codex client,
	// so both login modes send it, and the token exchange must repeat it.
	CodexRedirectURI = "http://localhost:1455/auth/callback"

	// CodexScopes are the scopes requested at login; offline_access yields the
	// refresh token.
	CodexScopes = "openid email profile offline_access"

	// codexCallbackAddr is where the loopback listener binds. The port is fixed
	// by CodexRedirectURI.
	codexCallbackAddr = "127.0.0.1:1455"
	codexCallbackPath = "/auth/callback"

	// codexRefreshScope is the scope the Codex CLI sends on refresh; it omits
	// offline_access, which only the authorization request needs.
	codexRefreshScope = "openid profile email"

	// codexDefaultPlanType is the plan reported when the id_token carries none.
	codexDefaultPlanType = "free"

	codexHTTPTimeout = 30 * time.Second

	// codexSessionRetention bounds how long finished sessions stay readable
	// before a new login prunes them.
	codexSessionRetention = 15 * time.Minute
)

// ErrCodexGrantInvalid means the token endpoint refused the refresh token for
// good (revoked, reused, expired, or answered 401). Retrying cannot help: the
// account must log in again.
var ErrCodexGrantInvalid = errors.New("codex refresh token rejected")

// codexTerminalGrantErrors are the OAuth error codes after which a refresh
// token never works again. OpenAI sends them both as the RFC 6749 "error"
// string and as error.code in its own error envelope, so they are matched in
// the body text rather than in one field.
var codexTerminalGrantErrors = []string{
	"invalid_grant",
	"refresh_token_reused",
	"refresh_token_expired",
	"refresh_token_invalidated",
	"token_revoked",
}

// CodexAccountResult holds the credentials and identity of a ChatGPT login or
// refresh.
type CodexAccountResult struct {
	AccessToken, RefreshToken, IDToken string
	ExpiresAt                          time.Time
	Email, AccountID, PlanType         string
}

// CodexAuthSession tracks an in-flight Codex login. The exported fields are set
// before StartAuthSession returns and never change afterwards; the rest is read
// through Snapshot.
type CodexAuthSession struct {
	ID        string
	Mode      string // "loopback" or "manual"
	AuthURL   string
	CreatedAt time.Time

	state        string
	codeVerifier string

	mu         sync.Mutex
	status     string // "pending", "completed", "failed", "expired", "cancelled"
	errMsg     string
	account    *CodexAccountResult
	exchanging bool
	listener   net.Listener
	server     *http.Server
	done       chan struct{}
}

// CodexAuthSessionSnapshot is a copy of a session's state for status polls.
// It carries no credentials.
type CodexAuthSessionSnapshot struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Mode      string    `json:"mode"`
	AuthURL   string    `json:"auth_url"`
	Error     string    `json:"error,omitempty"`
	Email     string    `json:"email,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Snapshot returns a thread-safe copy of the session's current state.
func (s *CodexAuthSession) Snapshot() CodexAuthSessionSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := CodexAuthSessionSnapshot{
		ID:        s.ID,
		Status:    s.status,
		Mode:      s.Mode,
		AuthURL:   s.AuthURL,
		Error:     s.errMsg,
		CreatedAt: s.CreatedAt,
	}
	if s.account != nil {
		snap.Email = s.account.Email
	}
	return snap
}

// finish moves a pending session to a terminal status and releases it. A
// session that already ended keeps its status. The caller holds s.mu.
func (s *CodexAuthSession) finish(status, errMsg string) {
	if s.status == "pending" {
		s.status = status
		s.errMsg = errMsg
	}
	s.release()
}

// complete records a successful token exchange. It wins over a cancel or an
// expiry that landed while the exchange ran: the tokens exist and the code
// cannot be redeemed again. The caller holds s.mu.
func (s *CodexAuthSession) complete(result *CodexAccountResult) {
	acc := *result
	s.account = &acc
	s.status = "completed"
	s.errMsg = ""
	s.exchanging = false
	s.release()
}

// claim reserves the session for one token exchange, so a loopback callback and
// a manual completion cannot both redeem a code. The caller holds s.mu.
func (s *CodexAuthSession) claim() error {
	if s.status != "pending" {
		return fmt.Errorf("auth session is %s", s.status)
	}
	if s.exchanging {
		return errors.New("auth session is already being completed")
	}
	s.exchanging = true
	return nil
}

// release stops the expiry timer and the callback server. The listener closes
// here, so the fixed port is free when release returns; the server shutdown
// runs in the background because release is also called from the callback
// handler, whose own request Shutdown would otherwise wait for. The caller
// holds s.mu.
func (s *CodexAuthSession) release() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	if s.listener != nil {
		_ = s.listener.Close()
		s.listener = nil
	}
	if srv := s.server; srv != nil {
		s.server = nil
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}()
	}
}

// CodexOAuthManager manages ChatGPT login sessions and Codex token requests.
type CodexOAuthManager struct {
	mu           sync.RWMutex
	sessions     map[string]*CodexAuthSession
	httpClient   *http.Client
	authorizeURL string
	tokenURL     string
	onComplete   func(*CodexAccountResult)
}

// NewCodexOAuthManager creates a manager. The token endpoint never redirects
// a valid request, and a followed redirect would turn the form POST into a GET
// elsewhere, so redirects are returned as responses and fail the call.
func NewCodexOAuthManager() *CodexOAuthManager {
	return &CodexOAuthManager{
		sessions: make(map[string]*CodexAuthSession),
		httpClient: &http.Client{
			Timeout:   codexHTTPTimeout,
			Transport: transport.Upstream(),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		authorizeURL: CodexAuthorizeURL,
		tokenURL:     CodexTokenURL,
	}
}

// SetEndpoints overrides URLs for testing. Empty arguments keep the current
// value.
func (m *CodexOAuthManager) SetEndpoints(authorizeURL, tokenURL string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if authorizeURL != "" {
		m.authorizeURL = authorizeURL
	}
	if tokenURL != "" {
		m.tokenURL = tokenURL
	}
}

// OnComplete sets the hook called with the account of each session completed
// through the loopback callback, before its status turns "completed", so a
// status poll that sees "completed" also finds the account registered. Manual
// completions do not call it: CompleteManualAuth returns the account to its
// caller instead.
func (m *CodexOAuthManager) OnComplete(fn func(*CodexAccountResult)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onComplete = fn
}

// generateCodexPKCE returns a PKCE verifier of 96 random bytes in base64url
// (128 characters, the RFC 7636 maximum, as the Codex CLI sends) and its S256
// challenge.
func generateCodexPKCE() (verifier, challenge string, err error) {
	raw, err := generateRandomBytes(96)
	if err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// buildAuthorizeURL builds the auth.openai.com authorization URL with the
// parameters the Codex CLI sends.
func (m *CodexOAuthManager) buildAuthorizeURL(codeChallenge, state string) string {
	m.mu.RLock()
	authorizeURL := m.authorizeURL
	m.mu.RUnlock()

	params := url.Values{
		"client_id":                  {CodexClientID},
		"response_type":              {"code"},
		"redirect_uri":               {CodexRedirectURI},
		"scope":                      {CodexScopes},
		"state":                      {state},
		"code_challenge":             {codeChallenge},
		"code_challenge_method":      {"S256"},
		"prompt":                     {"login"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}
	return authorizeURL + "?" + params.Encode()
}

// StartAuthSession starts a login. mode "manual" waits for CompleteManualAuth;
// any other value is loopback mode, which also listens on 127.0.0.1:1455 for
// the browser redirect. When that port is taken by another process the session
// falls back to manual mode. Both modes use the same authorization URL, so a
// loopback session can still be completed manually.
func (m *CodexOAuthManager) StartAuthSession(mode string) (*CodexAuthSession, error) {
	sessionIDBytes, err := generateRandomBytes(16)
	if err != nil {
		return nil, fmt.Errorf("generate session id: %w", err)
	}
	stateBytes, err := generateRandomBytes(16)
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}
	verifier, challenge, err := generateCodexPKCE()
	if err != nil {
		return nil, fmt.Errorf("generate pkce: %w", err)
	}

	state := hex.EncodeToString(stateBytes)
	session := &CodexAuthSession{
		ID:           hex.EncodeToString(sessionIDBytes),
		Mode:         "manual",
		AuthURL:      m.buildAuthorizeURL(challenge, state),
		CreatedAt:    time.Now(),
		state:        state,
		codeVerifier: verifier,
		status:       "pending",
		done:         make(chan struct{}),
	}

	if mode != "manual" {
		m.startLoopback(session)
	}

	m.registerSession(session)
	go m.expireAfter(session, DefaultSessionTimeout)

	slog.Info("Codex OAuth auth session started", "session_id", session.ID, "mode", session.Mode)
	return session, nil
}

// startLoopback binds the callback listener and switches the session to
// loopback mode. A bind failure leaves the session in manual mode.
func (m *CodexOAuthManager) startLoopback(session *CodexAuthSession) {
	// The port is fixed, so an earlier login still waiting on it would push
	// every new one into manual mode until it expires. The newest login is the
	// one the user is looking at.
	m.releaseCallbackPort()

	listener, err := net.Listen("tcp", codexCallbackAddr)
	if err != nil {
		slog.Warn("failed to bind Codex OAuth callback listener, falling back to manual", "addr", codexCallbackAddr, "error", err)
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc(codexCallbackPath, func(w http.ResponseWriter, r *http.Request) {
		m.handleLoopbackCallback(session, w, r)
	})
	srv := &http.Server{
		Handler:     mux,
		ReadTimeout: 10 * time.Second,
		// The handler runs the token exchange before it answers.
		WriteTimeout: codexHTTPTimeout + 10*time.Second,
	}

	session.Mode = "loopback"
	session.listener = listener
	session.server = srv

	go func() {
		err := srv.Serve(listener)
		if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			return
		}
		slog.Error("Codex OAuth loopback server error", "session_id", session.ID, "error", err)
		session.mu.Lock()
		session.finish("failed", fmt.Sprintf("callback server error: %v", err))
		session.mu.Unlock()
	}()
}

// releaseCallbackPort cancels the pending sessions that hold the callback
// listener.
func (m *CodexOAuthManager) releaseCallbackPort() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		s.mu.Lock()
		if s.listener != nil {
			slog.Info("Codex OAuth session superseded by a newer login", "session_id", s.ID)
			s.finish("cancelled", "superseded by a newer login")
		}
		s.mu.Unlock()
	}
}

// expireAfter ends the session once timeout passes, unless it ended earlier.
func (m *CodexOAuthManager) expireAfter(session *CodexAuthSession, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-timer.C:
		session.mu.Lock()
		session.finish("expired", "authentication session expired")
		session.mu.Unlock()
	case <-session.done:
	}
}

func (m *CodexOAuthManager) registerSession(session *CodexAuthSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(codexSessionRetention)
	m.sessions[session.ID] = session
}

// PruneExpiredSessions removes sessions older than maxAge and stops any of
// them still listening.
func (m *CodexOAuthManager) PruneExpiredSessions(maxAge time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(maxAge)
}

// pruneLocked is PruneExpiredSessions with m.mu held.
func (m *CodexOAuthManager) pruneLocked(maxAge time.Duration) {
	now := time.Now()
	for id, s := range m.sessions {
		if now.Sub(s.CreatedAt) <= maxAge {
			continue
		}
		s.mu.Lock()
		s.finish("expired", "authentication session expired")
		s.mu.Unlock()
		delete(m.sessions, id)
	}
}

// GetSession returns a snapshot of the session with the given ID.
func (m *CodexOAuthManager) GetSession(id string) (CodexAuthSessionSnapshot, bool) {
	session, ok := m.session(id)
	if !ok {
		return CodexAuthSessionSnapshot{}, false
	}
	return session.Snapshot(), true
}

func (m *CodexOAuthManager) session(id string) (*CodexAuthSession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, ok := m.sessions[id]
	return session, ok
}

// CancelSession ends a pending session as "cancelled" and frees its callback
// port. The session stays readable, so a status poll sees the cancellation.
func (m *CodexOAuthManager) CancelSession(id string) error {
	session, ok := m.session(id)
	if !ok {
		return errors.New("auth session not found or expired")
	}
	session.mu.Lock()
	session.finish("cancelled", "authentication cancelled")
	session.mu.Unlock()
	return nil
}

// handleLoopbackCallback processes the browser redirect on the loopback
// listener.
func (m *CodexOAuthManager) handleLoopbackCallback(session *CodexAuthSession, w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	code := query.Get("code")
	state := query.Get("state")

	if errParam := query.Get("error"); errParam != "" {
		msg := "OAuth error: " + errParam
		if desc := query.Get("error_description"); desc != "" {
			msg += " (" + desc + ")"
		}
		session.mu.Lock()
		session.finish("failed", msg)
		session.mu.Unlock()
		writeCodexCallbackPage(w, http.StatusBadRequest, "Authentication Failed", errParam)
		return
	}

	if code == "" || state == "" {
		slog.Warn("Codex OAuth loopback callback rejected: missing code or state", "session_id", session.ID, "has_code", code != "", "has_state", state != "")
		session.mu.Lock()
		session.finish("failed", "missing code or state parameter")
		session.mu.Unlock()
		writeCodexCallbackPage(w, http.StatusBadRequest, "Authentication Failed", "Missing authorization code or state.")
		return
	}

	session.mu.Lock()
	if state != session.state {
		slog.Warn("Codex OAuth loopback callback rejected: state mismatch", "session_id", session.ID)
		session.finish("failed", "invalid state parameter (CSRF protection)")
		session.mu.Unlock()
		writeCodexCallbackPage(w, http.StatusBadRequest, "Authentication Failed", "State verification failed.")
		return
	}
	if err := session.claim(); err != nil {
		session.mu.Unlock()
		writeCodexCallbackPage(w, http.StatusConflict, "Authentication Not Completed", err.Error())
		return
	}
	verifier := session.codeVerifier
	session.mu.Unlock()

	slog.Info("Codex OAuth loopback callback received", "session_id", session.ID)

	result, err := m.exchangeCode(context.Background(), code, verifier)
	if err != nil {
		slog.Error("Codex OAuth loopback token exchange failed", "session_id", session.ID, "error", err)
		session.mu.Lock()
		session.exchanging = false
		session.finish("failed", fmt.Sprintf("token exchange failed: %v", err))
		session.mu.Unlock()
		writeCodexCallbackPage(w, http.StatusInternalServerError, "Token Exchange Failed", err.Error())
		return
	}

	m.mu.RLock()
	hook := m.onComplete
	m.mu.RUnlock()
	if hook != nil {
		acc := *result
		hook(&acc)
	}

	session.mu.Lock()
	session.complete(result)
	session.mu.Unlock()

	slog.Info("Codex OAuth loopback authentication successful", "session_id", session.ID, "email", result.Email, "plan_type", result.PlanType)
	writeCodexCallbackPage(w, http.StatusOK, "Login Successful", "Authenticated as "+result.Email)
}

// writeCodexCallbackPage renders the page the browser shows after the
// redirect. detail is escaped: the error parameter arrives on the query string.
func writeCodexCallbackPage(w http.ResponseWriter, status int, heading, detail string) {
	color := "#ef4444"
	if status == http.StatusOK {
		color = "#22c55e"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><head><title>%[1]s</title><style>body{font-family:sans-serif;text-align:center;padding:40px;background:#18181b;color:#f4f4f5;}h2{color:%[2]s;}</style></head><body><h2>%[1]s</h2><p>%[3]s</p><p>You can close this tab and return to Antigravity Proxy.</p></body></html>`,
		template.HTMLEscapeString(heading), color, template.HTMLEscapeString(detail))
}

// CompleteManualAuth finishes a login from what the user pasted: the URL the
// browser was redirected to (its state must match the session) or the bare
// authorization code. A failed exchange leaves the session pending, so a
// mistyped paste can be corrected.
func (m *CodexOAuthManager) CompleteManualAuth(sessionID, input string) (*CodexAccountResult, error) {
	session, ok := m.session(sessionID)
	if !ok {
		return nil, errors.New("auth session not found or expired")
	}

	session.mu.Lock()
	if session.status == "completed" && session.account != nil {
		acc := *session.account
		session.mu.Unlock()
		return &acc, nil
	}
	session.mu.Unlock()

	code, state, err := parseCodexCallbackInput(input)
	if err != nil {
		return nil, err
	}

	session.mu.Lock()
	if state != "" && state != session.state {
		session.mu.Unlock()
		return nil, errors.New("state mismatch: the URL belongs to a different login attempt")
	}
	if err := session.claim(); err != nil {
		session.mu.Unlock()
		return nil, err
	}
	verifier := session.codeVerifier
	session.mu.Unlock()

	result, err := m.exchangeCode(context.Background(), code, verifier)
	if err != nil {
		slog.Error("Codex OAuth manual token exchange failed", "session_id", sessionID, "error", err)
		session.mu.Lock()
		session.exchanging = false
		session.mu.Unlock()
		return nil, err
	}

	session.mu.Lock()
	session.complete(result)
	session.mu.Unlock()

	slog.Info("Codex OAuth manual authentication successful", "session_id", sessionID, "email", result.Email, "plan_type", result.PlanType)
	return result, nil
}

// parseCodexCallbackInput extracts the code from a pasted redirect URL, its
// query string, or a bare code. state is empty only for a bare code; a URL
// without one is rejected, since its state is what ties it to the session.
func parseCodexCallbackInput(input string) (code, state string, err error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", "", errors.New("authorization code or redirect URL cannot be empty")
	}

	var rawQuery string
	switch {
	case strings.Contains(s, "?"):
		u, err := url.Parse(s)
		if err != nil {
			return "", "", fmt.Errorf("parse redirect URL: %w", err)
		}
		rawQuery = u.RawQuery
	case strings.Contains(s, "code=") || strings.Contains(s, "error="):
		rawQuery = s
	default:
		return s, "", nil
	}

	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", "", fmt.Errorf("parse redirect URL query: %w", err)
	}
	if errParam := values.Get("error"); errParam != "" {
		if desc := values.Get("error_description"); desc != "" {
			return "", "", fmt.Errorf("OAuth error: %s (%s)", errParam, desc)
		}
		return "", "", fmt.Errorf("OAuth error: %s", errParam)
	}
	code = values.Get("code")
	if code == "" {
		return "", "", errors.New("redirect URL has no code parameter")
	}
	state = values.Get("state")
	if state == "" {
		return "", "", errors.New("redirect URL has no state parameter")
	}
	return code, state, nil
}

// exchangeCode redeems an authorization code.
func (m *CodexOAuthManager) exchangeCode(ctx context.Context, code, verifier string) (*CodexAccountResult, error) {
	status, body, err := m.postTokenForm(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {CodexClientID},
		"code":          {code},
		"redirect_uri":  {CodexRedirectURI},
		"code_verifier": {verifier},
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed (status %d): %s", status, strings.TrimSpace(string(body)))
	}
	return codexResultFromTokenResponse(body, time.Now())
}

// RefreshToken exchanges a refresh token for a new token set. OpenAI rotates
// refresh tokens on every use, so the result's RefreshToken replaces the old
// one. Errors after which the account must log in again wrap
// ErrCodexGrantInvalid.
func (m *CodexOAuthManager) RefreshToken(ctx context.Context, refreshToken string) (*CodexAccountResult, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("%w: missing refresh token", ErrCodexGrantInvalid)
	}
	status, body, err := m.postTokenForm(ctx, url.Values{
		"client_id":     {CodexClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {codexRefreshScope},
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		detail := strings.TrimSpace(string(body))
		if codexGrantGone(status, body) {
			return nil, fmt.Errorf("%w: token refresh failed (status %d): %s", ErrCodexGrantInvalid, status, detail)
		}
		return nil, fmt.Errorf("token refresh failed (status %d): %s", status, detail)
	}
	return codexResultFromTokenResponse(body, time.Now())
}

// codexGrantGone reports whether a failed refresh reply means the refresh
// token is dead. Only 4xx replies count: a 5xx carries no verdict on the grant.
func codexGrantGone(status int, body []byte) bool {
	if status == http.StatusUnauthorized {
		return true
	}
	if status < 400 || status >= 500 {
		return false
	}
	lower := strings.ToLower(string(body))
	for _, code := range codexTerminalGrantErrors {
		if strings.Contains(lower, code) {
			return true
		}
	}
	return false
}

// postTokenForm POSTs a form to the token endpoint and returns the status and
// body.
func (m *CodexOAuthManager) postTokenForm(ctx context.Context, form url.Values) (int, []byte, error) {
	m.mu.RLock()
	tokenURL := m.tokenURL
	httpClient := m.httpClient
	m.mu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, fmt.Errorf("create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("execute token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read token response: %w", err)
	}
	return resp.StatusCode, body, nil
}

// codexResultFromTokenResponse validates a token endpoint reply. Like the
// Codex CLI it requires an access token, a refresh token and a positive
// expires_in; a reply missing one is not a usable credential.
func codexResultFromTokenResponse(body []byte, now time.Time) (*CodexAccountResult, error) {
	var resp struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken string  `json:"refresh_token"`
		IDToken      string  `json:"id_token"`
		ExpiresIn    float64 `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse token response: %w", err)
	}
	if strings.TrimSpace(resp.AccessToken) == "" {
		return nil, errors.New("token response missing access_token")
	}
	if strings.TrimSpace(resp.RefreshToken) == "" {
		return nil, errors.New("token response missing refresh_token")
	}
	if resp.ExpiresIn <= 0 {
		return nil, errors.New("token response missing or invalid expires_in")
	}

	email, accountID, planType := ParseCodexIDToken(resp.IDToken)
	return &CodexAccountResult{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		IDToken:      resp.IDToken,
		ExpiresAt:    codexTokenExpiry(resp.AccessToken, resp.ExpiresIn, now),
		Email:        email,
		AccountID:    accountID,
		PlanType:     planType,
	}, nil
}

// codexTokenExpiry prefers the access token's own exp claim, which is what
// the backend enforces, over expires_in, which is relative to a response that
// may have been in flight for a while.
func codexTokenExpiry(accessToken string, expiresIn float64, now time.Time) time.Time {
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if decodeJWTClaims(accessToken, &claims) && claims.Exp > 0 {
		return time.Unix(int64(claims.Exp), 0)
	}
	return now.Add(time.Duration(expiresIn * float64(time.Second)))
}

// ParseCodexIDToken reads the account claims from a ChatGPT id_token without
// verifying it; the token comes straight from the token endpoint over TLS.
// planType defaults to "free" when the claim is missing. A token that does
// not decode yields three empty strings, so callers keep what they had.
func ParseCodexIDToken(idToken string) (email, accountID, planType string) {
	var claims struct {
		Email string `json:"email"`
		Auth  struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
			ChatGPTPlanType  string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if !decodeJWTClaims(idToken, &claims) {
		return "", "", ""
	}
	planType = strings.TrimSpace(claims.Auth.ChatGPTPlanType)
	if planType == "" {
		planType = codexDefaultPlanType
	}
	return claims.Email, claims.Auth.ChatGPTAccountID, planType
}

// decodeJWTClaims decodes a JWT payload into claims and reports success.
func decodeJWTClaims(token string, claims any) bool {
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(segments[1], "="))
	if err != nil {
		return false
	}
	return json.Unmarshal(payload, claims) == nil
}
