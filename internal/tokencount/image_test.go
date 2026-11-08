package tokencount

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
)

// The rows of the table at
// https://platform.claude.com/docs/en/build-with-claude/vision#evaluate-image-size,
// the A4 scan of
// https://platform.claude.com/docs/en/build-with-claude/vision-coordinates#how-claude-resizes-and-pads-images
// (resized to 924×1307 on the standard tier, 39 × 55 = 2,145 on high
// resolution), a portrait screenshot that must resize like its landscape
// twin, and a 2690×1793 image whose short edge lands on a tie: 1345 / (2690 /
// 1793) = 896.5 rounds to the even 896 as the live API does, giving 1345×896 =
// 49 × 32 = 1,568; rounding the tie up would settle on 1344×896 = 1,536.
func TestClaudeImageTokensFollowAnthropicsTable(t *testing.T) {
	standard := pricingFor("claude-opus-4-6-thinking")
	highRes := pricingFor("claude-opus-4-7")
	for _, tc := range []struct {
		width, height     int
		standard, highRes int
	}{
		{200, 200, 64, 64},
		{1000, 1000, 1296, 1296},
		{1092, 1092, 1521, 1521},
		{1920, 1080, 1560, 2691},
		{2000, 1500, 1564, 3888},
		{3840, 2160, 1560, 4784},
		{1075, 1520, 1551, 2145},
		{1080, 1920, 1560, 2691},
		{2690, 1793, 1568, 4760},
	} {
		if got := standard.tokens(tc.width, tc.height); got != tc.standard {
			t.Errorf("standard tier %dx%d = %d tokens, want %d", tc.width, tc.height, got, tc.standard)
		}
		if got := highRes.tokens(tc.width, tc.height); got != tc.highRes {
			t.Errorf("high-resolution tier %dx%d = %d tokens, want %d", tc.width, tc.height, got, tc.highRes)
		}
	}
}

func TestClaudeResolutionTierComesFromTheModelVersion(t *testing.T) {
	for model, want := range map[string]bool{
		"claude-opus-4-7":            true,
		"claude-sonnet-5-5":          true,
		"claude-sonnet-5":            true,
		"claude-sonnet-5.5-high":     true,
		"claude-fable-5-1":           true,
		"claude-opus-4-6-thinking":   false,
		"claude-haiku-4-5-20251001":  false,
		"claude-opus-4-20250514":     false,
		"claude-3-7-sonnet-20250219": false,
		"claude-custom-model":        false,
	} {
		if got := claudeHighResolution(model); got != want {
			t.Errorf("claudeHighResolution(%q) = %v, want %v", model, got, want)
		}
	}
	// Only Claude has a high-resolution tier; a version number in another
	// family's id does not reach it.
	if got := pricingFor("gpt-oss-120b-medium").tokens(1920, 1080); got != 1560 {
		t.Errorf("gpt-oss 1920x1080 = %d tokens, want the standard tier's 1560", got)
	}
}

// https://ai.google.dev/gemini-api/docs/tokens: images with both sides at
// most 384 px count 258 tokens; larger ones 258 per 768×768 tile.
func TestGeminiImageTokensCountTiles(t *testing.T) {
	pricing := pricingFor("gemini-3.1-pro-high")
	for _, tc := range []struct{ width, height, want int }{
		{384, 384, 258},
		{385, 100, 258},
		{768, 768, 258},
		{769, 768, 516},
		{1920, 1080, 1548},
		{100, 2000, 774},
	} {
		if got := pricing.tokens(tc.width, tc.height); got != tc.want {
			t.Errorf("gemini %dx%d = %d tokens, want %d", tc.width, tc.height, got, tc.want)
		}
	}
}

func encodedImage(t *testing.T, format string, width, height int) string {
	t.Helper()
	picture := image.NewGray(image.Rect(0, 0, width, height))
	var encoded bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&encoded, picture)
	case "jpeg":
		err = jpeg.Encode(&encoded, picture, nil)
	case "gif":
		err = gif.Encode(&encoded, picture, nil)
	}
	if err != nil {
		t.Fatalf("encode %s: %v", format, err)
	}
	return base64.StdEncoding.EncodeToString(encoded.Bytes())
}

func base64ImageBlock(mediaType, data string) map[string]any {
	return map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "base64", "media_type": mediaType, "data": data},
	}
}

func TestImageBlocksArePricedFromTheirHeader(t *testing.T) {
	readable := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			base64ImageBlock("image/png", encodedImage(t, "png", 200, 200)),
			map[string]any{"type": "text", "text": "look"},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": []any{
				base64ImageBlock("image/jpeg", encodedImage(t, "jpeg", 56, 28)),
				base64ImageBlock("image/gif", encodedImage(t, "gif", 29, 28)),
			}},
		}},
	}}
	unreadable := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			base64ImageBlock("image/webp", "UklGRiQAAABXRUJQVlA4IBgAAAAwAQCdASoBAAEAAwA0JaQAA3AA/vuUAAA="),
			map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/a.png"}},
		}},
	}}

	for _, tc := range []struct {
		model            string
		readable, unread int
	}{
		// PNG 200×200 = 8×8 patches, JPEG 56×28 = 2×1, GIF 29×28 = 2×1.
		{"claude-sonnet-4-6", 64 + 2 + 2, 2 * 1568},
		{"claude-opus-4-7", 64 + 2 + 2, 2 * 4784},
		{"gpt-oss-120b-medium", 64 + 2 + 2, 2 * 1568},
		{"gemini-3.1-pro", 3 * 258, 2 * 1600},
	} {
		pricing := pricingFor(tc.model)
		if got := measureRequest(pricing, readable); got != (measurement{chars: 4, imageTokens: tc.readable}) {
			t.Errorf("%s readable images = %+v, want 4 chars and %d image tokens", tc.model, got, tc.readable)
		}
		if got := measureRequest(pricing, unreadable); got != (measurement{imageTokens: tc.unread}) {
			t.Errorf("%s unreadable images = %+v, want %d image tokens", tc.model, got, tc.unread)
		}
	}
}
