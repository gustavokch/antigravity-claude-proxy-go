package ccusage

type dedupeKind uint8

const (
	// keyExact identifies one response.
	keyExact dedupeKind = iota
	// keyReplay routes an entry to the sidechain replays of its message.
	keyReplay
	// keyReplayEntry indexes only sidechain entries, for parent candidates.
	keyReplayEntry
)

type dedupeKey struct {
	kind      dedupeKind
	messageID string
	requestID string
	sessionID string
	tsMs      int64
}

// exactKey is msgId:reqId when the entry has a request ID. Without one,
// gateways can reuse a message ID for every response, so the key is scoped to
// the session and the timestamp instead.
func exactKey(e *Entry) dedupeKey {
	if e.RequestID != "" {
		return dedupeKey{kind: keyExact, messageID: e.MessageID, requestID: e.RequestID}
	}
	return dedupeKey{kind: keyExact, messageID: e.MessageID, sessionID: e.SessionID, tsMs: e.Timestamp.UnixMilli()}
}

func replayKey(kind dedupeKind, messageID, sessionID string) dedupeKey {
	return dedupeKey{kind: kind, messageID: messageID, sessionID: sessionID}
}

type dedupeRef struct {
	index int
	// alias, when set, is the session this route was registered for, which
	// may differ from the survivor's own session after a cross-session match.
	alias    string
	hasAlias bool
}

// Deduper keeps one entry per response as entries arrive, following
// ccusage's rules:
//   - the exact key (see exactKey) matches copies of one response;
//   - a sidechain entry that replays a parent message in the same session
//     matches the parent, even with a new request ID;
//   - of two matches, the non-sidechain entry wins, then the one with more
//     tokens, then the one with a speed set.
//
// Unlike ccusage, a ledger entry always beats a non-ledger entry: the ledger
// holds the final usage and the account, while Claude Code writes several
// lines per message with partial output counts.
//
// Entries without a message ID are always kept. A Deduper is not safe for
// concurrent use.
type Deduper struct {
	entries []Entry
	index   map[dedupeKey][]dedupeRef
}

// NewDeduper returns an empty Deduper.
func NewDeduper() *Deduper {
	return &Deduper{index: make(map[dedupeKey][]dedupeRef)}
}

// Dedupe runs entries through a new Deduper and returns the kept set.
func Dedupe(entries []Entry) []Entry {
	d := NewDeduper()
	for _, e := range entries {
		d.Add(e)
	}
	return d.entries
}

// Len returns the number of kept entries.
func (d *Deduper) Len() int { return len(d.entries) }

// Entries returns a copy of the kept entries in first-seen order. A replaced
// entry keeps its predecessor's position.
func (d *Deduper) Entries() []Entry {
	return append([]Entry(nil), d.entries...)
}

// Add ingests e. kept reports whether e is now in the kept set; when e
// replaced an earlier entry, replaced is that entry.
func (d *Deduper) Add(e Entry) (replaced *Entry, kept bool) {
	if e.MessageID == "" {
		d.entries = append(d.entries, e)
		return nil, true
	}
	exact := exactKey(&e)
	idx := d.find(&e, exact)
	if idx < 0 {
		idx = len(d.entries)
		d.entries = append(d.entries, e)
		d.push(exact, idx)
		d.push(replayKey(keyReplay, e.MessageID, e.SessionID), idx)
		if e.IsSidechain {
			d.push(replayKey(keyReplayEntry, e.MessageID, e.SessionID), idx)
		}
		return nil, true
	}

	cur := &d.entries[idx]
	if e.SessionID != cur.SessionID {
		// Either copy may survive, so keep both session routes for later
		// sidechain replays.
		for _, sid := range []string{e.SessionID, cur.SessionID} {
			d.pushAlias(replayKey(keyReplay, e.MessageID, sid), idx, sid)
			d.pushAlias(replayKey(keyReplayEntry, e.MessageID, sid), idx, sid)
		}
	}
	if !shouldReplace(&e, cur) {
		return nil, false
	}
	prev := *cur
	d.entries[idx] = e
	// The survivor is already indexed under its previous keys; register only
	// the ones that changed.
	if exactKey(&prev) != exact {
		d.push(exact, idx)
	}
	if prev.SessionID != e.SessionID || prev.IsSidechain != e.IsSidechain || prev.MessageID != e.MessageID {
		d.push(replayKey(keyReplay, e.MessageID, e.SessionID), idx)
		if e.IsSidechain {
			d.push(replayKey(keyReplayEntry, e.MessageID, e.SessionID), idx)
		}
	}
	return &prev, true
}

// find returns the index of the kept entry e duplicates, or -1.
func (d *Deduper) find(e *Entry, exact dedupeKey) int {
	ts := e.Timestamp.UnixMilli()
	for _, ref := range d.index[exact] {
		x := &d.entries[ref.index]
		if x.MessageID == e.MessageID && x.RequestID == e.RequestID &&
			(e.RequestID != "" || (x.SessionID == e.SessionID && x.Timestamp.UnixMilli() == ts)) {
			return ref.index
		}
	}

	// Sidechain logs can replay parent messages with new request IDs. A
	// replay needs a sidechain on at least one side, so a parent candidate
	// only looks through sidechain entries.
	kind := keyReplayEntry
	if e.IsSidechain {
		kind = keyReplay
	}
	for _, ref := range d.index[replayKey(kind, e.MessageID, e.SessionID)] {
		x := &d.entries[ref.index]
		sid := x.SessionID
		if ref.hasAlias {
			sid = ref.alias
		}
		// Requestless replays match across timestamps; replays that carry a
		// request ID need the parent's timestamp.
		requestless := e.RequestID == "" && x.RequestID == ""
		if sid == e.SessionID && x.MessageID == e.MessageID &&
			(requestless || x.Timestamp.UnixMilli() == ts) &&
			(e.IsSidechain || x.IsSidechain) {
			return ref.index
		}
	}
	return -1
}

func (d *Deduper) push(key dedupeKey, idx int) {
	for _, ref := range d.index[key] {
		if ref.index == idx && !ref.hasAlias {
			return
		}
	}
	d.index[key] = append(d.index[key], dedupeRef{index: idx})
}

func (d *Deduper) pushAlias(key dedupeKey, idx int, sessionID string) {
	for _, ref := range d.index[key] {
		if ref.index == idx && ref.hasAlias && ref.alias == sessionID {
			return
		}
	}
	d.index[key] = append(d.index[key], dedupeRef{index: idx, alias: sessionID, hasAlias: true})
}

// shouldReplace reports whether candidate beats the kept entry it matches.
func shouldReplace(candidate, existing *Entry) bool {
	if cl, xl := candidate.Source == SourceLedger, existing.Source == SourceLedger; cl != xl {
		return cl
	}
	if candidate.IsSidechain != existing.IsSidechain {
		return existing.IsSidechain
	}
	if ct, xt := candidate.TotalTokens(), existing.TotalTokens(); ct != xt {
		return ct > xt
	}
	return candidate.Speed != "" && existing.Speed == ""
}
