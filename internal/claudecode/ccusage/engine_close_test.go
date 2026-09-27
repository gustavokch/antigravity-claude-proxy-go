package ccusage

import (
	"testing"
	"time"
)

// Refresh runs under the engine's own context, so after Close it reads
// nothing, while an open engine reads the same files.
func TestEngine_RefreshAfterCloseReadsNothing(t *testing.T) {
	f := newEngineFixture(t)
	writeFile(t, f.transcript("sess-1"), transcriptLine(f.clock.Now().Add(-time.Hour), "req_011CA", "msg_01A", "claude-test", 1, 2))

	open := f.engine(t, nil)
	open.Refresh()
	if n := len(open.Entries()); n != 1 {
		t.Fatalf("open engine: entries = %d, want 1", n)
	}

	closed := f.engine(t, nil)
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	closed.Refresh()
	if n := len(closed.Entries()); n != 0 {
		t.Errorf("closed engine: entries = %d, want 0", n)
	}
}
