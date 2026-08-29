package ccr

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"antigravity-go-proxy/internal/headroom"
)

// defaultMinChunkBytes is the minimum byte size for a payload to be demoted.
const defaultMinChunkBytes = 2048

// RetrieveToolDefinition defines the tool schema injected for CCR retrieval.
var RetrieveToolDefinition = map[string]any{
	"name":        "headroom_retrieve",
	"description": "Retrieve the full content of a demoted context chunk by its chunk ID.",
	"input_schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"chunk_id": map[string]any{
				"type":        "string",
				"description": "The chunk ID to retrieve, formatted as chunk_<hash>.",
			},
		},
		"required": []any{"chunk_id"},
	},
}

// CCRStage performs Content-Conditioned Retrieval demotion of oversized tool results
// in historical (frozen) conversation turns.
type CCRStage struct {
	store *CCRStore
}

// NewStage creates a new CCRStage backed by the given CCRStore.
func NewStage(store *CCRStore) *CCRStage {
	return &CCRStage{store: store}
}

func (s *CCRStage) Name() string { return "ccr" }

// FormatChunkToken renders the standard demoted chunk placeholder.
func FormatChunkToken(id, content string) string {
	lines := strings.Count(content, "\n") + 1
	preview := makePreview(content, 60)
	return fmt.Sprintf("[HEADROOM_CHUNK id=%q lines=%d preview=%q]", id, lines, preview)
}

func makePreview(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "\n"); idx != -1 {
		s = s[:idx]
	}
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, `"`, `'`)
	if len(s) > maxLen {
		if maxLen > 3 {
			s = s[:maxLen-3] + "..."
		} else {
			s = s[:maxLen]
		}
	}
	return s
}

func (s *CCRStage) Execute(ctx context.Context, reqCtx *headroom.RequestContext, cfg *headroom.Config) error {
	if !cfg.CCR.Enabled || reqCtx == nil || reqCtx.Request == nil {
		return nil
	}
	if s.store == nil {
		return nil
	}

	minBytes := cfg.CCR.MinChunkBytes
	if minBytes <= 0 {
		minBytes = defaultMinChunkBytes
	}

	log := reqCtx.Log()
	dbg := log.Enabled(ctx, slog.LevelDebug)

	// Only demote if FrozenPrefixIndex >= 0 (there is at least one message outside the live window)
	if reqCtx.FrozenPrefixIndex >= 0 {
		headroom.WalkToolResultText(reqCtx.Request, 0, func(idx, ord int, get func() string, set func(string)) {
			if idx > reqCtx.FrozenPrefixIndex {
				return // live turn; keep inline
			}
			if headroom.SkipVerbatim(reqCtx, cfg, ord) {
				return // file content the model will quote back; demotion would
				// force a retrieve round trip before any Edit could match
			}
			before := get()
			if len(before) < minBytes {
				return
			}
			id, ok := s.store.Put(before)
			if !ok {
				return
			}
			token := FormatChunkToken(id, before)
			set(token)
			reqCtx.RecordRewrite(before, token)
			reqCtx.ChunksStored++
			if dbg {
				log.Debug("ccr demoted chunk",
					"stage", s.Name(), "chunk_id", id,
					"ordinal", ord, "message_index", idx,
					"chunk_bytes", len(before), "store_bytes", s.store.Bytes())
			}
		})
	}

	// Invariant I4: Only inject tool if the client provided a tools list.
	// Invariant I2: Inject unconditionally when tools are present to maintain cache stability.
	if tools, ok := reqCtx.Request["tools"].([]any); ok && len(tools) > 0 {
		hasRetrieve := false
		for _, rawTool := range tools {
			if toolMap, isMap := rawTool.(map[string]any); isMap {
				if name, _ := toolMap["name"].(string); name == "headroom_retrieve" {
					hasRetrieve = true
					break
				}
			}
		}
		if !hasRetrieve {
			reqCtx.Request["tools"] = append(tools, RetrieveToolDefinition)
		}
	}

	return nil
}
