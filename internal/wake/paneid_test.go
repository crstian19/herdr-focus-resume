package wake

import "testing"

// Pairs observed in a real Herdr 0.9.5 session.json vs. `herdr pane list`.
func TestEncodeNumberMatchesHerdr(t *testing.T) {
	cases := []struct {
		n    uint64
		want string
	}{
		{1, "1"}, {6, "6"}, {11, "B"}, {16, "G"}, {18, "J"}, {28, "W"},
		{43, "1B"}, {45, "1D"}, {46, "1E"}, {57, "1S"}, {59, "1V"},
		{60, "1W"}, {65, "21"}, {75, "2B"}, {0, "0"}, {31, "Z"}, {32, "10"},
	}
	for _, c := range cases {
		if got := EncodeNumber(c.n); got != c.want {
			t.Errorf("EncodeNumber(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestPaneID(t *testing.T) {
	if got := PaneID("wE", 75); got != "wE:p2B" {
		t.Fatalf("PaneID = %q, want wE:p2B", got)
	}
}

func FuzzNumberRoundTrip(f *testing.F) {
	for _, n := range []uint64{0, 1, 31, 32, 46, 1 << 40, ^uint64(0) >> 4} {
		f.Add(n)
	}
	f.Fuzz(func(t *testing.T, n uint64) {
		n >>= 4 // keep within 12 digits
		s := EncodeNumber(n)
		got, ok := DecodeNumber(s)
		if !ok || got != n {
			t.Fatalf("round trip %d -> %q -> %d (%v)", n, s, got, ok)
		}
		for _, banned := range "ILOU" {
			for _, c := range s {
				if c == banned {
					t.Fatalf("%q contains %q, not in Crockford alphabet", s, banned)
				}
			}
		}
	})
}
