package mitm

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/andybalholm/brotli"
)

// Observation is everything the registry learns from one exchange. It carries
// no headers and no body text: RawID is hashed on entry to the registry, and
// Fields hold enum-like tokens only.
type Observation struct {
	Route  string
	RawID  string
	Status int
	Fields map[string]string
}

var (
	idSegment = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)
	// A field value is kept only if it looks like a plain token. Titles,
	// prompts and other free text never match.
	tokenValue = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,64}$`)
)

const (
	maxObservedBody = 1 << 20
	maxInflatedBody = 4 << 20
)

// routeVerb lowercases a standard HTTP method for use in a route name. Any
// other token comes from the client's request line and is free text, so it
// maps to "other" instead of reaching the registry and the API.
func routeVerb(method string) string {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions:
		return strings.ToLower(method)
	}
	return "other"
}

// classifyRoute maps a request to a masked route name and the raw session id
// found in the path. It returns an empty route for anything it does not track.
func classifyRoute(method, target string) (route, rawID string) {
	path := target
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	verb := routeVerb(method)
	segs := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case method == http.MethodPost && path == "/v1/sessions":
		return "sessions.create", ""
	case method == http.MethodGet && path == "/v1/environment_providers":
		return "environments.list", ""
	case len(segs) >= 4 && segs[0] == "v1" && segs[1] == "code" && segs[2] == "sessions" && idSegment.MatchString(segs[3]):
		rest := segs[4:]
		switch {
		case len(rest) == 0:
			return "code.session." + verb, segs[3]
		case len(rest) == 1 && rest[0] == "events":
			return "code.session.events." + verb, segs[3]
		case len(rest) == 2 && rest[0] == "events" && rest[1] == "stream":
			return "code.session.events.stream", segs[3]
		default:
			return "code.session.other", segs[3]
		}
	case len(segs) >= 3 && segs[0] == "v1" && segs[1] == "sessions" && idSegment.MatchString(segs[2]):
		if len(segs) == 3 {
			return "sessions." + verb, segs[2]
		}
		return "sessions.other", segs[2]
	}
	return "", ""
}

// parsesBody reports whether the response body of route is worth reading.
func parsesBody(route string) bool {
	switch route {
	case "sessions.create", "sessions.get", "code.session.get":
		return true
	}
	return false
}

// decodeBody inflates a gzip or brotli response body. It returns nil for
// missing, unknown or truncated encodings, so nothing is parsed then.
func decodeBody(body []byte, contentEncoding string) []byte {
	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "", "identity":
		return body
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil
		}
		inflated, err := io.ReadAll(io.LimitReader(zr, maxInflatedBody+1))
		if err != nil || len(inflated) > maxInflatedBody {
			return nil
		}
		return inflated
	case "br":
		br := brotli.NewReader(bytes.NewReader(body))
		inflated, err := io.ReadAll(io.LimitReader(br, maxInflatedBody+1))
		if err != nil || len(inflated) > maxInflatedBody {
			return nil
		}
		return inflated
	}
	return nil
}

// summarizeBody extracts the session id and enum-like status fields from a
// JSON response body. Unknown shapes yield nothing rather than an error.
func summarizeBody(body []byte, contentEncoding string) (rawID string, fields map[string]string) {
	body = decodeBody(body, contentEncoding)
	if body == nil {
		return "", nil
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", nil
	}
	// The read route wraps its payload under "response_shape".
	if inner, ok := doc["response_shape"].(map[string]any); ok {
		doc = inner
	}
	fields = map[string]string{}
	if id, ok := doc["id"].(string); ok && idSegment.MatchString(id) {
		rawID = id
	}
	set := func(name string, value any) {
		if s, ok := value.(string); ok && tokenValue.MatchString(s) {
			fields[name] = s
		}
	}
	set("environmentKind", doc["environment_kind"])
	set("sessionStatus", doc["session_status"])
	set("statusBucket", doc["status_bucket"])
	set("connectionStatus", doc["connection_status"])
	set("model", doc["configured_model"])
	if cfg, ok := doc["config"].(map[string]any); ok {
		if _, has := fields["model"]; !has {
			set("model", cfg["model"])
		}
	}
	if created, ok := doc["created_at"].(string); ok {
		fields["createdAt"] = created // parsed, and dropped if not RFC3339, by the registry
	}
	return rawID, fields
}
