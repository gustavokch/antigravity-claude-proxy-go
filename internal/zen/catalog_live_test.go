//go:build zen_live

package zen

import (
	"context"
	"testing"
	"time"
)

// TestAnthropicWireIDsLiveCatalog is the same drift check as
// TestAnthropicWireIDsCoverLiveClaudeModels /
// TestAnthropicWireIDsHaveNoStaleEntries, but against today's catalog instead
// of the checked-in snapshot. It needs the network, so it is excluded from
// `go test ./...` by the build tag. Run it with:
//
//	go test -tags zen_live ./internal/zen/ -run TestAnthropicWireIDsLiveCatalog -v
//
// When it fails, refresh the file named by catalogSnapshotPath from the source
// URL recorded in it and reconcile AnthropicWireIDs with the diff.
func TestAnthropicWireIDsLiveCatalog(t *testing.T) {
	// Generous on purpose: this is an on-demand check, and a slow IPv6 dial
	// alone can cost several seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// FetchModels is the production catalog path (harness headers, TLS client
	// routing, the real ModelItem parse), bounded by the ctx above.
	items, err := NewClient(30*time.Second, 0).FetchModels(ctx, "", DefaultBaseURL)
	if err != nil {
		t.Fatalf("fetch live catalog: %v", err)
	}
	ids := make([]string, len(items))
	for i, m := range items {
		ids[i] = m.ID
	}
	unclaimed, stale := wireDrift(ids)

	if len(unclaimed) > 0 {
		t.Errorf("live claude-* ids not in AnthropicWireIDs: %v", unclaimed)
	}
	if len(stale) > 0 {
		t.Errorf("AnthropicWireIDs entries absent from the live catalog: %v", stale)
	}
}
