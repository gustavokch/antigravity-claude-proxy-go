package tokencount

import (
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"hash"
	"maps"
	"slices"
	"time"
)

const (
	// conversationIdleTTL drops a conversation's memory an hour after the
	// last request observed in it, the lifetime of a session pin.
	conversationIdleTTL = time.Hour
	// maxConversations caps the memory; the least recently observed
	// conversation goes first.
	maxConversations = 10000
)

// conversation is what Estimate keeps of the last request observed in a
// session for one family: its real input count, how many messages it had,
// and digests that tell whether a later request continues it.
type conversation struct {
	key       string
	realInput int
	messages  int
	prefix    [sha256.Size]byte // digestMessages of all its messages
	context   [sha256.Size]byte // digestContext of its system and tools
	lastSeen  time.Time
}

func memoryKey(session, family string) string {
	return session + "\x00" + family
}

func newConversation(key string, request map[string]any, realInput int) conversation {
	messages, _ := request["messages"].([]any)
	return conversation{
		key:       key,
		realInput: realInput,
		messages:  len(messages),
		prefix:    digestMessages(messages),
		context:   digestContext(request),
	}
}

// continuedBy reports whether request has the same system and tools as the
// remembered one and starts with all of its messages.
func (memory conversation) continuedBy(request map[string]any, messages []any) bool {
	return len(messages) >= memory.messages &&
		digestContext(request) == memory.context &&
		digestMessages(messages[:memory.messages]) == memory.prefix
}

// rememberLocked stores memory as its session's latest conversation.
func (c *Counter) rememberLocked(memory conversation, now time.Time) {
	c.pruneLocked(now)
	memory.lastSeen = now
	if element, ok := c.conversations[memory.key]; ok {
		element.Value = memory
		c.order.MoveToFront(element)
		return
	}
	if c.order.Len() >= maxConversations {
		c.dropLocked(c.order.Back())
	}
	c.conversations[memory.key] = c.order.PushFront(memory)
}

func (c *Counter) recallLocked(key string, now time.Time) (conversation, bool) {
	c.pruneLocked(now)
	element, ok := c.conversations[key]
	if !ok {
		return conversation{}, false
	}
	return element.Value.(conversation), true
}

// pruneLocked drops the conversations idle for more than conversationIdleTTL.
// order runs from the latest observation to the oldest, so they are all at
// its back.
func (c *Counter) pruneLocked(now time.Time) {
	for element := c.order.Back(); element != nil; element = c.order.Back() {
		if now.Sub(element.Value.(conversation).lastSeen) <= conversationIdleTTL {
			return
		}
		c.dropLocked(element)
	}
}

func (c *Counter) dropLocked(element *list.Element) {
	delete(c.conversations, element.Value.(conversation).key)
	c.order.Remove(element)
}

// digestContext hashes what a request carries besides its messages: the
// system prompt and the tool list.
func digestContext(request map[string]any) [sha256.Size]byte {
	digest := sha256.New()
	writeDigest(digest, request["system"])
	writeDigest(digest, request["tools"])
	return sum(digest)
}

// digestMessages hashes messages as compact JSON, one encoding per message.
func digestMessages(messages []any) [sha256.Size]byte {
	digest := sha256.New()
	for _, message := range messages {
		writeDigest(digest, message)
	}
	return sum(digest)
}

// writeDigest writes value to digest as compact JSON. encoding/json writes map
// keys sorted, so two decodes of one body hash alike whatever key order the
// client sent. cache_control markers are left out: a client that moves its
// cache breakpoint to the newest message, as Claude Code does, sends the
// earlier messages without the marker they carried the turn before, and a
// marker costs no tokens. Encode writes nothing for a value that does not
// encode, which leaves it out on both sides alike.
func writeDigest(digest hash.Hash, value any) {
	stripped, _ := withoutCacheControl(value)
	_ = json.NewEncoder(digest).Encode(stripped)
}

func sum(digest hash.Hash) [sha256.Size]byte {
	var out [sha256.Size]byte
	digest.Sum(out[:0])
	return out
}

// withoutCacheControl returns value without its cache_control members, at
// any depth, and whether it removed any. It copies only the maps and slices
// on the way to a marker: the request still belongs to the caller, who sends
// it upstream.
func withoutCacheControl(value any) (any, bool) {
	switch value := value.(type) {
	case map[string]any:
		var out map[string]any
		for key, member := range value {
			if key == "cache_control" {
				if out == nil {
					out = maps.Clone(value)
				}
				delete(out, key)
				continue
			}
			if stripped, changed := withoutCacheControl(member); changed {
				if out == nil {
					out = maps.Clone(value)
				}
				out[key] = stripped
			}
		}
		if out == nil {
			return value, false
		}
		return out, true
	case []any:
		var out []any
		for i, element := range value {
			if stripped, changed := withoutCacheControl(element); changed {
				if out == nil {
					out = slices.Clone(value)
				}
				out[i] = stripped
			}
		}
		if out == nil {
			return value, false
		}
		return out, true
	}
	return value, false
}
