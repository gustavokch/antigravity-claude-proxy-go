package zen

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

// catalogSnapshot is the checked-in record of the live Zen model catalog, as
// captured in testdata/. It is the reference the wire-id lists are diffed
// against, so that a new upstream id shows up as a reviewable diff instead of
// a silent fall-through to a gateway that cannot speak the wire.
type catalogSnapshot struct {
	Fetched string   `json:"fetched"`
	Source  string   `json:"source"`
	IDs     []string `json:"ids"`
}

func loadCatalogSnapshot(t *testing.T) catalogSnapshot {
	t.Helper()
	raw, err := os.ReadFile("testdata/catalog-2026-09-30.json")
	if err != nil {
		t.Fatalf("read catalog snapshot: %v", err)
	}
	var snap catalogSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("parse catalog snapshot: %v", err)
	}
	if snap.Source == "" {
		t.Fatal("catalog snapshot has no source url")
	}
	return snap
}

// TestAnthropicWireIDsCoverLiveClaudeModels is the regression guard for the
// bug class: a live claude-* id that AnthropicWireIDs does not claim is
// reported by the proxy as forwardable-looking but resolves to WireNone, so
// the request falls through to a gateway that cannot speak the Anthropic wire.
func TestAnthropicWireIDsCoverLiveClaudeModels(t *testing.T) {
	snap := loadCatalogSnapshot(t)

	var unclaimed []string
	for _, id := range snap.IDs {
		if !strings.HasPrefix(id, "claude-") {
			continue
		}
		if !IsAnthropicWire(id) {
			unclaimed = append(unclaimed, id)
		}
	}
	sort.Strings(unclaimed)
	if len(unclaimed) > 0 {
		t.Errorf("live claude-* ids not in AnthropicWireIDs: %v\n"+
			"they will fall through to a gateway that cannot speak the Anthropic wire; "+
			"add them to AnthropicWireIDs and refresh the snapshot", unclaimed)
	}
}

// TestAnthropicWireIDsHaveNoStaleEntries keeps the list honest in the other
// direction: an id Zen no longer serves should not keep claiming the wire.
func TestAnthropicWireIDsHaveNoStaleEntries(t *testing.T) {
	snap := loadCatalogSnapshot(t)
	live := make(map[string]bool, len(snap.IDs))
	for _, id := range snap.IDs {
		live[id] = true
	}

	var stale []string
	for _, id := range AnthropicWireIDs {
		if !live[id] {
			stale = append(stale, id)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("AnthropicWireIDs entries not in the catalog snapshot: %v\n"+
			"either the id was retired upstream or the snapshot is stale", stale)
	}
}
