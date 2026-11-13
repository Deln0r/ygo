package anyval

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/Deln0r/ygo/internal/block"
)

// TestAppendStringify: values with no JavaScript twin in the fixtures are
// written as JSON.stringify writes the value a JavaScript peer holds (node
// 26: JSON.stringify([Math.fround(0.1), 2**60+1, new Uint8Array(0),
// [undefined], {a: undefined, b: 1}, -Infinity, -0, 5e-324, 1e-7, 1.5e300,
// 0.000001234])).
func TestAppendStringify(t *testing.T) {
	for _, c := range []struct {
		name string
		v    any
		want string
	}{
		{"float32 widened as JavaScript reads it", float32(0.1), "0.10000000149011612"},
		{"int64 beyond 2^53 as the double it becomes", int64(1<<60 + 1), "1152921504606847000"},
		{"empty bytes", []byte{}, "{}"},
		{"nil bytes", []byte(nil), "null"},
		{"undefined in an array", []any{block.Undefined{}}, "[null]"},
		{"undefined entry left out", map[string]any{"a": block.Undefined{}, "b": int64(1)}, `{"b":1}`},
		{"-Infinity", math.Inf(-1), "null"},
		{"negative zero", math.Copysign(0, -1), "0"},
		{"smallest denormal", 5e-324, "5e-324"},
		{"1e-7", 1e-7, "1e-7"},
		{"1.5e300", 1.5e300, "1.5e+300"},
		{"0.000001234", 0.000001234, "0.000001234"},
		{"BigInt digits", block.BigInt(9007199254740993), "9007199254740993"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := string(AppendStringify(nil, ForJSON(c.v))); got != c.want {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
	// A struct marshals to JSON text first and is then written as that
	// text parsed: field order kept, & not escaped.
	type link struct {
		Src string `json:"src"`
		Alt string `json:"alt"`
	}
	if got := string(AppendStringify(nil, link{"a&b", "<x>"})); got != `{"src":"a&b","alt":"<x>"}` {
		t.Fatalf("struct: got %s", got)
	}
	if got := string(AppendStringify(nil, json.RawMessage(`{"b":1,"0":2}`))); got != `{"0":2,"b":1}` {
		t.Fatalf("raw: got %s", got)
	}
}

// TestParseJSON_RejectsWhatJSONParseRejects: ParseJSON takes the texts
// JSON.parse takes and refuses the ones it throws on (node 26, each list
// checked there), so a text either reads as JavaScript reads it or is not
// read at all.
func TestParseJSON_RejectsWhatJSONParseRejects(t *testing.T) {
	for _, s := range []string{
		"01", "[1,]", `{"a":1,}`, "{'a':1}", "\"\x01\"", "\v1", "NaN", "\ufeff1",
		"1 2", "-", "1.", ".5", "1e", `"\x"`, `"\u12"`, "tru", `{"a"}`, "[", `"abc`,
		"Infinity", `{"a":1 "b":2}`, "\u00a01",
	} {
		if _, err := ParseJSON([]byte(s)); err == nil {
			t.Errorf("ParseJSON(%q) accepted a text JSON.parse rejects", s)
		}
	}
	for _, s := range []string{"  1  ", "-0", "1E+2", `"é"`, "\t\n\r 1", "[]", "{}", `"\/"`} {
		if _, err := ParseJSON([]byte(s)); err != nil {
			t.Errorf("ParseJSON(%q): %v, but JSON.parse takes it", s, err)
		}
	}
	// Nesting up to 10000 levels parses, as encoding/json's decoder does,
	// past it does not.
	deep := func(n int) []byte {
		b := make([]byte, 0, 2*n)
		for range n {
			b = append(b, '[')
		}
		for range n {
			b = append(b, ']')
		}
		return b
	}
	if _, err := ParseJSON(deep(10000)); err != nil {
		t.Errorf("10000 levels: %v", err)
	}
	if _, err := ParseJSON(deep(10002)); err == nil {
		t.Error("10002 levels parsed")
	}
}

// TestIsArrayIndexKey: canonical decimal integers below 2^32-1 only, as
// ECMAScript defines an array index; a long digit string must not wrap.
func TestIsArrayIndexKey(t *testing.T) {
	for k, want := range map[string]bool{
		"0": true, "9": true, "10": true, "4294967294": true,
		"4294967295": false, "01": false, "-1": false, "": false, "1.0": false,
		"18446744073709551617": false, "99999999999": false, "a1": false,
	} {
		if got := IsArrayIndexKey(k); got != want {
			t.Errorf("IsArrayIndexKey(%q) = %v, want %v", k, got, want)
		}
	}
}
