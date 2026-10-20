package ccidentity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"unicode/utf16"

	"github.com/cespare/xxhash/v2"
)

// cchSeed is the xxHash64 seed of the cch field in the billing header.
//
// Claude Code's JavaScript writes the placeholder cch=00000, and native code in
// its Bun runtime replaces it with xxHash64(preimage, cchSeed) & 0xFFFFF as five
// hex characters before the body is sent. The seed is not a string in the
// binary; it was recovered by public reverse engineering
// (github.com/BYK/loreai packages/gateway/src/cch.ts, unchanged since 2.1.138).
// Every body captured from 2.1.304 matches it; three of them are kept in
// testdata/claude-code-2.1.304-bodies.jsonl.gz.
const cchSeed = 0x4d659218e32a3268

// cchPlaceholder is the cch value the hash is computed over. The JavaScript
// writes it, and the native code hashes the body with it still in place.
const cchPlaceholder = "cch=00000"

// versionSuffixSalt salts the third component of cc_version. It is a literal
// in the 2.1.304 JavaScript (var cW="59cf53e54c78").
const versionSuffixSalt = "59cf53e54c78"

// versionSuffix is the third component of cc_version, e.g. the "456" in
// 2.1.304.456. Claude Code 2.1.304 computes it as:
//
//	sha256(salt + text[4] + text[7] + text[20] + version).hex()[:3]
//
// where text is the first text block of the conversation's first user prompt,
// indexed in UTF-16 code units, and a missing character is "0". It is the same
// for every request of a conversation and differs between conversations, which
// is why it looked per-process in captures that ran one prompt per process.
func versionSuffix(prompt, version string) string {
	units := utf16.Encode([]rune(prompt))
	var b []byte
	b = append(b, versionSuffixSalt...)
	for _, index := range [...]int{4, 7, 20} {
		if index >= len(units) {
			b = append(b, '0')
			continue
		}
		// A lone surrogate half becomes U+FFFD, as the UTF-8 encoding of the
		// hashed string does in the real client.
		b = append(b, string(rune(units[index]))...)
	}
	b = append(b, version...)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:2])[:3]
}

// signCCH replaces the cch placeholder at offset in body with the value the
// real client computes for these exact bytes. body is modified in place; the
// placeholder and the value have the same length.
//
// The hashed bytes are the body with the placeholder still in place, the value
// of the first "model" key emptied and the first "max_tokens" field removed
// (the 2.1.172+ preimage; see cchPreimage). ApplyBody writes model as the first
// key, as Claude Code does, so "the first" is the top-level field.
func signCCH(body []byte, offset int) {
	digest := xxhash.NewWithSeed(cchSeed)
	_, _ = digest.Write(cchPreimage(body))
	value := digest.Sum64() & 0xfffff
	const hexDigits = "0123456789abcdef"
	start := offset + len("cch=")
	for i := 4; i >= 0; i-- {
		body[start+i] = hexDigits[value&0xf]
		value >>= 4
	}
}

// cchPreimage is the byte sequence Claude Code feeds to xxHash64: body with
// the value of the first `"model":"<value>"` emptied and the first
// `"max_tokens":<digits>` field that has a comma beside it removed together
// with that comma, the leading one when both exist. These are the client's own
// patterns, /("model":")[^"]*(")/ and /"max_tokens":\d+,|,"max_tokens":\d+/,
// each replaced once. Each edit is a no-op when its pattern does not match.
//
// The two edits cannot interact: a model value holds no quote, and the
// max_tokens pattern starts with a comma or a quote, so both spans can be found
// in the original bytes and cut in one pass.
func cchPreimage(body []byte) []byte {
	type span struct{ start, end int }
	var cuts []span

	const modelKey = `"model":"`
	if at := bytes.Index(body, []byte(modelKey)); at >= 0 {
		start := at + len(modelKey)
		if end := bytes.IndexByte(body[start:], '"'); end > 0 {
			cuts = append(cuts, span{start, start + end})
		}
	}

	const maxTokensKey = `"max_tokens":`
	for from := 0; ; {
		at := bytes.Index(body[from:], []byte(maxTokensKey))
		if at < 0 {
			break
		}
		at += from
		end := at + len(maxTokensKey)
		for end < len(body) && body[end] >= '0' && body[end] <= '9' {
			end++
		}
		if end > at+len(maxTokensKey) {
			if at > 0 && body[at-1] == ',' {
				cuts = append(cuts, span{at - 1, end})
				break
			}
			if end < len(body) && body[end] == ',' {
				cuts = append(cuts, span{at, end + 1})
				break
			}
		}
		from = at + 1
	}

	if len(cuts) == 2 && cuts[1].start < cuts[0].start {
		cuts[0], cuts[1] = cuts[1], cuts[0]
	}
	out := make([]byte, 0, len(body))
	last := 0
	for _, cut := range cuts {
		out = append(out, body[last:cut.start]...)
		last = cut.end
	}
	return append(out, body[last:]...)
}
