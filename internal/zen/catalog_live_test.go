//go:build zen_live

package zen

import (
	"context"
	"testing"
	"time"
)

// liveCatalogIDs fetches today's catalog through FetchModels, the production
// catalog path (harness headers, TLS client routing, the real ModelItem parse),
// bounded by a generous context: this is an on-demand check, and a slow IPv6
// dial alone can cost several seconds.
func liveCatalogIDs(t *testing.T) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	items, err := NewClient(30*time.Second, 0).FetchModels(ctx, "", DefaultBaseURL)
	if err != nil {
		t.Fatalf("fetch live catalog: %v", err)
	}
	ids := make([]string, len(items))
	for i, m := range items {
		ids[i] = m.ID
	}
	return ids
}

// TestAnthropicWireIDsLiveCatalog is the same drift check as
// TestAnthropicWireIDsCoverLiveClaudeModels /
// TestAnthropicWireIDsHaveNoStaleEntries, but against today's catalog instead
// of the checked-in snapshot. It needs the network, so it is excluded from
// `go test ./...` by the build tag. Run it with:
//
//	go test -tags zen_live ./internal/zen/ -run 'WireIDsLiveCatalog' -v
//
// When it fails, refresh the file named by catalogSnapshotPath from the source
// URL recorded in it and reconcile AnthropicWireIDs with the diff.
func TestAnthropicWireIDsLiveCatalog(t *testing.T) {
	unclaimed, stale := wireDrift(liveCatalogIDs(t))

	if len(unclaimed) > 0 {
		t.Errorf("live claude-* ids not in AnthropicWireIDs: %v", unclaimed)
	}
	if len(stale) > 0 {
		t.Errorf("AnthropicWireIDs entries absent from the live catalog: %v", stale)
	}
}

// TestResponsesWireIDsLiveCatalog is the ResponsesWireIDs twin of the test
// above; responsesDrift is shared with the snapshot tests.
func TestResponsesWireIDsLiveCatalog(t *testing.T) {
	unclaimed, stale := responsesDrift(liveCatalogIDs(t))

	if len(unclaimed) > 0 {
		t.Errorf("live gpt-*/grok-*/muse-* ids not routed to the Responses wire: %v", unclaimed)
	}
	if len(stale) > 0 {
		t.Errorf("ResponsesWireIDs entries absent from the live catalog: %v", stale)
	}
}
