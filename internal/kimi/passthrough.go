package kimi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"

	"antigravity-go-proxy/internal/transport"
)

// upstreamTransport is the one transport every Kimi forward shares, so
// keep-alive pools survive across requests. Without it the ReverseProxy uses
// http.DefaultTransport, which puts no bound on time to response headers: a
// gateway that accepts the connection and then stays silent holds the request,
// its goroutine, and its connection open until the client disconnects.
var upstreamTransport = transport.Upstream()

// ForwardMessages transparently forwards an /v1/messages request to a Kimi
// gateway. It rewrites Authorization, preserves the Anthropic version/beta
// headers the client sent, and re-emits the JSON body from `body` (so the
// caller can mutate it before forwarding).
//
// On proxy error, it writes a 502 with an `api_error` body so the client
// receives a structured response matching the rest of the proxy.
func ForwardMessages(w http.ResponseWriter, r *http.Request, baseURL, apiKey string, body []byte) {
	ForwardMessagesWithHook(w, r, baseURL, apiKey, body, nil)
}

// ForwardMessagesWithHook behaves like ForwardMessages and additionally calls
// onResponse with the upstream status code once a successful (status < 400)
// response arrives, before the body is copied, so callers can observe
// successful forwards. A nil hook is a plain forward.
func ForwardMessagesWithHook(w http.ResponseWriter, r *http.Request, baseURL, apiKey string, body []byte, onResponse func(int)) {
	var modify func(*http.Response) error
	if onResponse != nil {
		modify = func(resp *http.Response) error {
			if resp.StatusCode < 400 {
				onResponse(resp.StatusCode)
			}
			return nil
		}
	}
	ForwardMessagesWithModify(w, r, baseURL, apiKey, body, nil, modify)
}

// ForwardMessagesWithModify behaves like ForwardMessages and accepts a custom
// ModifyResponse function. identity, when non-nil, overrides request headers
// (e.g. KimiCLI fingerprint headers for OAuth-authenticated upstreams).
func ForwardMessagesWithModify(w http.ResponseWriter, r *http.Request, baseURL, apiKey string, body []byte, identity http.Header, modify func(*http.Response) error) {
	target, err := url.Parse(NormalizeBaseURL(baseURL) + "/v1/messages")
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "Invalid Kimi target URL: "+err.Error())
		return
	}

	proxy := &httputil.ReverseProxy{
		FlushInterval:  -1,
		Transport:      upstreamTransport,
		ModifyResponse: modify,
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.URL.Path = target.Path
			req.URL.RawQuery = target.RawQuery
			req.Host = target.Host

			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))

			// Always set Bearer; clients that sent x-api-key to the proxy are
			// also covered because we strip any prior auth header.
			setUpstreamHeaders(req.Header, apiKey, r.Header, identity)
		},
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, proxyErr error) {
			slog.Default().Error("kimi upstream proxy error", "error", proxyErr, "url", target.String())
			writeAPIError(rw, http.StatusBadGateway, "api_error", "Kimi upstream error: "+proxyErr.Error())
		},
	}

	proxy.ServeHTTP(w, r)
}

// setUpstreamHeaders sets what every Kimi request carries over whatever the
// header already holds: the Bearer credential and no x-api-key, the identity
// headers (KimiCLI's for a Kimi Code login), which override the client's, and
// the client's anthropic-version and anthropic-beta.
func setUpstreamHeaders(h http.Header, apiKey string, client, identity http.Header) {
	h.Set("Authorization", "Bearer "+apiKey)
	h.Del("x-api-key")
	for k, vs := range identity {
		if len(vs) > 0 {
			h.Set(k, vs[0])
		}
	}
	if av := client.Get("anthropic-version"); av != "" {
		h.Set("anthropic-version", av)
	}
	if ab := client.Get("anthropic-beta"); ab != "" {
		h.Set("anthropic-beta", ab)
	}
}

// countClient carries count_tokens calls on the forwards' shared transport.
var countClient = &http.Client{Transport: upstreamTransport}

// CountTokens posts an Anthropic count_tokens body to
// <base>/v1/messages/count_tokens with the headers a messages forward sends
// (setUpstreamHeaders). The caller reads and closes the response.
func CountTokens(ctx context.Context, baseURL, apiKey string, body []byte, client, identity http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, NormalizeBaseURL(baseURL)+"/v1/messages/count_tokens", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build Kimi count_tokens request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	setUpstreamHeaders(req.Header, apiKey, client, identity)
	resp, err := countClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call Kimi count_tokens: %w", err)
	}
	return resp, nil
}

// writeAPIError mirrors the helper in internal/api/server.go so the kimi
// package does not import the api package (which would be a cycle).
func writeAPIError(w http.ResponseWriter, status int, kind, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload := map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    kind,
			"message": msg,
		},
	}
	_ = json.NewEncoder(w).Encode(payload)
}
