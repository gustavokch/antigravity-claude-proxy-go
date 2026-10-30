package cloudcode

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
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
		// net/http adds Accept-Encoding: gzip itself (as agy's does) and then
		// owns decompression.
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
	client.generationEndpoints = []string{server.URL}
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
	client.generationEndpoints = []string{server.URL}
	_, err := client.StreamGenerateContent(context.Background(), map[string]string{"x": "y"}, RequestOptions{}, func(event SSEEvent) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error on corrupt gzip stream, got nil")
	}
}

func TestLoadCodeAssistSendsAgysExactBody(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}
		if want := `{"metadata":{"ideType":"ANTIGRAVITY"}}`; string(body) != want {
			t.Errorf("body = %s, want %s", body, want)
		}
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()
	client := New(Options{AccessToken: "token", HTTPClient: server.Client()})
	client.provisioningEndpoints = []string{server.URL}
	if _, err := client.LoadCodeAssist(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Every call is pinned to agy's host with no cross-host fallback: agy 1.4.10
// sends all of its Cloud Code calls to daily, and a thought signature issued by
// one host is rejected by the other. The no-fallback behaviour itself is
// covered by TestGenerationTargetsOnlyGenerationEndpoints.
func TestEndpointsMatchAgyHost(t *testing.T) {
	t.Parallel()
	want := []string{DailyEndpoint}
	for name, endpoints := range map[string][]string{
		"generation": GenerationEndpoints, "content": ContentEndpoints, "provisioning": ProvisioningEndpoints,
	} {
		if !reflect.DeepEqual(endpoints, want) {
			t.Errorf("%s endpoints = %#v, want %#v", name, endpoints, want)
		}
	}
}

// TestRequestHeadsOnTheWireMatchAgy reads the request head as the production
// transport writes it, before a server could reorder or canonicalise it, and
// compares the header order with agy 1.4.10's: a unary call carries
// Content-Length and a streaming call is chunked, and Accept-Encoding comes
// last because net/http, not the client, adds it
// (.reference/agy-1.4.10-wire-20261030.txt).
func TestRequestHeadsOnTheWireMatchAgy(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	type captured struct {
		head []string
		body string
	}
	requests := make(chan captured, 2)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				var raw bytes.Buffer
				request, err := http.ReadRequest(bufio.NewReader(io.TeeReader(conn, &raw)))
				if err != nil {
					t.Error(err)
					return
				}
				body, _ := io.ReadAll(request.Body)
				head, _, _ := strings.Cut(raw.String(), "\r\n\r\n")
				requests <- captured{head: strings.Split(head, "\r\n")[1:], body: string(body)}
				_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}")
			}(conn)
		}
	}()

	client := New(Options{AccessToken: "token"})
	client.provisioningEndpoints = []string{"http://" + listener.Addr().String()}
	client.generationEndpoints = client.provisioningEndpoints
	if _, err := client.LoadCodeAssist(context.Background()); err != nil {
		t.Fatal(err)
	}
	unary := <-requests
	if _, err := client.StreamGenerateContent(context.Background(), map[string]string{"x": "y"}, RequestOptions{}, func(SSEEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	stream := <-requests

	names := func(head []string) []string {
		var out []string
		for _, line := range head {
			name, _, _ := strings.Cut(line, ":")
			out = append(out, name)
		}
		return out
	}
	if got, want := names(unary.head), []string{"Host", "User-Agent", "Content-Length", "Authorization", "Content-Type", "Accept-Encoding"}; !reflect.DeepEqual(got, want) {
		t.Errorf("unary header order = %v, want %v", got, want)
	}
	if got, want := names(stream.head), []string{"Host", "User-Agent", "Transfer-Encoding", "Authorization", "Content-Type", "Accept-Encoding"}; !reflect.DeepEqual(got, want) {
		t.Errorf("streaming header order = %v, want %v", got, want)
	}
	if stream.head[2] != "Transfer-Encoding: chunked" || stream.head[5] != "Accept-Encoding: gzip" {
		t.Errorf("streaming head = %q", stream.head)
	}
	if stream.body != `{"x":"y"}` {
		t.Errorf("streaming body = %q", stream.body)
	}
}

func TestGenerationTargetsOnlyGenerationEndpoints(t *testing.T) {
	t.Parallel()
	var metadataCalls, generationCalls atomic.Int32
	metadata := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		metadataCalls.Add(1)
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer metadata.Close()
	generation := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		generationCalls.Add(1)
		http.Error(writer, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`, http.StatusTooManyRequests)
	}))
	defer generation.Close()

	client := New(Options{AccessToken: "token", HTTPClient: generation.Client()})
	client.contentEndpoints = []string{metadata.URL}
	client.generationEndpoints = []string{generation.URL}

	_, streamErr := client.StreamGenerateContent(context.Background(), map[string]string{"x": "y"}, RequestOptions{}, func(SSEEvent) error { return nil })
	_, jsonErr := client.GenerateContent(context.Background(), map[string]string{"x": "y"}, RequestOptions{})
	for name, err := range map[string]error{"stream": streamErr, "json": jsonErr} {
		if got := FindHTTPError(err); got == nil || got.StatusCode != http.StatusTooManyRequests {
			t.Errorf("%s error = %v, want upstream 429", name, err)
		}
	}
	if metadataCalls.Load() != 0 || generationCalls.Load() != 2 {
		t.Fatalf("metadata=%d generation=%d, want 0 and 2: a generation 429 must not reach another host",
			metadataCalls.Load(), generationCalls.Load())
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

func TestStreamHasNoTotalTimeout(t *testing.T) {
	t.Parallel()
	// A total Client.Timeout covers the full body read and kills SSE
	// streams mid-generation; the downstream client then sees a stream
	// that ends without message_stop ("stream ended before message_stop").
	// The timeout must bound TTFB/unary only, never an open stream.
	client := New(Options{AccessToken: "token", Timeout: 5 * time.Minute})
	if client.httpClient.Timeout != 0 {
		t.Errorf("httpClient.Timeout = %v, want 0 (kills long SSE streams)", client.httpClient.Timeout)
	}
	if !reflect.DeepEqual(client.transport.TLSClientConfig, &tls.Config{}) {
		t.Errorf("TLS config must stay empty for agy JA3 parity: %#v", client.transport.TLSClientConfig)
	}
}

func TestSlowStreamSurvivesConfiguredTimeout(t *testing.T) {
	t.Parallel()
	// Headers arrive fast; body trickles past the configured timeout.
	// With a total Client.Timeout this fails; without it, it completes.
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		flusher, _ := writer.(http.Flusher)
		_, _ = writer.Write([]byte("event: a\ndata: {\"n\":1}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(400 * time.Millisecond)
		_, _ = writer.Write([]byte("event: b\ndata: {\"n\":2}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer server.Close()

	client := New(Options{AccessToken: "token", Timeout: 100 * time.Millisecond})
	client.generationEndpoints = []string{server.URL}
	var events []SSEEvent
	if _, err := client.StreamGenerateContent(context.Background(), map[string]string{"x": "y"}, RequestOptions{}, func(event SSEEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatalf("slow stream killed: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2 (stream truncated?)", len(events))
	}
}

func TestDoJSONEndpointFallbackUnderTimeout(t *testing.T) {
	t.Parallel()
	// If the primary endpoint hangs and times out, the fallback endpoint
	// must still get its own timeout window instead of failing immediately
	// with context deadline exceeded.
	release := make(chan struct{})
	primary := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-release:
		case <-request.Context().Done():
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer primary.Close()
	defer close(release)

	var secondaryCalls atomic.Int32
	secondary := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		secondaryCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	defer secondary.Close()

	client := New(Options{AccessToken: "token", Timeout: 100 * time.Millisecond})
	resp, err := client.DoJSON(context.Background(), []string{primary.URL, secondary.URL}, "/test", map[string]string{}, RequestOptions{})
	if err != nil {
		t.Fatalf("DoJSON failed: %v", err)
	}
	if secondaryCalls.Load() != 1 {
		t.Errorf("secondary calls = %d, want 1", secondaryCalls.Load())
	}
	if resp.Endpoint != secondary.URL {
		t.Errorf("endpoint = %q, want %q", resp.Endpoint, secondary.URL)
	}
}

func TestFindHTTPError(t *testing.T) {
	t.Parallel()
	err400 := &HTTPError{StatusCode: http.StatusBadRequest, Status: "400", Body: "Corrupted thought signature"}
	err401 := &HTTPError{StatusCode: http.StatusUnauthorized, Status: "401", Body: "Unauthorized"}
	err403 := &HTTPError{StatusCode: http.StatusForbidden, Status: "403", Body: "Forbidden"}
	err429 := &HTTPError{StatusCode: http.StatusTooManyRequests, Status: "429", Body: "RESOURCE_EXHAUSTED"}
	err500 := &HTTPError{StatusCode: http.StatusInternalServerError, Status: "500", Body: "Internal Error"}
	var typedNil *HTTPError

	cases := []struct {
		name string
		err  error
		want *HTTPError
	}{
		{"nil", nil, nil},
		{"unrelated", errors.New("other"), nil},
		{"typed nil", error(typedNil), nil},
		{"single", err429, err429},
		{"wrapped", fmt.Errorf("outer: %w", err429), err429},
		{"429 beats earlier 400", errors.Join(err400, err429), err429},
		{"429 beats earlier 500", errors.Join(err500, err429), err429},
		{"401 beats earlier 500", errors.Join(err500, err401), err401},
		{"403 beats earlier 500", errors.Join(err500, err403), err403},
		{"500 beats earlier 400", errors.Join(err400, err500), err500},
		{"typed nil skipped in join", errors.Join(error(typedNil), err429), err429},
		{"429 inside wrapped join", fmt.Errorf("max retries exceeded: %w", errors.Join(err400, err429)), err429},
	}
	for _, tc := range cases {
		if got := FindHTTPError(tc.err); got != tc.want {
			t.Errorf("%s: FindHTTPError = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLoadCodeAssistAt_UsesGivenEndpoint(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.Path
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()
	client := New(Options{AccessToken: "token", HTTPClient: server.Client()})
	// Default provisioning endpoints must NOT be used.
	client.provisioningEndpoints = []string{"http://127.0.0.1:1"}
	if _, err := client.LoadCodeAssistAt(context.Background(), []string{server.URL}); err != nil {
		t.Fatalf("LoadCodeAssistAt error: %v", err)
	}
	if gotPath != PathLoadCodeAssist {
		t.Errorf("path = %q, want %q", gotPath, PathLoadCodeAssist)
	}
}
