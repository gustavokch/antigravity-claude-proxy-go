package claudecode

import (
	"reflect"
	"strings"
	"testing"
)

func TestRouter_ResolveModel(t *testing.T) {
	r := NewRouter(nil) // Use default allowlist

	tests := []struct {
		input       string
		expectedID  string
		expectFound bool
	}{
		{"claude-sonnet-5", "claude-sonnet-5", true},
		{"sonnet-5", "claude-sonnet-5", true},
		{"claude-opus-5", "claude-opus-5", true},
		{"opus-5", "claude-opus-5", true},
		{"claude-fable-5", "claude-fable-5", true},
		{"fable-5", "claude-fable-5", true},
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5-20251001", true},
		{"claude-haiku-4-5", "claude-haiku-4-5-20251001", true},
		{"haiku-4-5", "claude-haiku-4-5-20251001", true},
		{"claude-haiku-4.5", "claude-haiku-4-5-20251001", true},
		{"haiku-4.5", "claude-haiku-4-5-20251001", true},
		{"claude-3-7-sonnet-20250219", "claude-3-7-sonnet-20250219", true},
		{"claude-3-7-sonnet", "claude-3-7-sonnet-20250219", true},
		{"claude-3.7-sonnet", "claude-3-7-sonnet-20250219", true},
		{"sonnet-3-7", "claude-3-7-sonnet-20250219", true},
		{"sonnet-3.7", "claude-3-7-sonnet-20250219", true},
		{"claude-3-5-sonnet", "claude-3-5-sonnet-20241022", true},
		{"claude-3.5-sonnet", "claude-3-5-sonnet-20241022", true},
		{"sonnet-3-5", "claude-3-5-sonnet-20241022", true},
		{"sonnet-3.5", "claude-3-5-sonnet-20241022", true},
		{"CLAUDE-FABLE-5", "claude-fable-5", true},
		{"claude-3-5-haiku", "claude-3-5-haiku-20241022", true},
		{"claude-3.5-haiku", "claude-3-5-haiku-20241022", true},
		{"haiku-3-5", "claude-3-5-haiku-20241022", true},
		{"haiku-3.5", "claude-3-5-haiku-20241022", true},
		{"claude-3-opus", "claude-3-opus-20240229", true},
		{"opus-3", "claude-3-opus-20240229", true},
		{"sonnet-3-5-custom-build", "claude-3-5-sonnet-20241022", true},
		{"sonnet-3.5-custom-build", "claude-3-5-sonnet-20241022", true},
		{"sonnet-3-custom-build", "claude-3-sonnet-20240229", true},
		{"haiku-3-5-custom-build", "claude-3-5-haiku-20241022", true},
		{"haiku-3-custom-build", "claude-3-haiku-20240307", true},
		{"claude", "", false},
		{"c", "", false},
		{"claude-", "", false},
		{"non-existent-model", "", false},
		{"", "", false},
	}

	for _, tt := range tests {
		canonical, found := r.ResolveModel(tt.input)
		if found != tt.expectFound {
			t.Errorf("ResolveModel(%q): expected found=%v, got %v", tt.input, tt.expectFound, found)
		}
		if found && canonical != tt.expectedID {
			t.Errorf("ResolveModel(%q): expected ID %q, got %q", tt.input, tt.expectedID, canonical)
		}
		if r.IsModelAllowed(tt.input) != tt.expectFound {
			t.Errorf("IsModelAllowed(%q): expected %v, got %v", tt.input, tt.expectFound, r.IsModelAllowed(tt.input))
		}
	}
}

func TestRouter_UpdateAllowlist(t *testing.T) {
	custom := []ModelConfig{
		{
			ID:          "custom-claude-model",
			Alias:       "custom-alias",
			Aliases:     []string{"custom-alias-2"},
			DisplayName: "Custom",
			Enabled:     true,
		},
		{
			ID:      "disabled-model",
			Enabled: false,
		},
	}

	r := NewRouter(custom)
	if !r.IsModelAllowed("custom-claude-model") {
		t.Errorf("expected custom-claude-model to be allowed")
	}
	if !r.IsModelAllowed("custom-alias") {
		t.Errorf("expected custom-alias to be allowed")
	}
	if !r.IsModelAllowed("custom-alias-2") {
		t.Errorf("expected custom-alias-2 to be allowed")
	}
	if r.IsModelAllowed("disabled-model") {
		t.Errorf("expected disabled-model to NOT be allowed")
	}
	if r.IsModelAllowed("claude-sonnet-5") {
		t.Errorf("expected default models to not be present after custom allowlist")
	}

	models := r.GetAllowedModels()
	if len(models) != 1 {
		t.Errorf("expected 1 enabled model, got %d", len(models))
	}
}

func TestDefaultAllowlist_Claude5Limits(t *testing.T) {
	models := DefaultAllowlist()
	byID := make(map[string]ModelConfig)
	for _, m := range models {
		byID[m.ID] = m
	}

	targets := []string{"claude-opus-5", "claude-sonnet-5", "claude-fable-5", "claude-fable-5-1"}
	for _, id := range targets {
		m, ok := byID[id]
		if !ok {
			t.Fatalf("expected model %s in DefaultAllowlist", id)
		}
		if m.ContextLen != 1000000 {
			t.Errorf("model %s ContextLen = %d, want 1000000", id, m.ContextLen)
		}
		if m.MaxOutputTokens != 128000 {
			t.Errorf("model %s MaxOutputTokens = %d, want 128000", id, m.MaxOutputTokens)
		}
	}
}

func TestDefaultAllowlist_Claude5AliasesMatchCatalogue(t *testing.T) {
	normalize := func(names ...string) map[string]bool {
		set := make(map[string]bool, len(names))
		for _, n := range names {
			// Compare spellings as written (lowercased only). Dot-to-hyphen
			// normalization would collapse the very drift this test guards
			// against: the router must list dotted aliases explicitly instead
			// of relying on the resolve-time fallback.
			n = strings.ToLower(strings.TrimSpace(n))
			if n != "" {
				set[n] = true
			}
		}
		return set
	}

	for _, id := range []string{"claude-fable-5", "claude-fable-5-1", "claude-opus-5", "claude-sonnet-5"} {
		t.Run(id, func(t *testing.T) {
			var routerEntry *ModelConfig
			for i, m := range DefaultAllowlist() {
				if m.ID == id {
					routerEntry = &DefaultAllowlist()[i]
				}
			}
			if routerEntry == nil {
				t.Fatalf("%s missing from DefaultAllowlist", id)
			}
			routerSet := normalize(append([]string{routerEntry.ID, routerEntry.Alias}, routerEntry.Aliases...)...)

			var catalogueEntry *DiscoveredModel
			for i, m := range DefaultClaudeCatalogue() {
				if m.ID == id {
					catalogueEntry = &DefaultClaudeCatalogue()[i]
				}
			}
			if catalogueEntry == nil {
				t.Fatalf("%s missing from DefaultClaudeCatalogue", id)
			}
			catalogueSet := normalize(append([]string{catalogueEntry.ID}, catalogueEntry.Aliases...)...)

			for name := range catalogueSet {
				if !routerSet[name] {
					t.Errorf("alias %q in catalogue but not in router default allowlist", name)
				}
			}
			for name := range routerSet {
				if !catalogueSet[name] {
					t.Errorf("alias %q in router default allowlist but not in catalogue", name)
				}
			}
		})
	}
}

func TestExpandAliases(t *testing.T) {
	cases := []struct {
		name string
		in   ModelConfig
		want []string
	}{
		{"comma separated alias", ModelConfig{Alias: "a, b ,c"}, []string{"a", "b", "c"}},
		{"single alias", ModelConfig{Alias: "a"}, []string{"a"}},
		{"aliases list", ModelConfig{Aliases: []string{"x", "y"}}, []string{"x", "y"}},
		{"both merged and deduped", ModelConfig{Alias: "a, b", Aliases: []string{"B", "c"}}, []string{"a", "b", "c"}},
		{"empty", ModelConfig{}, nil},
		{"whitespace only", ModelConfig{Alias: "  ,"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.ExpandAliases()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("tc.in.ExpandAliases(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestRouter_UpdateAllowlist_CommaSeparatedAlias(t *testing.T) {
	// Shape produced by the WebUI importCCDefaults button before the fix:
	// all aliases comma-joined into the Alias string, Aliases empty.
	router := NewRouter([]ModelConfig{{
		ID:      "claude-fable-5",
		Alias:   "claude-fable-5, fable-5, fable, claude-fable",
		Enabled: true,
	}})
	for _, req := range []string{"fable-5", "fable", "claude-fable", "FABLE-5"} {
		got, ok := router.ResolveModel(req)
		if !ok || got != "claude-fable-5" {
			t.Errorf("ResolveModel(%q) = %q, %v; want claude-fable-5, true", req, got, ok)
		}
	}
	// Dated suffix resolves through the per-alias prefix mapping.
	if got, ok := router.ResolveModel("fable-5-20260101"); !ok || got != "claude-fable-5" {
		t.Errorf("prefix ResolveModel(fable-5-20260101) = %q, %v; want claude-fable-5, true", got, ok)
	}
}
