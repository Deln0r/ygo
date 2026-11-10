package anyval

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/Deln0r/ygo/internal/block"
)

// TestForJSON: values render as JSON.stringify writes them (Node 26,
// checked by hand): an undefined object entry is dropped, an undefined
// array entry or top-level value is null, -0 is 0. A BigInt, where
// JSON.stringify throws, is a JSON number with all its digits.
func TestForJSON(t *testing.T) {
	negZero := math.Copysign(0, -1)
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"undefined", block.Undefined{}, `null`},
		{"object entry undefined", map[string]any{"a": block.Undefined{}, "b": int64(1)}, `{"b":1}`},
		{"array entry undefined", []any{block.Undefined{}, int64(1)}, `[null,1]`},
		{"-0", negZero, `0`},
		{"float32 -0", float32(negZero), `0`},
		{"nested -0", map[string]any{"z": []any{negZero}}, `{"z":[0]}`},
		{"BigInt beyond 2^53", block.BigInt(1<<53 + 1), `9007199254740993`},
		{"BigInt min", block.BigInt(math.MinInt64), `-9223372036854775808`},
		{"unchanged", map[string]any{"s": "x", "n": nil, "f": 2.5}, `{"f":2.5,"n":null,"s":"x"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := json.Marshal(ForJSON(c.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != c.want {
				t.Fatalf("got %s, want %s", raw, c.want)
			}
		})
	}
}

// TestForJSON_DoesNotChangeItsInput: the document's stored values must keep
// their undefined entries and their sign.
func TestForJSON_DoesNotChangeItsInput(t *testing.T) {
	negZero := math.Copysign(0, -1)
	in := map[string]any{"a": block.Undefined{}, "z": []any{negZero}}
	_ = ForJSON(in)
	if _, ok := in["a"].(block.Undefined); !ok {
		t.Fatal("ForJSON removed the undefined entry from its input")
	}
	if f := in["z"].([]any)[0].(float64); !math.Signbit(f) {
		t.Fatal("ForJSON changed -0 in its input")
	}
}

// TestUndefinedMarshalJSON: a caller marshalling a stored value itself gets
// null for undefined rather than {} for an empty struct.
func TestUndefinedMarshalJSON(t *testing.T) {
	raw, err := json.Marshal(map[string]any{"a": block.Undefined{}})
	if err != nil || string(raw) != `{"a":null}` {
		t.Fatalf("got %s %v, want {\"a\":null}", raw, err)
	}
}
