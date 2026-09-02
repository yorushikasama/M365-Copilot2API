package chathub

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"testing"
)

func pngDataURL(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 0x40, A: 0x80})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// TestReencodeProducesDecodableBaselineJPEG covers the sanitizer retry: the copy
// must be a real JPEG, because the retry is the only chance a rejected image has.
func TestReencodeProducesDecodableBaselineJPEG(t *testing.T) {
	out, ok := reencodeImageDataURL(pngDataURL(t, 64, 48))
	if !ok {
		t.Fatal("re-encode rejected a valid PNG")
	}
	if !strings.HasPrefix(out, "data:image/jpeg;base64,") {
		t.Fatalf("unexpected data URL prefix: %.32s", out)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.SplitN(out, ",", 2)[1])
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if format != "jpeg" {
		t.Fatalf("format = %q, want jpeg", format)
	}
	if cfg.Width != 64 || cfg.Height != 48 {
		t.Fatalf("size = %dx%d, want 64x48", cfg.Width, cfg.Height)
	}
}

func TestReencodeDownscalesOversizedImages(t *testing.T) {
	out, ok := reencodeImageDataURL(pngDataURL(t, reencodeMaxEdge+600, 100))
	if !ok {
		t.Fatal("re-encode rejected an oversized PNG")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.SplitN(out, ",", 2)[1])
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != reencodeMaxEdge {
		t.Fatalf("width = %d, want %d", cfg.Width, reencodeMaxEdge)
	}
	if cfg.Height <= 0 || cfg.Height >= 100 {
		t.Fatalf("height = %d, want a proportional downscale below 100", cfg.Height)
	}
}

func TestReencodeRefusesUndecodablePayloads(t *testing.T) {
	for name, in := range map[string]string{
		"no comma":   "data:image/png;base64" + strings.Repeat("A", 8),
		"not base64": "data:image/png,not-base64-at-all",
		"garbage":    "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("this is not an image")),
	} {
		if _, ok := reencodeImageDataURL(in); ok {
			t.Fatalf("%s: re-encode accepted an undecodable payload", name)
		}
	}
}

// TestClassifyUploadFailureSeparatesRefusalFromOutage pins the distinction that
// decides whether the gateway retries: a sanitizer refusal is about the bytes,
// an upstream 5xx or 429 is not.
func TestClassifyUploadFailureSeparatesRefusalFromOutage(t *testing.T) {
	sanitizer := `{"fileSanitizer":"None","result":{"value":"ImageSanitizationError","message":"Sorry, I wasn't able to respond to that."}}`
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"sanitizer 417", http.StatusExpectationFailed, sanitizer, ErrAttachmentRejected},
		{"unsupported media", http.StatusUnsupportedMediaType, `{"result":{"value":"UnsupportedFileType"}}`, ErrAttachmentRejected},
		{"bad request", http.StatusBadRequest, `{"result":{"value":"InvalidRequest"}}`, ErrAttachmentRejected},
		{"server error", http.StatusInternalServerError, `{"result":{"value":"InternalError"}}`, ErrAttachmentUploadFailed},
		{"rate limited", http.StatusTooManyRequests, `{"result":{"value":"Throttled"}}`, ErrAttachmentUploadFailed},
		{"gateway timeout", http.StatusRequestTimeout, "", ErrAttachmentUploadFailed},
	}
	for _, tc := range cases {
		got := classifyUploadFailure(tc.status, tc.body)
		if !errors.Is(got, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	if msg := classifyUploadFailure(http.StatusExpectationFailed, sanitizer).Error(); !strings.Contains(msg, "ImageSanitizationError") {
		t.Fatalf("refusal message drops the upstream reason: %s", msg)
	}
}
