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
	"appSpoofTitle", "appSpoofDefault", "appSpoofCustom", "appSpoofDesc",
	"appSpoofFieldTitle", "appSpoofFieldCategories", "appSpoofFieldReferer",
}

// kimiPanelKeys are the i18n keys referenced by the Kimi Code gateway section
// and Discover modal in views/settings.html. Every locale must define them.
var kimiPanelKeys = []string{
	"kimiGateway", "kimiDesc", "kimiBaseUrl", "kimiApiKey", "kimiDiscover",
	"kimiAllowlist", "kimiColEnabled", "kimiColId", "kimiColAlias",
	"kimiColDisplay", "kimiBadge", "familyKimi", "kimiSavedSuccess",
	"kimiDiscoverTitle", "kimiDiscoverDesc", "kimiDiscoverImport",
}

var presetKeys = []string{
	"configPresets", "saveAsPreset", "deletePreset", "presetHint",
	"unsavedChangesTitle", "unsavedChangesMessage", "loadAnyway",
	"savePresetTitle", "savePresetDesc", "presetName",
	"presetNamePlaceholder", "savePreset",
}

var headroomKeys = []string{
	"headroomSettings", "headroomDesc", "headroomEnabled", "headroomCacheNotice",
	"headroomSmartCrusher", "headroomSmartCrusherDesc",
	"headroomTabularArrays", "headroomTabularArraysDesc",
	"headroomCodeCompressor", "headroomCodeCompressorDesc",
	"headroomLiveTurns", "headroomLiveTurnsDesc",
	"headroomOutputShaper", "headroomOutputShaperDesc",
	"headroomVerbositySteering", "headroomVerbositySteeringDesc",
	"headroomSteeringText", "headroomSteeringTextPlaceholder",
	"headroomEffortRouting", "headroomEffortRoutingDesc",
	"headroomThinkingBudget",
	"headroomCcr", "headroomCcrDesc", "headroomCcrEnabled",
	"headroomMaxStoreMB", "headroomMinChunkBytes",
	"headroomStatsTitle", "headroomBytesSaved", "headroomCompressionRatio",
	"headroomRequestsCompressed", "headroomThinkingClamped", "headroomCcrRetrievals",
}

var commandCrusherKeys = []string{
	"commandCrusherSettings", "commandCrusherDesc", "commandCrusherEnabled",
	"commandCrusherRequiresEngine", "commandCrusherToolsTitle",
	"commandCrusherSafetyTitle", "commandCrusherSafetyDesc",
	"commandCrusherRunners", "commandCrusherLinters", "commandCrusherCompilers",
	"commandCrusherVCS",
}

var locales = []string{"en", "pt"}

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
			re := regexp.MustCompile(`(?m)^\s+` + key + `\s*:`)
			if !re.MatchString(src) {
				t.Errorf("locale %s missing key %q", locale, key)
			}
		}
	}
}

func TestTranslations_KimiKeys(t *testing.T) {
	for _, locale := range locales {
		src := loadLocale(t, locale)
		for _, key := range kimiPanelKeys {
			re := regexp.MustCompile(`(?m)^\s+` + key + `\s*:`)
			if !re.MatchString(src) {
				t.Errorf("locale %s missing key %q", locale, key)
			}
		}
	}
}

func TestTranslations_PresetKeys(t *testing.T) {
	for _, locale := range locales {
		src := loadLocale(t, locale)
		for _, key := range presetKeys {
			re := regexp.MustCompile(`(?m)^\s+` + key + `\s*:`)
			if !re.MatchString(src) {
				t.Errorf("locale %s missing key %q", locale, key)
			}
		}
	}
}

func TestTranslations_HeadroomKeys(t *testing.T) {
	for _, locale := range locales {
		src := loadLocale(t, locale)
		for _, key := range headroomKeys {
			re := regexp.MustCompile(`(?m)^\s+` + key + `\s*:`)
			if !re.MatchString(src) {
				t.Errorf("locale %s missing key %q", locale, key)
			}
		}
	}
}

func TestTranslations_CommandCrusherKeys(t *testing.T) {
	for _, locale := range locales {
		src := loadLocale(t, locale)
		for _, key := range commandCrusherKeys {
			re := regexp.MustCompile(`(?m)^\s+` + key + `\s*:`)
			if !re.MatchString(src) {
				t.Errorf("locale %s missing key %q", locale, key)
			}
		}
	}
}

func TestTranslations_PinnedToHasNoTrailingColon(t *testing.T) {
	re := regexp.MustCompile(`(?m)^\s+pinnedTo:\s*"([^"]*)"`)
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

func TestTranslations_PinnedToTemplateAppendsColon(t *testing.T) {
	b, err := Assets.ReadFile("public/views/settings.html")
	if err != nil {
		t.Fatalf("read settings.html: %v", err)
	}
	src := string(b)
	if !strings.Contains(src, `t('pinnedTo')`) {
		t.Fatal("settings.html does not reference t('pinnedTo')")
	}
	re := regexp.MustCompile(`t\('pinnedTo'\)[^<]*</span>\s*:`)
	if !re.MatchString(src) {
		t.Error("settings.html must append ':' after the pinnedTo span; value-level test depends on this")
	}
}

func TestTranslations_AppSpoofTemplateReferences(t *testing.T) {
	b, err := Assets.ReadFile("public/views/settings.html")
	if err != nil {
		t.Fatalf("read settings.html: %v", err)
	}
	src := string(b)
	appSpoofKeys := []string{
		"appSpoofTitle", "appSpoofDefault", "appSpoofCustom",
		"appSpoofDesc", "appSpoofFieldTitle", "appSpoofFieldCategories",
		"appSpoofFieldReferer",
	}
	for _, key := range appSpoofKeys {
		if !strings.Contains(src, fmt.Sprintf("t('%s')", key)) {
			t.Errorf("settings.html does not reference translation key %q", key)
		}
	}
}

func TestTranslations_StoreFallbackSafety(t *testing.T) {
	b, err := Assets.ReadFile("public/js/store.js")
	if err != nil {
		t.Fatalf("read store.js: %v", err)
	}
	src := string(b)
	// store.js must guard against missing locales when calling t()
	if strings.Contains(src, "this.translations[this.lang][key]") {
		t.Errorf("store.js unconditionally indexes this.translations[this.lang][key]; must provide safe fallback")
	}
}

