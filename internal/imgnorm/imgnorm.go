// Package imgnorm turns an uploaded logo or signature into a small,
// re-encoded PNG. Re-encoding means we never serve the uploaded bytes
// themselves (no polyglots, metadata or oversized files), and the size
// limits keep certificate pages and preview images fast.
package imgnorm

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"

	_ "image/jpeg" // decoder

	"golang.org/x/image/draw"
)

// MaxUpload is the largest file accepted, in bytes.
const MaxUpload = 2 << 20

// maxSourcePixels guards against decompression bombs: a tiny file that
// claims enormous dimensions.
const maxSourcePixels = 40_000_000

// Limits are the largest output dimensions for each kind of image.
var Limits = map[string]image.Point{
	"logo":      {X: 600, Y: 240},
	"signature": {X: 600, Y: 200},
}

// ErrNotImage means the upload wasn't a PNG or JPEG we could read.
var ErrNotImage = errors.New("upload a PNG or JPEG image")

// Normalize decodes a PNG or JPEG, scales it down to fit the kind's
// limits, and returns it as PNG with its dimensions. For signatures, a
// white (scanned paper) background is made transparent so the signature
// sits on the certificate in light and dark mode.
func Normalize(data []byte, kind string) ([]byte, int, int, error) {
	limit, ok := Limits[kind]
	if !ok {
		return nil, 0, 0, fmt.Errorf("unknown image kind %q", kind)
	}
	if len(data) > MaxUpload {
		return nil, 0, 0, fmt.Errorf("image is larger than %d MB", MaxUpload>>20)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "png" && format != "jpeg") {
		return nil, 0, 0, ErrNotImage
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxSourcePixels {
		return nil, 0, 0, fmt.Errorf("image dimensions %dx%d are too large", cfg.Width, cfg.Height)
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, ErrNotImage
	}
	b := src.Bounds()
	w, h := fit(b.Dx(), b.Dy(), limit)
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	if kind == "signature" {
		whiteToTransparent(dst)
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, dst); err != nil {
		return nil, 0, 0, err
	}
	return buf.Bytes(), w, h, nil
}

// fit scales (w, h) down, keeping the aspect ratio, to fit inside max.
func fit(w, h int, max image.Point) (int, int) {
	if w <= max.X && h <= max.Y {
		return w, h
	}
	sw, sh := float64(max.X)/float64(w), float64(max.Y)/float64(h)
	s := min(sw, sh)
	return max1(int(float64(w)*s + 0.5)), max1(int(float64(h)*s + 0.5))
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// whiteToTransparent fades near-white pixels out, so paper doesn't show.
// Only images without existing transparency are changed.
func whiteToTransparent(img *image.NRGBA) {
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] != 0xff {
			return // already has transparency
		}
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.NRGBAAt(x, y)
			lum := (299*int(c.R) + 587*int(c.G) + 114*int(c.B)) / 1000
			switch {
			case lum >= 235:
				c.A = 0
			case lum > 180: // soften the edge of the ink
				c.A = uint8(255 * (235 - lum) / 55)
			}
			img.SetNRGBA(x, y, color.NRGBA{c.R, c.G, c.B, c.A})
		}
	}
}
