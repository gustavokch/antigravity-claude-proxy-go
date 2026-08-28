package api

import (
	"bytes"
	"encoding/json"
)

// retrieveToolName is the CCR tool whose calls are internal to the proxy and
// must never reach a downstream SSE client.
const retrieveToolName = "headroom_retrieve"

// ccrStreamState tracks content-block bookkeeping for one upstream iteration of
// the CCR hydration loop.
//
// Upstream numbers its content blocks 0..N-1 per iteration. Downstream must see
// a single gapless 0-based sequence across every iteration, with the
// headroom_retrieve blocks removed entirely. This type owns that mapping: the
// caller asks whether a block may be emitted and, if so, at which index.
type ccrStreamState struct {
	baseIndex int // downstream indexes already consumed by earlier iterations

	blocks   []map[string]any      // upstream index -> accumulated block
	jsonBufs map[int]*bytes.Buffer // upstream index -> tool_use input JSON

	suppressed map[int]bool // upstream index -> hidden from the client
	downstream map[int]int  // upstream index -> downstream index
	nextLocal  int          // downstream slots consumed by this iteration

	// orphanEvents counts delta/stop events for an index that never had a
	// content_block_start. They are dropped: emitting them under an unmapped
	// upstream index would collide with the downstream numbering.
	orphanEvents int
}

func newCCRStreamState(baseIndex int) *ccrStreamState {
	return &ccrStreamState{
		baseIndex:  baseIndex,
		jsonBufs:   make(map[int]*bytes.Buffer),
		suppressed: make(map[int]bool),
		downstream: make(map[int]int),
	}
}

// StartBlock records a content_block_start. emit is false when the block is a
// headroom_retrieve tool_use, in which case downstreamIdx is meaningless and the
// caller must not write the event.
func (s *ccrStreamState) StartBlock(upstreamIdx int, block map[string]any) (int, bool) {
	for len(s.blocks) <= upstreamIdx {
		s.blocks = append(s.blocks, nil)
	}
	s.blocks[upstreamIdx] = block

	bType, _ := block["type"].(string)
	if bType == "tool_use" {
		s.jsonBufs[upstreamIdx] = &bytes.Buffer{}
		if name, _ := block["name"].(string); name == retrieveToolName {
			s.suppressed[upstreamIdx] = true
			return 0, false
		}
	}

	idx := s.baseIndex + s.nextLocal
	s.nextLocal++
	s.downstream[upstreamIdx] = idx
	return idx, true
}

// MapIndex resolves the downstream index for a content_block_delta or
// content_block_stop. emit is false for suppressed blocks and for orphans.
func (s *ccrStreamState) MapIndex(upstreamIdx int) (int, bool) {
	if s.suppressed[upstreamIdx] {
		return 0, false
	}
	idx, ok := s.downstream[upstreamIdx]
	if !ok {
		s.orphanEvents++
		return 0, false
	}
	return idx, true
}

// AppendJSON accumulates an input_json_delta fragment for a tool_use block.
// Fragments are buffered for suppressed blocks too: the retrieve call's chunk_id
// is only readable once the whole input JSON has arrived.
func (s *ccrStreamState) AppendJSON(upstreamIdx int, partial string) {
	if buf, exists := s.jsonBufs[upstreamIdx]; exists {
		buf.WriteString(partial)
	}
}

// AppendText accumulates a text_delta onto the block, so the replayed assistant
// message carries the full text upstream produced.
func (s *ccrStreamState) AppendText(upstreamIdx int, text string) {
	if upstreamIdx < len(s.blocks) && s.blocks[upstreamIdx] != nil {
		prevText, _ := s.blocks[upstreamIdx]["text"].(string)
		s.blocks[upstreamIdx]["text"] = prevText + text
	}
}

// Finalize parses each buffered tool_use input into block["input"] and returns
// the headroom_retrieve calls in document order. Call once, after the upstream
// stream has ended.
func (s *ccrStreamState) Finalize() []map[string]any {
	var retrieveCalls []map[string]any
	for idx, block := range s.blocks {
		if block == nil {
			continue
		}
		if buf, exists := s.jsonBufs[idx]; exists && buf.Len() > 0 {
			var inputMap map[string]any
			if err := json.Unmarshal(buf.Bytes(), &inputMap); err == nil {
				block["input"] = inputMap
			}
		}
		if bType, _ := block["type"].(string); bType == "tool_use" {
			if bName, _ := block["name"].(string); bName == retrieveToolName {
				retrieveCalls = append(retrieveCalls, block)
			}
		}
	}
	return retrieveCalls
}

// VisibleCount is how many downstream indexes this iteration consumed. The
// caller adds it — not len(blocks) — to the base index before the next
// iteration, so suppressed blocks leave no gap.
func (s *ccrStreamState) VisibleCount() int {
	return s.nextLocal
}

// AssistantBlocks returns every non-nil block, suppressed ones included, for the
// assistant message replayed upstream on the next hydration round. Upstream must
// still see its own tool_use blocks or the tool_result ids will not resolve.
func (s *ccrStreamState) AssistantBlocks() []any {
	var validBlocks []any
	for _, b := range s.blocks {
		if b != nil {
			validBlocks = append(validBlocks, b)
		}
	}
	return validBlocks
}

// stripRetrieveBlocks removes headroom_retrieve tool_use blocks from an
// Anthropic response content array, in place on the decoded map. Used on the
// terminal response of every non-streaming path, including the one reached when
// the hydration cap is hit with retrieve calls still outstanding.
func stripRetrieveBlocks(resp map[string]any) {
	if resp == nil {
		return
	}
	content, ok := resp["content"].([]any)
	if !ok || len(content) == 0 {
		return
	}
	filtered := make([]any, 0, len(content))
	for _, item := range content {
		m, ok := item.(map[string]any)
		if !ok {
			filtered = append(filtered, item)
			continue
		}
		if bType, _ := m["type"].(string); bType == "tool_use" {
			if bName, _ := m["name"].(string); bName == retrieveToolName {
				continue
			}
		}
		filtered = append(filtered, item)
	}
	resp["content"] = filtered
}

// stripRetrieveBlocksJSON does the same over a raw JSON body, returning the
// original bytes unchanged when the body is not a decodable Anthropic response
// or contains no retrieve blocks.
func stripRetrieveBlocksJSON(body []byte) []byte {
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return body
	}
	content, ok := resp["content"].([]any)
	if !ok || len(content) == 0 {
		return body
	}
	hasRetrieve := false
	for _, item := range content {
		if m, ok := item.(map[string]any); ok {
			if bType, _ := m["type"].(string); bType == "tool_use" {
				if bName, _ := m["name"].(string); bName == retrieveToolName {
					hasRetrieve = true
					break
				}
			}
		}
	}
	if !hasRetrieve {
		return body
	}
	stripRetrieveBlocks(resp)
	out, err := json.Marshal(resp)
	if err != nil {
		return body
	}
	return out
}
