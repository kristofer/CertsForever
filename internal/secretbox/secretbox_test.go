package secretbox

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestSealOpen(t *testing.T) {
	key, err := ParseKey(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := New(key)
	sealed := b.Seal([]byte("JBSWY3DPEHPK3PXP"), "totp:user:1")
	if bytes.Contains(sealed, []byte("JBSWY3DP")) {
		t.Fatal("plaintext visible in sealed value")
	}
	if pt, err := b.Open(sealed, "totp:user:1"); err != nil || string(pt) != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("open: %q %v", pt, err)
	}
	if again := b.Seal([]byte("JBSWY3DPEHPK3PXP"), "totp:user:1"); bytes.Equal(again, sealed) {
		t.Error("nonce reused")
	}

	// Moved to another row, tampered with, or a different key: all refused.
	if _, err := b.Open(sealed, "totp:user:2"); !errors.Is(err, ErrDecrypt) {
		t.Error("value moved to another user still opened")
	}
	tampered := append([]byte{}, sealed...)
	tampered[len(tampered)-1] ^= 1
	if _, err := b.Open(tampered, "totp:user:1"); !errors.Is(err, ErrDecrypt) {
		t.Error("tampered value opened")
	}
	other, _ := New(bytes.Repeat([]byte{7}, 32))
	if _, err := other.Open(sealed, "totp:user:1"); !errors.Is(err, ErrDecrypt) {
		t.Error("opened under another key")
	}
	if _, err := b.Open([]byte{1, 2, 3}, "x"); !errors.Is(err, ErrDecrypt) {
		t.Error("short input")
	}
}

func TestParseKey(t *testing.T) {
	raw := bytes.Repeat([]byte{0x5a}, 32)
	for _, in := range []string{
		strings.Repeat("5a", 32),
		base64.StdEncoding.EncodeToString(raw),
		base64.RawURLEncoding.EncodeToString(raw),
		"  " + strings.Repeat("5A", 32) + "\n",
	} {
		k, err := ParseKey(in)
		if err != nil || !bytes.Equal(k, raw) {
			t.Errorf("ParseKey(%q) = %x, %v", in, k, err)
		}
	}
	for _, bad := range []string{"", "short", strings.Repeat("5a", 31), base64.StdEncoding.EncodeToString(raw[:16])} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) accepted", bad)
		}
	}
}
