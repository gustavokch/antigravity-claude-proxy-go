package format

import (
	"sync"
	"time"
)

const signatureCacheTTL = 2 * time.Hour

type signatureEntry struct {
	family    ModelFamily
	createdAt time.Time
}

// SignatureCache records the model family that produced a thinking signature.
// It is process-local for two hours because Claude clients may strip these fields.
type SignatureCache struct {
	mu       sync.RWMutex
	now      func() time.Time
	thinking map[string]signatureEntry
}

func NewSignatureCache() *SignatureCache {
	return &SignatureCache{
		now:      time.Now,
		thinking: make(map[string]signatureEntry),
	}
}

func (cache *SignatureCache) CacheThinking(signature string, family ModelFamily) {
	if cache == nil || len(signature) < MinSignatureLength {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.thinking[signature] = signatureEntry{family: family, createdAt: cache.now()}
}

func (cache *SignatureCache) ThinkingFamily(signature string) ModelFamily {
	if cache == nil || signature == "" {
		return FamilyUnknown
	}
	cache.mu.RLock()
	entry, ok := cache.thinking[signature]
	cache.mu.RUnlock()
	if !ok {
		return FamilyUnknown
	}
	if cache.now().Sub(entry.createdAt) > signatureCacheTTL {
		cache.mu.Lock()
		delete(cache.thinking, signature)
		cache.mu.Unlock()
		return FamilyUnknown
	}
	return entry.family
}

func (cache *SignatureCache) Clear() {
	if cache == nil {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	clear(cache.thinking)
}
