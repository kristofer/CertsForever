// Package certid generates and normalizes public certificate IDs.
//
// IDs look like ZCW-7K3M9QF2XA: the issuing client's prefix (2–4 letters),
// a dash, and 10 Crockford base32 characters (50 random bits). They are
// unguessable, easy to read aloud, and tolerant of the usual transcription
// mistakes (case, missing dash, O/0, I/L/1).
package certid

import (
	"crypto/rand"
	"strings"
)

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford base32
const bodyLen = 10

// ValidPrefix reports whether p is a usable client prefix: 2–4 letters A–Z.
func ValidPrefix(p string) bool {
	if len(p) < 2 || len(p) > 4 {
		return false
	}
	for _, r := range p {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

// New returns a fresh random certificate ID with the given client prefix.
// It panics on an invalid prefix (a programming error; prefixes are
// validated when clients are created).
func New(prefix string) string {
	if !ValidPrefix(prefix) {
		panic("certid: invalid prefix " + prefix)
	}
	b := make([]byte, bodyLen)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	out := make([]byte, bodyLen)
	for i, v := range b {
		out[i] = alphabet[v&31] // 256 is a multiple of 32, so this is unbiased
	}
	return prefix + "-" + string(out)
}

// Normalize turns user input ("zcw-7k3m 9qf2xa", "ZCW7K3M9QF2XA") into the
// canonical ID. ok is false if the input cannot be a valid ID. The prefix is
// required: IDs are only unique together with it.
func Normalize(s string) (id string, ok bool) {
	s = strings.NewReplacer(" ", "", "_", "").Replace(strings.ToUpper(strings.TrimSpace(s)))
	var prefix, body string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		prefix, body = s[:i], strings.ReplaceAll(s[i+1:], "-", "")
	} else if len(s) > bodyLen {
		prefix, body = s[:len(s)-bodyLen], s[len(s)-bodyLen:]
	}
	if !ValidPrefix(prefix) || len(body) != bodyLen {
		return "", false
	}
	var b strings.Builder
	for _, r := range body {
		switch r {
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		if !strings.ContainsRune(alphabet, r) {
			return "", false
		}
		b.WriteRune(r)
	}
	return prefix + "-" + b.String(), true
}

// Prefix returns the client prefix of a canonical ID ("ZCW" for "ZCW-…").
func Prefix(id string) string {
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	return ""
}
