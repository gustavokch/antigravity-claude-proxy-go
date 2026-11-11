package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	OAuthAuthURL        = "https://accounts.google.com/o/oauth2/v2/auth"
	OAuthTokenURL       = "https://oauth2.googleapis.com/token"
	OAuthUserInfoURL    = "https://www.googleapis.com/oauth2/v1/userinfo"
	DefaultCallbackPort = 51121
)

var OAuthScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

// AccountSaver is implemented by account managers to persist OAuth credentials.
type AccountSaver interface {
	SaveOAuthAccount(email, refreshToken string) error
}

type AccountSaverFunc func(email, refreshToken string) error

func (f AccountSaverFunc) SaveOAuthAccount(email, refreshToken string) error {
	return f(email, refreshToken)
}

// GeneratePKCE creates a PKCE code_verifier and code_challenge (S256).
func GeneratePKCE() (verifier string, challenge string, err error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", "", fmt.Errorf("generate random bytes: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(bytes)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return verifier, challenge, nil
}

// GenerateState creates a random hex string for state validation.
func GenerateState() string {
	bytes := make([]byte, 16)
	_, _ = rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// GetAuthorizationURL builds the Google OAuth authorization URL and returns the
// client whose loopback registration Google accepts for it.
func GetAuthorizationURL(customRedirectURI string) (authURL, verifier, state string, err error) {
	authURL, _, verifier, state, err = prepareSignIn(customRedirectURI)
	return authURL, verifier, state, err
}

func defaultRedirectURI() string {
	return fmt.Sprintf("http://localhost:%d/oauth-callback", DefaultCallbackPort)
}

// probeAuthorizationURL reports whether Google accepts the client and redirect
// named in an authorization URL. It is a variable so a test can answer without
// reaching accounts.google.com.
var probeAuthorizationURL = googleAcceptsAuthorizationURL

// googleAcceptsAuthorizationURL asks the authorization endpoint whether it
// accepts the URL's client and redirect_uri, without following the redirect.
// Google validates both before it authenticates anyone: a registration it
// rejects lands on its own error page, while an accepted one redirects to the
// sign-in page (no session is carried here) or to the client's redirect_uri.
func googleAcceptsAuthorizationURL(authURL string) (accepted bool, reason string, err error) {
	client := &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Get(authURL)
	if err != nil {
		return false, "", fmt.Errorf("ask Google about the redirect registration: %w", err)
	}
	defer response.Body.Close()
	location, err := response.Location()
	if err != nil {
		return false, "", fmt.Errorf("authorization endpoint answered %d without a redirect", response.StatusCode)
	}
	if !strings.Contains(location.Path, "/oauth/error") {
		return true, "", nil
	}
	return false, DecodeAuthError(location.Query().Get("authError")), nil
}

var oauthErrorCode = regexp.MustCompile(`[a-z]+(?:_[a-z]+)+`)

// DecodeAuthError returns the error code Google puts first in an authError
// query parameter, a base64 protobuf whose field 1 is the code
// ("redirect_uri_mismatch", "invalid_client", ...).
func DecodeAuthError(value string) string {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(value, "="))
	if err != nil {
		if raw, err = base64.StdEncoding.DecodeString(value); err != nil {
			return "undecodable authError"
		}
	}
	if len(raw) > 2 && raw[0] == 0x0a && int(raw[1]) <= len(raw)-2 {
		return string(raw[2 : 2+int(raw[1])])
	}
	if code := oauthErrorCode.Find(raw); code != nil {
		return string(code)
	}
	return "unrecognised authError"
}

// prepareSignIn builds the authorization URL for redirectURI along with the
// credentials to redeem its code.
func prepareSignIn(customRedirectURI string) (authURL string, credentials []oauthClientCredentials, verifier, state string, err error) {
	redirectURI := customRedirectURI
	if redirectURI == "" {
		redirectURI = defaultRedirectURI()
	}
	candidates, err := (Manager{}).oauthCredentialCandidates(agyLoginClientID)
	if err != nil {
		return "", nil, "", "", fmt.Errorf("resolve OAuth client credentials: %w", err)
	}
	return signInURL(candidates, redirectURI)
}

// agyCLIClientID is the OAuth client the installed agy signs in with. Cloud
// Code ties an account's entitlement to the client that issued its token: a
// token from this client resolves to the consumer tier with the managed
// "aicode-consumers" project, while another embedded client that also accepts a
// loopback redirect resolves to standard-tier with no project, which leaves the
// account unable to generate. agy 1.5.5's own stored token names this client in
// its `aud` claim, and its loadCodeAssist answer carries the project.
const agyCLIClientID = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"

// preferSignInClient moves the agy CLI client to the front. The credentials of
// an agy login go first already; this covers a machine without one.
func preferSignInClient(candidates []oauthClientCredentials) []oauthClientCredentials {
	for index, candidate := range candidates {
		if candidate.clientID != agyCLIClientID {
			continue
		}
		if index == 0 {
			return candidates
		}
		ordered := make([]oauthClientCredentials, 0, len(candidates))
		ordered = append(ordered, candidate)
		ordered = append(ordered, candidates[:index]...)
		return append(ordered, candidates[index+1:]...)
	}
	return candidates
}

// signInURL picks the first candidate client Google accepts for a loopback
// redirect and builds the authorization URL for it, returning that client's
// credential pairs first so the token exchange runs as the same client.
//
// A client's redirect registration is not knowable locally: agy 1.4.10 added a
// client registered only for https://antigravity.google/oauth-callback, which
// answers a loopback redirect with redirect_uri_mismatch, and ordering by the
// login claim puts that client first when agy's own login issued it. Sign-in
// therefore asks Google once per distinct client; an installed-app client
// accepts any loopback port, so the ephemeral port StartFlow binds is fine.
// Accepting the redirect is necessary but not sufficient: only the agy CLI
// client's tokens reach the consumer tier, so that client goes first.
func signInURL(candidates []oauthClientCredentials, redirectURI string) (authURL string, credentials []oauthClientCredentials, verifier, state string, err error) {
	if len(candidates) == 0 {
		return "", nil, "", "", errors.New("no OAuth client credentials found in the installed agy")
	}
	candidates = preferSignInClient(candidates)
	var reason string
	tried := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if _, seen := tried[candidate.clientID]; seen {
			continue
		}
		tried[candidate.clientID] = struct{}{}

		authURL, verifier, state, err = authorizationURL(candidate.clientID, redirectURI)
		if err != nil {
			return "", nil, "", "", err
		}
		accepted, why, err := probeAuthorizationURL(authURL)
		if err != nil {
			return "", nil, "", "", err
		}
		if accepted {
			return authURL, credentialsForClient(candidates, candidate.clientID), verifier, state, nil
		}
		reason = why
	}
	if len(tried) == 1 {
		for clientID := range tried {
			prefix, _, _ := strings.Cut(clientID, "-")
			return "", nil, "", "", fmt.Errorf("OAuth client %s-… rejects the loopback redirect %s (%s); register that redirect for the client", prefix, redirectURI, reason)
		}
	}
	return "", nil, "", "", fmt.Errorf("none of the %d embedded agy OAuth clients accepts the loopback redirect %s (last answer: %s)", len(tried), redirectURI, reason)
}

// credentialsForClient returns the pairs of the client that authorized, keeping
// their tried-secret order.
func credentialsForClient(candidates []oauthClientCredentials, clientID string) []oauthClientCredentials {
	pairs := make([]oauthClientCredentials, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.clientID == clientID {
			pairs = append(pairs, candidate)
		}
	}
	return pairs
}

func authorizationURL(clientID, redirectURI string) (authURL, verifier, state string, err error) {
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		return "", "", "", err
	}
	state = GenerateState()

	params := url.Values{}
	params.Set("client_id", clientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("response_type", "code")
	params.Set("scope", strings.Join(OAuthScopes, " "))
	params.Set("access_type", "offline")
	params.Set("prompt", "consent")
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("state", state)

	return fmt.Sprintf("%s?%s", OAuthAuthURL, params.Encode()), verifier, state, nil
}

// PendingFlow tracks active OAuth session.
type PendingFlow struct {
	Verifier    string
	State       string
	Port        int
	RedirectURI string
	Credentials []oauthClientCredentials
	Server      *http.Server
	CreatedAt   time.Time
}

// AuthResult holds the returned tokens and user identity from OAuth flow.
type AuthResult struct {
	Email        string
	RefreshToken string
	AccessToken  string
}

// OAuthManager manages browser-based OAuth flows and callback listeners.
type OAuthManager struct {
	mu           sync.Mutex
	accountSaver AccountSaver
	flows        map[string]*PendingFlow
}

// NewOAuthManager creates a new OAuthManager.
func NewOAuthManager(accountSaver AccountSaver) *OAuthManager {
	return &OAuthManager{
		accountSaver: accountSaver,
		flows:        make(map[string]*PendingFlow),
	}
}

// StartFlow starts an ephemeral callback server and returns the authorization URL.
func (om *OAuthManager) StartFlow() (string, string, error) {
	// Bind an ephemeral port: an installed-app client accepts any loopback
	// port, and a fixed one collides with a running proxy that already has a
	// flow open (its own listener), which used to push sign-in onto a port the
	// OAuth client did not register.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", fmt.Errorf("bind OAuth callback listener: %w", err)
	}
	boundPort := listener.Addr().(*net.TCPAddr).Port

	redirectURI := fmt.Sprintf("http://localhost:%d/oauth-callback", boundPort)
	authURL, credentials, verifier, state, err := prepareSignIn(redirectURI)
	if err != nil {
		_ = listener.Close()
		return "", "", err
	}
	om.mu.Lock()
	defer om.mu.Unlock()

	// Clean up any existing active flows and close any open listeners
	for state, f := range om.flows {
		if f.Server != nil {
			_ = f.Server.Close()
		}
		delete(om.flows, state)
	}

	mux := http.NewServeMux()
	srv := &http.Server{Handler: mux}

	flow := &PendingFlow{
		Verifier:    verifier,
		State:       state,
		Port:        boundPort,
		RedirectURI: redirectURI,
		Credentials: credentials,
		Server:      srv,
		CreatedAt:   time.Now(),
	}
	om.flows[state] = flow

	mux.HandleFunc("/oauth-callback", om.callbackHandler(srv))

	go func() {
		_ = srv.Serve(listener)
	}()

	return authURL, state, nil
}

// callbackHandler serves the loopback redirect. It is a method rather than a
// closure inside StartFlow so a test can drive it without binding one of the
// fixed callback ports.
func (om *OAuthManager) callbackHandler(srv *http.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := q.Get("code")
		cbState := q.Get("state")
		errParam := q.Get("error")

		if errParam != "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			// errParam comes from the redirect query string, so it is caller
			// data: escaped, or an error page carrying error=<script>… runs the
			// script on the loopback origin.
			fmt.Fprintf(w, "<html><body><h1>❌ Authentication Failed</h1><p>%s</p></body></html>", template.HTMLEscapeString(errParam))
			go func() { _ = srv.Close() }()
			return
		}

		om.mu.Lock()
		activeFlow, exists := om.flows[cbState]
		if !exists {
			om.mu.Unlock()
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "<html><body><h1>❌ State Mismatch</h1><p>CSRF check failed.</p></body></html>")
			go func() { _ = srv.Close() }()
			return
		}
		flowVerifier := activeFlow.Verifier
		flowRedirectURI := activeFlow.RedirectURI
		flowCredentials := activeFlow.Credentials
		if flowRedirectURI == "" && activeFlow.Port > 0 {
			flowRedirectURI = fmt.Sprintf("http://localhost:%d/oauth-callback", activeFlow.Port)
		}
		delete(om.flows, cbState)
		om.mu.Unlock()

		if code == "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, "<html><body><h1>❌ Missing Code</h1></body></html>")
			go func() { _ = srv.Close() }()
			return
		}

		// Complete flow in background
		go func() {
			defer func() { _ = srv.Close() }()
			_, _ = om.completeFlow(code, flowVerifier, flowRedirectURI, flowCredentials)
		}()

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "<html><body style=\"font-family:system-ui;text-align:center;padding:40px;\"><h1 style=\"color:#28a745;\">✅ Authentication Successful!</h1><p>You can close this window.</p><script>setTimeout(()=>window.close(),2000);</script></body></html>")
	}
}

// completeFlow exchanges code for tokens, gets user info, and saves account. It
// redeems as credentials[0]'s client, the one the authorization URL named.
func (om *OAuthManager) completeFlow(code, verifier, redirectURI string, credentials []oauthClientCredentials) (*AuthResult, error) {
	if len(credentials) == 0 {
		return nil, errors.New("complete OAuth flow without client credentials")
	}

	if redirectURI == "" {
		redirectURI = defaultRedirectURI()
	}

	tokenRes, err := redeemAuthorizationCode(OAuthTokenURL, credentials, code, verifier, redirectURI)
	if err != nil {
		return nil, err
	}

	// Fetch user email
	email, err := FetchUserEmail(tokenRes.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("fetch user email: %w", err)
	}

	result := &AuthResult{
		Email:        email,
		RefreshToken: tokenRes.RefreshToken,
		AccessToken:  tokenRes.AccessToken,
	}

	if om.accountSaver != nil {
		if err := om.accountSaver.SaveOAuthAccount(email, tokenRes.RefreshToken); err != nil {
			return nil, fmt.Errorf("save account: %w", err)
		}
	}

	return result, nil
}

type authorizationTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// redeemAuthorizationCode exchanges code for the client the authorization URL
// named, candidates[0].clientID. Only that client's secret is unknown, so each
// of its pairs is tried until Google stops answering invalid_client. Google
// authenticates the client before it redeems the code, so a rejected pair
// leaves the code usable for the next one.
func redeemAuthorizationCode(tokenURL string, candidates []oauthClientCredentials, code, verifier, redirectURI string) (authorizationTokens, error) {
	var err error
	for _, credentials := range candidates {
		if credentials.clientID != candidates[0].clientID {
			break
		}
		var tokens authorizationTokens
		var invalidClient bool
		tokens, invalidClient, err = redeemWithCredentials(tokenURL, credentials, code, verifier, redirectURI)
		if !invalidClient {
			return tokens, err
		}
	}
	return authorizationTokens{}, err
}

func redeemWithCredentials(tokenURL string, credentials oauthClientCredentials, code, verifier, redirectURI string) (authorizationTokens, bool, error) {
	form := url.Values{}
	form.Set("client_id", credentials.clientID)
	form.Set("client_secret", credentials.clientSecret)
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", redirectURI)

	resp, err := http.PostForm(tokenURL, form)
	if err != nil {
		return authorizationTokens{}, false, fmt.Errorf("token exchange request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		invalidClient := resp.StatusCode == http.StatusUnauthorized && strings.Contains(string(body), "invalid_client")
		return authorizationTokens{}, invalidClient, fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, string(body))
	}

	var tokens authorizationTokens
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return authorizationTokens{}, false, fmt.Errorf("decode token response: %w", err)
	}
	if tokens.AccessToken == "" {
		return authorizationTokens{}, false, errors.New("no access token received")
	}
	return tokens, false, nil
}

// FetchUserEmail queries Google userinfo endpoint for email address.
func FetchUserEmail(accessToken string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, OAuthUserInfoURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("userinfo request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("userinfo failed (%d): %s", resp.StatusCode, string(body))
	}

	var info struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("decode userinfo response: %w", err)
	}
	if info.Email == "" {
		return "", errors.New("no email returned in userinfo response")
	}
	return info.Email, nil
}

// ServeHTTP implements http.Handler for WebUI /api/auth/url and /api/auth/complete.
func (om *OAuthManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.URL.Path == "/api/auth/url" && r.Method == http.MethodGet {
		authURL, state, err := om.StartFlow()
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"url":    authURL,
			"state":  state,
		})
		return
	}

	if r.URL.Path == "/api/auth/complete" && r.Method == http.MethodPost {
		var body struct {
			CallbackInput string `json:"callbackInput"`
			State         string `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.CallbackInput == "" || body.State == "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "error": "Missing callbackInput or state"})
			return
		}

		om.mu.Lock()
		flow, exists := om.flows[body.State]
		if !exists {
			om.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "error": "OAuth flow expired or not found"})
			return
		}
		delete(om.flows, body.State)
		if flow.Server != nil {
			_ = flow.Server.Close()
		}
		om.mu.Unlock()

		code := body.CallbackInput
		if strings.Contains(code, "code=") {
			u, err := url.Parse(code)
			if err == nil {
				if parsedCode := u.Query().Get("code"); parsedCode != "" {
					code = parsedCode
				}
			}
		}

		flowRedirectURI := flow.RedirectURI
		if flowRedirectURI == "" && flow.Port > 0 {
			flowRedirectURI = fmt.Sprintf("http://localhost:%d/oauth-callback", flow.Port)
		}
		acc, err := om.completeFlow(code, flow.Verifier, flowRedirectURI, flow.Credentials)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "error": err.Error()})
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":  "ok",
			"email":   acc.Email,
			"message": fmt.Sprintf("Account %s added successfully", acc.Email),
		})
		return
	}

	w.WriteHeader(http.StatusNotFound)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "error", "error": "Not found"})
}

// OpenBrowser opens the URL in the user's default browser.
func OpenBrowser(url string) error {
	var cmd string
	var args []string

	switch runtime.GOOS {
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start", url}
	case "darwin":
		cmd = "open"
		args = []string{url}
	default: // "linux", "freebsd", "openbsd", "netbsd"
		cmd = "xdg-open"
		args = []string{url}
	}
	return exec.Command(cmd, args...).Start()
}
