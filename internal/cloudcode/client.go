package cloudcode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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

	PathLoadCodeAssist           = "/v1internal:loadCodeAssist"
	PathOnboardUser              = "/v1internal:onboardUser"
	PathFetchAvailableModels     = "/v1internal:fetchAvailableModels"
	PathRetrieveUserQuota        = "/v1internal:retrieveUserQuota"
	PathRetrieveUserQuotaSummary = "/v1internal:retrieveUserQuotaSummary"
	PathGenerateContent          = "/v1internal:generateContent"
	PathStreamGenerate           = "/v1internal:streamGenerateContent?alt=sse"

	DefaultUserAgentVersion = "1.4.10"
	AgyBuildCL              = "1009308193"
	AgyAuthMethod           = "consumer"
)

// Every Cloud Code call goes to daily, as agy 1.4.10 sends all of them there
// (.reference/agy-1.4.10-wire-20261030.txt). There is no cross-host fallback:
// agy shows none, and for generation a thought signature issued by one host is
// rejected by the other ("Corrupted thought signature"), so a fallback turns a
// retryable 429 into a permanent 400.
var (
	GenerationEndpoints   = []string{DailyEndpoint}
	ContentEndpoints      = []string{DailyEndpoint}
	ProvisioningEndpoints = []string{DailyEndpoint}
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
	generationEndpoints   []string
	provisioningEndpoints []string
	defaultHeader         http.Header
	timeout               time.Duration
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

// LoadCodeAssistRequest is the body agy 1.4.10 sends on every loadCodeAssist,
// byte for byte {"metadata":{"ideType":"ANTIGRAVITY"}}: the IDE type by enum
// name, and no platform, pluginType, duetProject or mode, even once agy knows
// its project (.reference/agy-1.4.10-wire-20261030.txt).
type LoadCodeAssistRequest struct {
	Metadata LoadCodeAssistMetadata `json:"metadata"`
}

type LoadCodeAssistMetadata struct {
	IdeType string `json:"ideType"`
}

// OnboardUserRequest keeps the older ClientMetadata form: no agy onboardUser
// request has been captured, because a capture needs an account that is not
// onboarded yet.
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

func collectHTTPErrors(err error, out *[]*HTTPError) {
	if err == nil {
		return
	}
	if httpErr, ok := err.(*HTTPError); ok {
		if httpErr != nil {
			*out = append(*out, httpErr)
		}
		return
	}
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			collectHTTPErrors(e, out)
		}
	case interface{ Unwrap() error }:
		collectHTTPErrors(u.Unwrap(), out)
	}
}

// FindHTTPError returns the most actionable *HTTPError in err's tree,
// walking both Unwrap() error and Unwrap() []error (errors.Join).
// Precedence: 429 (rotate with backoff) > 401/403 (invalidate the
// account) > 5xx > first found. Typed-nil *HTTPError values are skipped.
func FindHTTPError(err error) *HTTPError {
	if err == nil {
		return nil
	}
	var httpErrors []*HTTPError
	collectHTTPErrors(err, &httpErrors)
	if len(httpErrors) == 0 {
		return nil
	}
	for _, httpErr := range httpErrors {
		if httpErr.StatusCode == http.StatusTooManyRequests {
			return httpErr
		}
	}
	for _, httpErr := range httpErrors {
		if httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden {
			return httpErr
		}
	}
	for _, httpErr := range httpErrors {
		if httpErr.StatusCode >= 500 {
			return httpErr
		}
	}
	return httpErrors[0]
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

func newTransport(responseHeaderTimeout time.Duration) *http.Transport {
	return &http.Transport{
		TLSClientConfig:       &tls.Config{},
		MaxIdleConns:          1000,
		MaxIdleConnsPerHost:   500,
		IdleConnTimeout:       90 * time.Second,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}
}

var (
	defaultTransport = newTransport(0)
	transportsMu     sync.Mutex
	transports       = map[time.Duration]*http.Transport{
		0: defaultTransport,
	}
)

func transportForTimeout(timeout time.Duration) *http.Transport {
	if timeout < 0 {
		timeout = 0
	}
	transportsMu.Lock()
	defer transportsMu.Unlock()
	tr, ok := transports[timeout]
	if !ok {
		tr = newTransport(timeout)
		transports[timeout] = tr
	}
	return tr
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
		// No total Client.Timeout: it covers the full body read and kills
		// long SSE streams mid-generation (client then sees a stream that
		// ends without message_stop). Cancellation comes from the request
		// context. The --upstream-timeout value instead bounds time to
		// response headers (TTFB) via ResponseHeaderTimeout and bounds
		// unary (non-streaming) requests via a per-request context timeout
		// in DoJSON. Dial/TLS timeouts are local timers; they do not alter
		// the ClientHello, so the agy JA3/JA4 match is unaffected.
		//
		// Transports are cached per configured timeout duration so idle
		// connections and keep-alive pools are shared across accounts.
		transport = transportForTimeout(options.Timeout)
		client = &http.Client{Transport: transport}
	}

	// Header set matches agy's wire ground truth (MITM captures of 1.1.25,
	// .reference/agy-headers-mitm-20260903.txt, and of 1.4.10): agy sets only
	// Authorization, Content-Type and User-Agent on Cloud Code requests — no
	// X-Client-*, no x-goog-api-client, no Accept. Accept-Encoding: gzip is
	// net/http's own: the transport adds it after the request's headers and
	// decompresses the response. Setting it here would move it ahead of
	// Authorization on the wire.
	userAgent := agyUserAgent(options.UserAgentVersion)
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+options.AccessToken)
	header.Set("Content-Type", "application/json")
	header.Set("User-Agent", userAgent)

	return &Client{
		httpClient:            client,
		transport:             transport,
		accessToken:           options.AccessToken,
		userAgent:             userAgent,
		contentEndpoints:      append([]string(nil), ContentEndpoints...),
		generationEndpoints:   append([]string(nil), GenerationEndpoints...),
		provisioningEndpoints: append([]string(nil), ProvisioningEndpoints...),
		defaultHeader:         header,
		timeout:               options.Timeout,
	}
}

// Metadata is the ClientMetadata of an onboardUser request.
func Metadata(projectID string) ClientMetadata {
	return ClientMetadata{
		IdeType:     9,
		Platform:    platformEnum(),
		DuetProject: projectID,
		PluginType:  2,
	}
}

func (c *Client) LoadCodeAssist(ctx context.Context) (Response, error) {
	return c.LoadCodeAssistAt(ctx, c.provisioningEndpoints)
}

// LoadCodeAssistAt is LoadCodeAssist against an explicit endpoint list.
func (c *Client) LoadCodeAssistAt(ctx context.Context, endpoints []string) (Response, error) {
	request := LoadCodeAssistRequest{Metadata: LoadCodeAssistMetadata{IdeType: "ANTIGRAVITY"}}
	return c.DoJSON(ctx, endpoints, PathLoadCodeAssist, request, RequestOptions{})
}

func (c *Client) OnboardUser(ctx context.Context, tierID, projectID string) (Response, error) {
	request := OnboardUserRequest{TierID: tierID, Metadata: Metadata(projectID)}
	return c.DoJSON(ctx, c.contentEndpoints, PathOnboardUser, request, RequestOptions{})
}

func (c *Client) FetchAvailableModels(ctx context.Context, projectID string) (Response, error) {
	return c.FetchAvailableModelsAt(ctx, c.contentEndpoints, projectID)
}

// FetchAvailableModelsAt is FetchAvailableModels against an explicit endpoint list.
func (c *Client) FetchAvailableModelsAt(ctx context.Context, endpoints []string, projectID string) (Response, error) {
	request := map[string]string{}
	if projectID != "" {
		request["project"] = projectID
	}
	return c.DoJSON(ctx, endpoints, PathFetchAvailableModels, request, RequestOptions{})
}

// RetrieveUserQuotaSummary fetches the live per-bucket quota summary
// (remaining fractions + reset times). Unlike fetchAvailableModels'
// static quotaInfo, this RPC is the authoritative consumption signal.
func (c *Client) RetrieveUserQuotaSummary(ctx context.Context, projectID string) (Response, error) {
	request := map[string]string{}
	if projectID != "" {
		request["project"] = projectID
	}
	return c.DoJSON(ctx, c.contentEndpoints, PathRetrieveUserQuotaSummary, request, RequestOptions{})
}

// RetrieveUserQuota fetches per-model token-bucket quotas (model_id keyed).
func (c *Client) RetrieveUserQuota(ctx context.Context, projectID string) (Response, error) {
	request := map[string]string{}
	if projectID != "" {
		request["project"] = projectID
	}
	return c.DoJSON(ctx, c.contentEndpoints, PathRetrieveUserQuota, request, RequestOptions{})
}

func (c *Client) GenerateContent(ctx context.Context, payload any, options RequestOptions) (Response, error) {
	return c.DoJSON(ctx, c.generationEndpoints, PathGenerateContent, payload, options)
}

func (c *Client) StreamGenerateContent(ctx context.Context, payload any, options RequestOptions, consume func(SSEEvent) error) (Response, error) {
	return c.DoSSE(ctx, c.generationEndpoints, PathStreamGenerate, payload, options, consume)
}

func (c *Client) DoJSON(ctx context.Context, endpoints []string, path string, payload any, options RequestOptions) (Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return Response{}, fmt.Errorf("encode Cloud Code request: %w", err)
	}
	var failures []error
	for _, endpoint := range endpoints {
		result, err := c.doJSONAttempt(ctx, endpoint, path, body, options)
		if err == nil {
			return result, nil
		}
		var reqErr *requestCreationError
		if errors.As(err, &reqErr) {
			return Response{}, reqErr.err
		}
		failures = append(failures, err)
	}
	return Response{}, errors.Join(failures...)
}

type requestCreationError struct {
	err error
}

func (e *requestCreationError) Error() string {
	return e.err.Error()
}

func (c *Client) doJSONAttempt(ctx context.Context, endpoint, path string, body []byte, options RequestOptions) (Response, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	request, err := c.newRequest(ctx, endpoint, path, body, options)
	if err != nil {
		return Response{}, &requestCreationError{err: err}
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return Response{}, fmt.Errorf("Cloud Code request to %s: %w", endpoint, err)
	}
	return readResponse(endpoint, response)
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
		// agy streams the body of a streaming call (Transfer-Encoding:
		// chunked) where its unary calls send Content-Length. An unknown
		// length gives the same framing and header order; GetBody stays set,
		// so the request can still be replayed on a stale connection.
		request.ContentLength = -1
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
		err = ParseSSE(response.Body, consume)
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

func readResponse(endpoint string, response *http.Response) (Response, error) {
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
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
