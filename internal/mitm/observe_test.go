package mitm

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
)

func TestClassifyRoute(t *testing.T) {
	cases := []struct {
		method, target, route, id string
	}{
		{"POST", "/v1/sessions", "sessions.create", ""},
		{"POST", "/v1/sessions?x=1", "sessions.create", ""},
		{"GET", "/v1/environment_providers", "environments.list", ""},
		{"GET", "/v1/code/sessions/session_01ABCDEFGHJK", "code.session.get", "session_01ABCDEFGHJK"},
		{"POST", "/v1/code/sessions/session_01ABCDEFGHJK/events", "code.session.events.post", "session_01ABCDEFGHJK"},
		{"GET", "/v1/code/sessions/session_01ABCDEFGHJK/events/stream?after=3", "code.session.events.stream", "session_01ABCDEFGHJK"},
		{"POST", "/v1/code/sessions/session_01ABCDEFGHJK/archive", "code.session.other", "session_01ABCDEFGHJK"},
		{"GET", "/v1/sessions/session_01ABCDEFGHJK", "sessions.get", "session_01ABCDEFGHJK"},
		{"GET", "/v1/messages", "", ""},
		{"GET", "/v1/code/sessions/x", "", ""}, // too short to be an id
		{"GET", "/v1/oauth/token", "", ""},
	}
	for _, c := range cases {
		route, id := classifyRoute(c.method, c.target)
		if route != c.route || id != c.id {
			t.Errorf("%s %s = (%q,%q), want (%q,%q)", c.method, c.target, route, id, c.route, c.id)
		}
	}
}

func TestClassifyRouteDoesNotEchoUnknownMethods(t *testing.T) {
	const id = "session_01ABCDEFGHJK"
	cases := []struct{ method, target, route string }{
		{"<svg/onload=alert(1)>", "/v1/code/sessions/" + id, "code.session.other"},
		{"BREW", "/v1/code/sessions/" + id + "/events", "code.session.events.other"},
		{"Get", "/v1/sessions/" + id, "sessions.other"},
	}
	for _, c := range cases {
		route, gotID := classifyRoute(c.method, c.target)
		if route != c.route || gotID != id {
			t.Errorf("%q %s = (%q,%q), want (%q,%q)", c.method, c.target, route, gotID, c.route, id)
		}
	}
}

func TestSummarizeCreateResponse(t *testing.T) {
	body := `{"id":"session_01ABCDEFGHJK","session_status":"running","status_bucket":"working","environment_kind":"anthropic_cloud",
	  "connection_status":"connected","configured_model":"claude-opus-5-5","created_at":"2026-09-28T20:00:00Z",
	  "title":"my private prompt text","session_url":"https://claude.ai/code/session_01ABCDEFGHJK"}`
	id, fields := summarizeBody([]byte(body), "")
	if id != "session_01ABCDEFGHJK" {
		t.Fatalf("id = %q", id)
	}
	want := map[string]string{"sessionStatus": "running", "statusBucket": "working", "environmentKind": "anthropic_cloud",
		"connectionStatus": "connected", "model": "claude-opus-5-5", "createdAt": "2026-09-28T20:00:00Z"}
	for k, v := range want {
		if fields[k] != v {
			t.Errorf("fields[%s] = %q, want %q", k, fields[k], v)
		}
	}
	for _, v := range fields {
		if strings.Contains(v, "private") || strings.Contains(v, "http") {
			t.Errorf("free text leaked into fields: %q", v)
		}
	}
}

func TestSummarizeReadResponseWrapperAndConfigModel(t *testing.T) {
	body := `{"response_shape":{"connection_status":"connected","config":{"model":"claude-opus-5-5"},"environment_kind":"anthropic_cloud"}}`
	_, fields := summarizeBody([]byte(body), "")
	if fields["model"] != "claude-opus-5-5" || fields["environmentKind"] != "anthropic_cloud" {
		t.Fatalf("fields = %v", fields)
	}
}

func TestSummarizeDropsFreeTextValues(t *testing.T) {
	body := `{"id":"session_01ABCDEFGHJK","session_status":"waiting for the user to reply, please","status_bucket":"ok"}`
	_, fields := summarizeBody([]byte(body), "")
	if _, ok := fields["sessionStatus"]; ok {
		t.Fatal("free text must not be kept")
	}
	if fields["statusBucket"] != "ok" {
		t.Fatal("token value should be kept")
	}
}

func TestSummarizeGzipAndUnknownEncoding(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(`{"id":"session_01ABCDEFGHJK","status_bucket":"ok"}`))
	zw.Close()
	id, fields := summarizeBody(buf.Bytes(), "gzip")
	if id == "" || fields["statusBucket"] != "ok" {
		t.Fatalf("gzip body not decoded: %q %v", id, fields)
	}
	if id, fields := summarizeBody([]byte("garbage"), "br"); id != "" || len(fields) != 0 {
		t.Fatal("unknown encoding must yield nothing")
	}
	var brBuf bytes.Buffer
	bw := brotli.NewWriter(&brBuf)
	bw.Write([]byte(`{"id":"session_01ABCDEFGHJK","status_bucket":"ok"}`))
	bw.Close()
	if id, fields := summarizeBody(brBuf.Bytes(), "br"); id != "session_01ABCDEFGHJK" || fields["statusBucket"] != "ok" {
		t.Fatalf("br body not decoded: %q %v", id, fields)
	}
	if id, _ := summarizeBody([]byte("not json"), ""); id != "" {
		t.Fatal("non-JSON must yield nothing")
	}
}

func TestRegistryHashesIDsAndMerges(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	reg := NewRegistry(10, time.Hour, func() time.Time { return now })
	reg.Observe(Observation{Route: "sessions.create", RawID: "session_01ABCDEFGHJK", Fields: map[string]string{"model": "claude-opus-5-5"}})
	reg.Observe(Observation{Route: "code.session.events.post", RawID: "session_01ABCDEFGHJK"})
	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("len = %d", len(list))
	}
	s := list[0]
	if s.ID != HashID("session_01ABCDEFGHJK") || len(s.ID) != 12 || strings.Contains(s.ID, "session") {
		t.Fatalf("id = %q", s.ID)
	}
	if s.Requests != 2 || s.Model != "claude-opus-5-5" || s.LastRoute != "code.session.events.post" {
		t.Fatalf("session = %+v", s)
	}
	blob, _ := json.Marshal(list)
	if strings.Contains(string(blob), "01ABCDEFGHJK") {
		t.Fatal("raw id leaked into JSON")
	}
	if got, ok := reg.Get(s.ID); !ok || got.ID != s.ID {
		t.Fatal("Get by display id failed")
	}
}

func TestRegistryIgnoresObservationsWithoutID(t *testing.T) {
	reg := NewRegistry(10, time.Hour, nil)
	reg.Observe(Observation{Route: "environments.list"})
	if len(reg.List()) != 0 {
		t.Fatal("no id, no session")
	}
}

func TestRegistryTTLAndCap(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	reg := NewRegistry(2, time.Hour, clock)
	reg.Observe(Observation{RawID: "session_AAAAAAAA1"})
	now = now.Add(time.Minute)
	reg.Observe(Observation{RawID: "session_BBBBBBBB2"})
	now = now.Add(time.Minute)
	reg.Observe(Observation{RawID: "session_CCCCCCCC3"})
	if len(reg.List()) != 2 {
		t.Fatalf("cap not enforced: %d", len(reg.List()))
	}
	if _, ok := reg.Get(HashID("session_AAAAAAAA1")); ok {
		t.Fatal("oldest should have been evicted")
	}
	now = now.Add(2 * time.Hour)
	if len(reg.List()) != 0 {
		t.Fatal("TTL not enforced")
	}
}

func TestRegistry_DoesNotCreatePhantomSessionOnNon2xx(t *testing.T) {
	reg := NewRegistry(10, time.Hour, nil)

	// An observation for an unknown session ID with 404 Not Found
	reg.Observe(Observation{
		Route:  "sessions.get",
		RawID:  "session_nonexistent_404",
		Status: 404,
	})
	if len(reg.List()) != 0 {
		t.Fatalf("404 must not create a session, got: %v", reg.List())
	}

	// 403 Forbidden
	reg.Observe(Observation{
		Route:  "sessions.get",
		RawID:  "session_forbidden_403",
		Status: 403,
	})
	if len(reg.List()) != 0 {
		t.Fatalf("403 must not create a session, got: %v", reg.List())
	}

	// 500 Internal Server Error
	reg.Observe(Observation{
		Route:  "sessions.get",
		RawID:  "session_error_500",
		Status: 500,
	})
	if len(reg.List()) != 0 {
		t.Fatalf("500 must not create a session, got: %v", reg.List())
	}

	// 200 OK creates the session
	reg.Observe(Observation{
		Route:  "sessions.create",
		RawID:  "session_valid_200",
		Status: 200,
	})
	if len(reg.List()) != 1 {
		t.Fatalf("200 must create a session, got: %v", reg.List())
	}

	// Once the session is known, a subsequent non-2xx updates activity without creating a duplicate
	reg.Observe(Observation{
		Route:  "code.session.events.post",
		RawID:  "session_valid_200",
		Status: 500,
	})
	if len(reg.List()) != 1 {
		t.Fatalf("subsequent error must not duplicate session, got: %v", reg.List())
	}
}
