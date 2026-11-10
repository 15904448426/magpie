package gateway

import (
	"bytes"
	"encoding/base64"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strings"
	"unicode"

	"github.com/tidwall/gjson"

	"github.com/yetone/magpie/internal/provider"
)

// A gateway image hook can fail to start or time out. Keeping the request as
// it came then sends every inline image to the model, including an old
// screenshot that may be larger than the provider's image path accepts. Keep
// at most one small image from the latest user message; replace every other
// inline image with text so the turn can still run.
const (
	hookImageFallbackText = "[An image was left out because the image history hook did not run. If the image is still needed, use an image path mentioned nearby or ask the user to send it again.]"

	hookImageMaxPixels = 1_000_000
	hookImageMaxTokens = 80_000
	hookImageMaxBytes  = 1 << 20
	hookImageByteToken = 13
	hookImagePixToken  = 24
)

type hookImage struct {
	at        int
	end       int
	with      string
	itemIndex int
	encoded   string
	mime      string
}

// withoutInlineImagesOnHookFailure is body with inline images left out when
// no gateway image hook could prepare it. The newest small image in the latest
// user message is kept; all other inline images become text.
func withoutInlineImagesOnHookFailure(to provider.Protocol, body []byte, compact bool) ([]byte, int) {
	var items []gjson.Result
	getIn(body, historyOf(to)).ForEach(func(_, item gjson.Result) bool {
		items = append(items, item)
		return true
	})
	if len(items) == 0 {
		return body, 0
	}
	latest := latestUser(to, items)
	var found []hookImage
	for itemIndex, item := range items {
		var walk func(gjson.Result, int)
		walk = func(value gjson.Result, depth int) {
			if depth > 6 || !value.IsObject() && !value.IsArray() {
				return
			}
			if value.IsObject() {
				if encoded, mime, ok := inlineImage(value, to); ok {
					end := value.Index + len(value.Raw)
					if value.Index > 0 && end <= len(body) && string(body[value.Index:end]) == value.Raw {
						found = append(found, hookImage{
							at: value.Index, end: end, itemIndex: itemIndex,
							encoded: encoded, mime: mime,
							with: imageWithText(to, value, hookImageFallbackText),
						})
					}
					return
				}
			}
			value.ForEach(func(_, child gjson.Result) bool {
				walk(child, depth+1)
				return true
			})
		}
		walk(item, 0)
	}
	if len(found) == 0 {
		return body, 0
	}

	keep := -1
	if !compact {
		for i := len(found) - 1; i >= 0; i-- {
			if found[i].itemIndex == latest && safeHookImage(found[i]) {
				keep = i
				break
			}
		}
	}
	var out bytes.Buffer
	out.Grow(len(body))
	last, left := 0, 0
	for i, img := range found {
		if i == keep {
			continue
		}
		out.Write(body[last:img.at])
		out.WriteString(img.with)
		last = img.end
		left++
	}
	if left == 0 {
		return body, 0
	}
	out.Write(body[last:])
	return out.Bytes(), left
}

// inlineImage is the base64 payload of an inline image part, never a remote
// URL or a blob reference.
func inlineImage(part gjson.Result, to provider.Protocol) (encoded, mime string, ok bool) {
	switch to {
	case provider.Responses:
		if part.Get("type").String() != "input_image" {
			return "", "", false
		}
		if encoded, mime, ok = inlineString(part.Get("image_url"), ""); ok {
			return encoded, mime, true
		}
		if image := part.Get("image"); image.IsObject() {
			return inlineString(image.Get("data"), image.Get("media_type").String()+image.Get("mediaType").String())
		}
	case provider.Chat:
		switch part.Get("type").String() {
		case "image_url", "input_image":
		default:
			if !part.Get("image_url").Exists() {
				return "", "", false
			}
		}
		url := part.Get("image_url")
		if encoded, mime, ok = inlineString(url, part.Get("media_type").String()); ok {
			return encoded, mime, true
		}
		if url.IsObject() {
			if encoded, mime, ok = inlineString(url.Get("url"), part.Get("media_type").String()); ok {
				return encoded, mime, true
			}
		}
		if encoded, mime, ok = inlineString(part.Get("image"), part.Get("media_type").String()); ok {
			return encoded, mime, true
		}
	case provider.Anthropic:
		if part.Get("type").String() != "image" || part.Get("source.type").String() != "base64" {
			return "", "", false
		}
		data := part.Get("source.data").String()
		if data == "" {
			return "", "", false
		}
		return data, part.Get("source.media_type").String(), true
	case provider.Gemini:
		for _, key := range []string{"inlineData", "inline_data"} {
			data := part.Get(key)
			mime := data.Get("mimeType").String() + data.Get("mime_type").String()
			if data.IsObject() && strings.HasPrefix(strings.ToLower(mime), "image/") {
				if encoded := data.Get("data").String(); encoded != "" {
					return encoded, mime, true
				}
			}
		}
	}
	return "", "", false
}

func inlineString(value gjson.Result, mime string) (encoded, foundMime string, ok bool) {
	if !value.Exists() {
		return "", "", false
	}
	text := value.String()
	if strings.HasPrefix(strings.ToLower(text), "data:image/") {
		comma := strings.IndexByte(text, ',')
		semi := strings.IndexByte(text, ';')
		if comma < 0 || semi < 0 || semi > comma {
			return "", "", false
		}
		return text[comma+1:], strings.TrimPrefix(strings.ToLower(text[:semi]), "data:"), true
	}
	if text == "" || !strings.HasPrefix(strings.ToLower(mime), "image/") {
		return "", "", false
	}
	return text, mime, true
}

func safeHookImage(img hookImage) bool {
	if len(img.encoded) > hookImageMaxBytes {
		return false
	}
	clean := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, img.encoded)
	raw, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return false
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return false
	}
	pixels := cfg.Width * cfg.Height
	if pixels > hookImageMaxPixels {
		return false
	}
	tokens := max(
		(len(clean)+hookImageByteToken-1)/hookImageByteToken,
		(pixels+hookImagePixToken-1)/hookImagePixToken,
	)
	return tokens <= hookImageMaxTokens
}
