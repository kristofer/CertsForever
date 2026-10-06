package ogimage

import (
	"bytes"
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
