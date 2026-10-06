// Package totp implements time-based one-time passwords (RFC 6238, SHA-1,
// 30-second steps, 6 digits): the codes authenticator apps produce.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	Period = 30 // seconds per step
	Digits = 6
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret returns a random 160-bit secret, base32-encoded (what users
// type into an authenticator app).
func NewSecret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b32.EncodeToString(b)
}

// Counter is the time step for t.
func Counter(t time.Time) int64 { return t.Unix() / Period }

func decode(secret string) ([]byte, error) {
	s := strings.ToUpper(strings.NewReplacer(" ", "", "-", "").Replace(secret))
	s = strings.TrimRight(s, "=")
	key, err := b32.DecodeString(s)
	if err != nil || len(key) == 0 {
		return nil, fmt.Errorf("totp: invalid secret")
	}
	return key, nil
}

func hotp(key []byte, counter int64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, v%mod)
}

// Code returns the code for a time step.
func Code(secret string, counter int64) (string, error) {
	key, err := decode(secret)
	if err != nil {
		return "", err
	}
	return hotp(key, counter, Digits), nil
}

// Verify checks code against the steps around t (±skew steps, to allow for
// clock drift) and returns the step it matched. Callers must also reject
// steps already used (see store.UseTOTPCounter) to stop replays.
func Verify(secret, code string, t time.Time, skew int) (counter int64, ok bool) {
	key, err := decode(secret)
	if err != nil {
		return 0, false
	}
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != Digits {
		return 0, false
	}
	now := Counter(t)
	for d := -int64(skew); d <= int64(skew); d++ {
		c := now + d
		if subtle.ConstantTimeCompare([]byte(hotp(key, c, Digits)), []byte(code)) == 1 {
			return c, true
		}
	}
	return 0, false
}

// URI is the otpauth:// link authenticator apps understand (and that a QR
// code would encode).
func URI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(Digits))
	q.Set("period", fmt.Sprint(Period))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// Group formats a secret in blocks of four for reading and typing.
func Group(secret string) string {
	var b strings.Builder
	for i, r := range secret {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}
