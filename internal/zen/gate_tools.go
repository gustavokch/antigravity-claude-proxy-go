package zen

import (
	_ "embed"
	"encoding/json"
	"sync"
)

// gate_tools.json holds the genuine OpenCode client's "bash" and "read"
// Chat-Completions function definitions, captured verbatim from a real
// free-tier request. The Zen free-tier gate requires every request body to
// carry functions named exactly "bash" and "read" — packet-verified: a body
// with only "bash" (or only "read") is rejected with 403 FreeTierError, while
// names like "Edit"/"Write" are not inspected and tool schemas are ignored.
// Clients that do not define these functions (or define them as Anthropic's
// "Bash"/"Read") get the captured definitions injected, so the body passes
// the gate and still carries the schemas the genuine harness would send.

//go:embed gate_tools.json
var gateToolDefsJSON []byte

var (
	gateToolOnce sync.Once
	gateToolRaw  map[string]json.RawMessage
)

// gateToolDef returns the captured OpenCode function definition for a
// gate-required tool name ("bash" or "read"), or nil when unknown or
// unparseable. Each call unmarshals afresh so callers may mutate the result.
func gateToolDef(name string) map[string]any {
	gateToolOnce.Do(func() {
		raw := map[string]json.RawMessage{}
		if err := json.Unmarshal(gateToolDefsJSON, &raw); err == nil {
			gateToolRaw = raw
		}
	})
	raw, ok := gateToolRaw[name]
	if !ok {
		return nil
	}
	var def map[string]any
	if err := json.Unmarshal(raw, &def); err != nil {
		return nil
	}
	return def
}
