// Package certid generates and normalizes public certificate IDs.
//
// IDs look like ZCW-7K3M9QF2XA: a fixed prefix plus 10 Crockford base32
// characters (50 random bits). They are unguessable, easy to read aloud,
// and tolerant of the usual transcription mistakes (O/0, I/L/1, case).
package certid

import (
	"crypto/rand"
	"strings"
)

// Prefix is prepended to every certificate ID.
const Prefix = "ZCW-"

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford base32
const bodyLen = 10

// New returns a fresh random certificate ID.
func New() string {
	b := make([]byte, bodyLen)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	out := make([]byte, bodyLen)
	for i, v := range b {
		out[i] = alphabet[v&31] // 256 is a multiple of 32, so this is unbiased
	}
	return Prefix + string(out)
}

// Normalize turns user input ("zcw-7k3m 9qf2xa", "7K3M9QF2XA") into the
// canonical ID. ok is false if the input cannot be a valid ID.
func Normalize(s string) (id string, ok bool) {
	s = strings.NewReplacer("-", "", " ", "", "_", "").Replace(strings.ToUpper(strings.TrimSpace(s)))
	if p := strings.TrimSuffix(Prefix, "-"); len(s) == len(p)+bodyLen && strings.HasPrefix(s, p) {
		s = s[len(p):]
	}
	if len(s) != bodyLen {
		return "", false
	}
	var b strings.Builder
	for _, r := range s {
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
	return Prefix + b.String(), true
}
