package encoding

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/Deln0r/ygo/internal/anyval"
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
			if got := string(appendStringify(nil, anyval.ForJSON(c.v))); got != c.want {
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
	if got := string(appendStringify(nil, link{"a&b", "<x>"})); got != `{"src":"a&b","alt":"<x>"}` {
		t.Fatalf("struct: got %s", got)
	}
	if got := string(appendStringify(nil, json.RawMessage(`{"b":1,"0":2}`))); got != `{"0":2,"b":1}` {
		t.Fatalf("raw: got %s", got)
	}
}
