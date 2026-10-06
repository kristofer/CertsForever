package certid

import "testing"

func TestNewIsCanonical(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := New()
		norm, ok := Normalize(id)
		if !ok || norm != id {
			t.Fatalf("New() = %q not canonical (norm %q, ok %v)", id, norm, ok)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
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
		{"7K3M9QF2XA", "ZCW-7K3M9QF2XA", true},
		{"ZCW-7K3M9QF2XO", "ZCW-7K3M9QF2X0", true}, // O -> 0
		{"ZCW-7K3M9QF2XL", "ZCW-7K3M9QF2X1", true}, // L -> 1
		{"ZCW-7K3M9QF2X", "", false},               // too short
		{"ZCW-7K3M9QF2XU", "", false},              // U is not Crockford
		{"<script>", "", false},
	}
	for _, c := range cases {
		got, ok := Normalize(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("Normalize(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}
