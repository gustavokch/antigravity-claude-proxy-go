package ccusage

import (
	"math"
	"slices"
	"time"
)

// DefaultBlockDuration is the length of a Claude billing window.
const DefaultBlockDuration = 5 * time.Hour

// blockWarningThreshold is the share of the token limit past which a
// projection is reported as a warning.
const blockWarningThreshold = 0.8

// Token limit statuses returned by LimitStatus.
const (
	LimitOK      = "ok"
	LimitWarning = "warning"
	LimitExceeds = "exceeds"
)

// blockIDLayout is ccusage's RFC 3339 format with milliseconds.
const blockIDLayout = "2006-01-02T15:04:05.000Z"

// Block is one billing window, or a gap between two windows with no usage.
type Block struct {
	// ID is the start in RFC 3339 with milliseconds, prefixed with "gap-"
	// for a gap block.
	ID    string
	Start time.Time
	// End is Start plus the block duration, clipped to the start of the next
	// anchored window for a block that would overlap it. A gap block ends at
	// the next entry, or at the next anchored window if that starts first.
	End time.Time
	// ActualEnd is the last entry's timestamp; zero for a gap block.
	ActualEnd time.Time
	IsActive  bool
	IsGap     bool
	// Anchored marks a block whose start came from a known window start
	// rather than ccusage's UTC-hour floor.
	Anchored bool
	// Entries are the block's entries in timestamp order. They share the
	// backing array of the sorted copy IdentifyBlocks makes, so the blocks of
	// one call cost no more memory than that copy.
	Entries []Entry

	InputTokens       int64
	OutputTokens      int64
	CacheCreateTokens int64
	CacheReadTokens   int64
	CostUSD           float64
	// Models are the display models in first-seen order, without synthetic
	// messages.
	Models            []string
	UsageLimitResetAt *time.Time
}

// TotalTokens is the sum of the block's four token buckets.
func (b Block) TotalTokens() int64 {
	return satAdd(satAdd(b.InputTokens, b.OutputTokens), satAdd(b.CacheCreateTokens, b.CacheReadTokens))
}

// IdentifyBlocks groups entries into billing windows of length dur, pricing
// each entry with EntryCost under mode and p. now decides which block is
// active. The input slice is not modified.
//
// Without anchors this is exactly ccusage's rule: a block starts at the first
// entry floored to the UTC hour and takes every following entry until one is
// more than dur after the block start or more than dur after the previous
// entry. When the previous entry is more than dur back, a gap block runs from
// that entry plus dur to the new entry.
//
// Anchors are known window starts, such as a 5h reset from the unified rate
// limit headers minus five hours. They are truncated to the millisecond,
// sorted and deduplicated, and anchors less than a minute apart are merged
// into the later one, since they are the same window observed twice with
// clock jitter. An anchor window runs from its anchor for dur, or to the next
// anchor if that comes sooner: real windows never overlap, so a later anchor
// means the earlier window ended early. An entry inside an anchor window joins
// that window's block, which starts at the anchor and ends at the window end.
// Entries outside every anchor window follow the unanchored rule, except that
// such a block never starts before the end of the previous anchor window and
// never ends after the start of the next one. Gap blocks use the same "more
// than dur since the previous entry" test on both kinds of block; a gap before
// an anchored block ends at the anchor, and is left out if that leaves it
// empty.
//
// Timestamps are truncated to the millisecond first, as ccusage compares
// milliseconds, so sub-millisecond ledger times cannot move a boundary. The
// blocks' entries carry the truncated timestamps.
func IdentifyBlocks(entries []Entry, dur time.Duration, now time.Time, anchors []time.Time, mode CostMode, p Pricer) []Block {
	if len(entries) == 0 || dur <= 0 {
		return nil
	}
	sorted := slices.Clone(entries)
	for i := range sorted {
		sorted[i].Timestamp = sorted[i].Timestamp.Truncate(time.Millisecond)
	}
	slices.SortStableFunc(sorted, func(a, b Entry) int { return a.Timestamp.Compare(b.Timestamp) })
	anchors = normalizeAnchors(anchors, dur)
	ends := anchorEnds(anchors, dur)

	var (
		blocks   []Block
		open     bool
		start    time.Time
		end      time.Time
		anchored bool
		lo       int
		ai       int // first anchor whose window has not ended before the current entry
	)
	closeBlock := func(hi int) {
		blocks = append(blocks, newBlock(start, end, anchored, sorted[lo:hi:hi], now, dur, mode, p))
	}
	for i := range sorted {
		ts := sorted[i].Timestamp
		for ai < len(anchors) && !ts.Before(ends[ai]) {
			ai++
		}
		inAnchor := ai < len(anchors) && !ts.Before(anchors[ai])

		if open {
			last := sorted[i-1].Timestamp
			sinceLast := ts.Sub(last)
			var split bool
			if inAnchor {
				split = !anchored || !start.Equal(anchors[ai])
			} else {
				split = anchored || ts.After(end) || sinceLast > dur
			}
			if !split {
				continue
			}
			closeBlock(i)
			if sinceLast > dur {
				gapEnd := ts
				if inAnchor && anchors[ai].Before(gapEnd) {
					gapEnd = anchors[ai]
				}
				if gapEnd.After(last.Add(dur)) {
					blocks = append(blocks, newGapBlock(last, gapEnd, dur))
				}
			}
		}

		open, lo = true, i
		if inAnchor {
			start, end, anchored = anchors[ai], ends[ai], true
			continue
		}
		start, anchored = floorToHour(ts), false
		if ai > 0 {
			if prevEnd := ends[ai-1]; start.Before(prevEnd) {
				start = prevEnd
			}
		}
		end = start.Add(dur)
		if ai < len(anchors) && end.After(anchors[ai]) {
			end = anchors[ai]
		}
	}
	if open {
		closeBlock(len(sorted))
	}
	return blocks
}

// anchorMergeWindow is how close two anchors must be to count as the same
// window.
const anchorMergeWindow = time.Minute

// normalizeAnchors truncates anchors to the millisecond, sorts and
// deduplicates them, and merges anchors less than anchorMergeWindow (or dur,
// if shorter) apart into the later one.
func normalizeAnchors(anchors []time.Time, dur time.Duration) []time.Time {
	if len(anchors) == 0 {
		return nil
	}
	sorted := make([]time.Time, 0, len(anchors))
	for _, a := range anchors {
		if !a.IsZero() {
			sorted = append(sorted, a.UTC().Truncate(time.Millisecond))
		}
	}
	slices.SortFunc(sorted, func(a, b time.Time) int { return a.Compare(b) })
	merge := min(anchorMergeWindow, dur)
	out := sorted[:0]
	for _, a := range sorted {
		if n := len(out); n > 0 && a.Sub(out[n-1]) < merge {
			out[n-1] = a
			continue
		}
		out = append(out, a)
	}
	return out
}

// anchorEnds returns where each anchor window ends: dur after its anchor, or
// at the next anchor if that is sooner.
func anchorEnds(anchors []time.Time, dur time.Duration) []time.Time {
	ends := make([]time.Time, len(anchors))
	for i, a := range anchors {
		ends[i] = a.Add(dur)
		if i+1 < len(anchors) && anchors[i+1].Before(ends[i]) {
			ends[i] = anchors[i+1]
		}
	}
	return ends
}

// floorToHour floors t to the UTC hour, as ccusage does in milliseconds.
func floorToHour(t time.Time) time.Time {
	ms := t.UnixMilli()
	const hourMs = int64(time.Hour / time.Millisecond)
	floored := ms - ((ms%hourMs)+hourMs)%hourMs
	return time.UnixMilli(floored).UTC()
}

func newBlock(start, end time.Time, anchored bool, entries []Entry, now time.Time, dur time.Duration, mode CostMode, p Pricer) Block {
	b := Block{
		ID:       start.UTC().Format(blockIDLayout),
		Start:    start,
		End:      end,
		Anchored: anchored,
		Entries:  entries,
	}
	if len(entries) > 0 {
		b.ActualEnd = entries[len(entries)-1].Timestamp
		b.IsActive = now.Sub(b.ActualEnd) < dur && now.Before(end)
	}
	for i := range entries {
		e := &entries[i]
		b.InputTokens = satAdd(b.InputTokens, e.Input)
		b.OutputTokens = satAdd(b.OutputTokens, e.Output)
		b.CacheCreateTokens = satAdd(b.CacheCreateTokens, e.CacheCreate)
		b.CacheReadTokens = satAdd(b.CacheReadTokens, e.CacheRead)
		b.CostUSD += EntryCost(*e, mode, p)
		if e.DisplayModel != "" && !slices.Contains(b.Models, e.DisplayModel) {
			b.Models = append(b.Models, e.DisplayModel)
		}
		if b.UsageLimitResetAt == nil && e.UsageLimitResetAt != nil {
			t := *e.UsageLimitResetAt
			b.UsageLimitResetAt = &t
		}
	}
	return b
}

func newGapBlock(last, next time.Time, dur time.Duration) Block {
	start := last.Add(dur)
	return Block{
		ID:    "gap-" + start.UTC().Format(blockIDLayout),
		Start: start,
		End:   next,
		IsGap: true,
	}
}

// BurnRate is a block's usage rate between its first and last entry.
type BurnRate struct {
	TokensPerMinute float64 `json:"tokensPerMinute"`
	// TokensPerMinuteForIndicator counts input and output tokens only.
	TokensPerMinuteForIndicator float64 `json:"tokensPerMinuteForIndicator"`
	CostPerHour                 float64 `json:"costPerHour"`
}

// BurnRateOf returns the block's burn rate, or nil for a gap block or a
// block whose first and last entries are at the same millisecond (including
// a single-entry block).
func BurnRateOf(b Block) *BurnRate {
	if len(b.Entries) == 0 || b.IsGap {
		return nil
	}
	first := b.Entries[0].Timestamp.UnixMilli()
	last := b.Entries[len(b.Entries)-1].Timestamp.UnixMilli()
	minutes := float64(last-first) / float64(time.Minute/time.Millisecond)
	if minutes <= 0 {
		return nil
	}
	nonCache := float64(satAdd(b.InputTokens, b.OutputTokens))
	return &BurnRate{
		TokensPerMinute:             float64(b.TotalTokens()) / minutes,
		TokensPerMinuteForIndicator: nonCache / minutes,
		CostPerHour:                 b.CostUSD / minutes * 60,
	}
}

// Projection is where an active block ends up if its burn rate holds until
// the block's end.
type Projection struct {
	TotalTokens      int64   `json:"totalTokens"`
	TotalCost        float64 `json:"totalCost"`
	RemainingMinutes int64   `json:"remainingMinutes"`
}

// Project extrapolates an active block's burn rate to its end, as seen at
// now. Like ccusage it rounds the remaining minutes and the token total to
// the nearest integer (halves away from zero) and the cost to the cent. It
// returns nil for an inactive or gap block and when BurnRateOf is nil.
func Project(b Block, now time.Time) *Projection {
	if !b.IsActive || b.IsGap {
		return nil
	}
	burn := BurnRateOf(b)
	if burn == nil {
		return nil
	}
	remainingMs := b.End.UnixMilli() - now.UnixMilli()
	remaining := math.Round(float64(remainingMs) / float64(time.Minute/time.Millisecond))
	tokens := float64(b.TotalTokens()) + burn.TokensPerMinute*remaining
	cost := b.CostUSD + burn.CostPerHour/60*remaining
	return &Projection{
		TotalTokens:      floatToInt64(math.Round(tokens)),
		TotalCost:        math.Round(cost*100) / 100,
		RemainingMinutes: floatToInt64(remaining),
	}
}

// floatToInt64 converts like Rust's saturating "as u64": NaN and negatives
// become 0 and values past the range saturate.
func floatToInt64(f float64) int64 {
	switch {
	case math.IsNaN(f) || f <= 0:
		return 0
	case f >= math.MaxInt64:
		return math.MaxInt64
	}
	return int64(f)
}

// MaxTokensFromHistory returns the largest token total of a completed block:
// neither a gap nor active. It is ccusage's "max" token limit.
func MaxTokensFromHistory(blocks []Block) int64 {
	var maxTokens int64
	for i := range blocks {
		b := &blocks[i]
		if b.IsGap || b.IsActive {
			continue
		}
		maxTokens = max(maxTokens, b.TotalTokens())
	}
	return maxTokens
}

// LimitStatus compares projected tokens with a token limit: LimitExceeds
// above the limit, LimitWarning above 80% of it, LimitOK otherwise. A limit
// of 0 or less means no limit is known, as with ccusage's "max" limit before
// any block has completed, and returns "".
func LimitStatus(projected, limit int64) string {
	switch {
	case limit <= 0:
		return ""
	case projected > limit:
		return LimitExceeds
	case float64(projected) > float64(limit)*blockWarningThreshold:
		return LimitWarning
	}
	return LimitOK
}
