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

// AnthropicWireIDs is the static allowlist of Zen catalog ids that speak the
// Anthropic /v1/messages wire format (Phase 1 forward target). Nothing in the
// catalog marks wire format, so this list is maintained against the docs
// endpoint table with the live catalog as authoritative. Everything else
// (gpt-*, gemini-*, grok-*, deepseek-*, glm-*, minimax-*, kimi-*, muse-*,
// jev-*, big-pickle, *-free) is a different wire format and must never reach
// /zen/v1/messages.
var AnthropicWireIDs = []string{
	"claude-fable-5-1",
	"claude-fable-5",
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

var anthropicWireSet map[string]struct{}

func init() {
	anthropicWireSet = make(map[string]struct{}, len(AnthropicWireIDs))
	for _, id := range AnthropicWireIDs {
		anthropicWireSet[strings.ToLower(id)] = struct{}{}
	}
}

// stripOpencodePrefix removes a leading "opencode/" (case-insensitive), the
// id format used in OpenCode client config.
func stripOpencodePrefix(id string) string {
	if len(id) >= 9 && strings.EqualFold(id[:9], "opencode/") {
		return id[9:]
	}
	return id
}

// IsAnthropicWire reports whether id is in the Anthropic-wire subset, after
// stripping an "opencode/" prefix and lowercasing.
func IsAnthropicWire(id string) bool {
	id = stripOpencodePrefix(strings.TrimSpace(id))
	if id == "" {
		return false
	}
	_, ok := anthropicWireSet[strings.ToLower(id)]
	return ok
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
