package ogimage

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"testing"
)

func TestRenderDimensions(t *testing.T) {
	b, err := Render(Card{
		Org:     "Zip Code Wilmington",
		Heading: "Certificate of Completion",
		Name:    strings.Repeat("Very Long Name ", 10), // forces shrink + ellipsis
		Course:  "Java Full-Stack Developer",
		Footer:  "Issued October 2026 · certs.example.org/c/ZCW-7K3M9QF2XA",
		Revoked: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if got := img.Bounds().Size(); got.X != Width || got.Y != Height {
		t.Fatalf("size = %v, want %dx%d", got, Width, Height)
	}
}

func TestRenderAccentAndLogo(t *testing.T) {
	accent, ok := ParseHex("#AA3300")
	if !ok {
		t.Fatal("parse")
	}
	for _, bad := range []string{"", "aa3300", "#aa330", "#gg3300", "#aa33001"} {
		if _, ok := ParseHex(bad); ok {
			t.Errorf("ParseHex(%q) ok", bad)
		}
	}
	logo := image.NewRGBA(image.Rect(0, 0, 600, 200))
	draw.Draw(logo, logo.Bounds(), image.NewUniform(color.RGBA{0, 0, 255, 255}), image.Point{}, draw.Src)
	b, err := Render(Card{Org: "Zip Code Wilmington", Heading: "H", Name: "N", Course: "C", Footer: "F",
		Accent: accent, Logo: logo})
	if err != nil {
		t.Fatal(err)
	}
	img, _ := png.Decode(bytes.NewReader(b))
	if r, g, bb, _ := img.At(5, 300).RGBA(); r>>8 != 0xaa || g>>8 != 0x33 || bb>>8 != 0 {
		t.Fatalf("accent bar = %x %x %x", r>>8, g>>8, bb>>8)
	}
	// The logo is scaled into the top-right badge.
	if r, g, bb, _ := img.At(Width-108-12-10, 84+12+10).RGBA(); r>>8 > 10 || g>>8 > 10 || bb>>8 < 245 {
		t.Fatalf("logo pixel = %x %x %x", r>>8, g>>8, bb>>8)
	}
}
