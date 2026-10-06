package imgnorm

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func encodePNG(t *testing.T, img image.Image) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestNormalizeScalesDown(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2000, 400))
	out, w, h, err := Normalize(encodePNG(t, src), "logo")
	if err != nil {
		t.Fatal(err)
	}
	if w != 600 || h != 120 {
		t.Fatalf("got %dx%d", w, h)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil || img.Bounds().Dx() != 600 {
		t.Fatalf("output: %v", err)
	}
}

func TestNormalizeKeepsSmall(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 100, 50))
	_, w, h, err := Normalize(encodePNG(t, src), "logo")
	if err != nil || w != 100 || h != 50 {
		t.Fatalf("%dx%d %v", w, h, err)
	}
}

func TestSignatureWhiteBecomesTransparent(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 20, 10))
	for i := range src.Pix {
		src.Pix[i] = 0xff // white paper
	}
	src.Set(5, 5, color.Black) // ink
	var b bytes.Buffer
	jpeg.Encode(&b, src, &jpeg.Options{Quality: 100})
	out, _, _, err := Normalize(b.Bytes(), "signature")
	if err != nil {
		t.Fatal(err)
	}
	img, _ := png.Decode(bytes.NewReader(out))
	if _, _, _, a := img.At(0, 0).RGBA(); a != 0 {
		t.Fatalf("paper alpha = %d", a)
	}
}

func TestNormalizeRejects(t *testing.T) {
	for name, data := range map[string][]byte{
		"text":  []byte("<svg onload=alert(1)>"),
		"empty": nil,
		"gif":   []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"),
		"huge":  make([]byte, MaxUpload+1),
	} {
		if _, _, _, err := Normalize(data, "logo"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A PNG header claiming 100000x100000 is refused before decoding.
	bomb := encodePNG(t, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	bomb[16], bomb[17], bomb[18], bomb[19] = 0, 1, 0x86, 0xa0 // width 100000
	bomb[20], bomb[21], bomb[22], bomb[23] = 0, 1, 0x86, 0xa0 // height 100000
	if _, _, _, err := Normalize(bomb, "logo"); err == nil {
		t.Error("decompression bomb accepted")
	}
	if _, _, _, err := Normalize(encodePNG(t, image.NewRGBA(image.Rect(0, 0, 1, 1))), "banner"); err == nil {
		t.Error("unknown kind accepted")
	}
}
