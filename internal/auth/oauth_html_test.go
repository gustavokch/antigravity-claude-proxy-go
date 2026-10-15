package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The OAuth callback pages interpolate a value into HTML. The error parameter
// arrives on the redirect query string, so an unescaped page turns
// error=<script>… into a script running on the loopback origin.

func TestGoogleCallbackEscapesErrorParam(t *testing.T) {
	om := NewOAuthManager(AccountSaverFunc(func(email, refreshToken string) error { return nil }))
	handler := om.callbackHandler(&http.Server{})

	req := httptest.NewRequest(http.MethodGet, "/oauth-callback?error=%3Cscript%3Ealert(1)%3C%2Fscript%3E", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("error parameter not escaped: %s", body)
	}
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("error parameter reached the page as markup: %s", body)
	}
}

func TestClaudeCodeCallbackEscapesErrorParam(t *testing.T) {
	m := NewClaudeCodeOAuthManager()
	session := &ClaudeCodeAuthSession{ID: "s1", State: "state-1", doneChan: make(chan struct{})}

	req := httptest.NewRequest(http.MethodGet, "/callback?error=%3Cimg+src%3Dx+onerror%3Dalert(1)%3E", nil)
	rec := httptest.NewRecorder()
	m.handleLoopbackCallback(session, rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "&lt;img") {
		t.Errorf("error parameter not escaped: %s", body)
	}
	if strings.Contains(body, "<img src=x onerror=alert(1)>") {
		t.Errorf("error parameter reached the page as markup: %s", body)
	}
	if session.Status != "failed" {
		t.Errorf("session status = %q, want failed", session.Status)
	}
}
