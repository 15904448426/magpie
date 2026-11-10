package gateway

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestWithoutInlineImagesOnHookFailure(t *testing.T) {
	small := testPNG(t, 32, 32)
	large := testPNG(t, 1100, 1000)
	remote := "https://example.com/latest.png"
	for _, tc := range []struct {
		name string
		to   provider.Protocol
		body string
	}{
		{
			"responses", provider.Responses,
			fmt.Sprintf(`{"model":"group/auto","input":[`+
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"old"},{"type":"input_image","image_url":%q}]},`+
				`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"seen"}]},`+
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"new"},{"type":"input_image","image_url":%q},{"type":"input_image","image_url":%q},{"type":"input_image","image_url":%q}]}]}`,
				"data:image/png;base64,"+base64.StdEncoding.EncodeToString(small),
				"data:image/png;base64,"+base64.StdEncoding.EncodeToString(large),
				"data:image/png;base64,"+base64.StdEncoding.EncodeToString(small),
				remote,
			),
		},
		{
			"chat", provider.Chat,
			fmt.Sprintf(`{"model":"group/auto","messages":[`+
				`{"role":"user","content":[{"type":"text","text":"old"},{"type":"image_url","image_url":{"url":%q}}]},`+
				`{"role":"assistant","content":"seen"},`+
				`{"role":"user","content":[{"type":"text","text":"new"},{"type":"image_url","image_url":{"url":%q}},{"type":"image_url","image_url":{"url":%q}},{"type":"image_url","image_url":{"url":%q}}]}]}`,
				"data:image/png;base64,"+base64.StdEncoding.EncodeToString(small),
				"data:image/png;base64,"+base64.StdEncoding.EncodeToString(large),
				"data:image/png;base64,"+base64.StdEncoding.EncodeToString(small),
				remote,
			),
		},
		{
			"anthropic", provider.Anthropic,
			fmt.Sprintf(`{"model":"group/auto","messages":[`+
				`{"role":"user","content":[{"type":"text","text":"old"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}]},`+
				`{"role":"assistant","content":"seen"},`+
				`{"role":"user","content":[{"type":"text","text":"new"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}},{"type":"image","source":{"type":"base64","media_type":"image/png","data":%q}}]}]}`,
				base64.StdEncoding.EncodeToString(small),
				base64.StdEncoding.EncodeToString(large),
				base64.StdEncoding.EncodeToString(small),
			),
		},
		{
			"gemini", provider.Gemini,
			fmt.Sprintf(`{"contents":[`+
				`{"role":"user","parts":[{"text":"old"},{"inlineData":{"mimeType":"image/png","data":%q}}]},`+
				`{"role":"model","parts":[{"text":"seen"}]},`+
				`{"role":"user","parts":[{"text":"new"},{"inlineData":{"mimeType":"image/png","data":%q}},{"inlineData":{"mimeType":"image/png","data":%q}}]}]}`,
				base64.StdEncoding.EncodeToString(small),
				base64.StdEncoding.EncodeToString(large),
				base64.StdEncoding.EncodeToString(small),
			),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, left := withoutInlineImagesOnHookFailure(tc.to, []byte(tc.body), false)
			if left != 2 {
				t.Fatalf("left out %d images, want old and large", left)
			}
			text := string(out)
			if got := strings.Count(text, base64.StdEncoding.EncodeToString(small)); got != 1 {
				t.Fatalf("kept small image %d times, want once: %.400s", got, text)
			}
			if strings.Contains(text, base64.StdEncoding.EncodeToString(large)[:64]) {
				t.Fatal("large image stayed inline")
			}
			if strings.Count(text, hookImageFallbackText) != 2 {
				t.Fatalf("fallback notes = %d, want 2", strings.Count(text, hookImageFallbackText))
			}
			if strings.Contains(tc.body, remote) && !strings.Contains(text, remote) {
				t.Fatal("remote image URL was changed")
			}
		})
	}
}

func TestHookImageFallbackLeavesRequestWithNoInlineImage(t *testing.T) {
	body := []byte(`{"model":"group/auto","input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	out, left := withoutInlineImagesOnHookFailure(provider.Responses, body, false)
	if left != 0 || !bytes.Equal(out, body) {
		t.Fatalf("text-only request changed: %s", out)
	}
}

func TestHookImageFallbackLeavesNoImageForCompaction(t *testing.T) {
	small := testPNG(t, 32, 32)
	body := []byte(fmt.Sprintf(`{"model":"group/auto","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"new"},{"type":"input_image","image_url":%q}]},{"type":"compaction_trigger"}]}`, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(small)))
	out, left := withoutInlineImagesOnHookFailure(provider.Responses, body, true)
	if left != 1 || strings.Contains(string(out), "data:image/") {
		t.Fatalf("compaction kept an image: left=%d %s", left, out)
	}
}

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	img.Set(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
