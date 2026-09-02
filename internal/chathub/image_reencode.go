package chathub

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/draw"
	"image/jpeg"
	"strings"

	_ "image/gif"
	_ "image/png"
)

// reencodeMaxEdge bounds the retry copy. The upstream sanitizer is more willing
// to accept modest images, and Copilot downscales large inputs anyway.
const reencodeMaxEdge = 2048

// reencodeImageDataURL rebuilds an image data URL as a baseline JPEG, dropping
// EXIF, ICC profiles, embedded thumbnails and progressive scans. It reports
// false when the payload cannot be decoded or the result would not differ, so
// callers can skip a pointless retry.
func reencodeImageDataURL(dataURL string) (string, bool) {
	comma := strings.IndexByte(dataURL, ',')
	if comma < 0 {
		return "", false
	}
	header := strings.ToLower(dataURL[:comma])
	if !strings.Contains(header, ";base64") {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(dataURL[comma+1:])
	if err != nil {
		return "", false
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", false
	}
	img = flattenAndFit(img)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		return "", false
	}
	if buf.Len() == 0 {
		return "", false
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), true
}

// flattenAndFit composites onto opaque white, because JPEG has no alpha, and
// scales the longest edge down to reencodeMaxEdge.
func flattenAndFit(src image.Image) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return src
	}
	dstW, dstH := w, h
	if longest := maxInt(w, h); longest > reencodeMaxEdge {
		ratio := float64(reencodeMaxEdge) / float64(longest)
		dstW = maxInt(1, int(float64(w)*ratio))
		dstH = maxInt(1, int(float64(h)*ratio))
	}
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	if dstW == w && dstH == h {
		draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
		return dst
	}
	scaleNearest(dst, src)
	return dst
}

// scaleNearest keeps the retry path dependency-free; the sanitizer cares about
// the container, not about resampling quality.
func scaleNearest(dst *image.RGBA, src image.Image) {
	sb := src.Bounds()
	db := dst.Bounds()
	for y := db.Min.Y; y < db.Max.Y; y++ {
		sy := sb.Min.Y + y*sb.Dy()/db.Dy()
		for x := db.Min.X; x < db.Max.X; x++ {
			sx := sb.Min.X + x*sb.Dx()/db.Dx()
			dst.Set(x, y, src.At(sx, sy))
		}
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
