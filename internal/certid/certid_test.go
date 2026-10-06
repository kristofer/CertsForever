package certid

import "testing"

func TestNewIsCanonical(t *testing.T) {
	seen := map[string]bool{}
	for _, prefix := range []string{"ZCW", "TWA", "AB", "ABCD"} {
		for i := 0; i < 500; i++ {
			id := New(prefix)
			norm, ok := Normalize(id)
			if !ok || norm != id {
				t.Fatalf("New(%q) = %q not canonical (norm %q, ok %v)", prefix, id, norm, ok)
			}
			if Prefix(id) != prefix {
				t.Fatalf("Prefix(%q) = %q", id, Prefix(id))
			}
			if seen[id] {
				t.Fatalf("duplicate id %q", id)
			}
			seen[id] = true
		}
	}
}

func TestNewPanicsOnBadPrefix(t *testing.T) {
	for _, p := range []string{"", "Z", "ZCWXY", "zcw", "Z1"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("New(%q) should panic", p)
				}
			}()
			New(p)
		}()
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"ZCW-7K3M9QF2XA", "ZCW-7K3M9QF2XA", true},
		{"zcw-7k3m 9qf2xa", "ZCW-7K3M9QF2XA", true},
		{"ZCW7K3M9QF2XA", "ZCW-7K3M9QF2XA", true},    // dash omitted
		{"twa-7k3m-9qf2-xa", "TWA-7K3M9QF2XA", true}, // grouped
		{"AB-7K3M9QF2XA", "AB-7K3M9QF2XA", true},
		{"ZCW-7K3M9QF2XO", "ZCW-7K3M9QF2X0", true}, // O -> 0
		{"ZCW-7K3M9QF2XL", "ZCW-7K3M9QF2X1", true}, // L -> 1
		{"7K3M9QF2XA", "", false},                  // prefix required
		{"ZCW-7K3M9QF2X", "", false},               // too short
		{"ZCW-7K3M9QF2XU", "", false},              // U is not Crockford
		{"Z1W-7K3M9QF2XA", "", false},              // prefix must be letters
		{"ABCDE-7K3M9QF2XA", "", false},            // prefix too long
		{"<script>", "", false},
	}
	for _, c := range cases {
		got, ok := Normalize(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("Normalize(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
