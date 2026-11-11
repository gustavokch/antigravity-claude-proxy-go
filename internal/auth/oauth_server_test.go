package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// stubProbe answers the Google registration probe without reaching the network.
func stubProbe(t *testing.T, probe func(authURL string) (bool, string, error)) {
	t.Helper()
	previous := probeAuthorizationURL
	probeAuthorizationURL = probe
	t.Cleanup(func() { probeAuthorizationURL = previous })
}

func TestPKCEAndState(t *testing.T) {
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE error: %v", err)
	}
	if len(verifier) == 0 || len(challenge) == 0 {
		t.Fatalf("empty verifier or challenge")
	}
	state := GenerateState()
	if len(state) != 32 {
		t.Fatalf("expected 32 hex chars for state, got %d", len(state))
	}
}

func TestGetAuthorizationURL(t *testing.T) {
	t.Setenv(oauthClientIDEnv, "pinned-client.apps.googleusercontent.com")
	t.Setenv(oauthClientSecretEnv, "pinned-secret")
	stubProbe(t, func(string) (bool, string, error) { return true, "", nil })

	authURL, verifier, state, err := GetAuthorizationURL("")
	if err != nil {
		t.Fatalf("GetAuthorizationURL error: %v", err)
	}
	if !strings.HasPrefix(authURL, OAuthAuthURL) {
		t.Errorf("unexpected auth URL prefix: %s", authURL)
	}
	if !strings.Contains(authURL, "code_challenge=") || !strings.Contains(authURL, "state="+state) {
		t.Errorf("auth URL missing parameters: %s", authURL)
	}
	if !strings.Contains(authURL, "client_id=pinned-client.apps.googleusercontent.com") {
		t.Errorf("auth URL did not use the configured client: %s", authURL)
	}
	if len(verifier) == 0 {
		t.Errorf("empty verifier")
	}
}

// A client registered only for a hosted callback answers a loopback redirect
// with redirect_uri_mismatch, which is what a browser showed for sign-in.
func TestSignInURLSkipsClientsThatRejectLoopback(t *testing.T) {
	candidates := []oauthClientCredentials{
		{clientID: "hosted", clientSecret: "hosted-secret"},
		{clientID: "loopback", clientSecret: "wrong"},
		{clientID: "loopback", clientSecret: "right"},
	}
	var probed []string
	stubProbe(t, func(authURL string) (bool, string, error) {
		parsed, err := url.Parse(authURL)
		if err != nil {
			t.Fatalf("parse probed URL: %v", err)
		}
		clientID := parsed.Query().Get("client_id")
		probed = append(probed, clientID)
		if clientID == "hosted" {
			return false, "redirect_uri_mismatch", nil
		}
		return true, "", nil
	})

	redirectURI := "http://localhost:54321/oauth-callback"
	authURL, credentials, verifier, state, err := signInURL(candidates, redirectURI)
	if err != nil {
		t.Fatalf("signInURL error: %v", err)
	}
	if !strings.Contains(authURL, "client_id=loopback&") {
		t.Errorf("auth URL did not move to the loopback client: %s", authURL)
	}
	if !strings.Contains(authURL, "redirect_uri="+url.QueryEscape(redirectURI)) {
		t.Errorf("auth URL kept a different redirect: %s", authURL)
	}
	if strings.Join(probed, ",") != "hosted,loopback" {
		t.Errorf("probed %v, want each distinct client once in order", probed)
	}
	if len(credentials) != 2 || credentials[0].clientID != "loopback" || credentials[1].clientSecret != "right" {
		t.Errorf("credentials = %+v, want the loopback client's pairs in order", credentials)
	}
	if verifier == "" || state == "" {
		t.Errorf("empty verifier or state")
	}
}

// Accepting the loopback redirect is not enough: only the agy CLI client's
// tokens reach the consumer tier, so sign-in picks it before an earlier client.
func TestSignInURLPrefersTheEntitledCLIClient(t *testing.T) {
	candidates := []oauthClientCredentials{
		{clientID: "decoy", clientSecret: "decoy-secret"},
		{clientID: agyCLIClientID, clientSecret: "wrong"},
		{clientID: agyCLIClientID, clientSecret: "right"},
	}
	var probed []string
	stubProbe(t, func(authURL string) (bool, string, error) {
		parsed, err := url.Parse(authURL)
		if err != nil {
			t.Fatalf("parse probed URL: %v", err)
		}
		probed = append(probed, parsed.Query().Get("client_id"))
		return true, "", nil
	})

	authURL, credentials, _, _, err := signInURL(candidates, "http://localhost:54321/oauth-callback")
	if err != nil {
		t.Fatalf("signInURL error: %v", err)
	}
	if !strings.Contains(authURL, "client_id="+url.QueryEscape(agyCLIClientID)+"&") {
		t.Errorf("auth URL did not use the agy CLI client: %s", authURL)
	}
	if strings.Join(probed, ",") != agyCLIClientID {
		t.Errorf("probed %v, want only the agy CLI client", probed)
	}
	if len(credentials) != 2 || credentials[0].clientSecret != "wrong" || credentials[1].clientSecret != "right" {
		t.Errorf("credentials = %+v, want the CLI client's pairs in order", credentials)
	}
}

func TestOAuthManager_HTTPHandler(t *testing.T) {
	t.Setenv(oauthClientIDEnv, "pinned-client.apps.googleusercontent.com")
	t.Setenv(oauthClientSecretEnv, "pinned-secret")
	stubProbe(t, func(string) (bool, string, error) { return true, "", nil })

	saver := AccountSaverFunc(func(email, refreshToken string) error {
		return nil
	})
	om := NewOAuthManager(saver)

	t.Run("GET /api/auth/url", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/auth/url", nil)
		rec := httptest.NewRecorder()
		om.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var res struct {
			Status string `json:"status"`
			URL    string `json:"url"`
			State  string `json:"state"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if res.Status != "ok" || res.URL == "" || res.State == "" {
			t.Errorf("invalid auth url response: %+v", res)
		}
	})

	t.Run("POST /api/auth/complete with invalid state", func(t *testing.T) {
		body := map[string]string{
			"callbackInput": "4/code123",
			"state":         "non-existent-state",
		}
		data, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/complete", bytes.NewReader(data))
		rec := httptest.NewRecorder()
		om.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("Multiple StartFlow calls replace the active flow", func(t *testing.T) {
		url1, state1, err := om.StartFlow()
		if err != nil {
			t.Fatalf("first StartFlow failed: %v", err)
		}
		if url1 == "" || state1 == "" {
			t.Fatal("empty url1 or state1")
		}

		// Immediately start a second flow
		url2, state2, err := om.StartFlow()
		if err != nil {
			t.Fatalf("second StartFlow failed: %v", err)
		}
		if url2 == "" || state2 == "" {
			t.Fatal("empty url2 or state2")
		}
		if state1 == state2 {
			t.Fatal("expected different states for consecutive flows")
		}

		om.mu.Lock()
		_, oldExists := om.flows[state1]
		newFlow, newExists := om.flows[state2]
		om.mu.Unlock()

		if oldExists {
			t.Error("old flow was not cleaned up when starting new flow")
		}
		if !newExists || newFlow == nil {
			t.Error("new flow missing in active flows registry")
		} else if newFlow.RedirectURI == "" {
			t.Error("new flow missing RedirectURI")
		} else if len(newFlow.Credentials) == 0 || newFlow.Credentials[0].clientID != "pinned-client.apps.googleusercontent.com" {
			t.Errorf("new flow did not keep its sign-in credentials: %+v", newFlow.Credentials)
		}
	})

	t.Run("completeFlow with custom redirect URI", func(t *testing.T) {
		// An invalid code should fail gracefully after redeeming against redirectURI.
		credentials := []oauthClientCredentials{{clientID: "client", clientSecret: "secret"}}
		_, err := om.completeFlow("invalid_code", "verifier", "http://localhost:51125/oauth-callback", credentials)
		if err == nil {
			t.Error("expected error for invalid_code, got nil")
		}
	})
}

func TestRedeemAuthorizationCodeTriesOnlyTheAuthorizedClientsSecrets(t *testing.T) {
	candidates := []oauthClientCredentials{
		{clientID: "login", clientSecret: "wrong"},
		{clientID: "login", clientSecret: "right"},
		{clientID: "other", clientSecret: "right"},
	}
	var attempts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		attempts = append(attempts, r.Form.Get("client_id")+"/"+r.Form.Get("client_secret"))
		if r.Form.Get("client_id") != "login" || r.Form.Get("client_secret") != "right" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"access","refresh_token":"refresh"}`))
	}))
	defer server.Close()

	t.Run("finds the secret", func(t *testing.T) {
		attempts = nil
		tokens, err := redeemAuthorizationCode(server.URL, candidates, "code", "verifier", "http://localhost:51121/oauth-callback")
		if err != nil {
			t.Fatal(err)
		}
		if tokens.RefreshToken != "refresh" || strings.Join(attempts, ",") != "login/wrong,login/right" {
			t.Fatalf("tokens=%+v attempts=%v", tokens, attempts)
		}
	})

	t.Run("never switches client", func(t *testing.T) {
		attempts = nil
		withoutRightSecret := []oauthClientCredentials{candidates[0], candidates[2]}
		_, err := redeemAuthorizationCode(server.URL, withoutRightSecret, "code", "verifier", "http://localhost:51121/oauth-callback")
		if err == nil || !strings.Contains(err.Error(), "invalid_client") {
			t.Fatalf("err = %v", err)
		}
		if strings.Join(attempts, ",") != "login/wrong" {
			t.Fatalf("attempts=%v", attempts)
		}
	})
}
