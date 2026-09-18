package accounts

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// maxForensicsBodyLen caps a recorded upstream body. Google error JSON is
// small; the cap keeps an error page from bloating the forensics file.
const maxForensicsBodyLen = 4096

// Forensics429Entry is one recorded upstream 429. Response data only: no
// request headers, no tokens, no cookies, no prompt text (R2 of the
// cloudcode-429-throttle-dimension spec).
type Forensics429Entry struct {
	Timestamp   time.Time         `json:"timestamp"`
	Account     string            `json:"account,omitempty"`
	Project     string            `json:"project,omitempty"`
	Model       string            `json:"model,omitempty"`
	Endpoint    string            `json:"endpoint,omitempty"`
	Status      int               `json:"status"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        string            `json:"body,omitempty"`
	AppliedWait string            `json:"appliedWait,omitempty"`
	Failures    int               `json:"failures,omitempty"`
}

// Forensics429Recorder appends one JSON line per upstream 429 so the quota
// dimension (user vs project) survives for later analysis. R1 of the spec:
// the record must exist even when the operator is not watching the log.
type Forensics429Recorder struct {
	path string
	mu   sync.Mutex
}

// NewForensics429Recorder returns a recorder writing to path. An empty path
// disables recording; Record is then a no-op.
func NewForensics429Recorder(path string) *Forensics429Recorder {
	return &Forensics429Recorder{path: path}
}

func (recorder *Forensics429Recorder) Enabled() bool {
	return recorder != nil && recorder.path != ""
}

func (recorder *Forensics429Recorder) Record(entry Forensics429Entry) {
	if !recorder.Enabled() {
		return
	}
	if len(entry.Body) > maxForensicsBodyLen {
		entry.Body = entry.Body[:maxForensicsBodyLen]
	}
	line, err := json.Marshal(entry)
	if err != nil {
		slog.Warn("forensics: marshal 429 record", "error", err)
		return
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(recorder.path), 0700); err != nil {
		slog.Warn("forensics: create directory", "error", err)
		return
	}
	// Open per record: 429s are rare, and this survives external rotation.
	file, err := os.OpenFile(recorder.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		slog.Warn("forensics: open record file", "error", err)
		return
	}
	defer file.Close()
	if _, err := file.Write(append(line, '\n')); err != nil {
		slog.Warn("forensics: write 429 record", "error", err)
	}
}

// forensicsHeaders flattens upstream response headers for the record. These
// are headers Google sent to us — they never carry our credentials.
func forensicsHeaders(header http.Header) map[string]string {
	if len(header) == 0 {
		return nil
	}
	flat := make(map[string]string, len(header))
	for name, values := range header {
		flat[name] = strings.Join(values, ", ")
	}
	return flat
}
