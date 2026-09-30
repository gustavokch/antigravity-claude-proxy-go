package zen

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// defaultAnthropicVersion is sent when the client did not supply one.
const defaultAnthropicVersion = "2023-06-01"

// ForwardMessages transparently forwards an /v1/messages request to the Zen
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
	ForwardMessagesWithModify(w, r, baseURL, apiKey, body, modify)
}

// ForwardMessagesWithModify behaves like ForwardMessages and accepts a custom
// ModifyResponse function.
func ForwardMessagesWithModify(w http.ResponseWriter, r *http.Request, baseURL, apiKey string, body []byte, modify func(*http.Response) error) {
	forwardZenPath(w, r, baseURL, apiKey, body, "/v1/messages", modify, true)
}

// ForwardSystemOne transparently forwards a Jev systemone request to the Zen
// gateway: same rewrite rules as the messages wire — Bearer + x-api-key, the
// OpenCode harness identity, and a body re-emitted from `body` so the caller
// can mutate it before forwarding — against a different upstream path. The
// Anthropic version/beta headers are deliberately not sent: systemone is not
// an Anthropic wire and carries no message semantics.
//
// On proxy error, it writes a 502 with an `api_error` body so the client
// receives a structured response matching the rest of the proxy.
func ForwardSystemOne(w http.ResponseWriter, r *http.Request, baseURL, apiKey string, body []byte, modify func(*http.Response) error) {
	forwardZenPath(w, r, baseURL, apiKey, body, "/v1/systemone", modify, false)
}

// forwardZenPath is the shared ReverseProxy template behind
// ForwardMessagesWithModify and ForwardSystemOne. anthropicProtocol gates the
// Anthropic version/beta header block, which only the messages wire speaks.
func forwardZenPath(w http.ResponseWriter, r *http.Request, baseURL, apiKey string, body []byte, path string, modify func(*http.Response) error, anthropicProtocol bool) {
	target, err := url.Parse(NormalizeBaseURL(baseURL) + path)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "Invalid Zen target URL: "+err.Error())
		return
	}

	proxy := &httputil.ReverseProxy{
		FlushInterval:  -1,
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
			// also covered because we strip any prior auth header. The Zen
			// messages route authenticates via x-api-key only, so set that
			// too — otherwise a paid key would arrive as an unusable
			// Authorization header and get 401.
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Del("x-api-key")
			if apiKey != "" {
				req.Header.Set("x-api-key", apiKey)
			}

			// The outbound request starts as a clone of the inbound one, so the
			// client's anthropic-* headers arrive here regardless of what the
			// Director sets. Strip them first, then re-set the ones this wire
			// actually speaks; the messages branch below restores exactly what
			// the old unconditional code produced.
			req.Header.Del("anthropic-version")
			req.Header.Del("anthropic-beta")

			if anthropicProtocol {
				// Forward Anthropic protocol headers; default the version when
				// the client did not send one.
				if av := r.Header.Get("anthropic-version"); av != "" {
					req.Header.Set("anthropic-version", av)
				} else {
					req.Header.Set("anthropic-version", defaultAnthropicVersion)
				}
				if ab := r.Header.Get("anthropic-beta"); ab != "" {
					req.Header.Set("anthropic-beta", ab)
				}
			}

			// Claim the genuine OpenCode harness identity; the incoming
			// client's UA (claude-cli/…) is overwritten on purpose.
			ApplyHarnessHeaders(req)
		},
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, proxyErr error) {
			slog.Default().Error("zen upstream proxy error", "error", proxyErr, "url", target.String())
			writeAPIError(rw, http.StatusBadGateway, "api_error", "Zen upstream error: "+proxyErr.Error())
		},
	}

	// utls Bun handshake when the TLS disguise is on, on the one shared
	// transport so keep-alive pools survive across requests; unset keeps
	// the ReverseProxy default transport (a typed-nil *http.Transport
	// would panic in RoundTrip).
	if tr := Transport(); tr != nil {
		proxy.Transport = tr
	}

	proxy.ServeHTTP(w, r)
}

// writeAPIError mirrors the helper in internal/api/server.go so the zen
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
