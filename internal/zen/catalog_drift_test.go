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
const catalogSnapshotPath = "testdata/catalog-2026-10-18.json"

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

const (
	reasonGoogleWire    = "docs endpoint table serves it on /inference/google/v1beta, a wire the proxy does not speak"
	reasonSystemoneWire = "docs endpoint table serves it on /inference/systemone, a typed-decision protocol the Messages router cannot carry; it reaches Zen through the proxy's POST /v1/systemone passthrough"
	reasonUndocumented  = "in /zen/v1/models but absent from the docs endpoint table (2026-10-18): its wire is unverified, so it stays fail-closed"
)

// unforwardableCatalogIDs are catalog ids the proxy deliberately leaves on
// WireNone, each with the reason. A catalog id that is neither routed by
// WireFor nor listed here fails the drift test, so a new upstream id is a
// reviewable decision instead of an "Unsupported wire" badge nobody chose.
var unforwardableCatalogIDs = map[string]string{
	"claude-opus-5-5-backup": reasonUndocumented,
	"gemini-3-flash":         reasonGoogleWire,
	"gemini-3.1-pro":         reasonGoogleWire,
	"gemini-3.5-flash":       reasonGoogleWire,
	"gemini-3.5-flash-lite":  reasonGoogleWire,
	"gemini-3.6-flash":       reasonGoogleWire,
	"gemini-3.7-flash":       reasonGoogleWire,
	"gemini-3.8-flash":       reasonGoogleWire,
	"gemini-4-argon":         reasonGoogleWire,
	"jev-1.13":               reasonSystemoneWire,
	"jev-1.13-free":          reasonSystemoneWire,
	"test":                   reasonUndocumented,
}

// catalogDrift diffs the wire lists against a catalog id list: the catalog ids
// WireFor leaves on WireNone that unforwardableCatalogIDs does not acknowledge,
// and the wire-list entries absent from the catalog, both sorted. It is the
// single implementation behind the snapshot tests here and the zen_live test,
// so the two cannot disagree about what drift means.
func catalogDrift(catalog []string) (unrouted, stale []string) {
	live := make(map[string]bool, len(catalog))
	for _, id := range catalog {
		live[id] = true
		if _, wire := WireFor(id); wire != WireNone {
			continue
		}
		if _, acknowledged := unforwardableCatalogIDs[id]; acknowledged {
			continue
		}
		unrouted = append(unrouted, id)
	}
	for _, list := range wireLists {
		for _, id := range list.ids {
			if !live[id] {
				stale = append(stale, id)
			}
		}
	}
	sort.Strings(unrouted)
	sort.Strings(stale)
	return unrouted, stale
}

// TestCatalogIDsAreRouted is the regression guard for the bug class: a live id
// in no wire list resolves to WireNone, discovery marks it "Unsupported wire",
// and an allowlisted request for it falls through to a gateway that was never
// asked for that model.
func TestCatalogIDsAreRouted(t *testing.T) {
	unrouted, _ := catalogDrift(loadCatalogSnapshot(t).IDs)
	if len(unrouted) > 0 {
		t.Errorf("catalog ids on WireNone: %v\n"+
			"add each to the wire list the docs endpoint table names, "+
			"or to unforwardableCatalogIDs with the reason", unrouted)
	}
}

// TestWireIDListsHaveNoStaleEntries keeps the lists honest in the other
// direction: an id Zen no longer serves should not keep claiming a wire.
func TestWireIDListsHaveNoStaleEntries(t *testing.T) {
	_, stale := catalogDrift(loadCatalogSnapshot(t).IDs)
	if len(stale) > 0 {
		t.Errorf("wire-list entries not in the catalog snapshot: %v\n"+
			"either the id was retired upstream or the snapshot is stale", stale)
	}
}

// An exemption that outlives its reason hides the drift it was written for.
func TestUnforwardableExemptionsStayHonest(t *testing.T) {
	live := map[string]bool{}
	for _, id := range loadCatalogSnapshot(t).IDs {
		live[id] = true
	}
	for id := range unforwardableCatalogIDs {
		if !live[id] {
			t.Errorf("exempt id %q is no longer in the catalog snapshot; drop the exemption", id)
		}
		if _, wire := WireFor(id); wire != WireNone {
			t.Errorf("exempt id %q is now routed (wire %v); drop the exemption", id, wire)
		}
	}
}

// init() keys all three lists into one map, so an id listed twice silently
// takes the wire of whichever list is registered last.
func TestWireIDListsAreDisjoint(t *testing.T) {
	owner := map[string]string{}
	for _, list := range wireLists {
		for _, id := range list.ids {
			key := strings.ToLower(id)
			if prev, dup := owner[key]; dup {
				t.Errorf("id %q is in both %s and %s; init() keeps only the last writer", id, prev, list.name)
			}
			owner[key] = list.name
		}
	}
}
