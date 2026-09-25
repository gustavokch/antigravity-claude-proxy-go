package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubSleep records every requested duration and returns ctx.Err(), which is
// nil while the context lives — so tests never actually wait.
func stubSleep(record *[]time.Duration) func(context.Context, time.Duration) error {
	var mu sync.Mutex
	return func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		*record = append(*record, d)
		mu.Unlock()
		return ctx.Err()
	}
}

func waitForKimiSnapshot(t *testing.T, s *KimiAuthSession, wantStatus string, timeout time.Duration) KimiAuthSessionSnapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		snap := s.Snapshot()
		if snap.Status == wantStatus {
			return snap
		}
		if snap.Status != "pending" {
			t.Fatalf("session reached status %q (%q), waiting for %q", snap.Status, snap.Error, wantStatus)
		}
		time.Sleep(5 * time.Millisecond)
	}
	snap := s.Snapshot()
	t.Fatalf("timed out waiting for status %q; last status %q (%q)", wantStatus, snap.Status, snap.Error)
	return snap
}

// newKimiFake starts a fake auth.kimi.ai host. tokenResponses is consumed one
// per token POST once exhausted, the last response repeats.
func newKimiFake(t *testing.T, deviceJSON string, tokenResponses ...func() (int, string)) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var tokenPosts atomic.Int64
	idx := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/oauth/device_authorization":
			_, _ = w.Write([]byte(deviceJSON))
		case "/api/oauth/token":
			tokenPosts.Add(1)
			mu.Lock()
			i := idx
			if idx < len(tokenResponses)-1 {
				idx++
			}
			mu.Unlock()
			status, body := tokenResponses[i]()
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		case "/coding/v1/me":
			_, _ = w.Write([]byte(`{"user_id":"u1","email":"dev@example.com","nickname":"dev"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &tokenPosts
}

func newKimiTestManager(t *testing.T, srv *httptest.Server, sleeps *[]time.Duration) *KimiOAuthManager {
	t.Helper()
	mgr := NewKimiOAuthManager(nil)
	mgr.SetEndpoints(srv.URL, srv.URL+"/coding/v1", srv.Client())
	mgr.SetSleep(stubSleep(sleeps))
	return mgr
}

const kimiFakeDeviceJSON = `{
	"device_code": "dc-1",
	"user_code": "ABCD-1234",
	"verification_uri": "https://www.kimi.ai/code/authorize_device",
	"verification_uri_complete": "https://www.kimi.ai/code/authorize_device?user_code=ABCD-1234",
	"expires_in": 1800,
	"interval": 5
}`

func TestKimiDeviceFlowPendingThenSuccess(t *testing.T) {
	var sleeps []time.Duration
	srv, tokenPosts := newKimiFake(t, kimiFakeDeviceJSON,
		func() (int, string) {
			return 400, `{"error":"authorization_pending","error_description":"Authorization is pending"}`
		},
		func() (int, string) { return 400, `{"error":"authorization_pending"}` },
		func() (int, string) {
			return 200, `{"access_token":"at-1","refresh_token":"rt-1","expires_in":3600,"scope":"coding","token_type":"Bearer"}`
		},
	)
	mgr := newKimiTestManager(t, srv, &sleeps)

	session, err := mgr.StartDeviceAuth(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceAuth: %v", err)
	}
	snap := waitForKimiSnapshot(t, session, "completed", 2*time.Second)

	if got := tokenPosts.Load(); got != 3 {
		t.Errorf("token POSTs = %d, want 3", got)
	}
	if !reflect.DeepEqual(sleeps, []time.Duration{5 * time.Second, 5 * time.Second, 5 * time.Second}) {
		t.Errorf("intervals = %v, want [5s 5s 5s]", sleeps)
	}
	if snap.Device.UserCode != "ABCD-1234" {
		t.Errorf("UserCode = %q", snap.Device.UserCode)
	}
	if snap.Device.VerificationURIComplete != "https://www.kimi.ai/code/authorize_device?user_code=ABCD-1234" {
		t.Errorf("VerificationURIComplete = %q", snap.Device.VerificationURIComplete)
	}
	if snap.Token == nil || snap.Token.AccessToken != "at-1" || snap.Token.RefreshToken != "rt-1" {
		t.Fatalf("Token = %+v", snap.Token)
	}
	if snap.Token.TokenType != "Bearer" || snap.Token.Scope != "coding" {
		t.Errorf("TokenType/Scope = %q/%q", snap.Token.TokenType, snap.Token.Scope)
	}
	if until := time.Until(snap.Token.ExpiresAt); until <= 3500*time.Second || until > 3600*time.Second {
		t.Errorf("ExpiresAt in %v, want ~3600s", until)
	}
	if snap.User == nil || snap.User.Email != "dev@example.com" || snap.User.UserID != "u1" {
		t.Errorf("User = %+v", snap.User)
	}
}

func TestKimiDeviceFlowSlowDown(t *testing.T) {
	var sleeps []time.Duration
	srv, _ := newKimiFake(t, kimiFakeDeviceJSON,
		func() (int, string) { return 400, `{"error":"slow_down"}` },
		func() (int, string) { return 200, `{"access_token":"at-1","refresh_token":"rt-1","expires_in":3600}` },
	)
	mgr := newKimiTestManager(t, srv, &sleeps)

	session, err := mgr.StartDeviceAuth(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceAuth: %v", err)
	}
	waitForKimiSnapshot(t, session, "completed", 2*time.Second)
	if !reflect.DeepEqual(sleeps, []time.Duration{5 * time.Second, 10 * time.Second}) {
		t.Errorf("intervals = %v, want [5s 10s]", sleeps)
	}
}

func TestKimiDeviceFlowExpiredToken(t *testing.T) {
	var sleeps []time.Duration
	srv, _ := newKimiFake(t, kimiFakeDeviceJSON,
		func() (int, string) { return 400, `{"error":"expired_token"}` },
	)
	mgr := newKimiTestManager(t, srv, &sleeps)

	session, err := mgr.StartDeviceAuth(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceAuth: %v", err)
	}
	waitForKimiSnapshot(t, session, "expired", 2*time.Second)
}

func TestKimiDeviceFlowAccessDenied(t *testing.T) {
	var sleeps []time.Duration
	srv, _ := newKimiFake(t, kimiFakeDeviceJSON,
		func() (int, string) { return 400, `{"error":"access_denied","error_description":"user rejected"}` },
	)
	mgr := newKimiTestManager(t, srv, &sleeps)

	session, err := mgr.StartDeviceAuth(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceAuth: %v", err)
	}
	snap := waitForKimiSnapshot(t, session, "denied", 2*time.Second)
	if snap.Error != "user rejected" {
		t.Errorf("Error = %q, want error_description", snap.Error)
	}
}

func TestKimiCancelSessionStopsPolling(t *testing.T) {
	var sleeps []time.Duration
	srv, tokenPosts := newKimiFake(t, kimiFakeDeviceJSON,
		func() (int, string) { return 400, `{"error":"authorization_pending"}` },
	)
	mgr := newKimiTestManager(t, srv, &sleeps)

	session, err := mgr.StartDeviceAuth(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceAuth: %v", err)
	}
	mgr.CancelSession(session.ID)

	snap := waitForKimiSnapshot(t, session, "cancelled", 2*time.Second)
	if snap.Error != "authentication cancelled" {
		t.Errorf("Error = %q", snap.Error)
	}
	if _, exists := mgr.GetSession(session.ID); exists {
		t.Error("GetSession returned true after cancel")
	}
	count := tokenPosts.Load()
	time.Sleep(50 * time.Millisecond)
	if got := tokenPosts.Load(); got != count {
		t.Errorf("token POSTs kept growing after cancel: %d -> %d", count, got)
	}
}

func TestKimiStartDeviceAuthMissingUserCode(t *testing.T) {
	var sleeps []time.Duration
	srv, _ := newKimiFake(t, `{"device_code":"dc-1","expires_in":1800,"interval":5}`)
	mgr := newKimiTestManager(t, srv, &sleeps)

	session, err := mgr.StartDeviceAuth(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if session != nil {
		t.Error("expected nil session")
	}
}

func TestKimiRefreshRetryThenSuccess(t *testing.T) {
	var sleeps []time.Duration
	srv, tokenPosts := newKimiFake(t, kimiFakeDeviceJSON,
		func() (int, string) { return 500, `{"error":"server_error"}` },
		func() (int, string) { return 200, `{"access_token":"at-2","refresh_token":"rt-2","expires_in":1800}` },
	)
	mgr := newKimiTestManager(t, srv, &sleeps)

	tok, err := mgr.RefreshToken(context.Background(), "rt-old", srv.URL)
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if tok.AccessToken != "at-2" || tok.RefreshToken != "rt-2" {
		t.Errorf("token = %+v", tok)
	}
	if got := tokenPosts.Load(); got != 2 {
		t.Errorf("token POSTs = %d, want 2", got)
	}
	if !reflect.DeepEqual(sleeps, []time.Duration{time.Second}) {
		t.Errorf("sleeps = %v, want [1s]", sleeps)
	}
}

func TestKimiRefreshInvalidGrant(t *testing.T) {
	var sleeps []time.Duration
	srv, _ := newKimiFake(t, kimiFakeDeviceJSON,
		func() (int, string) { return 400, `{"error":"invalid_grant","error_description":"refresh revoked"}` },
	)
	mgr := newKimiTestManager(t, srv, &sleeps)

	_, err := mgr.RefreshToken(context.Background(), "rt-old", srv.URL)
	if !errors.Is(err, ErrKimiOAuthUnauthorized) {
		t.Fatalf("err = %v, want ErrKimiOAuthUnauthorized", err)
	}
}

func TestKimiRefreshMissingRefreshTokenInResponse(t *testing.T) {
	var sleeps []time.Duration
	srv, _ := newKimiFake(t, kimiFakeDeviceJSON,
		func() (int, string) { return 200, `{"access_token":"at-2","expires_in":1800}` },
	)
	mgr := newKimiTestManager(t, srv, &sleeps)

	_, err := mgr.RefreshToken(context.Background(), "rt-old", srv.URL)
	if err == nil || err.Error() != "OAuth response missing refresh_token" {
		t.Errorf("err = %v", err)
	}
}

func TestKimiRefreshEmptyToken(t *testing.T) {
	mgr := NewKimiOAuthManager(nil)
	_, err := mgr.RefreshToken(context.Background(), "", "")
	if !errors.Is(err, ErrKimiOAuthUnauthorized) {
		t.Errorf("err = %v, want ErrKimiOAuthUnauthorized", err)
	}
}

func TestKimiClaimCompletion(t *testing.T) {
	session := &KimiAuthSession{
		Status: "pending",
		Token:  &KimiTokenInfo{AccessToken: "at"},
	}
	if session.ClaimCompletion() {
		t.Error("ClaimCompletion true on pending session")
	}
	session.Status = "completed"
	if !session.ClaimCompletion() {
		t.Error("first ClaimCompletion = false, want true")
	}
	if session.ClaimCompletion() {
		t.Error("second ClaimCompletion = true, want false")
	}
}

func TestKimiTokenFromResponseExpiresInAsString(t *testing.T) {
	data := map[string]any{
		"access_token":  "at",
		"refresh_token": "rt",
		"expires_in":    "3600",
	}
	tok, err := tokenFromResponse(data, time.Now())
	if err != nil {
		t.Fatalf("tokenFromResponse: %v", err)
	}
	if until := time.Until(tok.ExpiresAt); until <= 3500*time.Second || until > 3600*time.Second {
		t.Errorf("ExpiresAt in %v, want ~3600s", until)
	}
}

func TestKimiStartDeviceAuthSupersedesEarlierSession(t *testing.T) {
	var sleeps []time.Duration
	srv, _ := newKimiFake(t, kimiFakeDeviceJSON,
		func() (int, string) { return 400, `{"error":"authorization_pending"}` },
	)
	mgr := newKimiTestManager(t, srv, &sleeps)

	first, err := mgr.StartDeviceAuth(context.Background())
	if err != nil {
		t.Fatalf("first StartDeviceAuth: %v", err)
	}
	second, err := mgr.StartDeviceAuth(context.Background())
	if err != nil {
		t.Fatalf("second StartDeviceAuth: %v", err)
	}
	defer mgr.CancelSession(second.ID)

	if snap := first.Snapshot(); snap.Status != "cancelled" {
		t.Errorf("first session status = %q, want cancelled", snap.Status)
	}
	if _, ok := mgr.GetSession(first.ID); ok {
		t.Error("first session still registered after a newer login started")
	}
	if _, ok := mgr.GetSession(second.ID); !ok {
		t.Error("second session not registered")
	}
}

func TestKimiStartDeviceAuthRejectsNonHTTPSVerificationURI(t *testing.T) {
	var sleeps []time.Duration
	srv, _ := newKimiFake(t, `{
		"device_code": "dc-1",
		"user_code": "ABCD-1234",
		"verification_uri": "https://www.kimi.ai/code/authorize_device",
		"verification_uri_complete": "javascript:alert(1)",
		"expires_in": 1800,
		"interval": 5
	}`, func() (int, string) { return 400, `{"error":"authorization_pending"}` })
	mgr := newKimiTestManager(t, srv, &sleeps)

	session, err := mgr.StartDeviceAuth(context.Background())
	if session != nil {
		mgr.CancelSession(session.ID)
	}
	if err == nil {
		t.Fatal("StartDeviceAuth accepted a javascript: verification URI")
	}
}
