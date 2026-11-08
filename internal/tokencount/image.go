package tokencount

import (
	"encoding/base64"
	"image"
	// Registered for image.DecodeConfig.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"strings"
)

// Image costs, from the providers' published rules:
//
//   - Claude, https://platform.claude.com/docs/en/build-with-claude/vision#evaluate-image-size
//     and the resize rule at
//     https://platform.claude.com/docs/en/build-with-claude/vision-coordinates#resize-your-image-before-uploading:
//     one token per 28×28 patch, ⌈w/28⌉ × ⌈h/28⌉, after the image is scaled down
//     to fit the model's tier (standard: 1,568 px long edge and 1,568 tokens;
//     Claude 4.7 and later: 2,576 px and 4,784 tokens).
//   - Gemini, https://ai.google.dev/gemini-api/docs/tokens: 258 tokens when
//     both sides are at most 384 px, otherwise 258 per 768×768 tile.
//
// Families with no published rule here (gpt-oss, Zen models) use Claude's
// standard tier.
const (
	claudePatch          = 28
	claudeStandardEdge   = 1568
	claudeStandardTokens = 1568
	claudeHighResEdge    = 2576
	claudeHighResTokens  = 4784

	geminiSmallEdge  = 384
	geminiTile       = 768
	geminiTileTokens = 258
	// geminiUnknownTokens prices a Gemini image whose size cannot be read:
	// Gemini has no cap to fall back on, so it counts a flat 1,600.
	geminiUnknownTokens = 1600
)

// imagePricing prices images for one model.
type imagePricing struct {
	gemini    bool
	maxEdge   int
	maxTokens int
}

func pricingFor(model string) imagePricing {
	family := Family(model)
	switch {
	case family == "gemini":
		return imagePricing{gemini: true}
	case family == "claude" && claudeHighResolution(model):
		return imagePricing{maxEdge: claudeHighResEdge, maxTokens: claudeHighResTokens}
	}
	return imagePricing{maxEdge: claudeStandardEdge, maxTokens: claudeStandardTokens}
}

// claudeHighResolution reports whether a Claude model id names Claude 4.7 or
// later. The version is the first number of one or two digits in the id and
// the one after it, so claude-opus-4-7, claude-sonnet-5.5-high and
// claude-3-7-sonnet-20250219 read 4.7, 5.5 and 3.7; a date is not a minor
// version, so claude-opus-4-20250514 reads 4.0.
func claudeHighResolution(model string) bool {
	fields := strings.FieldsFunc(model, func(r rune) bool {
		return r == '-' || r == '.' || r == '@' || r == '_'
	})
	for i, field := range fields {
		major, ok := shortNumber(field)
		if !ok {
			continue
		}
		minor := 0
		if i+1 < len(fields) {
			if value, ok := shortNumber(fields[i+1]); ok {
				minor = value
			}
		}
		return major > 4 || major == 4 && minor >= 7
	}
	return false
}

// shortNumber parses a field of one or two ASCII digits.
func shortNumber(field string) (int, bool) {
	if len(field) == 0 || len(field) > 2 {
		return 0, false
	}
	value := 0
	for i := range len(field) {
		if field[i] < '0' || field[i] > '9' {
			return 0, false
		}
		value = value*10 + int(field[i]-'0')
	}
	return value, true
}

// sourceTokens prices an image block's source. Only base64 PNG, JPEG and GIF
// data has a size that can be read; anything else (a URL, a file id, WebP,
// data that does not decode) costs the most an image of the tier can cost.
func (pricing imagePricing) sourceTokens(source any) int {
	width, height, ok := imageSize(source)
	if !ok {
		if pricing.gemini {
			return geminiUnknownTokens
		}
		return pricing.maxTokens
	}
	return pricing.tokens(width, height)
}

// imageSize reads the dimensions from the header of base64 image data. The
// decoder streams, so only the header is decoded, not the whole image.
func imageSize(source any) (width, height int, ok bool) {
	object, _ := source.(map[string]any)
	if object["type"] != "base64" {
		return 0, 0, false
	}
	data, _ := object["data"].(string)
	config, _, err := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, strings.NewReader(data)))
	if err != nil {
		return 0, 0, false
	}
	return config.Width, config.Height, true
}

func (pricing imagePricing) tokens(width, height int) int {
	if pricing.gemini {
		if width <= geminiSmallEdge && height <= geminiSmallEdge {
			return geminiTileTokens
		}
		return ceilDiv(width, geminiTile) * ceilDiv(height, geminiTile) * geminiTileTokens
	}
	width, height = pricing.resized(width, height)
	return claudePatches(width, height)
}

func claudePatches(width, height int) int {
	return ceilDiv(width, claudePatch) * ceilDiv(height, claudePatch)
}

func (pricing imagePricing) fits(width, height int) bool {
	return ceilDiv(width, claudePatch)*claudePatch <= pricing.maxEdge &&
		ceilDiv(height, claudePatch)*claudePatch <= pricing.maxEdge &&
		claudePatches(width, height) <= pricing.maxTokens
}

// resized is Anthropic's reference resize: the largest aspect-preserving size
// that fits the tier, found by binary search on the long edge, with the short
// edge rounded half to even as the live API does.
func (pricing imagePricing) resized(width, height int) (int, int) {
	if pricing.fits(width, height) {
		return width, height
	}
	if height > width {
		resizedHeight, resizedWidth := pricing.resized(height, width)
		return resizedWidth, resizedHeight
	}
	aspect := float64(width) / float64(height)
	shortEdge := func(longEdge int) int {
		return max(int(math.RoundToEven(float64(longEdge)/aspect)), 1)
	}
	low, high := 1, width // low always fits, high never does
	for low+1 < high {
		middle := (low + high) / 2
		if pricing.fits(middle, shortEdge(middle)) {
			low = middle
		} else {
			high = middle
		}
	}
	return low, shortEdge(low)
}

func ceilDiv(value, divisor int) int {
	return (value + divisor - 1) / divisor
}
