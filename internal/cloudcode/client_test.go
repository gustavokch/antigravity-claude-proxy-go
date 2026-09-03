package cloudcode

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTransportKeepsTLSZeroValueAndHTTP2Disabled(t *testing.T) {
	t.Parallel()
	client := New(Options{})
	if client.transport == nil {
		t.Fatal("dedicated transport was not created")
	}
	if !reflect.DeepEqual(client.transport.TLSClientConfig, &tls.Config{}) {
		t.Fatalf("TLS config is not empty: %#v", client.transport.TLSClientConfig)
	}
	if client.transport.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 must remain false for current-agy ALPN")
	}
	if client.transport.TLSNextProto != nil {
		t.Fatalf("TLSNextProto must remain nil: %#v", client.transport.TLSNextProto)
	}
}

func TestSharedTransportConfiguration(t *testing.T) {
	client := New(Options{AccessToken: "test-token"})
	if client.transport == nil {
		t.Fatal("expected non-nil transport")
	}
	if client.transport.MaxIdleConns < 1000 {
		t.Errorf("expected MaxIdleConns >= 1000, got %d", client.transport.MaxIdleConns)
	}
	if client.transport.MaxIdleConnsPerHost < 500 {
		t.Errorf("expected MaxIdleConnsPerHost >= 500, got %d", client.transport.MaxIdleConnsPerHost)
	}
}

func TestFetchAvailableModelsHeadersAndDailyFallback(t *testing.T) {
	t.Parallel()
	var dailyCalls, prodCalls int
	daily := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		dailyCalls++
		if request.URL.Path != PathFetchAvailableModels {
			t.Errorf("path = %q", request.URL.Path)
		}
		assertHeader(t, request, "Authorization", "Bearer token")
		assertHeader(t, request, "Content-Type", "application/json")
		assertHeader(t, request, "Accept-Encoding", "gzip")
		// agy 1.1.25 sends none of these (MITM ground truth, see
		// .reference/agy-headers-mitm-20260903.txt).
		assertNoHeader(t, request, "X-Client-Name")
		assertNoHeader(t, request, "X-Client-Version")
		assertNoHeader(t, request, "x-goog-api-client")
		assertNoHeader(t, request, "Accept")
		assertNoHeader(t, request, "X-Machine-Session-Id")
		ua := request.Header.Get("User-Agent")
		wantPrefix := "antigravity/cli/" + DefaultUserAgentVersion + " (aidev_client; os_type="
		if !strings.HasPrefix(ua, wantPrefix) ||
			!strings.Contains(ua, "; arch="+runtime.GOARCH+";") ||
			!strings.Contains(ua, "; cl="+AgyBuildCL+";") ||
			!strings.HasSuffix(ua, "; auth_method="+AgyAuthMethod+")") {
			t.Errorf("User-Agent = %q", ua)
		}
		var body map[string]string
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["project"] != "project" {
			t.Errorf("body = %#v", body)
		}
		http.Error(writer, "capacity", http.StatusServiceUnavailable)
	}))
	defer daily.Close()
	prod := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		prodCalls++
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"models":{}}`))
	}))
	defer prod.Close()

	client := New(Options{AccessToken: "token", HTTPClient: daily.Client()})
	client.contentEndpoints = []string{daily.URL, prod.URL}
	response, err := client.FetchAvailableModels(context.Background(), "project")
	if err != nil {
		t.Fatal(err)
	}
	if dailyCalls != 1 || prodCalls != 1 || response.Endpoint != prod.URL {
		t.Fatalf("daily=%d prod=%d endpoint=%q", dailyCalls, prodCalls, response.Endpoint)
	}
}

func TestDoJSONDecompressesGzipResponse(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// Manual Accept-Encoding disables net/http auto-decompression; the
		// client must decompress itself (MITM: agy sends Accept-Encoding: gzip).
		if got := request.Header.Get("Accept-Encoding"); got != "gzip" {
			t.Errorf("Accept-Encoding = %q", got)
		}
		var buf bytes.Buffer
		writer.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(&buf)
		_, _ = gz.Write([]byte(`{"models":{}}`))
		if err := gz.Close(); err != nil {
			t.Error(err)
		}
		_, _ = writer.Write(buf.Bytes())
	}))
	defer server.Close()

	client := New(Options{AccessToken: "token", HTTPClient: server.Client()})
	client.contentEndpoints = []string{server.URL}
	response, err := client.FetchAvailableModels(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(response.Body, &parsed); err != nil {
		t.Fatalf("response body was not decompressed: %v (body=%q)", err, response.Body)
	}
	if parsed["models"] == nil {
		t.Fatalf("parsed body = %#v", parsed)
	}
}

func TestDoSSEDecompressesGzipStream(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Accept-Encoding"); got != "gzip" {
			t.Errorf("Accept-Encoding = %q", got)
		}
		var buf bytes.Buffer
		writer.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(&buf)
		_, _ = gz.Write([]byte("event: message\r\ndata: {\"ok\":true}\r\n\r\n"))
		if err := gz.Close(); err != nil {
			t.Error(err)
		}
		_, _ = writer.Write(buf.Bytes())
	}))
	defer server.Close()

	client := New(Options{AccessToken: "token", HTTPClient: server.Client()})
	client.contentEndpoints = []string{server.URL}
	var events []SSEEvent
	if _, err := client.StreamGenerateContent(context.Background(), map[string]string{"x": "y"}, RequestOptions{}, func(event SSEEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Event != "message" || string(events[0].Data) != `{"ok":true}` {
		t.Fatalf("events = %#v (stream was not decompressed?)", events)
	}
}

func TestDoSSEClosesBodyOnGunzipError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Encoding", "gzip")
		_, _ = writer.Write([]byte("invalid-gzip-payload"))
	}))
	defer server.Close()

	client := New(Options{AccessToken: "token", HTTPClient: server.Client()})
	client.contentEndpoints = []string{server.URL}
	_, err := client.StreamGenerateContent(context.Background(), map[string]string{"x": "y"}, RequestOptions{}, func(event SSEEvent) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error on corrupt gzip stream, got nil")
	}
}

func TestLoadCodeAssistMetadata(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body LoadCodeAssistRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Mode != 1 || body.Metadata.IdeType != 9 || body.Metadata.Platform != platformEnum() || body.Metadata.PluginType != 2 || body.Metadata.DuetProject != "project" {
			t.Errorf("body = %#v", body)
		}
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()
	client := New(Options{AccessToken: "token", HTTPClient: server.Client()})
	client.provisioningEndpoints = []string{server.URL}
	if _, err := client.LoadCodeAssist(context.Background(), "project"); err != nil {
		t.Fatal(err)
	}
}

func TestContentAndProvisioningUseProductionBeforeDaily(t *testing.T) {
	t.Parallel()
	if len(ContentEndpoints) != 2 || ContentEndpoints[0] != ProdEndpoint || ContentEndpoints[1] != DailyEndpoint {
		t.Fatalf("content endpoint order = %#v", ContentEndpoints)
	}
	if len(ProvisioningEndpoints) != 2 || ProvisioningEndpoints[0] != ProdEndpoint || ProvisioningEndpoints[1] != DailyEndpoint {
		t.Fatalf("provisioning endpoint order = %#v", ProvisioningEndpoints)
	}
}

func TestParseSSE(t *testing.T) {
	t.Parallel()
	input := ": keepalive\r\nid: one\r\nevent: message\r\nretry: 1500\r\ndata: {\"a\":\r\ndata: 1}\r\n\r\ndata: [DONE]"
	var events []SSEEvent
	err := ParseSSE(strings.NewReader(input), func(event SSEEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %#v", events)
	}
	if events[0].Event != "message" || string(events[0].Data) != "{\"a\":\n1}" || events[0].ID != "one" || events[0].Retry != 1500*time.Millisecond {
		t.Fatalf("first event = %#v", events[0])
	}
	if string(events[1].Data) != "[DONE]" || events[1].ID != "one" {
		t.Fatalf("second event = %#v", events[1])
	}
}

func BenchmarkParseSSE(b *testing.B) {
	sseData := []byte("event: message\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}]}\n\n")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ParseSSE(bytes.NewReader(sseData), func(e SSEEvent) error {
			return nil
		})
	}
}

func assertHeader(t *testing.T, request *http.Request, name, want string) {
	t.Helper()
	if got := request.Header.Get(name); got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

func assertNoHeader(t *testing.T, request *http.Request, name string) {
	t.Helper()
	if got := request.Header.Get(name); got != "" {
		t.Errorf("%s unexpectedly present: %q", name, got)
	}
}
