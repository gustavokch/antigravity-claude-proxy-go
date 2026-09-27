package ccusage

import (
	"testing"
	"time"
)

func TestDeduper_Prune(t *testing.T) {
	old := fixtureEntry("msg_old", "req_old", false, 0, 10)
	old.Timestamp = dedupeTS.Add(-time.Hour)
	keep := fixtureEntry("msg_keep", "req_keep", false, 0, 10)
	side := fixtureEntry("msg_side", "req_side", true, 0, 5)
	requestless := fixtureEntry("msg_noreq", "", false, 0, 3)

	d := NewDeduper()
	for _, e := range []Entry{old, keep, side, requestless} {
		d.Add(e)
	}
	if n := d.Prune(dedupeTS); n != 1 {
		t.Fatalf("Prune dropped %d, want 1", n)
	}
	if n := d.Prune(dedupeTS); n != 0 {
		t.Fatalf("second Prune dropped %d, want 0", n)
	}
	got := d.Entries()
	if len(got) != 3 || got[0].MessageID != "msg_keep" || got[1].MessageID != "msg_side" || got[2].MessageID != "msg_noreq" {
		t.Fatalf("survivors = %+v", got)
	}

	// Survivors still dedupe through every route at their new positions.
	bigger := keep
	bigger.Output = 50
	if replaced, kept := d.Add(bigger); !kept || replaced == nil || replaced.Output != 10 {
		t.Errorf("exact key after prune: replaced=%v kept=%v", replaced, kept)
	}
	if _, kept := d.Add(requestless); kept {
		t.Error("requestless copy after prune was kept")
	}
	parent := fixtureEntry("msg_side", "req_other", false, 0, 5)
	if replaced, kept := d.Add(parent); !kept || replaced == nil || !replaced.IsSidechain {
		t.Errorf("replay route after prune: replaced=%v kept=%v", replaced, kept)
	}
	if d.Len() != 3 {
		t.Errorf("Len = %d, want 3", d.Len())
	}

	// A pruned entry added again is new: callers filter by the cutoff first.
	if _, kept := d.Add(old); !kept || d.Len() != 4 {
		t.Errorf("re-added pruned entry: kept=%v len=%d", kept, d.Len())
	}
}
