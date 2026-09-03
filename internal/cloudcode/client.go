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
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DailyEndpoint = "https://daily-cloudcode-pa.googleapis.com"
	ProdEndpoint  = "https://cloudcode-pa.googleapis.com"

	PathLoadCodeAssist       = "/v1internal:loadCodeAssist"
	PathOnboardUser          = "/v1internal:onboardUser"
	PathFetchAvailableModels = "/v1internal:fetchAvailableModels"
	PathGenerateContent      = "/v1internal:generateContent"
	PathStreamGenerate       = "/v1internal:streamGenerateContent?alt=sse"

	DefaultUserAgentVersion = "1.1.25"
	AgyBuildCL              = "975399401"
	AgyAuthMethod           = "consumer"
)

var (
	ContentEndpoints      = []string{ProdEndpoint, DailyEndpoint}
	ProvisioningEndpoints = []string{ProdEndpoint, DailyEndpoint}
)

type Options struct {
	AccessToken      string
	UserAgentVersion string
	Timeout          time.Duration
	HTTPClient       *http.Client
}

// Client implements the HTTPS JSON/SSE transport used by current agy.
type Client struct {
	httpClient            *http.Client
	transport             *http.Transport
	accessToken           string
	userAgent             string
	contentEndpoints      []string
	provisioningEndpoints []string
	defaultHeader         http.Header
}

type ClientMetadata struct {
	IdeType       int    `json:"ideType"`
	IdeVersion    string `json:"ideVersion,omitempty"`
	PluginVersion string `json:"pluginVersion,omitempty"`
	Platform      int    `json:"platform"`
	UpdateChannel string `json:"updateChannel,omitempty"`
	DuetProject   string `json:"duetProject,omitempty"`
	PluginType    int    `json:"pluginType"`
	IdeName       string `json:"ideName,omitempty"`
}

type LoadCodeAssistRequest struct {
	Metadata ClientMetadata `json:"metadata"`
	Mode     int            `json:"mode"`
}

type OnboardUserRequest struct {
	TierID   string         `json:"tierId"`
	Metadata ClientMetadata `json:"metadata"`
}

type Response struct {
	Endpoint   string
	StatusCode int
	Header     http.Header
	Body       []byte
}

type HTTPError struct {
	Endpoint   string
	StatusCode int
	Status     string
	Body       string
	Header     http.Header
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("Cloud Code request to %s failed (%s): %s", e.Endpoint, e.Status, e.Body)
}

type RequestOptions struct {
	Headers http.Header
}

type SSEEvent struct {
	Event string
	Data  []byte
	ID    string
	Retry time.Duration
}

var defaultTransport = &http.Transport{
	TLSClientConfig:     &tls.Config{},
	MaxIdleConns:        1000,
	MaxIdleConnsPerHost: 500,
	IdleConnTimeout:     90 * time.Second,
}

func SharedTransport() *http.Transport {
	return defaultTransport
}

func New(options Options) *Client {
	if options.UserAgentVersion == "" {
		options.UserAgentVersion = DefaultUserAgentVersion
	}

	var transport *http.Transport
	client := options.HTTPClient
	if client == nil {
		transport = defaultTransport
		client = &http.Client{Transport: transport, Timeout: options.Timeout}
	}

	// Header set matches agy 1.1.25 wire ground truth exactly (MITM capture
	// 2026-09-03, .reference/agy-headers-mitm-20260903.txt): agy sends only
	// Authorization, Content-Type, Accept-Encoding: gzip and User-Agent on
	// Cloud Code requests — no X-Client-*, no x-goog-api-client, no Accept.
	userAgent := agyUserAgent(options.UserAgentVersion)
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+options.AccessToken)
	header.Set("Content-Type", "application/json")
	header.Set("Accept-Encoding", "gzip")
	header.Set("User-Agent", userAgent)

	return &Client{
		httpClient:            client,
		transport:             transport,
		accessToken:           options.AccessToken,
		userAgent:             userAgent,
		contentEndpoints:      append([]string(nil), ContentEndpoints...),
		provisioningEndpoints: append([]string(nil), ProvisioningEndpoints...),
		defaultHeader:         header,
	}
}

func (c *Client) CloseIdleConnections() {
	c.httpClient.CloseIdleConnections()
}

func Metadata(projectID string) ClientMetadata {
	return ClientMetadata{
		IdeType:     9,
		Platform:    platformEnum(),
		DuetProject: projectID,
		PluginType:  2,
	}
}

func (c *Client) LoadCodeAssist(ctx context.Context, projectID string) (Response, error) {
	request := LoadCodeAssistRequest{Metadata: Metadata(projectID), Mode: 1}
	return c.DoJSON(ctx, c.provisioningEndpoints, PathLoadCodeAssist, request, RequestOptions{})
}

func (c *Client) OnboardUser(ctx context.Context, tierID, projectID string) (Response, error) {
	request := OnboardUserRequest{TierID: tierID, Metadata: Metadata(projectID)}
	return c.DoJSON(ctx, c.contentEndpoints, PathOnboardUser, request, RequestOptions{})
}

func (c *Client) FetchAvailableModels(ctx context.Context, projectID string) (Response, error) {
	request := map[string]string{}
	if projectID != "" {
		request["project"] = projectID
	}
	return c.DoJSON(ctx, c.contentEndpoints, PathFetchAvailableModels, request, RequestOptions{})
}

func (c *Client) GenerateContent(ctx context.Context, payload any, options RequestOptions) (Response, error) {
	return c.DoJSON(ctx, c.contentEndpoints, PathGenerateContent, payload, options)
}

func (c *Client) StreamGenerateContent(ctx context.Context, payload any, options RequestOptions, consume func(SSEEvent) error) (Response, error) {
	return c.DoSSE(ctx, c.contentEndpoints, PathStreamGenerate, payload, options, consume)
}

func (c *Client) DoJSON(ctx context.Context, endpoints []string, path string, payload any, options RequestOptions) (Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return Response{}, fmt.Errorf("encode Cloud Code request: %w", err)
	}
	var failures []error
	for _, endpoint := range endpoints {
		request, err := c.newRequest(ctx, endpoint, path, body, options)
		if err != nil {
			return Response{}, err
		}
		response, err := c.httpClient.Do(request)
		if err != nil {
			failures = append(failures, fmt.Errorf("Cloud Code request to %s: %w", endpoint, err))
			continue
		}
		result, responseErr := readResponse(endpoint, response)
		if responseErr != nil {
			failures = append(failures, responseErr)
			continue
		}
		return result, nil
	}
	return Response{}, errors.Join(failures...)
}

func (c *Client) DoSSE(ctx context.Context, endpoints []string, path string, payload any, options RequestOptions, consume func(SSEEvent) error) (Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return Response{}, fmt.Errorf("encode Cloud Code streaming request: %w", err)
	}
	var failures []error
	for _, endpoint := range endpoints {
		request, err := c.newRequest(ctx, endpoint, path, body, options)
		if err != nil {
			return Response{}, err
		}
		response, err := c.httpClient.Do(request)
		if err != nil {
			failures = append(failures, fmt.Errorf("Cloud Code stream to %s: %w", endpoint, err))
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			result, responseErr := readResponse(endpoint, response)
			if responseErr != nil {
				failures = append(failures, responseErr)
			} else {
				failures = append(failures, fmt.Errorf("unexpected streaming response: %#v", result))
			}
			continue
		}
		result := Response{Endpoint: endpoint, StatusCode: response.StatusCode, Header: response.Header}
		streamReader, streamErr := maybeGunzip(response)
		if streamErr != nil {
			failures = append(failures, fmt.Errorf("open Cloud Code stream from %s: %w", endpoint, streamErr))
			continue
		}
		err = ParseSSE(streamReader, consume)
		closeErr := response.Body.Close()
		if err != nil {
			return result, fmt.Errorf("parse Cloud Code SSE stream: %w", err)
		}
		if closeErr != nil {
			return result, fmt.Errorf("close Cloud Code SSE stream: %w", closeErr)
		}
		return result, nil
	}
	return Response{}, errors.Join(failures...)
}

func (c *Client) newRequest(ctx context.Context, endpoint, path string, body []byte, options RequestOptions) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Cloud Code request: %w", err)
	}
	request.Header = c.defaultHeader.Clone()
	for name, values := range options.Headers {
		request.Header.Del(name)
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	return request, nil
}

// maybeGunzip returns a decompressed view of response.Body when upstream
// honored our Accept-Encoding: gzip. Setting that header manually disables
// net/http's automatic decompression, so the client handles it here.
func maybeGunzip(response *http.Response) (io.Reader, error) {
	if !strings.EqualFold(response.Header.Get("Content-Encoding"), "gzip") {
		return response.Body, nil
	}
	return gzip.NewReader(response.Body)
}

func readResponse(endpoint string, response *http.Response) (Response, error) {
	defer response.Body.Close()
	bodyReader, err := maybeGunzip(response)
	if err != nil {
		return Response{}, fmt.Errorf("open Cloud Code response from %s: %w", endpoint, err)
	}
	body, err := io.ReadAll(io.LimitReader(bodyReader, 64<<20))
	if err != nil {
		return Response{}, fmt.Errorf("read Cloud Code response from %s: %w", endpoint, err)
	}
	result := Response{
		Endpoint:   endpoint,
		StatusCode: response.StatusCode,
		Header:     response.Header,
		Body:       body,
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return result, &HTTPError{
			Endpoint:   endpoint,
			StatusCode: response.StatusCode,
			Status:     response.Status,
			Body:       strings.TrimSpace(string(body)),
			Header:     response.Header,
		}
	}
	return result, nil
}

var sseBufferPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

// ParseSSE implements the event-stream field and multi-line data rules used by
// agy. A final unterminated event is dispatched at EOF.
func ParseSSE(reader io.Reader, consume func(SSEEvent) error) error {
	buf := sseBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer sseBufferPool.Put(buf)

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var event SSEEvent
	hasData := false

	dispatch := func() error {
		if !hasData {
			event.Event = ""
			event.Retry = 0
			return nil
		}
		event.Data = make([]byte, buf.Len())
		copy(event.Data, buf.Bytes())

		if consume != nil {
			if err := consume(event); err != nil {
				return err
			}
		}
		event.Event = ""
		event.Data = nil
		event.Retry = 0
		buf.Reset()
		hasData = false
		return nil
	}

	for scanner.Scan() {
		line := bytes.TrimSuffix(scanner.Bytes(), []byte("\r"))
		if len(line) == 0 {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if bytes.HasPrefix(line, []byte(":")) {
			continue
		}
		field, value, found := bytes.Cut(line, []byte(":"))
		if found && bytes.HasPrefix(value, []byte(" ")) {
			value = value[1:]
		}
		switch string(field) {
		case "event":
			event.Event = string(value)
		case "data":
			if hasData {
				buf.WriteByte('\n')
			}
			buf.Write(value)
			hasData = true
		case "id":
			if !bytes.ContainsRune(value, '\x00') {
				event.ID = string(value)
			}
		case "retry":
			milliseconds, err := strconv.ParseInt(string(value), 10, 64)
			if err == nil && milliseconds >= 0 {
				event.Retry = time.Duration(milliseconds) * time.Millisecond
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return dispatch()
}

// agyUserAgent reproduces the agy 1.1.25 User-Agent literal captured in the
// 2026-09-03 MITM run: version is the agy CLI version, cl its build
// changelist, auth_method its OAuth account class.
func agyUserAgent(version string) string {
	osName := runtime.GOOS
	if osName == "windows" {
		osName = "win32"
	}
	return fmt.Sprintf("antigravity/cli/%s (aidev_client; os_type=%s; arch=%s; cl=%s; auth_method=%s)",
		version, osName, runtime.GOARCH, AgyBuildCL, AgyAuthMethod)
}

func platformEnum() int {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/amd64":
		return 1
	case "darwin/arm64":
		return 2
	case "linux/amd64":
		return 3
	case "linux/arm64":
		return 4
	case "windows/amd64":
		return 5
	default:
		return 0
	}
}
