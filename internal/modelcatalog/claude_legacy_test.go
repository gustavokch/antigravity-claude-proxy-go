package modelcatalog

import (
	"fmt"
	"strings"
	"testing"
)

const (
	claude46SonnetID      = "claude-sonnet-4-6"
	claude46SonnetDisplay = "Claude Sonnet 4.6 (Thinking)"
	claude46OpusID        = "claude-opus-4-6-thinking"
	claude46OpusDisplay   = "Claude Opus 4.6 (Thinking)"

	claude55SonnetHighID      = "claude-sonnet-5-5-high"
	claude55SonnetHighDisplay = "Claude Sonnet 5.5 (High)"
	claude55SonnetMedID       = "claude-sonnet-5-5-medium"
	claude55SonnetMedDisplay  = "Claude Sonnet 5.5 (Medium)"
	claude55SonnetLowID       = "claude-sonnet-5-5-low"
	claude55SonnetLowDisplay  = "Claude Sonnet 5.5 (Low)"

	claude55OpusHighID      = "claude-opus-5-5-high"
	claude55OpusHighDisplay = "Claude Opus 5.5 (High)"
	claude55OpusMedID       = "claude-opus-5-5-medium"
	claude55OpusMedDisplay  = "Claude Opus 5.5 (Medium)"
	claude55OpusLowID       = "claude-opus-5-5-low"
	claude55OpusLowDisplay  = "Claude Opus 5.5 (Low)"
)

var (
	claude46Models = map[string]string{
		claude46SonnetID: claude46SonnetDisplay,
		claude46OpusID:   claude46OpusDisplay,
	}
	claude55Models = map[string]string{
		claude55SonnetHighID: claude55SonnetHighDisplay,
		claude55SonnetMedID:  claude55SonnetMedDisplay,
		claude55SonnetLowID:  claude55SonnetLowDisplay,
		claude55OpusHighID:   claude55OpusHighDisplay,
		claude55OpusMedID:    claude55OpusMedDisplay,
		claude55OpusLowID:    claude55OpusLowDisplay,
	}
)

func parseClaudeCatalog(t *testing.T, defs ...map[string]string) *Catalog {
	t.Helper()
	var ids []string
	var modelEntries []string
	defaultID := ""
	for _, m := range defs {
		for id, display := range m {
			if defaultID == "" {
				defaultID = id
			}
			ids = append(ids, fmt.Sprintf("%q", id))
			modelEntries = append(modelEntries, fmt.Sprintf(
				"%q:{\"displayName\":%q,\"supportsThinking\":true,\"maxTokens\":1000000,\"maxOutputTokens\":128000}",
				id, display,
			))
		}
	}
	raw := fmt.Sprintf(`{
		"defaultAgentModelId": %q,
		"agentModelSorts": [{"groups": [{"modelIds": [%s]}]}],
		"models": {%s}
	}`, defaultID, strings.Join(ids, ","), strings.Join(modelEntries, ","))
	c, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parseClaudeCatalog: %v", err)
	}
	return c
}

// TestLegacyClaude46Resolution tests that legacy Claude 4.6 names resolve to
// real 4.6 models when an account still publishes them, and fall back to the
// corresponding 5.5 High model only when 4.6 is absent from the account.
func TestLegacyClaude46Resolution(t *testing.T) {
	t.Parallel()

	unmigrated := parseClaudeCatalog(t, claude46Models)
	migrated := parseClaudeCatalog(t, claude55Models)
	both := parseClaudeCatalog(t, claude46Models, claude55Models)

	cases := []struct {
		name    string
		catalog *Catalog
		request string
		wantID  string
	}{
		// Unmigrated account (real 4.6 only): every legacy spelling resolves to 4.6.
		{"unmigrated exact sonnet", unmigrated, "claude-sonnet-4-6", claude46SonnetID},
		{"unmigrated thinking-suffixed sonnet", unmigrated, "claude-sonnet-4-6-thinking", claude46SonnetID},
		{"unmigrated dotted sonnet", unmigrated, "claude-sonnet-4.6", claude46SonnetID},
		{"unmigrated base opus", unmigrated, "claude-opus-4-6", claude46OpusID},
		{"unmigrated exact opus", unmigrated, "claude-opus-4-6-thinking", claude46OpusID},
		{"unmigrated dotted opus", unmigrated, "claude-opus-4.6", claude46OpusID},

		// Migrated account (5.5 only): legacy 4.6 names fall back to 5.5 High.
		{"migrated base sonnet", migrated, "claude-sonnet-4-6", claude55SonnetHighID},
		{"migrated thinking-suffixed sonnet", migrated, "claude-sonnet-4-6-thinking", claude55SonnetHighID},
		{"migrated base opus", migrated, "claude-opus-4-6", claude55OpusHighID},
		{"migrated thinking-suffixed opus", migrated, "claude-opus-4-6-thinking", claude55OpusHighID},

		// Account publishing both: real 4.6 takes precedence.
		{"both-account sonnet", both, "claude-sonnet-4-6", claude46SonnetID},
		{"both-account thinking sonnet", both, "claude-sonnet-4-6-thinking", claude46SonnetID},
		{"both-account base opus", both, "claude-opus-4-6", claude46OpusID},
		{"both-account thinking opus", both, "claude-opus-4-6-thinking", claude46OpusID},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.catalog.Resolve(tc.request)
			if err != nil {
				t.Fatalf("Resolve(%q) unexpected error: %v", tc.request, err)
			}
			if got.ID != tc.wantID {
				t.Errorf("Resolve(%q) = %q, want %q", tc.request, got.ID, tc.wantID)
			}
		})
	}
}
