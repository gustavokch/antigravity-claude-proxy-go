// clientprobe answers two questions the client drift hook
// (scripts/client_drift.py) cannot answer from outside the proxy's own code:
//
//	clientprobe cloudcode -proxy http://127.0.0.1:PORT
//	    Sends loadCodeAssist, fetchAvailableModels and a streamGenerateContent
//	    built by the production format.Builder through the given HTTP proxy
//	    with the proxy's own Cloud Code client and transport, using a token
//	    Google rejects. The interceptor records the ClientHello, header order
//	    and body this build sends, so the hook can diff them against what agy
//	    sent through the same interceptor. No account is used.
//
//	clientprobe oauth
//	    Builds the sign-in URL the WebUI would open (auth.GetAuthorizationURL)
//	    and asks Google whether it accepts that client and loopback redirect,
//	    without following the redirect to a sign-in page.
//
// Both print one JSON object on stdout.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/format"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: clientprobe cloudcode -proxy URL | clientprobe oauth")
		os.Exit(2)
	}
	var result any
	var err error
	switch os.Args[1] {
	case "cloudcode":
		result, err = probeCloudCode(os.Args[2:])
	case "oauth":
		result, err = oauth(os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "clientprobe:", err)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
}

// cloudCodeResult maps each request to the status Google answered or the
// transport error. A rejection is expected: only the request bytes matter.
type cloudCodeResult map[string]string

func probeCloudCode(args []string) (cloudCodeResult, error) {
	flags := flag.NewFlagSet("cloudcode", flag.ContinueOnError)
	proxy := flags.String("proxy", "", "HTTP proxy that records the requests")
	timeout := flags.Duration("timeout", 20*time.Second, "per-request timeout")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	proxyURL, err := url.Parse(*proxy)
	if err != nil || proxyURL.Host == "" {
		return nil, fmt.Errorf("-proxy %q is not a proxy URL", *proxy)
	}

	// Clone keeps the empty tls.Config and the HTTP/2 opt-out, so the hello and
	// the request bytes are the ones the proxy sends; only the route changes.
	transport := cloudcode.SharedTransport().Clone()
	transport.Proxy = http.ProxyURL(proxyURL)
	client := cloudcode.New(cloudcode.Options{
		AccessToken: "clientprobe-not-a-token",
		Timeout:     *timeout,
		HTTPClient:  &http.Client{Transport: transport},
	})
	// The dispatcher's build path with the options a catalog entry for this
	// model carries (fetchAvailableModels publishes gemini-3.8-flash-high as
	// MODEL_PLACEHOLDER_M318); only the prompt is synthetic.
	generation := format.NewBuilder().BuildCloudCodeRequestWithModel(map[string]any{
		"model":      "gemini-3.8-flash-high",
		"max_tokens": 1024,
		"stream":     true,
		"messages":   []any{map[string]any{"role": "user", "content": "Reply with exactly OK"}},
	}, "clientprobe-project", "clientprobe@example.invalid", format.ModelOptions{
		SupportsThinking: true, ThinkingLevel: "HIGH", MaxOutputTokens: 65536,
		Enum: "MODEL_PLACEHOLDER_M318", APIProvider: "API_PROVIDER_GOOGLE_GEMINI",
	})
	result := cloudCodeResult{}
	for name, call := range map[string]func(context.Context) (cloudcode.Response, error){
		"loadCodeAssist": func(ctx context.Context) (cloudcode.Response, error) { return client.LoadCodeAssist(ctx) },
		"fetchAvailableModels": func(ctx context.Context) (cloudcode.Response, error) {
			return client.FetchAvailableModels(ctx, "clientprobe-project")
		},
		"streamGenerateContent": func(ctx context.Context) (cloudcode.Response, error) {
			return client.StreamGenerateContent(ctx, generation, cloudcode.RequestOptions{}, func(cloudcode.SSEEvent) error { return nil })
		},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		response, err := call(ctx)
		cancel()
		if err != nil {
			result[name] = err.Error()
			continue
		}
		result[name] = fmt.Sprint(response.StatusCode)
	}
	return result, nil
}

type oauthResult struct {
	// ClientIDPrefix is the project-number part of the client ID: enough to
	// tell the embedded clients apart in a report without copying the ID.
	ClientIDPrefix string `json:"client_id_prefix"`
	RedirectURI    string `json:"redirect_uri"`
	Accepted       bool   `json:"accepted"`
	Error          string `json:"error,omitempty"`
}

var oauthErrorCode = regexp.MustCompile(`[a-z]+(?:_[a-z]+)+`)

func oauth(args []string) (oauthResult, error) {
	flags := flag.NewFlagSet("oauth", flag.ContinueOnError)
	redirectURI := flags.String("redirect-uri", fmt.Sprintf("http://localhost:%d/oauth-callback", auth.DefaultCallbackPort), "loopback redirect to test")
	if err := flags.Parse(args); err != nil {
		return oauthResult{}, err
	}
	authURL, _, _, err := auth.GetAuthorizationURL(*redirectURI)
	if err != nil {
		return oauthResult{}, err
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		return oauthResult{}, err
	}
	clientID := parsed.Query().Get("client_id")
	prefix, _, _ := strings.Cut(clientID, "-")
	result := oauthResult{ClientIDPrefix: prefix, RedirectURI: *redirectURI}

	client := &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Get(authURL)
	if err != nil {
		return oauthResult{}, err
	}
	response.Body.Close()
	location, err := response.Location()
	if err != nil {
		return oauthResult{}, fmt.Errorf("authorization endpoint answered %d without a redirect", response.StatusCode)
	}
	if !strings.Contains(location.Path, "/oauth/error") {
		result.Accepted = true
		return result, nil
	}
	result.Error = decodeAuthError(location.Query().Get("authError"))
	return result, nil
}

// decodeAuthError returns the error code Google puts first in the authError
// parameter, a base64 protobuf whose field 1 is the code
// ("redirect_uri_mismatch", "invalid_client", ...).
func decodeAuthError(value string) string {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(value, "="))
	if err != nil {
		if raw, err = base64.StdEncoding.DecodeString(value); err != nil {
			return "undecodable authError"
		}
	}
	if len(raw) > 2 && raw[0] == 0x0a && int(raw[1]) <= len(raw)-2 {
		return string(raw[2 : 2+int(raw[1])])
	}
	if code := oauthErrorCode.Find(raw); code != nil {
		return string(code)
	}
	return "unrecognised authError"
}
