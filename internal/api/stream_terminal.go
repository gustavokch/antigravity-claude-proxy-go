package api

import (
	"bytes"
	"errors"
	"io"
)

// errNoTerminalEvent marks an event stream whose body ended cleanly, with no read
// error, before any terminal event went past. A graceful close from an upstream
// or an intermediary looks exactly like this: the copy loop reads io.EOF and
// cannot tell it from a finished answer without looking at the events.
var errNoTerminalEvent = errors.New("event stream ended without a terminal event")

// terminalMarkers are the byte strings that end an Anthropic-shaped event
// stream: the message_stop event, an in-band error event (the upstream said it
// failed, and the client saw that), and the OpenAI-style [DONE] sentinel.
var terminalMarkers = [][]byte{
	[]byte("message_stop"),
	[]byte("event: error"),
	[]byte("[DONE]"),
}

// terminalCarry is how many trailing bytes of one read can start a marker that
// finishes in the next: the longest marker is 12 bytes.
const terminalCarry = 11

// terminalWatcher forwards reads unchanged and records whether a terminal marker
// went past. It matches bytes, not events, so it needs no SSE parser. A marker
// inside message text can only hide a break, never invent one.
type terminalWatcher struct {
	r    io.Reader
	tail []byte
	seen bool
}

func (w *terminalWatcher) Read(p []byte) (int, error) {
	n, err := w.r.Read(p)
	if n > 0 && !w.seen {
		w.scan(p[:n])
	}
	return n, err
}

func (w *terminalWatcher) scan(chunk []byte) {
	window := make([]byte, 0, len(w.tail)+len(chunk))
	window = append(append(window, w.tail...), chunk...)
	for _, marker := range terminalMarkers {
		if bytes.Contains(window, marker) {
			w.seen, w.tail = true, nil
			return
		}
	}
	if len(window) > terminalCarry {
		window = window[len(window)-terminalCarry:]
	}
	w.tail = append(w.tail[:0], window...)
}

// breakReason is the error to report once the copy has finished: the copy's own
// error when it has one, errNoTerminalEvent when the body ended cleanly with no
// terminal marker, and nil for a stream that finished.
func (w *terminalWatcher) breakReason(copyErr error) error {
	if copyErr != nil {
		return copyErr
	}
	if !w.seen {
		return errNoTerminalEvent
	}
	return nil
}
