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

// catalogSnapshotPath is the one place the snapshot file is named. Refreshing
// the catalog by renaming the file means changing this line and nothing else.
const catalogSnapshotPath = "testdata/catalog-2026-09-30.json"

func loadCatalogSnapshot(t *testing.T) catalogSnapshot {
	t.Helper()
	raw, err := os.ReadFile(catalogSnapshotPath)
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

// wireDrift diffs AnthropicWireIDs against a catalog id list: the claude-* ids
// IsAnthropicWire rejects, and the AnthropicWireIDs entries absent from the
// catalog, both sorted. It is the single implementation behind the snapshot
// tests here and the zen_live test, so the two cannot disagree about what
// drift means.
func wireDrift(catalog []string) (unclaimed, stale []string) {
	live := make(map[string]bool, len(catalog))
	for _, id := range catalog {
		live[id] = true
		if strings.HasPrefix(id, "claude-") && !IsAnthropicWire(id) {
			unclaimed = append(unclaimed, id)
		}
	}
	for _, id := range AnthropicWireIDs {
		if !live[id] {
			stale = append(stale, id)
		}
	}
	sort.Strings(unclaimed)
	sort.Strings(stale)
	return unclaimed, stale
}

// TestAnthropicWireIDsCoverLiveClaudeModels is the regression guard for the
// bug class: a live claude-* id that AnthropicWireIDs does not claim is
// reported by the proxy as forwardable-looking but resolves to WireNone, so
// the request falls through to a gateway that cannot speak the Anthropic wire.
func TestAnthropicWireIDsCoverLiveClaudeModels(t *testing.T) {
	snap := loadCatalogSnapshot(t)
	unclaimed, _ := wireDrift(snap.IDs)
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
	_, stale := wireDrift(snap.IDs)
	if len(stale) > 0 {
		t.Errorf("AnthropicWireIDs entries not in the catalog snapshot: %v\n"+
			"either the id was retired upstream or the snapshot is stale", stale)
	}
}
