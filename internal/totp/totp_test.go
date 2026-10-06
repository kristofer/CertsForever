package totp

import (
	"strings"
	"testing"
	"time"
)

// RFC 6238 Appendix B, SHA-1, secret "12345678901234567890".
func TestRFC6238Vectors(t *testing.T) {
	key := []byte("12345678901234567890")
	cases := []struct {
		unix int64
		want string // 8 digits
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	secret := b32.EncodeToString(key)
	for _, c := range cases {
		if got := hotp(key, c.unix/Period, 8); got != c.want {
			t.Errorf("T=%d: got %s want %s", c.unix, got, c.want)
		}
		six, _ := Code(secret, c.unix/Period)
		if six != c.want[2:] {
			t.Errorf("T=%d 6-digit: got %s want %s", c.unix, six, c.want[2:])
		}
	}
}

func TestVerifyWindow(t *testing.T) {
	secret := NewSecret()
	now := time.Unix(1_800_000_000, 0)
	code, _ := Code(secret, Counter(now)-1) // previous step (clock drift)
	if c, ok := Verify(secret, code, now, 1); !ok || c != Counter(now)-1 {
		t.Fatalf("previous step rejected: %v %v", c, ok)
	}
	if _, ok := Verify(secret, code, now.Add(2*Period*time.Second), 1); ok {
		t.Error("code accepted three steps later")
	}
	spaced := code[:3] + " " + code[3:]
	if _, ok := Verify(secret, spaced, now, 1); !ok {
		t.Error("spaced code rejected")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := Verify(secret, bad, now, 1); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, ok := Verify("not base32!", "123456", now, 1); ok {
		t.Error("bad secret accepted")
	}
	// Grouped and lowercase secrets (as users type them) still work.
	lower := strings.ToLower(Group(secret))
	code, _ = Code(secret, Counter(now))
	if _, ok := Verify(lower, code, now, 0); !ok {
		t.Error("grouped lowercase secret rejected")
	}
}

func TestURI(t *testing.T) {
	u := URI("CertsForever", "ada@example.com", "JBSWY3DPEHPK3PXP")
	for _, want := range []string{"otpauth://totp/CertsForever:ada@example.com?", "secret=JBSWY3DPEHPK3PXP",
		"issuer=CertsForever", "digits=6", "period=30"} {
		if !strings.Contains(u, want) {
			t.Errorf("URI %s missing %s", u, want)
		}
	}
	if len(NewSecret()) != 32 {
		t.Error("secret should be 32 base32 chars (160 bits)")
	}
}
