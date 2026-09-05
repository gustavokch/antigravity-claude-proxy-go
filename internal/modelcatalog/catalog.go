package modelcatalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Model is the subset of Cloud Code ModelDetails that affects agent model
// selection and request construction.
type Model struct {
	ID                       string
	UpstreamID               string
	DisplayName              string
	Description              string
	Disabled                 bool
	Recommended              bool
	SupportsThinking         bool
	SupportsAdaptiveThinking bool
	ThinkingBudget           int
	MinThinkingBudget        int
	ThinkingLevel            string
	MaxTokens                int
	MaxOutputTokens          int
	QuotaRemainingFraction   *float64
	QuotaResetTime           string
}

func (m Model) GetUpstreamID() string {
	if m.UpstreamID != "" {
		return m.UpstreamID
	}
	return m.ID
}

type Catalog struct {
	defaultID  string
	selectable []Model
	byID       map[string]Model
	byDisplay  map[string]Model
}

type SelectionError struct {
	Model      string
	Selectable []string
}

func (err *SelectionError) Error() string {
	if len(err.Selectable) > 0 {
		return fmt.Sprintf("model %q is not in agy's selectable agent model list. Available models: %s", err.Model, strings.Join(err.Selectable, ", "))
	}
	return fmt.Sprintf("model %q is not in agy's selectable agent model list", err.Model)
}

type responseDocument struct {
	Models              map[string]modelDetails `json:"models"`
	DefaultAgentModelID string                  `json:"defaultAgentModelId"`
	AgentModelSorts     []modelSort             `json:"agentModelSorts"`
}

type modelSort struct {
	Groups []modelGroup `json:"groups"`
}

type modelGroup struct {
	ModelIDs []string `json:"modelIds"`
}

type modelDetails struct {
	DisplayName              string    `json:"displayName"`
	Description              string    `json:"description"`
	Disabled                 bool      `json:"disabled"`
	Recommended              bool      `json:"recommended"`
	SupportsThinking         bool      `json:"supportsThinking"`
	SupportsAdaptiveThinking bool      `json:"supportsAdaptiveThinking"`
	ThinkingBudget           int       `json:"thinkingBudget"`
	MinThinkingBudget        int       `json:"minThinkingBudget"`
	MaxTokens                int       `json:"maxTokens"`
	MaxOutputTokens          int       `json:"maxOutputTokens"`
	QuotaInfo                quotaInfo `json:"quotaInfo"`
}

type quotaInfo struct {
	RemainingFraction *float64 `json:"remainingFraction"`
	ResetTime         string   `json:"resetTime"`
}

var routingAliases = map[string]string{
	// Cloud Code publishes gemini-3.1-pro-high in models, but current agy
	// selects the separate agent route for the same display model.
	"gemini-3.1-pro-high": "Gemini 3.1 Pro (High)",
	"gemini-3.1-pro":      "Gemini 3.1 Pro (High)",
	"gemini-pro":          "Gemini 3.1 Pro (High)",
	"gemini-3.6-flash":    "Gemini 3.6 Flash (High)",
	"gemini-3.7-flash":    "Gemini 3.7 Flash (High)",
	"gpt-oss-120b":        "GPT-OSS 120B (Medium)",
	// gemini-3.8-flash-{high,medium,low} first published by agy 1.1.25
	// (.reference/agy-models-20260903.txt, View A).
	"gemini-3.8-flash":        "Gemini 3.8 Flash (High)",
	"gemini-3.8-flash-high":   "Gemini 3.8 Flash (High)",
	"gemini-3.8-flash-medium": "Gemini 3.8 Flash (Medium)",
	"gemini-3.8-flash-low":    "Gemini 3.8 Flash (Low)",
	// Legacy gemini-3.5 IDs repoint to the gemini-3.8 family tier-preserving
	// (user decision 2026-09-03). Accounts that still publish real 3.5 are
	// unaffected: byID/byDisplay lookups win before routingAliases.
	"gemini-3.5-flash":           "Gemini 3.8 Flash (High)",
	"gemini-3.5-flash-high":      "Gemini 3.8 Flash (High)",
	"gemini-3.5-flash-medium":    "Gemini 3.8 Flash (Medium)",
	"gemini-3.5-flash-low":       "Gemini 3.8 Flash (Low)",
	"gemini-3.5-flash-extra-low": "Gemini 3.8 Flash (Low)",
	"gemini-3.7-flash-high":      "Gemini 3.7 Flash (High)",
	"gemini-3.7-flash-medium":    "Gemini 3.7 Flash (Medium)",
	"gemini-3.7-flash-low":       "Gemini 3.7 Flash (Low)",

	// Claude Models & Aliases mapped to Cloud Code display names
	"claude-sonnet-4-6-thinking": "Claude Sonnet 4.6 (Thinking)",
	"claude-sonnet-4-6":          "Claude Sonnet 4.6 (Thinking)",
	"claude-opus-4-6":            "Claude Opus 4.6 (Thinking)",
	"claude-opus-4-6-thinking":   "Claude Opus 4.6 (Thinking)",
	"claude-sonnet-5":            "Claude Sonnet 4.6 (Thinking)",
	"sonnet-5":                   "Claude Sonnet 4.6 (Thinking)",
	"sonnet":                     "Claude Sonnet 4.6 (Thinking)",
	"claude-opus-5":              "Claude Opus 4.6 (Thinking)",
	"opus-5":                     "Claude Opus 4.6 (Thinking)",
	"opus":                       "Claude Opus 4.6 (Thinking)",
	"claude-fable-5":             "Claude Sonnet 4.6 (Thinking)",
	"fable-5":                    "Claude Sonnet 4.6 (Thinking)",
	"fable":                      "Claude Sonnet 4.6 (Thinking)",
	"claude-haiku-4-5-20251001":  "Claude Sonnet 4.6 (Thinking)",
	"claude-haiku-4-5":           "Claude Sonnet 4.6 (Thinking)",
	"claude-haiku-4.5":           "Claude Sonnet 4.6 (Thinking)",
	"haiku-4-5":                  "Claude Sonnet 4.6 (Thinking)",
	"haiku-4.5":                  "Claude Sonnet 4.6 (Thinking)",
	"haiku":                      "Claude Sonnet 4.6 (Thinking)",
	"claude-3-7-sonnet-20250219": "Claude Sonnet 4.6 (Thinking)",
	"claude-3-7-sonnet":          "Claude Sonnet 4.6 (Thinking)",
	"claude-3.7-sonnet":          "Claude Sonnet 4.6 (Thinking)",
	"sonnet-3-7":                 "Claude Sonnet 4.6 (Thinking)",
	"sonnet-3.7":                 "Claude Sonnet 4.6 (Thinking)",
	"claude-3-5-sonnet-20241022": "Claude Sonnet 4.6 (Thinking)",
	"claude-3-5-sonnet":          "Claude Sonnet 4.6 (Thinking)",
	"claude-3.5-sonnet":          "Claude Sonnet 4.6 (Thinking)",
	"sonnet-3-5":                 "Claude Sonnet 4.6 (Thinking)",
	"sonnet-3.5":                 "Claude Sonnet 4.6 (Thinking)",
	"claude-3-5-haiku-20241022":  "Claude Sonnet 4.6 (Thinking)",
	"claude-3-5-haiku":           "Claude Sonnet 4.6 (Thinking)",
	"claude-3.5-haiku":           "Claude Sonnet 4.6 (Thinking)",
	"haiku-3-5":                  "Claude Sonnet 4.6 (Thinking)",
	"haiku-3.5":                  "Claude Sonnet 4.6 (Thinking)",
	"claude-3-opus-20240229":     "Claude Opus 4.6 (Thinking)",
	"claude-3-opus":              "Claude Opus 4.6 (Thinking)",
	"claude-3.0-opus":            "Claude Opus 4.6 (Thinking)",
	"opus-3":                     "Claude Opus 4.6 (Thinking)",
}

const gemini37TieredID = "gemini-3.7-flash-tiered"

const gemini38TieredID = "gemini-3.8-flash-tiered"

func Parse(body []byte) (*Catalog, error) {
	var document responseDocument
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("decode Cloud Code model catalog: %w", err)
	}
	if len(document.Models) == 0 {
		return nil, errors.New("Cloud Code model catalog is empty")
	}

	ids := make([]string, 0)
	seen := make(map[string]bool)
	for _, modelSort := range document.AgentModelSorts {
		for _, group := range modelSort.Groups {
			for _, id := range group.ModelIDs {
				if !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}
	}
	// Older responses did not include agentModelSorts. Keep a deterministic
	// compatibility fallback, while current agy's explicit list remains the
	// authoritative path.
	if len(ids) == 0 {
		for id, details := range document.Models {
			if details.DisplayName != "" && !details.Disabled && isAgentFamily(id) {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
	}

	catalog := &Catalog{
		defaultID: document.DefaultAgentModelID,
		byID:      make(map[string]Model),
		byDisplay: make(map[string]Model),
	}
	for _, id := range ids {
		details, exists := document.Models[id]
		if !exists || details.Disabled {
			continue
		}
		remaining := details.QuotaInfo.RemainingFraction
		if remaining == nil && details.QuotaInfo.ResetTime != "" {
			zero := 0.0
			remaining = &zero
		}
		model := Model{
			ID: id, DisplayName: details.DisplayName, Description: details.Description,
			Disabled: details.Disabled, Recommended: details.Recommended,
			SupportsThinking:         details.SupportsThinking,
			SupportsAdaptiveThinking: details.SupportsAdaptiveThinking,
			ThinkingBudget:           details.ThinkingBudget, MinThinkingBudget: details.MinThinkingBudget,
			MaxTokens: details.MaxTokens, MaxOutputTokens: details.MaxOutputTokens,
			QuotaRemainingFraction: remaining,
			QuotaResetTime:         details.QuotaInfo.ResetTime,
		}
		if model.DisplayName == "" {
			model.DisplayName = id
		}
		catalog.selectable = append(catalog.selectable, model)
		catalog.byID[strings.ToLower(id)] = model
		catalog.byDisplay[strings.ToLower(model.DisplayName)] = model
	}

	applyGemini37(catalog, document.Models)
	applyGemini38(catalog, document.Models)

	if len(catalog.selectable) == 0 {
		return nil, errors.New("Cloud Code catalog has no selectable agent models")
	}
	return catalog, nil
}

func (catalog *Catalog) DefaultID() string {
	if catalog == nil {
		return ""
	}
	if model, exists := catalog.byID[strings.ToLower(catalog.defaultID)]; exists {
		return model.ID
	}
	return catalog.selectable[0].ID
}

func (catalog *Catalog) Selectable() []Model {
	if catalog == nil {
		return nil
	}
	return append([]Model(nil), catalog.selectable...)
}

func (catalog *Catalog) Resolve(requested string) (Model, error) {
	if catalog == nil {
		return Model{}, errors.New("model catalog is unavailable")
	}
	key := strings.ToLower(strings.TrimSpace(requested))
	if key == "" {
		key = strings.ToLower(catalog.DefaultID())
	}
	if model, exists := catalog.byID[key]; exists {
		return model, nil
	}
	if model, exists := catalog.byDisplay[key]; exists {
		return model, nil
	}
	if displayName := routingAliases[key]; displayName != "" {
		if model, exists := catalog.byDisplay[strings.ToLower(displayName)]; exists {
			return model, nil
		}
	}
	normalized := strings.ReplaceAll(key, ".", "-")
	if displayName := routingAliases[normalized]; displayName != "" {
		if model, exists := catalog.byDisplay[strings.ToLower(displayName)]; exists {
			return model, nil
		}
	}

	available := make([]string, 0, len(catalog.selectable))
	for _, m := range catalog.selectable {
		available = append(available, m.ID)
	}
	return Model{}, &SelectionError{Model: requested, Selectable: available}
}

func ExtractReasoningParams(request map[string]any) (effort string, budget int, hasBudget bool, disabled bool) {
	if request == nil {
		return "", 0, false, false
	}
	if val, ok := request["reasoning_effort"]; ok {
		effort = strings.ToLower(fmt.Sprint(val))
	}
	if effort == "" {
		if val, ok := request["reasoning"]; ok {
			effort = strings.ToLower(fmt.Sprint(val))
		}
	}
	switch effort {
	case "xhigh", "extra-high", "very-high", "max", "maximum", "extreme":
		effort = "high"
	case "minimal":
		effort = "low"
	case "none", "disabled", "off", "false", "0":
		effort = "disabled"
		disabled = true
	}
	if thinking, ok := request["thinking"].(map[string]any); ok {
		if tType, exists := thinking["type"]; exists && strings.ToLower(fmt.Sprint(tType)) == "disabled" {
			disabled = true
		}
		if b, exists := thinking["budget_tokens"]; exists {
			switch v := b.(type) {
			case float64:
				budget = int(v)
				hasBudget = true
			case int:
				budget = v
				hasBudget = true
			case int64:
				budget = int(v)
				hasBudget = true
			}
		}
	}
	if b, ok := request["thinking_budget"]; ok {
		switch v := b.(type) {
		case float64:
			budget = int(v)
			hasBudget = true
		case int:
			budget = v
			hasBudget = true
		case int64:
			budget = int(v)
			hasBudget = true
		}
	}
	if effort == "none" || effort == "disabled" {
		disabled = true
	}
	if hasBudget && budget <= 0 && !disabled {
		thinking, _ := request["thinking"].(map[string]any)
		if thinking == nil || strings.ToLower(fmt.Sprint(thinking["type"])) != "enabled" {
			disabled = true
		}
	}
	if !disabled && effort == "" && hasBudget && budget > 0 {
		switch {
		case budget <= 2048:
			effort = "low"
		case budget < 12000:
			effort = "medium"
		default:
			effort = "high"
		}
	}
	return effort, budget, hasBudget, disabled
}

func (catalog *Catalog) ResolveWithRequest(requested string, request map[string]any) (Model, error) {
	model, err := catalog.Resolve(requested)
	if err != nil {
		return Model{}, err
	}
	if request == nil {
		return model, nil
	}

	effort, budget, hasBudget, disabled := ExtractReasoningParams(request)

	if disabled {
		if isGemini37Flash(model.ID) || isGemini37Flash(requested) {
			if variant, err := catalog.Resolve("gemini-3.7-flash-low"); err == nil {
				return variant, nil
			}
			model.ThinkingLevel = "LOW"
			model.SupportsThinking = true
			return model, nil
		}
		if isGemini38Flash(model.ID) || isGemini38Flash(requested) {
			if variant, err := catalog.Resolve("gemini-3.8-flash-low"); err == nil {
				return variant, nil
			}
			model.ThinkingLevel = "LOW"
			model.SupportsThinking = true
			return model, nil
		}
		model.SupportsThinking = false
		model.ThinkingBudget = 0
		return model, nil
	}

	if effort != "" {
		targetID := ""
		lowerReq := strings.ToLower(strings.TrimSpace(requested))
		switch {
		case strings.HasPrefix(lowerReq, "gemini-3.8-flash"):
			targetID = "gemini-3.8-flash-" + effort
		case strings.HasPrefix(lowerReq, "gemini-3.7-flash"):
			targetID = "gemini-3.7-flash-" + effort
		case strings.HasPrefix(lowerReq, "gemini-3.6-flash"):
			targetID = "gemini-3.6-flash-" + effort
		case strings.HasPrefix(lowerReq, "gemini-3.5-flash"):
			// Legacy 3.5 IDs repoint to the 3.8 family (user decision
			// 2026-09-03); fall back to a real 3.5 tier only when this
			// account has no 3.8 at all.
			targetID = "gemini-3.8-flash-" + effort
		}
		if targetID != "" {
			if variant, err := catalog.Resolve(targetID); err == nil {
				if hasBudget && budget > 0 {
					variant.ThinkingBudget = budget
				}
				return variant, nil
			}
			if strings.HasPrefix(lowerReq, "gemini-3.5-flash") {
				if variant, err := catalog.Resolve("gemini-3.5-flash-" + effort); err == nil {
					if hasBudget && budget > 0 {
						variant.ThinkingBudget = budget
					}
					return variant, nil
				}
			}
		}
	}

	if hasBudget && budget > 0 {
		model.ThinkingBudget = budget
	}
	return model, nil
}

func CleanModelIDAndName(id, displayName string) (string, string) {
	lowerID := strings.ToLower(id)
	cleanID := id
	cleanName := displayName

	switch {
	case strings.HasPrefix(lowerID, "gemini-3.8-flash"):
		cleanID = "gemini-3.8-flash"
		cleanName = "Gemini 3.8 Flash"
	case strings.HasPrefix(lowerID, "gemini-3.7-flash"):
		cleanID = "gemini-3.7-flash"
		cleanName = "Gemini 3.7 Flash"
	case strings.HasPrefix(lowerID, "gemini-3.6-flash"):
		cleanID = "gemini-3.6-flash"
		cleanName = "Gemini 3.6 Flash"
	case lowerID == "gemini-3-flash-agent" || lowerID == "gemini-3.5-flash-low" || lowerID == "gemini-3.5-flash-extra-low" || lowerID == "gemini-3.5-flash-high" || lowerID == "gemini-3.5-flash-medium":
		cleanID = "gemini-3.5-flash"
		cleanName = "Gemini 3.5 Flash"
	case lowerID == "gemini-pro-agent" || lowerID == "gemini-3.1-pro-high" || lowerID == "gemini-3.1-pro-low":
		cleanID = "gemini-3.1-pro"
		cleanName = "Gemini 3.1 Pro"
	case lowerID == "claude-opus-4-6-thinking" || lowerID == "claude-opus-4-6":
		cleanID = "claude-opus-4-6"
		cleanName = "Claude Opus 4.6"
	case lowerID == "gpt-oss-120b-medium" || lowerID == "gpt-oss-120b":
		cleanID = "gpt-oss-120b"
		cleanName = "GPT-OSS 120B"
	default:
		for _, suffix := range []string{" (High)", " (Medium)", " (Low)", " (Thinking)"} {
			cleanName = strings.TrimSuffix(cleanName, suffix)
		}
	}
	return cleanID, cleanName
}

func (catalog *Catalog) PublicModels() []Model {
	if catalog == nil {
		return nil
	}
	var result []Model
	seen := make(map[string]bool)
	for _, m := range catalog.selectable {
		cleanID, cleanName := CleanModelIDAndName(m.ID, m.DisplayName)
		if seen[cleanID] {
			continue
		}
		seen[cleanID] = true
		pubModel := m
		pubModel.ID = cleanID
		pubModel.DisplayName = cleanName
		result = append(result, pubModel)
	}
	return result
}

func isAgentFamily(id string) bool {
	lower := strings.ToLower(id)
	return strings.Contains(lower, "claude") || strings.Contains(lower, "gemini") || strings.Contains(lower, "gpt")
}

func isGemini37Flash(id string) bool {
	return strings.HasPrefix(strings.ToLower(id), "gemini-3.7-flash")
}

func isGemini38Flash(id string) bool {
	return strings.HasPrefix(strings.ToLower(id), "gemini-3.8-flash")
}

func modelFromDetails(id string, details modelDetails) Model {
	remaining := details.QuotaInfo.RemainingFraction
	if remaining == nil && details.QuotaInfo.ResetTime != "" {
		zero := 0.0
		remaining = &zero
	}
	model := Model{
		ID: id, DisplayName: details.DisplayName, Description: details.Description,
		Disabled: details.Disabled, Recommended: details.Recommended,
		SupportsThinking:         details.SupportsThinking,
		SupportsAdaptiveThinking: details.SupportsAdaptiveThinking,
		ThinkingBudget:           details.ThinkingBudget, MinThinkingBudget: details.MinThinkingBudget,
		MaxTokens: details.MaxTokens, MaxOutputTokens: details.MaxOutputTokens,
		QuotaRemainingFraction: remaining,
		QuotaResetTime:         details.QuotaInfo.ResetTime,
	}
	if model.DisplayName == "" {
		model.DisplayName = id
	}
	return model
}

// applyGemini37 publishes gemini-3.7-flash-{high,medium,low}. Cloud Code may
// serve 3.7 as a single gemini-3.7-flash-tiered runtime plus thinkingLevel;
// agy 1.1.25 also publishes the high/medium/low slugs directly
// (.reference/agy-models-20260903.txt). When the tiered model is present it
// backs every tier; when absent, upstream's own direct tier entries are kept
// verbatim, and only missing tiers fall back to the matching 3.6 Flash ID.
func applyGemini37(catalog *Catalog, models map[string]modelDetails) {
	variants := []struct {
		id, displayName, level, fallback string
	}{
		{"gemini-3.7-flash-high", "Gemini 3.7 Flash (High)", "HIGH", "gemini-3.6-flash-high"},
		{"gemini-3.7-flash-medium", "Gemini 3.7 Flash (Medium)", "MEDIUM", "gemini-3.6-flash-medium"},
		{"gemini-3.7-flash-low", "Gemini 3.7 Flash (Low)", "LOW", "gemini-3.6-flash-low"},
	}

	tiered, hasTiered := models[gemini37TieredID]
	hasTiered = hasTiered && !tiered.Disabled

	present := make(map[string]int, len(catalog.selectable))
	for i, model := range catalog.selectable {
		present[model.ID] = i
	}

	var prepend []Model
	for _, variant := range variants {
		var model Model
		if hasTiered {
			model = modelFromDetails(gemini37TieredID, tiered)
			model.UpstreamID = gemini37TieredID
			model.ThinkingLevel = variant.level
		} else if _, ok := catalog.byID[variant.id]; ok {
			// Upstream publishes this tier directly; keep it verbatim.
			continue
		} else if base, ok := catalog.byID["gemini-3.7-flash"]; ok {
			model = base
			model.UpstreamID = "gemini-3.7-flash"
			model.ThinkingLevel = variant.level
		} else if base, ok := catalog.byID[variant.fallback]; ok {
			model = base
			model.UpstreamID = variant.fallback
		} else {
			continue
		}
		model.ID = variant.id
		model.DisplayName = variant.displayName
		catalog.byID[variant.id] = model
		catalog.byDisplay[strings.ToLower(variant.displayName)] = model
		if index, exists := present[variant.id]; exists {
			catalog.selectable[index] = model
			continue
		}
		prepend = append(prepend, model)
		present[variant.id] = -1
	}
	if len(prepend) > 0 {
		catalog.selectable = append(prepend, catalog.selectable...)
	}
}

// applyGemini38 publishes gemini-3.8-flash-{high,medium,low} the same way
// applyGemini37 handles 3.7: a gemini-3.8-flash-tiered upstream entry backs
// every tier when present; otherwise direct upstream tier entries are kept
// verbatim and only missing tiers fall back to the matching 3.7 Flash ID.
func applyGemini38(catalog *Catalog, models map[string]modelDetails) {
	variants := []struct {
		id, displayName, level, fallback string
	}{
		{"gemini-3.8-flash-high", "Gemini 3.8 Flash (High)", "HIGH", "gemini-3.7-flash-high"},
		{"gemini-3.8-flash-medium", "Gemini 3.8 Flash (Medium)", "MEDIUM", "gemini-3.7-flash-medium"},
		{"gemini-3.8-flash-low", "Gemini 3.8 Flash (Low)", "LOW", "gemini-3.7-flash-low"},
	}

	tiered, hasTiered := models[gemini38TieredID]
	hasTiered = hasTiered && !tiered.Disabled

	present := make(map[string]int, len(catalog.selectable))
	for i, model := range catalog.selectable {
		present[model.ID] = i
	}

	var prepend []Model
	for _, variant := range variants {
		var model Model
		if hasTiered {
			model = modelFromDetails(gemini38TieredID, tiered)
			model.UpstreamID = gemini38TieredID
			model.ThinkingLevel = variant.level
		} else if _, ok := catalog.byID[variant.id]; ok {
			// Upstream publishes this tier directly; keep it verbatim.
			continue
		} else if base, ok := catalog.byID["gemini-3.8-flash"]; ok {
			model = base
			model.UpstreamID = "gemini-3.8-flash"
			model.ThinkingLevel = variant.level
		} else if base, ok := catalog.byID[variant.fallback]; ok {
			model = base
			model.UpstreamID = variant.fallback
		} else {
			continue
		}
		model.ID = variant.id
		model.DisplayName = variant.displayName
		catalog.byID[variant.id] = model
		catalog.byDisplay[strings.ToLower(variant.displayName)] = model
		if index, exists := present[variant.id]; exists {
			catalog.selectable[index] = model
			continue
		}
		prepend = append(prepend, model)
		present[variant.id] = -1
	}
	if len(prepend) > 0 {
		catalog.selectable = append(prepend, catalog.selectable...)
	}
}
