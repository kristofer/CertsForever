// Package secretbox encrypts small secrets at rest (e.g. TOTP secrets) with
// AES-256-GCM under the server's master key (CERTS_MASTER_KEY).
//
// Sealed format: version (1 byte, 0x01) || nonce (12 bytes) || ciphertext+tag.
// Callers pass associated data (e.g. "totp:user:42") so a sealed value can't
// be copied from one row to another and still decrypt.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
)

const version = 0x01

// ErrDecrypt means the value was tampered with, moved, or sealed under a
// different key.
var ErrDecrypt = errors.New("secretbox: cannot decrypt (wrong key or tampered data)")

// Box seals and opens values.
type Box struct {
	aead cipher.AEAD
}

// ParseKey accepts a 32-byte key as 64 hex characters or base64.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("master key must be 32 bytes, as 64 hex characters or base64 (try: openssl rand -hex 32)")
}

// New returns a Box for a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("secretbox: key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts plaintext, binding it to ad.
func (b *Box) Seal(plaintext []byte, ad string) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	out := append([]byte{version}, nonce...)
	return b.aead.Seal(out, nonce, plaintext, []byte(ad))
}

// Open decrypts a value sealed with the same key and ad.
func (b *Box) Open(sealed []byte, ad string) ([]byte, error) {
	ns := b.aead.NonceSize()
	if len(sealed) < 1+ns+b.aead.Overhead() || sealed[0] != version {
		return nil, ErrDecrypt
	}
	pt, err := b.aead.Open(nil, sealed[1:1+ns], sealed[1+ns:], []byte(ad))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}
