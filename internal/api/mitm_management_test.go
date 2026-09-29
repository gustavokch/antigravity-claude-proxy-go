package api

import (
	"antigravity-go-proxy/internal/config"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/mitm"
)

func startTestMitm(t *testing.T, srv *Server) *mitm.Runtime {
	t.Helper()
	rt, err := mitm.StartRuntime(mitm.RuntimeConfig{Dir: filepath.Join(t.TempDir(), "mitm"), Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Shutdown(t.Context()) })
	srv.mitm = rt
	return rt
}

func getJSON(t *testing.T, srv *Server, path string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body
}

func TestMitmManagementWhenDisabled(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)

	if _, body := getJSON(t, srv, "/api/mitm/status"); body["enabled"] != false {
		t.Fatalf("status = %v", body)
	}
	if rec, _ := getJSON(t, srv, "/api/mitm/ca.pem"); rec.Code != http.StatusNotFound {
		t.Fatalf("ca.pem code = %d, want 404", rec.Code)
	}
	if _, body := getJSON(t, srv, "/api/sessions/cloud"); body["enabled"] != false {
		t.Fatalf("sessions = %v", body)
	}
	if rec, _ := getJSON(t, srv, "/api/sessions/cloud/abc123abc123"); rec.Code != http.StatusNotFound {
		t.Fatalf("session get code = %d, want 404", rec.Code)
	}
}

func TestMitmManagementWhenRunning(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	rt := startTestMitm(t, srv)
	rt.Registry.Observe(mitm.Observation{Route: "sessions.create", RawID: "session_01ABCDEFGHJK",
		Fields: map[string]string{"model": "claude-opus-5-5"}})

	statusRec, status := getJSON(t, srv, "/api/mitm/status")
	if status["enabled"] != true || status["caFingerprint"] != rt.CA.Fingerprint() {
		t.Fatalf("status = %v", status)
	}
	if _, has := status["stats"]; !has {
		t.Fatal("status must carry stats")
	}

	caRec, _ := getJSON(t, srv, "/api/mitm/ca.pem")
	if caRec.Code != http.StatusOK || !strings.HasPrefix(caRec.Body.String(), "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("ca.pem = %d %q", caRec.Code, caRec.Body.String())
	}
	if strings.Contains(caRec.Body.String(), "PRIVATE KEY") {
		t.Fatal("CA key must never be served")
	}
	if got := caRec.Header().Get("Content-Type"); got != "application/x-pem-file" {
		t.Fatalf("ca.pem content type = %q", got)
	}

	listRec, list := getJSON(t, srv, "/api/sessions/cloud")
	sessions, _ := list["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %v", list)
	}
	first, ok := sessions[0].(map[string]any)
	if !ok {
		t.Fatalf("session entry is %T, want an object", sessions[0])
	}
	id := mitm.HashID("session_01ABCDEFGHJK")
	if first["id"] != id {
		t.Fatalf("id = %v, want %s", first["id"], id)
	}
	oneRec, one := getJSON(t, srv, "/api/sessions/cloud/"+id)
	if oneRec.Code != http.StatusOK || one["model"] != "claude-opus-5-5" {
		t.Fatalf("get one = %d %v", oneRec.Code, one)
	}
	if rec, _ := getJSON(t, srv, "/api/sessions/cloud/000000000000"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id code = %d, want 404", rec.Code)
	}
	// The raw session id must not appear in any payload the API serves.
	for name, body := range map[string]string{
		"status": statusRec.Body.String(), "ca.pem": caRec.Body.String(),
		"list": listRec.Body.String(), "get one": oneRec.Body.String(),
	} {
		if strings.Contains(body, "01ABCDEFGHJK") {
			t.Fatalf("raw session id leaked in the %s payload: %s", name, body)
		}
	}
}

func TestMitmRoutesRequireWebUIPassword(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	rt := startTestMitm(t, srv)
	rt.Registry.Observe(mitm.Observation{Route: "sessions.create", RawID: "session_01ABCDEFGHJK"})
	original := config.Get()
	t.Cleanup(func() { config.SetForTest(original) })
	cfg := original
	cfg.WebUIPassword = "s3cret"
	config.SetForTest(cfg)

	for _, path := range []string{
		"/api/mitm/status", "/api/mitm/ca.pem", "/api/sessions/cloud",
		"/api/sessions/cloud/" + mitm.HashID("session_01ABCDEFGHJK"),
	} {
		if rec, _ := getJSON(t, srv, path); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without password = %d, want 401", path, rec.Code)
		}
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("x-webui-password", "s3cret")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s with the password = %d, want 200", path, rec.Code)
		}
	}
}

func TestConfigSaveRejectsNonLoopbackMitmListen(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(`{"mitm":{"enabled":true,"listen":"0.0.0.0:8092"}}`))
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "loopback") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	ok := httptest.NewRecorder()
	srv.Handler().ServeHTTP(ok, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(`{"mitm":{"enabled":true}}`)))
	if ok.Code != http.StatusOK {
		t.Fatalf("valid partial mitm block rejected: %d %s", ok.Code, ok.Body.String())
	}
	if !config.Get().Mitm.Enabled || config.Get().Mitm.Listen != "127.0.0.1:8092" {
		t.Fatalf("saved = %+v", config.Get().Mitm)
	}
}

func postMitmConfig(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body)))
	return rec
}

// config.Save refreshes the in-memory config from the merged file, so on an
// install whose config.json predates the mitm block config.Get().Mitm loses its
// defaults after any unrelated Save. The Cloud toggle must not depend on it.
func TestConfigSaveMitmToggleAfterUnrelatedSave(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	path, err := config.ConfigFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"logLevel":"info"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Save(map[string]any{"debug": true}); err != nil {
		t.Fatal(err)
	}

	for _, enabled := range []bool{true, false} {
		rec := postMitmConfig(t, srv, fmt.Sprintf(`{"mitm":{"enabled":%t}}`, enabled))
		if rec.Code != http.StatusOK {
			t.Fatalf("enabled=%t: %d %s", enabled, rec.Code, rec.Body.String())
		}
		if got := config.Get().Mitm.Enabled; got != enabled {
			t.Fatalf("after enabled=%t, config.Get().Mitm.Enabled = %t", enabled, got)
		}
	}
}

func TestConfigSaveValidatesMitmBounds(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	for _, c := range []struct{ name, body, want string }{
		{"non-loopback listen", `{"mitm":{"listen":"0.0.0.0:8092"}}`, "loopback"},
		{"registryMax below range", `{"mitm":{"registryMax":0}}`, "registryMax"},
		{"registryMax above range", `{"mitm":{"registryMax":100001}}`, "registryMax"},
		{"ttl below range", `{"mitm":{"registryTtlMinutes":0}}`, "registryTtlMinutes"},
		{"ttl above range", `{"mitm":{"registryTtlMinutes":10081}}`, "registryTtlMinutes"},
		{"not an object", `{"mitm":"yes"}`, "Invalid mitm configuration format"},
	} {
		rec := postMitmConfig(t, srv, c.body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("%s: code=%d body=%s, want 400 mentioning %q", c.name, rec.Code, rec.Body.String(), c.want)
		}
	}
	for _, body := range []string{
		`{"mitm":{"registryMax":1,"registryTtlMinutes":1}}`,
		`{"mitm":{"registryMax":100000,"registryTtlMinutes":10080}}`,
	} {
		if rec := postMitmConfig(t, srv, body); rec.Code != http.StatusOK {
			t.Errorf("%s rejected: %d %s", body, rec.Code, rec.Body.String())
		}
	}
}
