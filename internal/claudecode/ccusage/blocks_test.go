package ccusage

import (
	"math"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

// blockEntry is an entry with 1000 input and 500 output tokens and a logged
// cost of $0.01.
func blockEntry(t *testing.T, ts string) Entry {
	cost := 0.01
	return Entry{
		Timestamp:    mustTime(t, ts),
		Model:        "claude-test",
		DisplayModel: "claude-test",
		Input:        1000,
		Output:       500,
		CostUSD:      &cost,
	}
}

func blockEntries(t *testing.T, ts ...string) []Entry {
	out := make([]Entry, len(ts))
	for i, s := range ts {
		out[i] = blockEntry(t, s)
	}
	return out
}

// farFuture keeps every block inactive.
var farFuture = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

func identify(entries []Entry, now time.Time, anchors ...time.Time) []Block {
	return IdentifyBlocks(entries, DefaultBlockDuration, now, anchors, CostModeDisplay, nil)
}

type blockShape struct {
	id      string
	start   string
	end     string
	gap     bool
	entries int
}

func checkBlocks(t *testing.T, got []Block, want []blockShape) {
	t.Helper()
	if len(got) != len(want) {
		for _, b := range got {
			t.Logf("block %s %s..%s gap=%v entries=%d", b.ID, b.Start.Format(time.RFC3339), b.End.Format(time.RFC3339), b.IsGap, len(b.Entries))
		}
		t.Fatalf("got %d blocks, want %d", len(got), len(want))
	}
	for i, w := range want {
		b := got[i]
		if b.ID != w.id || b.IsGap != w.gap || len(b.Entries) != w.entries {
			t.Errorf("block %d: id=%s gap=%v entries=%d, want id=%s gap=%v entries=%d",
				i, b.ID, b.IsGap, len(b.Entries), w.id, w.gap, w.entries)
		}
		if w.start != "" && !b.Start.Equal(mustTime(t, w.start)) {
			t.Errorf("block %d: start %s, want %s", i, b.Start.Format(time.RFC3339Nano), w.start)
		}
		if w.end != "" && !b.End.Equal(mustTime(t, w.end)) {
			t.Errorf("block %d: end %s, want %s", i, b.End.Format(time.RFC3339Nano), w.end)
		}
	}
}

func TestIdentifyBlocks_Empty(t *testing.T) {
	if got := identify(nil, farFuture); got != nil {
		t.Errorf("got %v, want nil", got)
	}
	if got := IdentifyBlocks(blockEntries(t, "2025-01-10T10:00:00Z"), 0, farFuture, nil, CostModeDisplay, nil); got != nil {
		t.Errorf("zero duration: got %v, want nil", got)
	}
}

func TestIdentifyBlocks_FloorsToUTCHourAndAggregates(t *testing.T) {
	later := blockEntry(t, "2025-01-10T12:40:00Z")
	later.Model, later.DisplayModel = "claude-other", "claude-other-fast"
	later.CacheCreate, later.CacheRead = 200, 300
	reset := mustTime(t, "2025-01-10T15:00:00Z")
	later.UsageLimitResetAt = &reset
	synthetic := blockEntry(t, "2025-01-10T11:00:00Z")
	synthetic.Model, synthetic.DisplayModel = syntheticModel, ""
	// Out of order on purpose: IdentifyBlocks sorts.
	entries := []Entry{later, blockEntry(t, "2025-01-10T10:37:12.345Z"), synthetic, blockEntry(t, "2025-01-10T12:00:00Z")}

	blocks := identify(entries, farFuture)
	checkBlocks(t, blocks, []blockShape{
		{id: "2025-01-10T10:00:00.000Z", start: "2025-01-10T10:00:00Z", end: "2025-01-10T15:00:00Z", entries: 4},
	})
	b := blocks[0]
	if b.InputTokens != 4000 || b.OutputTokens != 2000 || b.CacheCreateTokens != 200 || b.CacheReadTokens != 300 {
		t.Errorf("tokens = %d/%d/%d/%d", b.InputTokens, b.OutputTokens, b.CacheCreateTokens, b.CacheReadTokens)
	}
	if b.TotalTokens() != 6500 {
		t.Errorf("TotalTokens = %d, want 6500", b.TotalTokens())
	}
	if math.Abs(b.CostUSD-0.04) > 1e-12 {
		t.Errorf("CostUSD = %v, want 0.04", b.CostUSD)
	}
	if len(b.Models) != 2 || b.Models[0] != "claude-test" || b.Models[1] != "claude-other-fast" {
		t.Errorf("Models = %v", b.Models)
	}
	if b.UsageLimitResetAt == nil || !b.UsageLimitResetAt.Equal(reset) {
		t.Errorf("UsageLimitResetAt = %v, want %v", b.UsageLimitResetAt, reset)
	}
	if !b.ActualEnd.Equal(mustTime(t, "2025-01-10T12:40:00Z")) {
		t.Errorf("ActualEnd = %v", b.ActualEnd)
	}
	if b.IsActive || b.Anchored {
		t.Errorf("IsActive=%v Anchored=%v, want false", b.IsActive, b.Anchored)
	}
	// The input is left in its original order.
	if !entries[0].Timestamp.Equal(later.Timestamp) {
		t.Errorf("input slice was reordered")
	}
}

func TestIdentifyBlocks_CalculatesCost(t *testing.T) {
	e := blockEntry(t, "2025-01-10T10:00:00Z")
	e.Model, e.CostUSD = "test-model", nil
	p := testPricer(testPrice)
	blocks := IdentifyBlocks([]Entry{e}, DefaultBlockDuration, farFuture, nil, CostModeAuto, p)
	if want := EntryCost(e, CostModeAuto, p); want == 0 || blocks[0].CostUSD != want {
		t.Errorf("CostUSD = %v, want %v (non-zero)", blocks[0].CostUSD, want)
	}
}

func TestIdentifyBlocks_SplitAtDuration(t *testing.T) {
	tests := []struct {
		name string
		ts   []string
		want []blockShape
	}{
		{
			name: "exactly 5h after start stays",
			ts:   []string{"2025-01-10T10:00:00Z", "2025-01-10T12:00:00Z", "2025-01-10T14:00:00Z", "2025-01-10T15:00:00Z"},
			want: []blockShape{{id: "2025-01-10T10:00:00.000Z", entries: 4}},
		},
		{
			name: "5h and 1ms after start splits",
			ts:   []string{"2025-01-10T10:00:00Z", "2025-01-10T12:00:00Z", "2025-01-10T14:00:00Z", "2025-01-10T15:00:00.001Z"},
			want: []blockShape{
				{id: "2025-01-10T10:00:00.000Z", entries: 3},
				{id: "2025-01-10T15:00:00.000Z", start: "2025-01-10T15:00:00Z", end: "2025-01-10T20:00:00Z", entries: 1},
			},
		},
		{
			name: "exactly 5h since last entry stays",
			ts:   []string{"2025-01-10T10:00:00Z", "2025-01-10T15:00:00Z"},
			want: []blockShape{{id: "2025-01-10T10:00:00.000Z", entries: 2}},
		},
		{
			name: "split by start without a gap",
			ts:   []string{"2025-01-10T10:30:00Z", "2025-01-10T14:00:00Z", "2025-01-10T15:30:00Z"},
			want: []blockShape{
				{id: "2025-01-10T10:00:00.000Z", entries: 2},
				{id: "2025-01-10T15:00:00.000Z", entries: 1},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkBlocks(t, identify(blockEntries(t, tt.ts...), farFuture), tt.want)
		})
	}
}

func TestIdentifyBlocks_GapBlock(t *testing.T) {
	blocks := identify(blockEntries(t, "2025-01-10T10:20:00Z", "2025-01-10T11:00:00Z", "2025-01-10T20:30:00Z"), farFuture)
	checkBlocks(t, blocks, []blockShape{
		{id: "2025-01-10T10:00:00.000Z", entries: 2},
		{id: "gap-2025-01-10T16:00:00.000Z", start: "2025-01-10T16:00:00Z", end: "2025-01-10T20:30:00Z", gap: true},
		{id: "2025-01-10T20:00:00.000Z", start: "2025-01-10T20:00:00Z", end: "2025-01-11T01:00:00Z", entries: 1},
	})
	gap := blocks[1]
	if gap.IsActive || !gap.ActualEnd.IsZero() || gap.TotalTokens() != 0 || gap.Models != nil {
		t.Errorf("gap block carries data: %+v", gap)
	}
	if BurnRateOf(gap) != nil || Project(gap, farFuture) != nil {
		t.Errorf("gap block has a burn rate or projection")
	}
}

func TestIdentifyBlocks_ActiveDetection(t *testing.T) {
	entries := blockEntries(t, "2025-01-10T10:10:00Z", "2025-01-10T11:40:00Z")
	tests := []struct {
		now    string
		active bool
	}{
		{"2025-01-10T12:00:00Z", true},
		{"2025-01-10T14:59:59.999Z", true},
		{"2025-01-10T15:00:00Z", false}, // now reached the block end
		{"2025-01-10T09:00:00Z", true},
	}
	for _, tt := range tests {
		blocks := identify(entries, mustTime(t, tt.now))
		if len(blocks) != 1 || blocks[0].IsActive != tt.active {
			t.Errorf("now=%s: active=%v, want %v", tt.now, blocks[0].IsActive, tt.active)
		}
	}
	// With a 1h duration the block is 10:00-11:00 and stops being active
	// when now reaches its end.
	short := IdentifyBlocks(entries[:1], time.Hour, mustTime(t, "2025-01-10T10:59:00Z"), nil, CostModeDisplay, nil)
	if !short[0].IsActive {
		t.Errorf("1h block at 10:59 should be active")
	}
	short = IdentifyBlocks(entries[:1], time.Hour, mustTime(t, "2025-01-10T11:00:00Z"), nil, CostModeDisplay, nil)
	if short[0].IsActive {
		t.Errorf("1h block at 11:00 should be inactive")
	}
}

func TestBurnRate(t *testing.T) {
	single := identify(blockEntries(t, "2025-01-10T10:00:00Z"), farFuture)[0]
	if got := BurnRateOf(single); got != nil {
		t.Errorf("single entry: got %+v, want nil", got)
	}
	same := identify(blockEntries(t, "2025-01-10T10:00:00Z", "2025-01-10T10:00:00Z"), farFuture)[0]
	if got := BurnRateOf(same); got != nil {
		t.Errorf("zero duration: got %+v, want nil", got)
	}

	e := blockEntries(t, "2025-01-10T10:00:00Z", "2025-01-10T10:10:00Z")
	e[1].CacheRead = 3000
	b := identify(e, farFuture)[0]
	got := BurnRateOf(b)
	if got == nil {
		t.Fatal("got nil burn rate")
	}
	// 6000 tokens, 3000 of them input+output, and $0.02 over 10 minutes.
	if got.TokensPerMinute != 600 || got.TokensPerMinuteForIndicator != 300 || math.Abs(got.CostPerHour-0.12) > 1e-12 {
		t.Errorf("burn rate = %+v", got)
	}
}

func TestProject(t *testing.T) {
	// 3000 tokens and $0.02 over 10 minutes: 300 tokens and $0.002 a minute.
	e := blockEntries(t, "2025-01-10T10:00:00Z", "2025-01-10T10:10:00Z")
	now := mustTime(t, "2025-01-10T12:29:30Z") // 150.5 minutes before 15:00
	b := identify(e, now)[0]
	if !b.IsActive {
		t.Fatal("block should be active")
	}
	got := Project(b, now)
	if got == nil {
		t.Fatal("got nil projection")
	}
	// 150.5 rounds half away from zero to 151 minutes.
	want := Projection{TotalTokens: 3000 + 300*151, TotalCost: 0.32, RemainingMinutes: 151}
	if *got != want {
		t.Errorf("projection = %+v, want %+v", *got, want)
	}

	// The token total rounds to the nearest integer: 3000 tokens over 7
	// minutes is 428.571.../min, and 2 more minutes make 3857.14... tokens.
	e = blockEntries(t, "2025-01-10T10:00:00Z", "2025-01-10T10:07:00Z")
	now = mustTime(t, "2025-01-10T14:58:00Z")
	got = Project(identify(e, now)[0], now)
	if got == nil || got.TotalTokens != 3857 || got.RemainingMinutes != 2 || got.TotalCost != 0.03 {
		t.Errorf("projection = %+v, want 3857 tokens, 2 minutes, $0.03", got)
	}

	if got := Project(identify(e, farFuture)[0], farFuture); got != nil {
		t.Errorf("inactive block: got %+v, want nil", got)
	}
	single := blockEntries(t, "2025-01-10T10:00:00Z")
	if got := Project(identify(single, now)[0], now); got != nil {
		t.Errorf("single entry: got %+v, want nil", got)
	}
}

func TestMaxTokensFromHistory(t *testing.T) {
	e := blockEntries(t, "2025-01-10T10:00:00Z", "2025-01-10T10:30:00Z", "2025-01-10T20:00:00Z")
	now := mustTime(t, "2025-01-10T21:00:00Z")
	blocks := identify(e, now)
	// Completed block (3000), gap, active block (1500).
	if len(blocks) != 3 || !blocks[2].IsActive {
		t.Fatalf("unexpected blocks: %d", len(blocks))
	}
	if got := MaxTokensFromHistory(blocks); got != 3000 {
		t.Errorf("MaxTokensFromHistory = %d, want 3000", got)
	}
	if got := MaxTokensFromHistory(blocks[2:]); got != 0 {
		t.Errorf("active only: got %d, want 0", got)
	}
}

func TestLimitStatus(t *testing.T) {
	tests := []struct {
		projected, limit int64
		want             string
	}{
		{800, 1000, LimitOK},
		{801, 1000, LimitWarning},
		{1000, 1000, LimitWarning},
		{1001, 1000, LimitExceeds},
		{5, 0, ""},
	}
	for _, tt := range tests {
		if got := LimitStatus(tt.projected, tt.limit); got != tt.want {
			t.Errorf("LimitStatus(%d, %d) = %q, want %q", tt.projected, tt.limit, got, tt.want)
		}
	}
}

// The reference captures carry 5h resets of 1790165400 (12:10Z) and
// 1790202000 (22:20Z), so the real windows start at 07:10Z and 17:20Z.
func referenceAnchors() []time.Time {
	return []time.Time{
		time.Unix(1790165400, 0).Add(-DefaultBlockDuration),
		time.Unix(1790202000, 0).Add(-DefaultBlockDuration),
	}
}

func TestIdentifyBlocks_AnchoredAtReferenceResets(t *testing.T) {
	anchors := referenceAnchors()
	if got := anchors[0].UTC().Format(time.RFC3339); got != "2026-09-23T07:10:00Z" {
		t.Fatalf("first anchor = %s", got)
	}
	e := blockEntries(t,
		"2026-09-23T06:30:00Z", // before the first window: unanchored, clipped at 07:10
		"2026-09-23T07:10:00Z", // first window start
		"2026-09-23T08:48:42Z",
		"2026-09-23T12:09:59.999Z", // last instant of the first window
		"2026-09-23T12:10:00Z",     // first window end: unanchored, starts at 12:10 not 12:00
		"2026-09-23T13:30:00Z",
		"2026-09-23T17:20:00Z", // second window start
		"2026-09-23T22:19:00Z",
		"2026-09-23T22:30:00Z", // after the last window
	)
	now := mustTime(t, "2026-09-23T22:40:00Z")
	blocks := identify(e, now, anchors...)
	checkBlocks(t, blocks, []blockShape{
		{id: "2026-09-23T06:00:00.000Z", start: "2026-09-23T06:00:00Z", end: "2026-09-23T07:10:00Z", entries: 1},
		{id: "2026-09-23T07:10:00.000Z", start: "2026-09-23T07:10:00Z", end: "2026-09-23T12:10:00Z", entries: 3},
		{id: "2026-09-23T12:10:00.000Z", start: "2026-09-23T12:10:00Z", end: "2026-09-23T17:10:00Z", entries: 2},
		{id: "2026-09-23T17:20:00.000Z", start: "2026-09-23T17:20:00Z", end: "2026-09-23T22:20:00Z", entries: 2},
		{id: "2026-09-23T22:20:00.000Z", start: "2026-09-23T22:20:00Z", end: "2026-09-24T03:20:00Z", entries: 1},
	})
	if blocks[0].Anchored || !blocks[1].Anchored || blocks[2].Anchored || !blocks[3].Anchored || blocks[4].Anchored {
		t.Errorf("anchored flags wrong")
	}
	if !blocks[4].IsActive || blocks[3].IsActive {
		t.Errorf("active flags: last=%v previous=%v", blocks[4].IsActive, blocks[3].IsActive)
	}
}

func TestIdentifyBlocks_AnchoredActiveAndGap(t *testing.T) {
	anchors := referenceAnchors()
	// One entry inside the first window and one well after it: the anchored
	// block, then a gap from the entry plus 5h, then an unanchored block.
	e := blockEntries(t, "2026-09-23T08:00:00Z", "2026-09-23T13:30:00Z", "2026-09-23T17:25:00Z", "2026-09-23T17:40:00Z")
	now := mustTime(t, "2026-09-23T18:00:00Z")
	blocks := identify(e, now, anchors...)
	checkBlocks(t, blocks, []blockShape{
		{id: "2026-09-23T07:10:00.000Z", end: "2026-09-23T12:10:00Z", entries: 1},
		{id: "gap-2026-09-23T13:00:00.000Z", start: "2026-09-23T13:00:00Z", end: "2026-09-23T13:30:00Z", gap: true},
		{id: "2026-09-23T13:00:00.000Z", start: "2026-09-23T13:00:00Z", end: "2026-09-23T17:20:00Z", entries: 1},
		{id: "2026-09-23T17:20:00.000Z", start: "2026-09-23T17:20:00Z", end: "2026-09-23T22:20:00Z", entries: 2},
	})
	active := blocks[3]
	if !active.IsActive {
		t.Fatal("anchored block should be active")
	}
	// The projection runs to the anchored end: 260 minutes from 18:00.
	if p := Project(active, now); p == nil || p.RemainingMinutes != 260 {
		t.Errorf("projection = %+v, want 260 remaining minutes", p)
	}
}

func TestIdentifyBlocks_EmptyAnchorWindowSplitsUnanchoredBlock(t *testing.T) {
	// No entry falls in 07:10-12:10, but the window still separates the
	// entries around it.
	e := blockEntries(t, "2026-09-23T06:30:00Z", "2026-09-23T12:20:00Z")
	blocks := identify(e, farFuture, referenceAnchors()...)
	checkBlocks(t, blocks, []blockShape{
		{id: "2026-09-23T06:00:00.000Z", end: "2026-09-23T07:10:00Z", entries: 1},
		{id: "gap-2026-09-23T11:30:00.000Z", end: "2026-09-23T12:20:00Z", gap: true},
		{id: "2026-09-23T12:10:00.000Z", start: "2026-09-23T12:10:00Z", end: "2026-09-23T17:10:00Z", entries: 1},
	})
}

func TestNormalizeAnchors(t *testing.T) {
	base := mustTime(t, "2026-09-23T07:10:00Z")
	got := normalizeAnchors([]time.Time{
		base.Add(10 * time.Hour),
		base,
		{},
		base,                                 // duplicate
		base.Add(10*time.Hour + time.Second), // overlaps the previous anchor: the later wins
		base.Add(20 * time.Hour),
	}, DefaultBlockDuration)
	want := []time.Time{base, base.Add(10*time.Hour + time.Second), base.Add(20 * time.Hour)}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("anchor %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestIdentifyBlocks_EntriesDoNotAlias(t *testing.T) {
	blocks := identify(blockEntries(t, "2025-01-10T10:00:00Z", "2025-01-10T14:00:00Z", "2025-01-10T15:30:00Z"), farFuture)
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks", len(blocks))
	}
	next := blocks[1].Entries[0].Timestamp
	_ = append(blocks[0].Entries, Entry{})
	if !blocks[1].Entries[0].Timestamp.Equal(next) {
		t.Errorf("appending to one block's entries overwrote the next block")
	}
}

func TestIdentifyBlocks_GapStopsAtNextAnchor(t *testing.T) {
	// 08:00 is in the 07:10 window and 17:30 in the 17:20 window. The gap
	// from 13:00 would run to 17:30, inside the second window, so it stops at
	// 17:20.
	e := blockEntries(t, "2026-09-23T08:00:00Z", "2026-09-23T17:30:00Z")
	checkBlocks(t, identify(e, farFuture, referenceAnchors()...), []blockShape{
		{id: "2026-09-23T07:10:00.000Z", end: "2026-09-23T12:10:00Z", entries: 1},
		{id: "gap-2026-09-23T13:00:00.000Z", start: "2026-09-23T13:00:00Z", end: "2026-09-23T17:20:00Z", gap: true},
		{id: "2026-09-23T17:20:00.000Z", start: "2026-09-23T17:20:00Z", end: "2026-09-23T22:20:00Z", entries: 1},
	})

	// Back-to-back windows: the gap would start at 05:30, after the second
	// window's 05:00 start, so there is no gap block at all.
	anchors := []time.Time{mustTime(t, "2026-09-23T00:00:00Z"), mustTime(t, "2026-09-23T05:00:00Z")}
	e = blockEntries(t, "2026-09-23T00:30:00Z", "2026-09-23T05:40:00Z")
	checkBlocks(t, identify(e, farFuture, anchors...), []blockShape{
		{id: "2026-09-23T00:00:00.000Z", end: "2026-09-23T05:00:00Z", entries: 1},
		{id: "2026-09-23T05:00:00.000Z", end: "2026-09-23T10:00:00Z", entries: 1},
	})
}

func TestIdentifyBlocks_CloseAnchorsKeepBothWindows(t *testing.T) {
	// Anchors three hours apart are two real windows: the first ends early
	// at the second's start instead of being dropped.
	anchors := []time.Time{mustTime(t, "2026-09-23T00:00:00Z"), mustTime(t, "2026-09-23T03:00:00Z")}
	e := blockEntries(t, "2026-09-23T00:30:00Z", "2026-09-23T02:00:00Z", "2026-09-23T03:30:00Z")
	blocks := identify(e, farFuture, anchors...)
	checkBlocks(t, blocks, []blockShape{
		{id: "2026-09-23T00:00:00.000Z", start: "2026-09-23T00:00:00Z", end: "2026-09-23T03:00:00Z", entries: 2},
		{id: "2026-09-23T03:00:00.000Z", start: "2026-09-23T03:00:00Z", end: "2026-09-23T08:00:00Z", entries: 1},
	})
	if !blocks[0].Anchored || !blocks[1].Anchored {
		t.Errorf("both blocks should be anchored")
	}
}

func TestNormalizeAnchors_MergesOnlyWithinAMinute(t *testing.T) {
	base := mustTime(t, "2026-09-23T07:10:00Z")
	got := normalizeAnchors([]time.Time{
		base,
		base.Add(30 * time.Second), // same window: the later wins
		base.Add(2 * time.Hour),    // a distinct window
		base.Add(2*time.Hour + 59*time.Second + 999*time.Millisecond + 500*time.Microsecond),
	}, DefaultBlockDuration)
	want := []time.Time{base.Add(30 * time.Second), base.Add(2*time.Hour + 59*time.Second + 999*time.Millisecond)}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("anchor %d = %v, want %v", i, got[i], want[i])
		}
	}
	ends := anchorEnds([]time.Time{base, base.Add(2 * time.Hour)}, DefaultBlockDuration)
	if !ends[0].Equal(base.Add(2*time.Hour)) || !ends[1].Equal(base.Add(7*time.Hour)) {
		t.Errorf("ends = %v", ends)
	}
}

func TestIdentifyBlocks_TruncatesToMilliseconds(t *testing.T) {
	// 900µs past the block end is the same millisecond as the end, which
	// ccusage keeps in the block.
	e := blockEntries(t, "2026-09-23T10:00:00Z", "2026-09-23T15:00:00.000900Z")
	blocks := identify(e, farFuture)
	checkBlocks(t, blocks, []blockShape{
		{id: "2026-09-23T10:00:00.000Z", end: "2026-09-23T15:00:00Z", entries: 2},
	})
	if got := blocks[0].ActualEnd; !got.Equal(mustTime(t, "2026-09-23T15:00:00Z")) {
		t.Errorf("actual end = %s", got.Format(time.RFC3339Nano))
	}

	// A sub-millisecond anchor still admits an entry at its millisecond.
	anchor := mustTime(t, "2026-09-23T07:10:00.000400Z")
	blocks = identify(blockEntries(t, "2026-09-23T07:10:00Z"), farFuture, anchor)
	if len(blocks) != 1 || !blocks[0].Anchored {
		t.Fatalf("blocks = %+v, want one anchored block", blocks)
	}
}

func TestIdentifyBlocks_UnanchoredAfterClampedStartDoesNotOverlap(t *testing.T) {
	// 12:20 starts an unanchored block clamped to the 12:10 window end, which
	// runs to 17:10. 17:15 floors to 17:00 but must not start before 17:10.
	anchors := []time.Time{mustTime(t, "2026-09-23T07:10:00Z")}
	e := blockEntries(t, "2026-09-23T08:00:00Z", "2026-09-23T12:20:00Z", "2026-09-23T17:15:00Z")
	checkBlocks(t, identify(e, farFuture, anchors...), []blockShape{
		{id: "2026-09-23T07:10:00.000Z", start: "2026-09-23T07:10:00Z", end: "2026-09-23T12:10:00Z", entries: 1},
		{id: "2026-09-23T12:10:00.000Z", start: "2026-09-23T12:10:00Z", end: "2026-09-23T17:10:00Z", entries: 1},
		{id: "2026-09-23T17:10:00.000Z", start: "2026-09-23T17:10:00Z", end: "2026-09-23T22:10:00Z", entries: 1},
	})
}
