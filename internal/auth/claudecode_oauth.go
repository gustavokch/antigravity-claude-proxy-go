package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// ClaudeCodeClientID is the official OAuth client ID used by the Claude Code CLI.
	ClaudeCodeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

	// ClaudeCodeAuthorizeURL is the claude.ai OAuth authorization endpoint.
	ClaudeCodeAuthorizeURL = "https://claude.com/cai/oauth/authorize"

	// ClaudeCodeConsoleAuthorizeURL is the platform.claude.com console OAuth endpoint.
	ClaudeCodeConsoleAuthorizeURL = "https://platform.claude.com/oauth/authorize"

	// ClaudeCodeTokenURL is the OAuth token endpoint.
	ClaudeCodeTokenURL = "https://platform.claude.com/v1/oauth/token"

	// ClaudeCodeProfileURL is the endpoint to fetch user profile information.
	ClaudeCodeProfileURL = "https://api.anthropic.com/api/oauth/profile"

	// ClaudeCodeManualCallbackURL is the Anthropic callback page used for manual code copy.
	ClaudeCodeManualCallbackURL = "https://platform.claude.com/oauth/code/callback"

	// ClaudeCodeDefaultScopes are the standard scopes requested by Claude Code CLI.
	ClaudeCodeDefaultScopes = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"

	// DefaultSessionTimeout is the maximum duration an OAuth session stays active.
	DefaultSessionTimeout = 5 * time.Minute
)

// ClaudeCodeTokenResponse represents the OAuth token response from Anthropic.
type ClaudeCodeTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
}

// ClaudeCodeProfile represents the user profile response from Anthropic API.
type ClaudeCodeProfile struct {
	Account struct {
		UUID  string `json:"uuid"`
		Email string `json:"email"`
	} `json:"account"`
	Organization struct {
		UUID string `json:"uuid"`
		Name string `json:"name,omitempty"`
	} `json:"organization"`
}

// ClaudeCodeAccountResult contains the authenticated account information and credentials.
type ClaudeCodeAccountResult struct {
	Email            string    `json:"email"`
	AccountUUID      string    `json:"account_uuid"`
	OrganizationUUID string    `json:"organization_uuid,omitempty"`
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	ExpiresAt        time.Time `json:"expires_at"`
}

// ClaudeCodeAuthSession tracks an in-flight OAuth login attempt.
type ClaudeCodeAuthSession struct {
	ID            string                   `json:"id"`
	State         string                   `json:"state"`
	CodeVerifier  string                   `json:"-"`
	RedirectURI   string                   `json:"redirect_uri"`
	Port          int                      `json:"port,omitempty"`
	AuthURL       string                   `json:"auth_url"`
	ManualAuthURL string                   `json:"manual_auth_url"`
	Status        string                   `json:"status"` // "pending", "completed", "failed", "expired"
	Error         string                   `json:"error,omitempty"`
	Account       *ClaudeCodeAccountResult `json:"account,omitempty"`
	CreatedAt     time.Time                `json:"created_at"`

	server   *http.Server
	listener net.Listener
	doneChan chan struct{}
	mu       sync.Mutex
}

// ClaudeCodeAuthSessionSnapshot represents a thread-safe snapshot of session state.
type ClaudeCodeAuthSessionSnapshot struct {
	ID            string
	State         string
	RedirectURI   string
	Port          int
	AuthURL       string
	ManualAuthURL string
	Status        string
	Error         string
	Account       *ClaudeCodeAccountResult
	CreatedAt     time.Time
}

// Snapshot returns a thread-safe copy of the session's current state.
func (s *ClaudeCodeAuthSession) Snapshot() ClaudeCodeAuthSessionSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	var accCopy *ClaudeCodeAccountResult
	if s.Account != nil {
		acc := *s.Account
		accCopy = &acc
	}

	return ClaudeCodeAuthSessionSnapshot{
		ID:            s.ID,
		State:         s.State,
		RedirectURI:   s.RedirectURI,
		Port:          s.Port,
		AuthURL:       s.AuthURL,
		ManualAuthURL: s.ManualAuthURL,
		Status:        s.Status,
		Error:         s.Error,
		Account:       accCopy,
		CreatedAt:     s.CreatedAt,
	}
}

// ClaudeCodeOAuthManager manages OAuth authentication sessions and token requests.
type ClaudeCodeOAuthManager struct {
	sessions   map[string]*ClaudeCodeAuthSession
	mu         sync.RWMutex
	httpClient *http.Client

	// Configurable endpoints for testing
	authorizeURL        string
	consoleAuthorizeURL string
	tokenURL            string
	profileURL          string
	clientID            string
}

// NewClaudeCodeOAuthManager creates a new OAuth manager.
func NewClaudeCodeOAuthManager() *ClaudeCodeOAuthManager {
	return &ClaudeCodeOAuthManager{
		sessions:            make(map[string]*ClaudeCodeAuthSession),
		httpClient:          &http.Client{Timeout: 30 * time.Second},
		authorizeURL:        ClaudeCodeAuthorizeURL,
		consoleAuthorizeURL: ClaudeCodeConsoleAuthorizeURL,
		tokenURL:            ClaudeCodeTokenURL,
		profileURL:          ClaudeCodeProfileURL,
		clientID:            ClaudeCodeClientID,
	}
}

// SetEndpoints overrides URLs for testing.
func (m *ClaudeCodeOAuthManager) SetEndpoints(authorizeURL, tokenURL, profileURL string, httpClient *http.Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if authorizeURL != "" {
		m.authorizeURL = authorizeURL
	}
	if tokenURL != "" {
		m.tokenURL = tokenURL
	}
	if profileURL != "" {
		m.profileURL = profileURL
	}
	if httpClient != nil {
		m.httpClient = httpClient
	}
}

// generateRandomBytes generates n random cryptographically secure bytes.
func generateRandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("read random bytes: %w", err)
	}
	return b, nil
}

// GenerateClaudeCodePKCE generates a PKCE code_verifier and code_challenge (S256 base64rawURL).
func GenerateClaudeCodePKCE() (verifier, challenge string, err error) {
	bytes, err := generateRandomBytes(32)
	if err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(bytes)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return verifier, challenge, nil
}

// BuildAuthorizeURL builds the claude.ai authorization URL.
func (m *ClaudeCodeOAuthManager) BuildAuthorizeURL(redirectURI, codeChallenge, state string) string {
	params := url.Values{}
	params.Set("code", "true")
	params.Set("client_id", m.clientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", ClaudeCodeDefaultScopes)
	params.Set("code_challenge", codeChallenge)
	params.Set("code_challenge_method", "S256")
	params.Set("state", state)

	return fmt.Sprintf("%s?%s", m.authorizeURL, params.Encode())
}

// StartAuthSession creates a new authentication session with a dynamic loopback server.
func (m *ClaudeCodeOAuthManager) StartAuthSession(mode string) (*ClaudeCodeAuthSession, error) {
	sessionIDBytes, err := generateRandomBytes(16)
	if err != nil {
		return nil, fmt.Errorf("generate session id: %w", err)
	}
	sessionID := hex.EncodeToString(sessionIDBytes)

	stateBytes, err := generateRandomBytes(16)
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}
	state := hex.EncodeToString(stateBytes)

	verifier, challenge, err := GenerateClaudeCodePKCE()
	if err != nil {
		return nil, fmt.Errorf("generate pkce: %w", err)
	}

	session := &ClaudeCodeAuthSession{
		ID:           sessionID,
		State:        state,
		CodeVerifier: verifier,
		Status:       "pending",
		CreatedAt:    time.Now(),
		doneChan:     make(chan struct{}),
	}

	manualAuthURL := m.BuildAuthorizeURL(ClaudeCodeManualCallbackURL, challenge, state)
	session.ManualAuthURL = manualAuthURL

	if mode == "manual" {
		session.RedirectURI = ClaudeCodeManualCallbackURL
		session.AuthURL = manualAuthURL
		m.registerSession(session)
		return session, nil
	}

	// Loopback mode: bind temporary local HTTP listener
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		// Fallback to manual if port bind fails
		session.RedirectURI = ClaudeCodeManualCallbackURL
		session.AuthURL = manualAuthURL
		m.registerSession(session)
		return session, nil
	}

	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://localhost:%d/callback", port)
	authURL := m.BuildAuthorizeURL(redirectURI, challenge, state)

	session.RedirectURI = redirectURI
	session.Port = port
	session.AuthURL = authURL
	session.listener = listener

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		m.handleLoopbackCallback(session, w, r)
	})

	session.server = &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		if err := session.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			session.mu.Lock()
			if session.Status == "pending" {
				session.Status = "failed"
				session.Error = fmt.Sprintf("callback server error: %v", err)
			}
			session.mu.Unlock()
		}
	}()

	// Auto-expire session after timeout
	go func() {
		select {
		case <-time.After(DefaultSessionTimeout):
			m.ExpireSession(session.ID)
		case <-session.doneChan:
		}
	}()

	m.registerSession(session)
	return session, nil
}

func (m *ClaudeCodeOAuthManager) registerSession(session *ClaudeCodeAuthSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Prune stale sessions older than 15 minutes to prevent unbounded memory growth
	now := time.Now()
	for id, s := range m.sessions {
		if s == nil {
			delete(m.sessions, id)
			continue
		}
		s.mu.Lock()
		st := s.Status
		ca := s.CreatedAt
		s.mu.Unlock()
		if now.Sub(ca) > 15*time.Minute || (st != "pending" && now.Sub(ca) > 10*time.Minute) {
			m.closeSessionServer(s)
			delete(m.sessions, id)
		}
	}
	m.sessions[session.ID] = session
}

// PruneExpiredSessions removes sessions older than maxAge.
func (m *ClaudeCodeOAuthManager) PruneExpiredSessions(maxAge time.Duration) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	pruned := 0
	for id, s := range m.sessions {
		if s == nil {
			delete(m.sessions, id)
			pruned++
			continue
		}
		s.mu.Lock()
		ca := s.CreatedAt
		s.mu.Unlock()
		if now.Sub(ca) > maxAge {
			m.closeSessionServer(s)
			delete(m.sessions, id)
			pruned++
		}
	}
	return pruned
}

// GetSession returns an active auth session by ID.
func (m *ClaudeCodeOAuthManager) GetSession(sessionID string) (*ClaudeCodeAuthSession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, exists := m.sessions[sessionID]
	return session, exists
}

// CancelSession cancels and cleans up an active session.
func (m *ClaudeCodeOAuthManager) CancelSession(sessionID string) {
	m.mu.Lock()
	session, exists := m.sessions[sessionID]
	delete(m.sessions, sessionID)
	m.mu.Unlock()

	if !exists || session == nil {
		return
	}

	session.mu.Lock()
	if session.Status == "pending" {
		session.Status = "failed"
		session.Error = "authentication cancelled"
	}
	m.closeSessionServer(session)
	session.mu.Unlock()
}

// ExpireSession marks a session as expired and stops its listener.
func (m *ClaudeCodeOAuthManager) ExpireSession(sessionID string) {
	m.mu.Lock()
	session, exists := m.sessions[sessionID]
	m.mu.Unlock()

	if !exists || session == nil {
		return
	}

	session.mu.Lock()
	if session.Status == "pending" {
		session.Status = "expired"
		session.Error = "authentication session expired"
	}
	m.closeSessionServer(session)
	session.mu.Unlock()
}

func (m *ClaudeCodeOAuthManager) closeSessionServer(session *ClaudeCodeAuthSession) {
	select {
	case <-session.doneChan:
	default:
		close(session.doneChan)
	}

	if session.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = session.server.Shutdown(ctx)
		session.server = nil
	}
}

// handleLoopbackCallback processes the incoming redirect callback on the loopback server.
func (m *ClaudeCodeOAuthManager) handleLoopbackCallback(session *ClaudeCodeAuthSession, w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	code := query.Get("code")
	state := query.Get("state")
	errMsg := query.Get("error")
	errDesc := query.Get("error_description")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if errMsg != "" {
		session.mu.Lock()
		session.Status = "failed"
		session.Error = fmt.Sprintf("OAuth error: %s (%s)", errMsg, errDesc)
		session.mu.Unlock()

		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><title>Authentication Failed</title><style>body{font-family:sans-serif;text-align:center;padding:40px;background:#18181b;color:#f4f4f5;}.error{color:#ef4444;margin-top:20px;}</style></head><body><h2>Authentication Failed</h2><p class="error">` + errMsg + `</p><p>You can close this tab and return to Antigravity Proxy.</p></body></html>`))
		m.closeSessionServer(session)
		return
	}

	if code == "" || state == "" {
		session.mu.Lock()
		session.Status = "failed"
		session.Error = "missing code or state parameter"
		session.mu.Unlock()

		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body><h2>Authentication Failed</h2><p>Missing authorization code or state.</p></body></html>`))
		m.closeSessionServer(session)
		return
	}

	if state != session.State {
		session.mu.Lock()
		session.Status = "failed"
		session.Error = "invalid state parameter (CSRF protection)"
		session.mu.Unlock()

		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><body><h2>Authentication Failed</h2><p>State verification failed.</p></body></html>`))
		m.closeSessionServer(session)
		return
	}

	// Perform token exchange
	tokenResp, profile, err := m.ExchangeToken(code, session.CodeVerifier, session.RedirectURI, session.State)
	if err != nil {
		session.mu.Lock()
		session.Status = "failed"
		session.Error = fmt.Sprintf("token exchange failed: %v", err)
		session.mu.Unlock()

		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><title>Authentication Error</title><style>body{font-family:sans-serif;text-align:center;padding:40px;background:#18181b;color:#f4f4f5;}</style></head><body><h2>Token Exchange Failed</h2><p>` + err.Error() + `</p><p>Please return to Antigravity Proxy and try again.</p></body></html>`))
		m.closeSessionServer(session)
		return
	}

	expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	if tokenResp.ExpiresIn <= 0 {
		expiresAt = time.Now().Add(8 * time.Hour) // fallback default
	}

	accountResult := &ClaudeCodeAccountResult{
		Email:            profile.Account.Email,
		AccountUUID:      profile.Account.UUID,
		OrganizationUUID: profile.Organization.UUID,
		AccessToken:      tokenResp.AccessToken,
		RefreshToken:     tokenResp.RefreshToken,
		ExpiresAt:        expiresAt,
	}

	session.mu.Lock()
	session.Status = "completed"
	session.Account = accountResult
	session.Error = ""
	session.mu.Unlock()

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><title>Authentication Successful</title><style>body{font-family:sans-serif;text-align:center;padding:40px;background:#18181b;color:#f4f4f5;}.success{color:#22c55e;font-size:24px;margin-bottom:12px;}.email{font-weight:bold;color:#60a5fa;}</style></head><body><div class="success">&#10004; Login Successful</div><p>Authenticated as <span class="email">` + profile.Account.Email + `</span></p><p>You can close this tab and return to Antigravity Proxy.</p></body></html>`))

	m.closeSessionServer(session)
}

// CompleteManualAuth exchanges a manually entered authorization code for tokens.
func (m *ClaudeCodeOAuthManager) CompleteManualAuth(sessionID, rawCode string) (*ClaudeCodeAccountResult, error) {
	m.mu.RLock()
	session, exists := m.sessions[sessionID]
	m.mu.RUnlock()

	if !exists || session == nil {
		return nil, errors.New("auth session not found or expired")
	}

	session.mu.Lock()
	if session.Status == "completed" && session.Account != nil {
		acc := session.Account
		session.mu.Unlock()
		return acc, nil
	}
	verifier := session.CodeVerifier
	state := session.State
	session.mu.Unlock()

	code := strings.TrimSpace(rawCode)
	// Claude.ai callback URL format might be code#state or URL-encoded
	if strings.Contains(code, "#") {
		parts := strings.SplitN(code, "#", 2)
		code = parts[0]
	} else if strings.Contains(code, "code=") {
		if parsedURL, err := url.Parse(code); err == nil {
			if extractedCode := parsedURL.Query().Get("code"); extractedCode != "" {
				code = extractedCode
			}
		}
	}

	if code == "" {
		return nil, errors.New("authorization code cannot be empty")
	}

	tokenResp, profile, err := m.ExchangeToken(code, verifier, ClaudeCodeManualCallbackURL, state)
	if err != nil {
		session.mu.Lock()
		session.Status = "failed"
		session.Error = fmt.Sprintf("token exchange failed: %v", err)
		session.mu.Unlock()
		return nil, err
	}

	expiresAt := time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	if tokenResp.ExpiresIn <= 0 {
		expiresAt = time.Now().Add(8 * time.Hour)
	}

	accountResult := &ClaudeCodeAccountResult{
		Email:            profile.Account.Email,
		AccountUUID:      profile.Account.UUID,
		OrganizationUUID: profile.Organization.UUID,
		AccessToken:      tokenResp.AccessToken,
		RefreshToken:     tokenResp.RefreshToken,
		ExpiresAt:        expiresAt,
	}

	session.mu.Lock()
	session.Status = "completed"
	session.Account = accountResult
	session.Error = ""
	session.mu.Unlock()

	m.closeSessionServer(session)

	return accountResult, nil
}

// ExchangeToken performs the OAuth token exchange POST request with Anthropic.
func (m *ClaudeCodeOAuthManager) ExchangeToken(code, codeVerifier, redirectURI, state string) (*ClaudeCodeTokenResponse, *ClaudeCodeProfile, error) {
	reqBody := map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  redirectURI,
		"client_id":     m.clientID,
		"code_verifier": codeVerifier,
	}
	if state != "" {
		reqBody["state"] = state
	}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal token request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, m.tokenURL, strings.NewReader(string(jsonBytes)))
	if err != nil {
		return nil, nil, fmt.Errorf("create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-code/2.1.246")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("execute token request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("token exchange failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	var tokenResp ClaudeCodeTokenResponse
	if err := json.Unmarshal(respBody, &tokenResp); err != nil {
		return nil, nil, fmt.Errorf("parse token response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return nil, nil, errors.New("empty access token in token response")
	}

	// Fetch user profile
	profile, err := m.FetchProfile(tokenResp.AccessToken)
	if err != nil {
		// If profile fetch fails, generate a fallback placeholder
		profile = &ClaudeCodeProfile{}
		profile.Account.Email = "claude-user@claude.ai"
	}

	return &tokenResp, profile, nil
}

// FetchProfile retrieves the user email and account details using the access token.
func (m *ClaudeCodeOAuthManager) FetchProfile(accessToken string) (*ClaudeCodeProfile, error) {
	req, err := http.NewRequest(http.MethodGet, m.profileURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create profile request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-code/2.1.246")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute profile request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read profile response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("profile fetch failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	var profile ClaudeCodeProfile
	if err := json.Unmarshal(respBody, &profile); err != nil {
		return nil, fmt.Errorf("parse profile response: %w", err)
	}

	return &profile, nil
}

// RefreshToken refreshes an expired access token using the refresh token.
func (m *ClaudeCodeOAuthManager) RefreshToken(refreshToken string) (*ClaudeCodeTokenResponse, error) {
	if refreshToken == "" {
		return nil, errors.New("empty refresh token")
	}

	reqBody := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     m.clientID,
		"scope":         ClaudeCodeDefaultScopes,
	}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal refresh request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, m.tokenURL, strings.NewReader(string(jsonBytes)))
	if err != nil {
		return nil, fmt.Errorf("create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "claude-code/2.1.246")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute refresh request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token refresh failed (status %d): %s", resp.StatusCode, string(respBody))
	}

	var tokenResp ClaudeCodeTokenResponse
	if err := json.Unmarshal(respBody, &tokenResp); err != nil {
		return nil, fmt.Errorf("parse refresh response: %w", err)
	}

	return &tokenResp, nil
}
