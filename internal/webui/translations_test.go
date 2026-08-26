package webui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// openRouterPanelKeys are the i18n keys referenced by the OpenRouter allowlist
// table and provider routing panel in views/settings.html. Every locale must
// define them: store.js t() falls back to the raw key name when missing.
var openRouterPanelKeys = []string{
	"save", "remove", "alias", "localAlias", "loading", "contextLength",
	"providerRouting", "provider", "uptime", "latency", "tps", "score",
	"order", "pin", "pinned", "pinnedTo", "openRouterBadge",
}

var locales = []string{"en", "zh", "id", "pt", "tr"}

func loadLocale(t *testing.T, locale string) string {
	t.Helper()
	b, err := Assets.ReadFile(fmt.Sprintf("public/js/translations/%s.js", locale))
	if err != nil {
		t.Fatalf("read locale %s: %v", locale, err)
	}
	return string(b)
}

func TestTranslations_OpenRouterKeys(t *testing.T) {
	for _, locale := range locales {
		src := loadLocale(t, locale)
		for _, key := range openRouterPanelKeys {
			re := regexp.MustCompile(`(?m)^\s{4}` + key + `:`)
			if !re.MatchString(src) {
				t.Errorf("locale %s missing key %q", locale, key)
			}
		}
	}
}

func TestTranslations_PinnedToHasNoTrailingColon(t *testing.T) {
	// settings.html appends ":" after t('pinnedTo'), so a colon inside the
	// translated value renders doubled.
	re := regexp.MustCompile(`(?m)^\s{4}pinnedTo:\s*"([^"]*)"`)
	for _, locale := range locales {
		src := loadLocale(t, locale)
		m := re.FindStringSubmatch(src)
		if m == nil {
			t.Errorf("locale %s missing key %q", locale, "pinnedTo")
			continue
		}
		if strings.HasSuffix(m[1], ":") {
			t.Errorf("locale %s pinnedTo value %q ends with colon; template adds one", locale, m[1])
		}
	}
}
