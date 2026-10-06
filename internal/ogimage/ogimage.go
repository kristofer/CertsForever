// Package ogimage renders the 1200x627 social preview image that LinkedIn
// (and Slack, X, Facebook...) show when a certificate link is shared.
//
// It uses only the Go fonts bundled with golang.org/x/image, so there are
// no system font or headless-browser dependencies. Swap the palette and
// fonts for Zip Code Wilmington's brand when ready.
package ogimage

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"sync"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// Width and Height are LinkedIn's recommended share-image dimensions.
const (
	Width  = 1200
	Height = 627
)

// Palette — placeholder brand colors.
var (
	colBG     = color.RGBA{0x10, 0x1c, 0x2e, 0xff} // deep navy
	colAccent = color.RGBA{0x2f, 0xb5, 0xa4, 0xff} // teal
	colText   = color.RGBA{0xf5, 0xf3, 0xee, 0xff} // warm white
	colMuted  = color.RGBA{0xa7, 0xb3, 0xc4, 0xff}
	colRule   = color.RGBA{0x2a, 0x3a, 0x52, 0xff}
	colRevoke = color.RGBA{0xe0, 0x4f, 0x4f, 0xff}
)

// Card is the text that goes on the image.
type Card struct {
	Org     string // "Zip Code Wilmington"
	Heading string // "Certificate of Completion"
	Name    string // recipient
	Course  string // course title
	Footer  string // "Issued October 2026 · Verify at certs.example.org/c/ZCW-…"
	Revoked bool
	// Accent replaces the default teal (accent bar, org name, rule).
	Accent color.Color
	// Logo, if set, is drawn top right on a light badge.
	Logo image.Image
}

// ParseHex parses "#rrggbb"; ok is false for anything else.
func ParseHex(s string) (c color.RGBA, ok bool) {
	if len(s) != 7 || s[0] != '#' {
		return c, false
	}
	var v [3]uint8
	for i := range v {
		hi, ok1 := hexVal(s[1+2*i])
		lo, ok2 := hexVal(s[2+2*i])
		if !ok1 || !ok2 {
			return c, false
		}
		v[i] = hi<<4 | lo
	}
	return color.RGBA{v[0], v[1], v[2], 0xff}, true
}

func hexVal(b byte) (uint8, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	}
	return 0, false
}

var (
	fontsOnce    sync.Once
	boldF, regF  *opentype.Font
	fontsLoadErr error
)

func loadFonts() error {
	fontsOnce.Do(func() {
		if boldF, fontsLoadErr = opentype.Parse(gobold.TTF); fontsLoadErr != nil {
			return
		}
		regF, fontsLoadErr = opentype.Parse(goregular.TTF)
	})
	return fontsLoadErr
}

func newFace(f *opentype.Font, size float64) font.Face {
	fc, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		panic(err) // only fails on invalid options
	}
	return fc
}

// fit returns the largest face (down to min) at which text fits in maxW,
// and the text itself, ellipsized if it still doesn't fit at min.
func fit(f *opentype.Font, text string, maxW int, size, min float64) (font.Face, string) {
	for ; size > min; size -= 2 {
		fc := newFace(f, size)
		if font.MeasureString(fc, text).Ceil() <= maxW {
			return fc, text
		}
		fc.Close()
	}
	fc := newFace(f, min)
	r := []rune(text)
	for len(r) > 1 && font.MeasureString(fc, string(r)+"…").Ceil() > maxW {
		r = r[:len(r)-1]
	}
	if len(r) < len([]rune(text)) {
		return fc, strings.TrimSpace(string(r)) + "…"
	}
	return fc, text
}

func drawText(dst draw.Image, fc font.Face, x, y int, s string, c color.Color) {
	d := &font.Drawer{Dst: dst, Src: image.NewUniform(c), Face: fc, Dot: fixed.P(x, y)}
	d.DrawString(s)
}

func fillRect(dst draw.Image, r image.Rectangle, c color.Color) {
	draw.Draw(dst, r, image.NewUniform(c), image.Point{}, draw.Src)
}

// Render draws the card and returns PNG bytes.
func Render(c Card) ([]byte, error) {
	if err := loadFonts(); err != nil {
		return nil, err
	}
	accent := color.Color(colAccent)
	if c.Accent != nil {
		accent = c.Accent
	}
	img := image.NewRGBA(image.Rect(0, 0, Width, Height))
	fillRect(img, img.Bounds(), colBG)
	fillRect(img, image.Rect(0, 0, 20, Height), accent) // left accent bar

	// Thin inset frame.
	in := image.Rect(60, 52, Width-60, Height-52)
	fillRect(img, image.Rect(in.Min.X, in.Min.Y, in.Max.X, in.Min.Y+2), colRule)
	fillRect(img, image.Rect(in.Min.X, in.Max.Y-2, in.Max.X, in.Max.Y), colRule)
	fillRect(img, image.Rect(in.Min.X, in.Min.Y, in.Min.X+2, in.Max.Y), colRule)
	fillRect(img, image.Rect(in.Max.X-2, in.Min.Y, in.Max.X, in.Max.Y), colRule)

	const x = 108
	maxW := Width - 2*x

	orgMax := maxW
	if c.Logo != nil {
		orgMax = drawLogo(img, c.Logo, Width-x, 84) - x - 24
	}
	orgFace, org := fit(boldF, strings.ToUpper(c.Org), orgMax, 26, 18)
	defer orgFace.Close()
	drawText(img, orgFace, x, 128, org, accent)

	headFace := newFace(regF, 32)
	defer headFace.Close()
	drawText(img, headFace, x, 196, c.Heading, colMuted)

	nameFace, name := fit(boldF, c.Name, maxW, 84, 44)
	defer nameFace.Close()
	drawText(img, nameFace, x, 300, name, colText)

	courseFace, course := fit(regF, c.Course, maxW, 44, 26)
	defer courseFace.Close()
	drawText(img, courseFace, x, 372, course, colText)

	fillRect(img, image.Rect(x, 418, x+96, 422), accent)

	footFace, foot := fit(regF, c.Footer, maxW, 24, 18)
	defer footFace.Close()
	drawText(img, footFace, x, 520, foot, colMuted)

	if c.Revoked {
		badge := newFace(boldF, 30)
		defer badge.Close()
		w := font.MeasureString(badge, "REVOKED").Ceil()
		drawText(img, badge, Width-x-w, 520, "REVOKED", colRevoke)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// drawLogo draws logo inside a light rounded-ish badge whose top-right
// corner is (right, top), scaled to at most 240x88, and returns the
// badge's left edge.
func drawLogo(dst *image.RGBA, logo image.Image, right, top int) int {
	const maxW, maxH, pad = 240, 88, 12
	b := logo.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return right
	}
	scale := min(float64(maxW)/float64(w), float64(maxH)/float64(h), 1)
	w, h = max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
	badge := image.Rect(right-w-2*pad, top, right, top+h+2*pad)
	fillRect(dst, badge, colText)
	xdraw.CatmullRom.Scale(dst, image.Rect(badge.Min.X+pad, top+pad, badge.Min.X+pad+w, top+pad+h), logo, b, draw.Over, nil)
	return badge.Min.X
}
