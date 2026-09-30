//go:build zen_live

package zen

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// TestAnthropicWireIDsLiveCatalog is the same drift check as
// TestAnthropicWireIDsCoverLiveClaudeModels /
// TestAnthropicWireIDsHaveNoStaleEntries, but against today's catalog instead
// of the checked-in snapshot. It needs the network, so it is excluded from
// `go test ./...` by the build tag. Run it with:
//
//	go test -tags zen_live ./internal/zen/ -run TestAnthropicWireIDsLiveCatalog -v
//
// When it fails, refresh testdata/catalog-2026-09-30.json from the source URL
// recorded in that file and reconcile AnthropicWireIDs with the diff.
func TestAnthropicWireIDsLiveCatalog(t *testing.T) {
	resp, err := http.Get("https://opencode.ai/zen/v1/models")
	if err != nil {
		t.Fatalf("fetch live catalog: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live catalog status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode live catalog: %v", err)
	}

	var unclaimed, stale []string
	live := make(map[string]bool, len(body.Data))
	for _, m := range body.Data {
		live[m.ID] = true
		if strings.HasPrefix(m.ID, "claude-") && !IsAnthropicWire(m.ID) {
			unclaimed = append(unclaimed, m.ID)
		}
	}
	for _, id := range AnthropicWireIDs {
		if !live[id] {
			stale = append(stale, id)
		}
	}
	sort.Strings(unclaimed)
	sort.Strings(stale)

	if len(unclaimed) > 0 {
		t.Errorf("live claude-* ids not in AnthropicWireIDs: %v", unclaimed)
	}
	if len(stale) > 0 {
		t.Errorf("AnthropicWireIDs entries absent from the live catalog: %v", stale)
	}
}
