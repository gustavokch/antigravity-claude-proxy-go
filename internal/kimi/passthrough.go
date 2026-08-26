package kimi

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// ForwardMessages transparently forwards an /v1/messages request to a Kimi
// gateway. It rewrites Authorization, preserves the Anthropic version/beta
// headers the client sent, and re-emits the JSON body from `body` (so the
// caller can mutate it before forwarding).
//
// On proxy error, it writes a 502 with an `api_error` body so the client
// receives a structured response matching the rest of the proxy.
func ForwardMessages(w http.ResponseWriter, r *http.Request, baseURL, apiKey string, body []byte) {
	target, err := url.Parse(NormalizeBaseURL(baseURL) + "/v1/messages")
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "Invalid Kimi target URL: "+err.Error())
		return
	}

	proxy := &httputil.ReverseProxy{
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
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Del("x-api-key")

			// Forward Anthropic protocol headers if the client sent them.
			if av := r.Header.Get("anthropic-version"); av != "" {
				req.Header.Set("anthropic-version", av)
			}
			if ab := r.Header.Get("anthropic-beta"); ab != "" {
				req.Header.Set("anthropic-beta", ab)
			}
		},
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, proxyErr error) {
			slog.Default().Error("kimi upstream proxy error", "error", proxyErr, "url", target.String())
			writeAPIError(rw, http.StatusBadGateway, "api_error", "Kimi upstream error: "+proxyErr.Error())
		},
	}

	proxy.ServeHTTP(w, r)
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
