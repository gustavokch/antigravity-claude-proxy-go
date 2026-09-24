package zen

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ModelItem is one entry from Zen's /v1/models response. The catalog carries
// no context length, output limits, or pricing — id is the only usable datum.
type ModelItem struct {
	ID string `json:"id"`
}

// Wire identifies which Zen upstream endpoint a catalog id speaks.
type Wire int

const (
	// WireNone: not forwardable (Responses, Gemini-native, systemone, or
	// unknown ids).
	WireNone Wire = iota
	// WireAnthropic: POST /zen/v1/messages, transparent forward.
	WireAnthropic
	// WireChat: POST /zen/v1/chat/completions, translated from/to Anthropic.
	WireChat
)

// AnthropicWireIDs is the static allowlist of Zen catalog ids that speak the
// Anthropic /v1/messages wire format. Nothing in the catalog marks wire
// format, so this list is maintained against the docs endpoint table with the
// live catalog as authoritative.
var AnthropicWireIDs = []string{
	"claude-fable-5-1",
	"claude-fable-5",
	"claude-opus-5-5",
	"claude-opus-5",
	"claude-opus-4-8",
	"claude-opus-4-7",
	"claude-opus-4-6",
	"claude-opus-4-5",
	"claude-sonnet-5",
	"claude-sonnet-4-6",
	"claude-sonnet-4-5",
	"claude-sonnet-4",
	"claude-haiku-4-5",
	"qwen3.8-flash",
	"qwen3.6-plus",
	"qwen3.5-plus",
}

// ChatWireIDs is the static allowlist of Zen catalog ids that speak the
// OpenAI /v1/chat/completions wire format (docs endpoint table, cross-checked
// against the live catalog 2026-09-24). Requests to these ids are translated
// Anthropic→Chat Completions and the response back. Everything not in either
// list (gpt-*, grok-*, muse-* → /v1/responses; gemini-* → Gemini-native;
// jev-* → /v1/systemone) is not forwardable.
var ChatWireIDs = []string{
	"deepseek-v4.1-flash",
	"deepseek-v4-pro",
	"deepseek-v4-flash",
	"deepseek-v4-flash-vision-exp",
	"minimax-m3",
	"minimax-m2.7",
	"minimax-m2.5",
	"glm-5.3-flash",
	"glm-5.3",
	"glm-5.2",
	"glm-5.1",
	"glm-5",
	"kimi-k3",
	"kimi-k2.7-code",
	"kimi-k2.6",
	"kimi-k2.5",
	"big-pickle",
	"space-bunny-free",
	"mimo-v2.6-flash-free",
	"mimo-v2.5-free",
	"ling-3.0-flash-fin-free",
	"nemotron-3-ultra-free",
	"nemotron-3.5-lightning-free",
}

type wireEntry struct {
	canonical string
	wire      Wire
}

var wireSet map[string]wireEntry

func init() {
	wireSet = make(map[string]wireEntry, len(AnthropicWireIDs)+len(ChatWireIDs))
	for _, id := range AnthropicWireIDs {
		wireSet[strings.ToLower(id)] = wireEntry{id, WireAnthropic}
	}
	for _, id := range ChatWireIDs {
		wireSet[strings.ToLower(id)] = wireEntry{id, WireChat}
	}
}

// StripOpencodePrefix trims space and removes a leading "opencode/"
// (case-insensitive), the id format used in OpenCode client config.
func StripOpencodePrefix(s string) string {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) >= 9 && strings.EqualFold(trimmed[:9], "opencode/") {
		return strings.TrimSpace(trimmed[9:])
	}
	return trimmed
}

// WireFor maps id to its canonical catalog spelling and wire format after
// stripping an "opencode/" prefix and lowercasing. WireNone means the id is
// not forwardable.
func WireFor(id string) (canonical string, wire Wire) {
	cleaned := StripOpencodePrefix(id)
	if cleaned == "" {
		return "", WireNone
	}
	e, ok := wireSet[strings.ToLower(cleaned)]
	if !ok {
		return "", WireNone
	}
	return e.canonical, e.wire
}

// IsAnthropicWire reports whether id is in the Anthropic-wire subset.
func IsAnthropicWire(id string) bool {
	_, w := WireFor(id)
	return w == WireAnthropic
}

// IsForwardable reports whether the proxy can serve id through any
// supported Zen wire (Anthropic or Chat Completions).
func IsForwardable(id string) bool {
	_, w := WireFor(id)
	return w != WireNone
}

// Client fetches and caches the Zen model catalog. Safe for concurrent use.
type Client struct {
	httpClient *http.Client
	mu         sync.RWMutex
	cached     []ModelItem
	fetched    time.Time
	ttl        time.Duration
}

const defaultCatalogTTL = 5 * time.Minute

// DefaultClient is the package-level client used by the proxy.
var DefaultClient = NewClient(15*time.Second, defaultCatalogTTL)

// NewClient initializes a new Zen client with configurable timeout and cache TTL.
func NewClient(timeout, ttl time.Duration) *Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if ttl <= 0 {
		ttl = defaultCatalogTTL
	}
	return &Client{
		httpClient: &http.Client{Timeout: timeout},
		ttl:        ttl,
	}
}

type zenModelsResponse struct {
	Data []ModelItem `json:"data"`
}

// FetchModels GETs /v1/models from Zen, returns the parsed list, and caches
// it. The endpoint answers unauthenticated, so an empty apiKey is not an
// error — the WebUI can populate its picker before the operator pastes a key.
// A non-empty key is sent as Bearer for signature symmetry with Kimi.
func (c *Client) FetchModels(ctx context.Context, apiKey, baseURL string) ([]ModelItem, error) {
	base := strings.TrimRight(NormalizeBaseURL(baseURL), "/")
	url := base + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build Zen models request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := c.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call Zen /v1/models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("Zen /v1/models returned %d", resp.StatusCode)
	}
	var body zenModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode Zen /v1/models: %w", err)
	}
	c.mu.Lock()
	c.cached = body.Data
	c.fetched = time.Now()
	c.mu.Unlock()
	return body.Data, nil
}

// GetCachedModels returns the last FetchModels result, or an empty slice if
// nothing has been fetched yet.
func (c *Client) GetCachedModels() []ModelItem {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cached == nil {
		return []ModelItem{}
	}
	out := make([]ModelItem, len(c.cached))
	copy(out, c.cached)
	return out
}

// IsCacheValid reports whether the in-memory cache has valid non-expired models.
func (c *Client) IsCacheValid() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cached) > 0 && !c.fetched.IsZero() && time.Since(c.fetched) < c.ttl
}
