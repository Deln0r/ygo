package ygo_test

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"testing"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/encoding"
)

// anyValueScenario is one entry of testdata/any-value-fixtures.json
// (testdata/gen/gen-any-values.mjs, yjs 13.6.33).
type anyValueScenario struct {
	Name   string          `json:"name"`
	Kind   string          `json:"kind"`
	V1     *string         `json:"v1"`
	V2     string          `json:"v2"`
	Expect json.RawMessage `json:"expect"`
}

func loadAnyValueFixtures(t *testing.T) []anyValueScenario {
	t.Helper()
	raw, err := os.ReadFile("testdata/any-value-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Scenarios []anyValueScenario `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Scenarios) == 0 {
		t.Fatal("no scenarios")
	}
	return f.Scenarios
}

// tagged is the generator's encoding of a JS value: undefined, BigInt and a
// number are objects with "$"; null, strings and booleans are themselves.
func untag(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return untagValue(t, v)
}

func untagValue(t *testing.T, v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v // null, string, bool
	}
	switch m["$"] {
	case "undefined":
		return ygo.Undefined{}
	case "bigint":
		n, err := strconv.ParseInt(m["v"].(string), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		return ygo.BigInt(n)
	case "number":
		s := m["v"].(string)
		if s == "-0" {
			return math.Copysign(0, -1)
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			t.Fatal(err)
		}
		// What a Go caller writes for this JS number: an integer lib0 writes
		// as a varint is an int64, anything else a float64.
		if f == math.Trunc(f) && math.Abs(f) <= 0x7FFFFFFF {
			return int64(f)
		}
		return f
	case "array":
		var out []any
		for _, el := range m["v"].([]any) {
			out = append(out, untagValue(t, el))
		}
		return out
	case "object":
		out := map[string]any{}
		for k, el := range m["v"].(map[string]any) {
			out[k] = untagValue(t, el)
		}
		return out
	}
	t.Fatalf("unknown tag %v", m)
	return nil
}

// sameJSValue reports whether got, a value read from ygo, is the JS value
// want: undefined, null and BigInt by type, numbers by value and sign, and
// containers entry by entry. A Go == or reflect.DeepEqual cannot see the
// sign of zero.
func sameJSValue(got, want any) error {
	switch w := want.(type) {
	case ygo.Undefined:
		if _, ok := got.(ygo.Undefined); !ok {
			return fmt.Errorf("got %#v, want undefined", got)
		}
	case nil:
		if got != nil {
			return fmt.Errorf("got %#v, want null", got)
		}
	case ygo.BigInt:
		if g, ok := got.(ygo.BigInt); !ok || g != w {
			return fmt.Errorf("got %#v (%T), want BigInt %d", got, got, w)
		}
	case int64, float64:
		var wf float64
		if i, ok := w.(int64); ok {
			wf = float64(i)
		} else {
			wf = w.(float64)
		}
		var gf float64
		switch g := got.(type) {
		case int64:
			gf = float64(g)
		case float64:
			gf = g
		case float32:
			gf = float64(g)
		default:
			return fmt.Errorf("got %#v (%T), want number %v", got, got, wf)
		}
		if gf != wf || math.Signbit(gf) != math.Signbit(wf) {
			return fmt.Errorf("got %v (sign %v), want %v (sign %v)", gf, math.Signbit(gf), wf, math.Signbit(wf))
		}
	case string, bool:
		if got != want {
			return fmt.Errorf("got %#v, want %#v", got, want)
		}
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return fmt.Errorf("got %#v, want array of %d", got, len(w))
		}
		for i := range w {
			if err := sameJSValue(g[i], w[i]); err != nil {
				return fmt.Errorf("[%d]: %w", i, err)
			}
		}
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok || len(g) != len(w) {
			return fmt.Errorf("got %#v, want object with %d keys", got, len(w))
		}
		keys := make([]string, 0, len(w))
		for k := range w {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			gv, present := g[k]
			if !present {
				return fmt.Errorf("key %q missing", k)
			}
			if err := sameJSValue(gv, w[k]); err != nil {
				return fmt.Errorf("%q: %w", k, err)
			}
		}
	default:
		return fmt.Errorf("unexpected expectation %#v", want)
	}
	return nil
}

// readAnyValue reads the scenario's value from d the way an application
// would, or for a subdocument from its options in the update itself (the
// Doc API does not expose meta).
func readAnyValue(t *testing.T, s anyValueScenario, d *ygo.Doc) any {
	t.Helper()
	switch s.Kind {
	case "map":
		m := ygo.NewMap(d, "m")
		if !m.Has("k") {
			t.Fatal("key k missing")
		}
		return m.Get("k")
	case "array":
		return ygo.NewArray(d, "a").Get(0)
	case "format":
		delta := ygo.NewText(d, "t").ToDelta()
		if len(delta) == 0 {
			t.Fatal("empty delta")
		}
		return delta[0].Attributes["x"]
	case "embed":
		delta := ygo.NewText(d, "t").ToDelta()
		if len(delta) == 0 {
			t.Fatal("empty delta")
		}
		return delta[0].Embed
	case "subdoc":
		u, err := encoding.DecodeUpdateV2(mustHex(t, s.V2))
		if err != nil {
			t.Fatal(err)
		}
		for _, blocks := range u.Blocks {
			for _, b := range blocks {
				if b.Item != nil && b.Item.Content.Kind == block.KindDoc {
					return b.Item.Content.DocOpts["meta"]
				}
			}
		}
		t.Fatal("no subdocument in the update")
	}
	t.Fatalf("unknown kind %q", s.Kind)
	return nil
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAnyValues_Fixtures: a document yjs wrote with undefined, BigInt or -0
// reads back with those values, and ygo encodes it again to yjs's bytes, V1
// and V2. Before v1.33.0 ygo read undefined as nil, a BigInt as int64 and
// -0 as 0, and wrote back null, a number and 0.
func TestAnyValues_Fixtures(t *testing.T) {
	for _, s := range loadAnyValueFixtures(t) {
		t.Run(s.Name, func(t *testing.T) {
			want := untag(t, s.Expect)
			apply := map[string]func(*ygo.Doc) error{
				"V2": func(d *ygo.Doc) error { return ygo.ApplyUpdateV2(d, mustHex(t, s.V2)) },
			}
			if s.V1 != nil {
				v1 := *s.V1
				apply["V1"] = func(d *ygo.Doc) error { return ygo.ApplyUpdate(d, mustHex(t, v1)) }
			}
			for _, from := range []string{"V1", "V2"} {
				fn, ok := apply[from]
				if !ok {
					continue
				}
				d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
				if err := fn(d); err != nil {
					t.Fatalf("apply %s: %v", from, err)
				}
				// V1 format attributes are JSON, which has no -0; the value
				// check applies to what the Any path carries.
				if from != "V1" || s.Kind != "format" {
					if err := sameJSValue(readAnyValue(t, s, d), want); err != nil {
						t.Fatalf("value read from %s: %v", from, err)
					}
				}
				if s.V1 != nil {
					if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != *s.V1 {
						t.Fatalf("applied %s, re-encoded V1:\n got %s\nwant %s", from, got, *s.V1)
					}
				}
				// A V1 format attribute went through JSON, so a document
				// built from it no longer holds what the V2 bytes hold (-0 is
				// 0 there), in yjs as in ygo.
				if from == "V1" && s.Kind == "format" {
					continue
				}
				if got := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(d)); got != s.V2 {
					t.Fatalf("applied %s, re-encoded V2:\n got %s\nwant %s", from, got, s.V2)
				}
			}
		})
	}
}

// TestAnyValues_WrittenFromGo: the same values set through ygo's API, with
// the same client ID, give yjs's bytes. A subdocument's meta has no setter.
func TestAnyValues_WrittenFromGo(t *testing.T) {
	for _, s := range loadAnyValueFixtures(t) {
		if s.Kind == "subdoc" {
			continue
		}
		t.Run(s.Name, func(t *testing.T) {
			value := untag(t, s.Expect)
			d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
			var m *ygo.Map
			var a *ygo.Array
			var x *ygo.Text
			switch s.Kind {
			case "map":
				m = ygo.NewMap(d, "m")
			case "array":
				a = ygo.NewArray(d, "a")
			default:
				x = ygo.NewText(d, "t")
			}
			txn := d.WriteTxn()
			switch s.Kind {
			case "map":
				m.Set(txn, "k", value)
			case "array":
				a.Insert(txn, 0, value)
			case "format":
				if err := x.Insert(txn, 0, "ab"); err != nil {
					t.Fatal(err)
				}
				if err := x.Format(txn, 0, 2, ygo.Attrs{"x": value}); err != nil {
					t.Fatal(err)
				}
			case "embed":
				if err := x.InsertEmbed(txn, 0, value); err != nil {
					t.Fatal(err)
				}
			}
			txn.Commit()
			if s.V1 != nil {
				if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != *s.V1 {
					t.Fatalf("V1:\n got %s\nwant %s", got, *s.V1)
				}
			}
			if got := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(d)); got != s.V2 {
				t.Fatalf("V2:\n got %s\nwant %s", got, s.V2)
			}
		})
	}
}
