package codex

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// plainRT reaches the httptest server without the Chrome TLS dialer; the
// headers are the client's own either way.
var plainRT = http.DefaultTransport

func TestSendHeadersAndEndpoint(t *testing.T) {
	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("x-codex-plan-type", "plus")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached"}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/backend-api/codex/", plainRT)
	resp, err := c.Send(context.Background(), SendRequest{
		Token:       "tok",
		AccountID:   "acct-1",
		SessionID:   "cache-key",
		Model:       "gpt-5.5",
		ServiceTier: "priority",
		Body:        []byte(`{"model":"gpt-5.5"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d; Send must return the upstream status untouched", resp.StatusCode)
	}
	if got.Method != http.MethodPost || got.URL.Path != "/backend-api/codex/responses" {
		t.Errorf("request = %s %s", got.Method, got.URL.Path)
	}
	want := map[string]string{
		"Content-Type":         "application/json",
		"Authorization":        "Bearer tok",
		"Accept":               "text/event-stream",
		"User-Agent":           DefaultUserAgent,
		"Originator":           DefaultOriginator,
		"Chatgpt-Account-Id":   "acct-1",
		"Session-Id":           "cache-key",
		"X-Codex-Routing-Hint": "model=gpt-5.5;tier=priority",
	}
	for name, value := range want {
		if v := got.Header.Get(name); v != value {
			t.Errorf("%s = %q, want %q", name, v, value)
		}
	}
	if string(body) != `{"model":"gpt-5.5"}` {
		t.Errorf("body = %s", body)
	}
}

func TestSendOmitsOptionalHeadersAndHonoursIdentity(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}))
	defer srv.Close()

	c := NewClient(srv.URL, plainRT)
	resp, err := c.Send(context.Background(), SendRequest{
		Token:    "tok",
		Model:    "gpt-6-sol",
		Identity: IdentityConfig{UserAgent: "custom-ua/1", Originator: "custom"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, absent := range []string{"Session-Id", "Chatgpt-Account-Id"} {
		if v := got.Get(absent); v != "" {
			t.Errorf("%s = %q, want it absent", absent, v)
		}
	}
	if got.Get("X-Codex-Routing-Hint") != "model=gpt-6-sol" {
		t.Errorf("routing hint = %q, want no tier", got.Get("X-Codex-Routing-Hint"))
	}
	if got.Get("User-Agent") != "custom-ua/1" || got.Get("Originator") != "custom" {
		t.Errorf("identity = %q / %q", got.Get("User-Agent"), got.Get("Originator"))
	}
}

func TestSendDoesNotFollowRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			t.Error("redirect followed; the bearer token must not travel to the Location")
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	resp, err := NewClient(srv.URL, plainRT).Send(context.Background(), SendRequest{Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Errorf("status = %d, want the redirect returned as is", resp.StatusCode)
	}
}

func TestFetchModels(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		w.Header().Set("x-codex-primary-used-percent", "5")
		_, _ = io.WriteString(w, `{"models":[
			{"slug":"gpt-6-sol","display_name":"GPT-6-Sol","context_window":272000,"visibility":"list","supported_in_api":true,
			 "default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"}]},
			{"slug":"codex-auto-review","visibility":"hide","supported_in_api":true},
			{"slug":"gpt-6-chat-thinking","visibility":"list","supported_in_api":false},
			{"slug":"gpt-5.5","context_window":500000}
		]}`)
	}))
	defer srv.Close()

	models, header, err := NewClient(srv.URL, plainRT).FetchModels(context.Background(), "tok", "acct-1", IdentityConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodGet || got.URL.Path != "/models" || got.URL.Query().Get("client_version") != DefaultClientVersion {
		t.Errorf("request = %s %s", got.Method, got.URL)
	}
	for name, value := range map[string]string{
		"Accept": "application/json", "Authorization": "Bearer tok", "Chatgpt-Account-Id": "acct-1",
		"User-Agent": DefaultUserAgent, "Originator": DefaultOriginator,
	} {
		if v := got.Header.Get(name); v != value {
			t.Errorf("%s = %q, want %q", name, v, value)
		}
	}
	want := []DiscoveredModel{
		{ID: "gpt-6-sol", DisplayName: "GPT-6-Sol", ContextLength: 272000, Efforts: []string{"low", "medium", "high"}, DefaultEffort: "medium"},
		{ID: "gpt-5.5", ContextLength: 500000},
	}
	if !reflect.DeepEqual(models, want) {
		t.Errorf("models = %+v, want %+v (hidden and API-less entries left out)", models, want)
	}
	if header.Get("x-codex-primary-used-percent") != "5" {
		t.Error("response headers not returned")
	}
}

func TestFetchModelsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"detail":"token expired"}`)
	}))
	defer srv.Close()

	_, header, err := NewClient(srv.URL, plainRT).FetchModels(context.Background(), "tok", "", IdentityConfig{})
	var status *StatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusUnauthorized || status.Body != `{"detail":"token expired"}` {
		t.Fatalf("err = %v, want a StatusError with the 401 and its body", err)
	}
	if header == nil {
		t.Error("headers of the error reply not returned")
	}
}
