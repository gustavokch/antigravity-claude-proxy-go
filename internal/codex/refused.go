package codex

import (
	"encoding/json"
	"hash/maphash"
	"sync"
)

// maxRefusedSignatures bounds RefusedSignatures; past it the signature
// recorded first is forgotten.
const maxRefusedSignatures = 10000

// RefusedSignatures remembers the reasoning signatures the backend refused
// (IsInvalidEncryptedContent), so BuildRequest leaves them out of every later
// request. The client keeps a refused signature in its history and sends it
// back each turn; without this, each turn would pay the same 400 and resend.
//
// It keeps a 64-bit hash of each signature, not the signature (a few KiB). A
// collision drops a valid signature, which costs that turn the model's earlier
// reasoning, the same as a refusal does. The zero value is ready to use, and a
// nil *RefusedSignatures holds nothing.
type RefusedSignatures struct {
	mu    sync.Mutex
	seed  maphash.Seed
	set   map[uint64]struct{}
	order []uint64 // recording order, a ring once full
	next  int      // the oldest entry of order once it is full
}

// Add records signatures as refused.
func (r *RefusedSignatures) Add(signatures ...string) {
	if r == nil || len(signatures) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.set == nil {
		r.seed = maphash.MakeSeed()
		r.set = make(map[uint64]struct{})
	}
	for _, sig := range signatures {
		h := maphash.String(r.seed, sig)
		if _, ok := r.set[h]; ok {
			continue
		}
		if len(r.order) < maxRefusedSignatures {
			r.order = append(r.order, h)
		} else {
			delete(r.set, r.order[r.next])
			r.order[r.next] = h
			r.next = (r.next + 1) % maxRefusedSignatures
		}
		r.set[h] = struct{}{}
	}
}

// Has reports whether signature was refused.
func (r *RefusedSignatures) Has(signature string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.set) == 0 {
		return false
	}
	_, ok := r.set[maphash.String(r.seed, signature)]
	return ok
}

// ReasoningSignatures lists the encrypted_content of the reasoning items in a
// body BuildRequest built.
func ReasoningSignatures(body []byte) []string {
	var req struct {
		Input []struct {
			Type             string `json:"type"`
			EncryptedContent string `json:"encrypted_content"`
		} `json:"input"`
	}
	if json.Unmarshal(body, &req) != nil {
		return nil
	}
	var sigs []string
	for _, item := range req.Input {
		if item.Type == "reasoning" && item.EncryptedContent != "" {
			sigs = append(sigs, item.EncryptedContent)
		}
	}
	return sigs
}
