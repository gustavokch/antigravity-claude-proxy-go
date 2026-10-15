package api

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// partsReader hands out one part per Read call, then io.EOF, so a test controls
// exactly where a marker is cut between reads.
type partsReader struct{ parts []string }

func (r *partsReader) Read(p []byte) (int, error) {
	if len(r.parts) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.parts[0])
	r.parts[0] = r.parts[0][n:]
	if r.parts[0] == "" {
		r.parts = r.parts[1:]
	}
	return n, nil
}

// TestTerminalWatcherFindsAMarkerAnywhere: a marker is found whole, cut at every
// byte between two reads, and fed one byte at a time. A stream with no marker is
// never reported as finished.
func TestTerminalWatcherFindsAMarkerAnywhere(t *testing.T) {
	const preamble = "event: message_start\ndata: {\"type\":\"message_start\"}\n\ndata: {\"n\":1}\n\n"
	drain := func(w *terminalWatcher) {
		t.Helper()
		if _, err := io.Copy(io.Discard, w); err != nil {
			t.Fatal(err)
		}
	}

	for _, marker := range []string{"message_stop", "event: error", "[DONE]"} {
		stream := preamble + marker + "\n\n"

		w := &terminalWatcher{r: strings.NewReader(stream)}
		drain(w)
		if !w.seen {
			t.Errorf("%q not found in one read", marker)
		}
		for split := 1; split < len(stream); split++ {
			w := &terminalWatcher{r: &partsReader{parts: []string{stream[:split], stream[split:]}}}
			drain(w)
			if !w.seen {
				t.Errorf("%q cut at byte %d of %d was not found", marker, split, len(stream))
			}
		}
		w = &terminalWatcher{r: iotest.OneByteReader(strings.NewReader(stream))}
		drain(w)
		if !w.seen {
			t.Errorf("%q fed one byte at a time was not found", marker)
		}
	}

	w := &terminalWatcher{r: strings.NewReader(preamble)}
	drain(w)
	if w.seen {
		t.Error("a stream with no terminal marker was reported as finished")
	}
}

// TestTerminalWatcherBreakReason is the decision the copy sites hand to
// logStreamBreak: the copy's own error always wins, and a clean end is a break
// only when no terminal marker went past.
func TestTerminalWatcherBreakReason(t *testing.T) {
	readErr := errors.New("upstream reset")
	for _, tc := range []struct {
		name    string
		seen    bool
		copyErr error
		want    error
	}{
		{"finished", true, nil, nil},
		{"graceful early close", false, nil, errNoTerminalEvent},
		{"read error before any marker", false, readErr, readErr},
		{"read error after the marker", true, readErr, readErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &terminalWatcher{seen: tc.seen}
			if got := w.breakReason(tc.copyErr); got != tc.want {
				t.Errorf("breakReason(%v) = %v, want %v", tc.copyErr, got, tc.want)
			}
		})
	}
}
