package api

import (
	"sort"
	"strconv"
	"time"

	"antigravity-go-proxy/internal/codex"
	"antigravity-go-proxy/internal/config"
)

// codexUsageWindows returns the /v1/usage windows of the Codex accounts: for
// each enabled account, every usage window its last Codex response reported
// (the 5-hour one and the weekly one), in pool order. A window whose reset
// has passed is left out, since its reading describes a window that is over.
func (server *Server) codexUsageWindows(now time.Time) []any {
	cfg := config.Get().Codex
	if !cfg.Enabled && len(cfg.Accounts) == 0 {
		return nil
	}
	pool, _ := server.codexPoolFor(cfg)
	var windows []any
	var modelIDs []string
	for _, acc := range pool.Snapshots() {
		if !acc.Enabled || acc.Quota == nil {
			continue
		}
		for _, w := range []*codex.Window{acc.Quota.Primary, acc.Quota.Secondary} {
			if w == nil || !w.ResetAt.After(now) {
				continue
			}
			if modelIDs == nil {
				modelIDs = codexModelIDs(cfg)
			}
			used := min(100, max(0, w.UsedPercent))
			windows = append(windows, map[string]any{
				"label":              "Codex " + codexWindowLabel(w.WindowMinutes) + " (" + codexDisplayName(acc) + ")",
				"remaining_fraction": 1 - used/100,
				"used_percent":       used,
				"reset_at":           w.ResetAt.UTC().Format(time.RFC3339),
				"model_ids":          modelIDs,
				"provider":           "codex",
				"account_id":         acc.ID,
			})
		}
	}
	return windows
}

// codexWindowLabel names a usage window by its length: the backend's 5-hour
// and weekly windows by their usual names, anything else in minutes.
func codexWindowLabel(minutes int) string {
	switch minutes {
	case 300:
		return "5h"
	case 7 * 24 * 60:
		return "weekly"
	}
	return strconv.Itoa(minutes) + "m"
}

// codexDisplayName names an account in a window label: its name, else its
// email, else its ID.
func codexDisplayName(acc codex.AccountSnapshot) string {
	switch {
	case acc.Name != "":
		return acc.Name
	case acc.Email != "":
		return acc.Email
	}
	return acc.ID
}

// codexModelIDs lists, sorted, the enabled model IDs and aliases the Codex
// gateway serves. Every account draws on the same windows for all of them.
func codexModelIDs(cfg codex.Config) []string {
	seen := map[string]bool{}
	ids := []string{}
	for _, m := range codexAllowlist(cfg) {
		if !m.Enabled {
			continue
		}
		for _, id := range append([]string{m.ID}, m.ExpandAliases()...) {
			if id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	return ids
}
