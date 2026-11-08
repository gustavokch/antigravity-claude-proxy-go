package codex

import (
	"strconv"
	"testing"
)

// The store is bounded: past maxRefusedSignatures the signature recorded first
// is forgotten, and the rest stay refused.
func TestRefusedSignaturesForgetTheOldestPastTheCap(t *testing.T) {
	var r RefusedSignatures
	for i := range maxRefusedSignatures + 1 {
		r.Add("sig-" + strconv.Itoa(i))
	}
	if r.Has("sig-0") {
		t.Error("the first signature is still refused past the cap")
	}
	for _, sig := range []string{"sig-1", "sig-" + strconv.Itoa(maxRefusedSignatures)} {
		if !r.Has(sig) {
			t.Errorf("%s is no longer refused", sig)
		}
	}
	if len(r.set) != maxRefusedSignatures {
		t.Errorf("held %d signatures, want %d", len(r.set), maxRefusedSignatures)
	}
	var none *RefusedSignatures
	if none.Has("sig-1") {
		t.Error("a nil store refuses a signature")
	}
}
