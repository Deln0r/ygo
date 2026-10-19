package utf16

import "testing"

func TestValidString(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"abc", "abc"},
		{"a\U0001F600b", "a\U0001F600b"},
		{"\ufeffx\x00y\uFFFF", "\ufeffx\x00y\uFFFF"}, // BOM, NUL, noncharacter: valid
		{"\xff", "\uFFFD"},
		{"a\x80b", "a\uFFFDb"},
		{"\xe2\x82", "\uFFFD\uFFFD"},           // truncated sequence: one per byte
		{"\xed\xa0\x80", "\uFFFD\uFFFD\uFFFD"}, // encoded surrogate
		{"\xc0\xaf", "\uFFFD\uFFFD"},           // overlong
		{"\xc2", "\uFFFD"},
	}
	for _, c := range cases {
		got := ValidString(c.in)
		if got != c.want {
			t.Errorf("ValidString(%q) = %q, want %q", c.in, got, c.want)
		}
		if Length(got) != Length(c.in) {
			t.Errorf("ValidString(%q): length %d, raw length %d", c.in, Length(got), Length(c.in))
		}
	}
	// Fragments fixed one by one do not recombine into a valid character.
	if got := ValidString("\xc2") + ValidString("\xa2"); got != "\uFFFD\uFFFD" {
		t.Errorf("separate fragments = %q", got)
	}
}

func TestValidString_NoAllocationForValid(t *testing.T) {
	s := "plain ascii and \u00fcn\u00efc\u00f6d\u00e9 \U0001F600"
	if n := testing.AllocsPerRun(100, func() { _ = ValidString(s) }); n != 0 {
		t.Errorf("ValidString allocated %v times for a valid string", n)
	}
}
