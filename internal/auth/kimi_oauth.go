package auth

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// KimiCodeClientID is the OAuth client ID used by kimi-code (shared across regions).
	KimiCodeClientID = "17e5f671-d194-4dfb-9706-5516cb48c098"

	// KimiCodeOAuthHost is the global (overseas) Kimi OAuth host.
	KimiCodeOAuthHost = "https://auth.kimi.ai"

	// KimiCodeBaseURL is the global Kimi Code API base (Anthropic-compatible).
	KimiCodeBaseURL = "https://api.kimi.ai/coding/v1"
)

// ErrKimiOAuthUnauthorized means the stored Kimi OAuth credential was rejected
// and the user must log in again.
var ErrKimiOAuthUnauthorized = errors.New("kimi oauth unauthorized")

// KimiDeviceAuthorization is the RFC 8628 device authorization response.
type KimiDeviceAuthorization struct {
	UserCode                string `json:"user_code"`
	DeviceCode              string `json:"device_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// KimiTokenInfo is a parsed OAuth token response with an absolute expiry.
type KimiTokenInfo struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Scope        string    `json:"scope"`
	TokenType    string    `json:"token_type"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// KimiUserInfo is the /me profile (snake_case upstream).
type KimiUserInfo struct {
	UserID   string `json:"user_id"`
	Email    string `json:"email"`
	Nickname string `json:"nickname"`
}

// KimiAuthSession tracks an in-flight device-flow login attempt.
type KimiAuthSession struct {
	ID        string                  `json:"id"`
	OAuthHost string                  `json:"oauth_host"`
	BaseURL   string                  `json:"base_url"`
	Status    string                  `json:"status"` // pending|completed|expired|denied|cancelled|error
	Error     string                  `json:"error,omitempty"`
	Device    KimiDeviceAuthorization `json:"device"`
	Token     *KimiTokenInfo          `json:"token,omitempty"`
	User      *KimiUserInfo           `json:"user,omitempty"`
	CreatedAt time.Time               `json:"created_at"`

	registered bool
	cancel     context.CancelFunc
	mu         sync.Mutex
}

// KimiAuthSessionSnapshot is a thread-safe copy of session state.
type KimiAuthSessionSnapshot struct {
	ID        string
	OAuthHost string
	BaseURL   string
	Status    string
	Error     string
	Device    KimiDeviceAuthorization
	Token     *KimiTokenInfo
	User      *KimiUserInfo
	CreatedAt time.Time
}

// Snapshot returns a thread-safe copy of the session's current state.
func (s *KimiAuthSession) Snapshot() KimiAuthSessionSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	var tokenCopy *KimiTokenInfo
	if s.Token != nil {
		tok := *s.Token
		tokenCopy = &tok
	}
	var userCopy *KimiUserInfo
	if s.User != nil {
		u := *s.User
		userCopy = &u
	}

	return KimiAuthSessionSnapshot{
		ID:        s.ID,
		OAuthHost: s.OAuthHost,
		BaseURL:   s.BaseURL,
		Status:    s.Status,
		Error:     s.Error,
		Device:    s.Device,
		Token:     tokenCopy,
		User:      userCopy,
		CreatedAt: s.CreatedAt,
	}
}

// ClaimCompletion marks the session's completed token as consumed. It returns
// true once per completion (again only after ReleaseCompletion), so later
// status polls cannot re-save a stale token over one refreshed since.
func (s *KimiAuthSession) ClaimCompletion() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Status == "completed" && s.Token != nil && !s.registered {
		s.registered = true
		return true
	}
	return false
}

// ReleaseCompletion undoes a ClaimCompletion whose save failed, so a later
// status poll can retry persisting the token.
func (s *KimiAuthSession) ReleaseCompletion() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registered = false
}

// finish sets the terminal status while the session is still pending.
func (s *KimiAuthSession) finish(status, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Status == "pending" {
		s.Status = status
		s.Error = errMsg
	}
}

// KimiOAuthManager manages Kimi Code device-flow sessions and token requests.
type KimiOAuthManager struct {
	mu         sync.RWMutex
	sessions   map[string]*KimiAuthSession
	httpClient *http.Client
	identity   func() http.Header
	clientID   string
	oauthHost  string
	baseURL    string
	sleep      func(context.Context, time.Duration) error
}

// NewKimiOAuthManager creates a manager. identity supplies KimiCLI fingerprint
// headers; nil means no identity headers are added.
func NewKimiOAuthManager(identity func() http.Header) *KimiOAuthManager {
	return &KimiOAuthManager{
		sessions:   make(map[string]*KimiAuthSession),
		httpClient: &http.Client{Timeout: 30 * time.Second},
		identity:   identity,
		clientID:   KimiCodeClientID,
		oauthHost:  KimiCodeOAuthHost,
		baseURL:    KimiCodeBaseURL,
		sleep:      sleepCtx,
	}
}

// SetEndpoints overrides endpoints for testing. Empty or nil arguments keep
// the current value.
func (m *KimiOAuthManager) SetEndpoints(oauthHost, baseURL string, httpClient *http.Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if oauthHost != "" {
		m.oauthHost = oauthHost
	}
	if baseURL != "" {
		m.baseURL = baseURL
	}
	if httpClient != nil {
		m.httpClient = httpClient
	}
}

// SetSleep overrides the sleep function used between polls/retries.
func (m *KimiOAuthManager) SetSleep(fn func(context.Context, time.Duration) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fn != nil {
		m.sleep = fn
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// postForm POSTs a form and decodes the JSON object body. Non-JSON or
// non-object bodies yield an empty map; the status is returned either way.
func (m *KimiOAuthManager) postForm(ctx context.Context, endpoint string, form url.Values) (int, map[string]any, error) {
	m.mu.RLock()
	httpClient := m.httpClient
	identity := m.identity
	m.mu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, fmt.Errorf("OAuth request to %s failed: %w", endpoint, err)
	}
	if identity != nil {
		for k, vs := range identity() {
			if len(vs) > 0 {
				req.Header.Set(k, vs[0])
			}
		}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("OAuth request to %s failed: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, nil
	}
	data := map[string]any{}
	if len(body) > 0 {
		var decoded any
		if err := json.Unmarshal(body, &decoded); err == nil {
			if obj, ok := decoded.(map[string]any); ok {
				data = obj
			}
		}
	}
	return resp.StatusCode, data, nil
}

// kimiErrorDetail extracts the most descriptive error string from an OAuth
// error response.
func kimiErrorDetail(data map[string]any) string {
	for _, key := range []string{"error_description", "message"} {
		if s, ok := data[key].(string); ok && s != "" {
			return s
		}
	}
	errVal := data["error"]
	if s, ok := errVal.(string); ok && s != "" {
		return s
	}
	if obj, ok := errVal.(map[string]any); ok {
		if s, ok := obj["message"].(string); ok && s != "" {
			return s
		}
	}
	return "unknown"
}

// tokenFromResponse validates and parses an OAuth token response.
func tokenFromResponse(data map[string]any, now time.Time) (*KimiTokenInfo, error) {
	access, _ := data["access_token"].(string)
	if access == "" {
		return nil, errors.New("OAuth response missing access_token")
	}
	refresh, _ := data["refresh_token"].(string)
	if refresh == "" {
		return nil, errors.New("OAuth response missing refresh_token")
	}
	var expiresIn float64
	switch v := data["expires_in"].(type) {
	case float64:
		expiresIn = v
	case string:
		if parsed, err := strconv.ParseFloat(v, 64); err == nil {
			expiresIn = parsed
		}
	}
	if math.IsNaN(expiresIn) || math.IsInf(expiresIn, 0) || expiresIn <= 0 {
		return nil, errors.New("OAuth response missing or invalid expires_in")
	}
	scope, _ := data["scope"].(string)
	tokenType, _ := data["token_type"].(string)
	if tokenType == "" {
		tokenType = "Bearer"
	}
	return &KimiTokenInfo{
		AccessToken:  access,
		RefreshToken: refresh,
		Scope:        scope,
		TokenType:    tokenType,
		ExpiresAt:    now.Add(time.Duration(expiresIn * float64(time.Second))),
	}, nil
}

// StartDeviceAuth begins a device authorization flow and starts polling in the
// background. The returned session is registered in the manager.
func (m *KimiOAuthManager) StartDeviceAuth(ctx context.Context) (*KimiAuthSession, error) {
	m.mu.RLock()
	oauthHost := m.oauthHost
	baseURL := m.baseURL
	m.mu.RUnlock()

	status, data, err := m.postForm(ctx, oauthHost+"/api/oauth/device_authorization", url.Values{
		"client_id": {m.currentClientID()},
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("Device authorization failed (HTTP %d): %s", status, kimiErrorDetail(data))
	}

	userCode, _ := data["user_code"].(string)
	if userCode == "" {
		return nil, errors.New("Device authorization response missing user_code")
	}
	deviceCode, _ := data["device_code"].(string)
	if deviceCode == "" {
		return nil, errors.New("Device authorization response missing device_code")
	}
	verificationURI, _ := data["verification_uri"].(string)
	verificationURIComplete, _ := data["verification_uri_complete"].(string)
	if verificationURIComplete == "" && verificationURI == "" {
		return nil, errors.New("Device authorization response missing verification_uri_complete")
	}
	// The UI binds these to an href; only https may reach it.
	for _, uri := range []string{verificationURI, verificationURIComplete} {
		if uri == "" {
			continue
		}
		if u, err := url.Parse(uri); err != nil || u.Scheme != "https" {
			return nil, fmt.Errorf("Device authorization response has a non-https verification URI: %q", uri)
		}
	}
	interval := 5
	if v, ok := data["interval"].(float64); ok && v > 0 {
		interval = int(v)
	}
	expiresIn := 900
	if v, ok := data["expires_in"].(float64); ok && v > 0 {
		expiresIn = int(v)
	}

	idBytes, err := generateRandomBytes(16)
	if err != nil {
		return nil, fmt.Errorf("generate session id: %w", err)
	}
	session := &KimiAuthSession{
		ID:        hex.EncodeToString(idBytes),
		OAuthHost: oauthHost,
		BaseURL:   baseURL,
		Status:    "pending",
		CreatedAt: time.Now(),
		Device: KimiDeviceAuthorization{
			UserCode:                userCode,
			DeviceCode:              deviceCode,
			VerificationURI:         verificationURI,
			VerificationURIComplete: verificationURIComplete,
			ExpiresIn:               expiresIn,
			Interval:                interval,
		},
	}

	pollCtx, cancel := context.WithCancel(context.Background())
	session.cancel = cancel // before registerSession publishes the session
	m.registerSession(session)
	go m.pollDeviceGrant(pollCtx, session)
	return session, nil
}

// registerSession stores session as the only login in flight. A new device
// flow supersedes every earlier one: its poller is cancelled and its state,
// including any unclaimed token, is dropped.
func (m *KimiOAuthManager) registerSession(session *KimiAuthSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		s.mu.Lock()
		if s.cancel != nil {
			s.cancel()
		}
		if s.Status == "pending" {
			s.Status = "cancelled"
			s.Error = "superseded by a newer login"
		}
		s.mu.Unlock()
		delete(m.sessions, id)
	}
	m.sessions[session.ID] = session
}

// pollDeviceGrant polls the token endpoint until the flow resolves.
func (m *KimiOAuthManager) pollDeviceGrant(ctx context.Context, s *KimiAuthSession) {
	m.mu.RLock()
	oauthHost := m.oauthHost
	identity := m.identity
	sleep := m.sleep
	m.mu.RUnlock()

	interval := time.Duration(s.Device.Interval) * time.Second
	deadline := s.CreatedAt.Add(time.Duration(s.Device.ExpiresIn) * time.Second)

	for {
		if time.Now().After(deadline) {
			s.finish("expired", "Device flow timed out")
			return
		}

		if err := sleep(ctx, interval); err != nil {
			s.finish("cancelled", "authentication cancelled")
			return
		}

		status, data, err := m.postForm(ctx, oauthHost+"/api/oauth/token", url.Values{
			"client_id":   {m.currentClientID()},
			"device_code": {s.Device.DeviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		})
		if err != nil {
			if ctx.Err() != nil {
				s.finish("cancelled", "authentication cancelled")
			} else {
				s.finish("error", err.Error())
			}
			return
		}

		if status == http.StatusOK {
			if _, ok := data["access_token"].(string); ok {
				token, terr := tokenFromResponse(data, time.Now())
				if terr != nil {
					s.finish("error", terr.Error())
					return
				}
				userCtx, userCancel := context.WithTimeout(ctx, 10*time.Second)
				user, _ := m.fetchUserInfo(userCtx, s.BaseURL, token.AccessToken, identity)
				userCancel()

				s.mu.Lock()
				s.Token = token
				s.User = user
				s.Status = "completed"
				s.mu.Unlock()
				return
			}
		}

		if status >= 500 {
			s.finish("error", fmt.Sprintf("Device token polling server error (HTTP %d): %s", status, kimiErrorDetail(data)))
			return
		}

		switch data["error"] {
		case "authorization_pending":
			// keep polling
		case "slow_down":
			interval += 5 * time.Second
		case "expired_token":
			s.finish("expired", "device code expired")
			return
		case "access_denied":
			detail := kimiErrorDetail(data)
			if detail == "unknown" || detail == "access_denied" {
				detail = "access denied"
			}
			s.finish("denied", detail)
			return
		default:
			s.finish("error", fmt.Sprintf("Device token polling failed (HTTP %d): %s", status, kimiErrorDetail(data)))
			return
		}
	}
}

// GetSession returns an active auth session by ID.
func (m *KimiOAuthManager) GetSession(sessionID string) (*KimiAuthSession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, exists := m.sessions[sessionID]
	return session, exists
}

// CancelSession cancels and removes an active session.
func (m *KimiOAuthManager) CancelSession(sessionID string) {
	m.mu.Lock()
	session, exists := m.sessions[sessionID]
	delete(m.sessions, sessionID)
	m.mu.Unlock()

	if !exists || session == nil {
		return
	}

	session.mu.Lock()
	if session.cancel != nil {
		session.cancel()
	}
	if session.Status == "pending" {
		session.Status = "cancelled"
		session.Error = "authentication cancelled"
	}
	session.mu.Unlock()
}

// RefreshToken exchanges a refresh token for a new token set, retrying
// transient failures.
func (m *KimiOAuthManager) RefreshToken(ctx context.Context, refreshToken, oauthHost string) (*KimiTokenInfo, error) {
	if refreshToken == "" {
		return nil, fmt.Errorf("%w: missing refresh token", ErrKimiOAuthUnauthorized)
	}
	if oauthHost == "" {
		m.mu.RLock()
		oauthHost = m.oauthHost
		m.mu.RUnlock()
	}

	backoffs := []time.Duration{time.Second, 2 * time.Second}
	m.mu.RLock()
	sleep := m.sleep
	m.mu.RUnlock()
	var lastErr error
	for attempt := range 3 {
		status, data, err := m.postForm(ctx, oauthHost+"/api/oauth/token", url.Values{
			"client_id":     {m.currentClientID()},
			"grant_type":    {"refresh_token"},
			"refresh_token": {refreshToken},
		})
		if err == nil {
			if status == http.StatusOK {
				if _, ok := data["access_token"].(string); ok {
					return tokenFromResponse(data, time.Now())
				}
				lastErr = fmt.Errorf("Token refresh failed (HTTP %d): %s", status, kimiErrorDetail(data))
			} else if status == http.StatusUnauthorized || status == http.StatusForbidden || data["error"] == "invalid_grant" {
				return nil, fmt.Errorf("%w: %s", ErrKimiOAuthUnauthorized, kimiErrorDetail(data))
			} else if isRetryableStatus(status) {
				lastErr = fmt.Errorf("Token refresh failed (HTTP %d): %s", status, kimiErrorDetail(data))
			} else {
				return nil, fmt.Errorf("Token refresh failed (HTTP %d): %s", status, kimiErrorDetail(data))
			}
		} else {
			lastErr = err
		}

		if attempt < 2 {
			if serr := sleep(ctx, backoffs[attempt]); serr != nil {
				return nil, serr
			}
		}
	}
	return nil, lastErr
}

func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func (m *KimiOAuthManager) currentClientID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.clientID
}

// FetchUserInfo retrieves the Kimi user profile for an access token.
func (m *KimiOAuthManager) FetchUserInfo(ctx context.Context, baseURL, accessToken string) (*KimiUserInfo, error) {
	m.mu.RLock()
	identity := m.identity
	m.mu.RUnlock()
	return m.fetchUserInfo(ctx, baseURL, accessToken, identity)
}

func (m *KimiOAuthManager) fetchUserInfo(ctx context.Context, baseURL, accessToken string, identity func() http.Header) (*KimiUserInfo, error) {
	m.mu.RLock()
	httpClient := m.httpClient
	m.mu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/me", nil)
	if err != nil {
		return nil, err
	}
	if identity != nil {
		for k, vs := range identity() {
			if len(vs) > 0 {
				req.Header.Set(k, vs[0])
			}
		}
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Kimi userinfo returned %d", resp.StatusCode)
	}
	var info struct {
		UserID   string `json:"user_id"`
		Email    string `json:"email"`
		Nickname string `json:"nickname"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return nil, err
	}
	if info.UserID == "" {
		return nil, errors.New("malformed Kimi userinfo response")
	}
	return &KimiUserInfo{UserID: info.UserID, Email: info.Email, Nickname: info.Nickname}, nil
}
