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

// Image costs:
//
//   - Claude, https://platform.claude.com/docs/en/build-with-claude/vision#evaluate-image-size
//     and the resize rule at
//     https://platform.claude.com/docs/en/build-with-claude/vision-coordinates#resize-your-image-before-uploading:
//     one token per 28×28 patch, ⌈w/28⌉ × ⌈h/28⌉, after the image is scaled down
//     to fit the model's tier (standard: 1,568 px long edge and 1,568 tokens;
//     Claude 4.7 and later: 2,576 px and 4,784 tokens).
//   - Gemini, measured on Cloud Code (.reference/gemini-image-tokens-20261108.txt):
//     whatever its pixel size, the image fills a grid of at most 1,120 patches
//     with its aspect ratio kept, ⌊√(1120·w/h)⌋ × ⌊√(1120·h/w)⌋ tokens. The
//     Gemini API's 258-per-768×768-tile rule does not hold there.
//
// Families with no published rule here (gpt-oss, Zen models) use Claude's
// standard tier.
const (
	claudePatch          = 28
	claudeStandardEdge   = 1568
	claudeStandardTokens = 1568
	claudeHighResEdge    = 2576
	claudeHighResTokens  = 4784

	geminiGridTokens = 1120
)

// imagePricing prices images for one model.
type imagePricing struct {
	gemini    bool
	maxEdge   int
	maxTokens int
}

func pricingFor(model string) imagePricing {
	switch Family(model) {
	case "gemini":
		return imagePricing{gemini: true, maxTokens: geminiGridTokens}
	case "claude-4.7":
		return imagePricing{maxEdge: claudeHighResEdge, maxTokens: claudeHighResTokens}
	}
	return imagePricing{maxEdge: claudeStandardEdge, maxTokens: claudeStandardTokens}
}

// claude47OrLater reports whether a Claude model id names Claude 4.7 or later,
// the models with the high-resolution image tier and the newer tokenizer. The
// version is the first number of one or two digits in the id and the one after
// it, so claude-opus-4-7, claude-sonnet-5.5-high and claude-3-7-sonnet-20250219
// read 4.7, 5.5 and 3.7; a date is not a minor version, so
// claude-opus-4-20250514 reads 4.0.
func claude47OrLater(model string) bool {
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
		return pricing.maxTokens
	}
	return pricing.tokens(width, height)
}

// imageSize reads the dimensions from the header of base64 image data. The
// decoder streams, so only the header is decoded, not the whole image. A GIF
// header may declare a 0×0 screen; a size with no area is not a size.
func imageSize(source any) (width, height int, ok bool) {
	object, _ := source.(map[string]any)
	if object["type"] != "base64" {
		return 0, 0, false
	}
	data, _ := object["data"].(string)
	config, _, err := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, strings.NewReader(data)))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return 0, 0, false
	}
	return config.Width, config.Height, true
}

func (pricing imagePricing) tokens(width, height int) int {
	if pricing.gemini {
		// One division each, so a side that is a whole number stays whole.
		columns := max(1, int(math.Sqrt(float64(geminiGridTokens*width)/float64(height))))
		rows := max(1, int(math.Sqrt(float64(geminiGridTokens*height)/float64(width))))
		return min(columns*rows, geminiGridTokens)
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
