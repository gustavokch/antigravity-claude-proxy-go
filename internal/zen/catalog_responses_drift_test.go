package zen

import (
	"sort"
	"strings"
	"testing"
)

// responsesWireFamilies are the catalog id prefixes the docs endpoint table
// serves over /v1/responses.
var responsesWireFamilies = []string{"gpt-", "grok-", "muse-"}

// unclaimedResponsesFamilyIDs are catalog ids inside a Responses-wire family
// that ResponsesWireIDs deliberately does not claim, each with the reason. A
// family id that is neither claimed nor listed here fails the drift test, so a
// new upstream id is a reviewable decision instead of a silent fall-through.
var unclaimedResponsesFamilyIDs = map[string]string{
	"muse-spark-1.2-contributor-free": "in /zen/v1/models but absent from the docs endpoint table (2026-09-30): its wire is unverified, so it stays fail-closed",
}

// responsesDrift diffs ResponsesWireIDs against a catalog id list: the
// gpt-*/grok-*/muse-* ids WireFor does not route to the Responses wire and that
// are not acknowledged above, and the ResponsesWireIDs entries absent from the
// catalog, both sorted. It is the single implementation behind the snapshot
// tests here and the zen_live test, like wireDrift for the Anthropic list.
func responsesDrift(catalog []string) (unclaimed, stale []string) {
	live := make(map[string]bool, len(catalog))
	for _, id := range catalog {
		live[id] = true
		inFamily := false
		for _, prefix := range responsesWireFamilies {
			if strings.HasPrefix(id, prefix) {
				inFamily = true
				break
			}
		}
		if !inFamily {
			continue
		}
		if _, wire := WireFor(id); wire == WireResponses {
			continue
		}
		if _, acknowledged := unclaimedResponsesFamilyIDs[id]; acknowledged {
			continue
		}
		unclaimed = append(unclaimed, id)
	}
	for _, id := range ResponsesWireIDs {
		if !live[id] {
			stale = append(stale, id)
		}
	}
	sort.Strings(unclaimed)
	sort.Strings(stale)
	return unclaimed, stale
}

func TestResponsesWireIDsCoverLiveFamilies(t *testing.T) {
	unclaimed, _ := responsesDrift(loadCatalogSnapshot(t).IDs)
	if len(unclaimed) > 0 {
		t.Errorf("live gpt-*/grok-*/muse-* ids not routed to the Responses wire: %v\n"+
			"add them to ResponsesWireIDs, or to unclaimedResponsesFamilyIDs with the reason", unclaimed)
	}
}

func TestResponsesWireIDsHaveNoStaleEntries(t *testing.T) {
	_, stale := responsesDrift(loadCatalogSnapshot(t).IDs)
	if len(stale) > 0 {
		t.Errorf("ResponsesWireIDs entries not in the catalog snapshot: %v\n"+
			"either the id was retired upstream or the snapshot is stale", stale)
	}
}

// An exemption that outlives its reason hides the drift it was written for.
func TestUnclaimedResponsesExemptionsStayHonest(t *testing.T) {
	live := map[string]bool{}
	for _, id := range loadCatalogSnapshot(t).IDs {
		live[id] = true
	}
	for id := range unclaimedResponsesFamilyIDs {
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
	lists := []struct {
		name string
		ids  []string
	}{
		{"AnthropicWireIDs", AnthropicWireIDs},
		{"ChatWireIDs", ChatWireIDs},
		{"ResponsesWireIDs", ResponsesWireIDs},
	}
	owner := map[string]string{}
	for _, list := range lists {
		for _, id := range list.ids {
			key := strings.ToLower(id)
			if prev, dup := owner[key]; dup {
				t.Errorf("id %q is in both %s and %s; init() keeps only the last writer", id, prev, list.name)
			}
			owner[key] = list.name
		}
	}
}
