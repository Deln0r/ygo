package ygo_test

import (
	"encoding/hex"
	"encoding/json"
	"math"
	"testing"

	"github.com/Deln0r/ygo"
)

// Embeds and format values are written through the JSON content codec, which
// V1 encodes as a JSON string and V2 as a lib0 Any. Cross-language fixtures now
// cover the ordinary shapes in both directions; these tests cover what those
// cannot: values encoding/json accepts but a bare Any encoder would panic on,
// and the Go types a caller reads back.

type embedStruct struct {
	Image string `json:"image"`
	Width int    `json:"width"`
}

type customEmbed struct{ id int }

func (c customEmbed) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{"mention": c.id})
}

// TestV2_RichTextAcceptsWhatV1Accepts: the V2 JSON path used to reuse V1's
// encoding/json route, so every value V1 took, V2 took too. Moving V2 onto the
// Any codec it should always have used must not turn those into panics - the
// Any encoder rejects structs, typed maps and custom marshalers outright.
func TestV2_RichTextAcceptsWhatV1Accepts(t *testing.T) {
	for name, embed := range map[string]any{
		"struct":            embedStruct{Image: "x.png", Width: 120},
		"typed map":         map[string]string{"image": "x.png"},
		"typed slice":       []string{"a", "b"},
		"custom marshaler":  customEmbed{id: 7},
		"unsigned integer":  map[string]any{"n": uint32(9)},
		"nested typed data": map[string]any{"size": map[string]int{"w": 3, "h": 4}},
	} {
		t.Run(name, func(t *testing.T) {
			d := ygo.NewDocWithOptions(ygo.Options{ClientID: 21})
			txt := ygo.NewText(d, "t")
			txn := d.WriteTxn()
			if err := txt.Insert(txn, 0, "ab"); err != nil {
				t.Fatal(err)
			}
			if err := txt.InsertEmbed(txn, 1, embed); err != nil {
				t.Fatal(err)
			}
			txn.Commit()

			var v2 []byte
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("EncodeStateAsUpdateV2 panicked on an embed V1 accepts: %v", r)
					}
				}()
				v2 = ygo.EncodeStateAsUpdateV2(d)
			}()

			// Both formats must carry the same payload, read back the same way.
			fromV1 := ygo.NewDoc()
			if err := ygo.ApplyUpdate(fromV1, ygo.EncodeStateAsUpdate(d)); err != nil {
				t.Fatal(err)
			}
			fromV2 := ygo.NewDoc()
			if err := ygo.ApplyUpdateV2(fromV2, v2); err != nil {
				t.Fatalf("our own V2 bytes did not decode: %v", err)
			}
			a, b := embedOf(t, fromV1), embedOf(t, fromV2)
			if a != b {
				t.Fatalf("embed differs by wire format:\n  V1 %s\n  V2 %s", a, b)
			}
		})
	}
}

// TestV2_AttributeTypesMatchV1: a caller reading a format attribute must get
// the same Go type whichever wire format delivered the document. V1 decodes
// JSON, so every number is a float64; the Any codec reads a varint as an
// int64. Without mapping one onto the other, `attrs["header"].(float64)`
// would work on a V1 document and panic on the identical V2 one.
func TestV2_AttributeTypesMatchV1(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 22})
	txt := ygo.NewText(d, "t")
	txn := d.WriteTxn()
	if err := txt.InsertWithAttributes(txn, 0, "Title\n", map[string]any{
		"header": 1, "scale": 1.5, "ratio": 0.1, "align": "center", "bold": true,
		"nested": map[string]any{"level": 2},
	}); err != nil {
		t.Fatal(err)
	}
	txn.Commit()

	fromV1 := ygo.NewDoc()
	if err := ygo.ApplyUpdate(fromV1, ygo.EncodeStateAsUpdate(d)); err != nil {
		t.Fatal(err)
	}
	fromV2 := ygo.NewDoc()
	if err := ygo.ApplyUpdateV2(fromV2, ygo.EncodeStateAsUpdateV2(d)); err != nil {
		t.Fatal(err)
	}
	a1, a2 := attrsOf(t, fromV1), attrsOf(t, fromV2)
	for k, v1 := range a1 {
		v2 := a2[k]
		if typeName(v1) != typeName(v2) {
			t.Errorf("attribute %q: V1 gives %s, V2 gives %s", k, typeName(v1), typeName(v2))
		}
	}
	nested, ok := a2["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested attribute from V2 is %s, want map[string]any", typeName(a2["nested"]))
	}
	if got := typeName(nested["level"]); got != "float64" {
		t.Errorf("nested number from V2 is %s, want float64 like V1", got)
	}
}

func embedOf(t *testing.T, d *ygo.Doc) string {
	t.Helper()
	txt := ygo.NewText(d, "t")
	rt := d.ReadTxn()
	defer rt.Close()
	for _, op := range txt.ToDelta() {
		if op.Embed != nil {
			b, err := json.Marshal(op.Embed)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
	}
	t.Fatal("no embed in the document")
	return ""
}

func attrsOf(t *testing.T, d *ygo.Doc) map[string]any {
	t.Helper()
	txt := ygo.NewText(d, "t")
	rt := d.ReadTxn()
	defer rt.Close()
	for _, op := range txt.ToDelta() {
		if len(op.Attributes) > 0 {
			return map[string]any(op.Attributes)
		}
	}
	t.Fatal("no attributes in the document")
	return nil
}

func typeName(v any) string {
	if v == nil {
		return "nil"
	}
	return fmtType(v)
}

func fmtType(v any) string {
	switch v.(type) {
	case float64:
		return "float64"
	case float32:
		return "float32"
	case int64:
		return "int64"
	case int:
		return "int"
	case string:
		return "string"
	case bool:
		return "bool"
	case map[string]any:
		return "map[string]any"
	case []any:
		return "[]any"
	default:
		return "other"
	}
}

// TestV2_RelayPreservesYjsValues: a server relaying or merging V2 updates
// decodes a peer's rich text and encodes it again. Values JSON cannot carry
// must survive that: the first version of the fix routed every value through
// encoding/json, and a yjs embed {n: Infinity, keep: 1} came back out as null -
// the whole embed, not just n - while a Uint8Array came back as base64 text.
//
// The inputs are real Y.encodeStateAsUpdateV2 output from yjs 13.6.32: one
// client inserts "ab" and an embed between the two characters.
func TestV2_RelayPreservesYjsValues(t *testing.T) {
	yjsBytes := map[string]string{
		"binary":   "0000025f0202010001020504008400c506037461624101010100000103007601046461746174030102ff00",
		"plain":    "0000025f0202010001020504008400c50603746162410101010000010300760201617d01016277017800",
		"infinity": "0000025f0202010001020504008400c506037461624101010100000103007602016e7c7f800000046b6565707d0100",
		"nan":      "0000025f0202010001020504008400c506037461624101010100000103007602016e7b7ff8000000000000046b6565707d0100",
	}
	relay := func(t *testing.T, hexIn string) (*ygo.Doc, []byte) {
		t.Helper()
		in, err := hex.DecodeString(hexIn)
		if err != nil {
			t.Fatal(err)
		}
		d := ygo.NewDoc()
		if err := ygo.ApplyUpdateV2(d, in); err != nil {
			t.Fatalf("decoding yjs V2 rich text: %v", err)
		}
		return d, ygo.EncodeStateAsUpdateV2(d)
	}

	// Single-key and binary payloads come back byte for byte. (Multi-key
	// objects cannot: the Any encoder writes keys sorted, JS in insertion order.)
	for _, name := range []string{"binary", "plain"} {
		t.Run(name+" is byte-identical", func(t *testing.T) {
			_, out := relay(t, yjsBytes[name])
			if got := hex.EncodeToString(out); got != yjsBytes[name] {
				t.Fatalf("relayed bytes differ from yjs:\n got %s\nwant %s", got, yjsBytes[name])
			}
		})
	}

	// Non-finite numbers survive as values; key order changes the bytes.
	for name, check := range map[string]func(float64) bool{
		"infinity": func(f float64) bool { return math.IsInf(f, 1) },
		"nan":      math.IsNaN,
	} {
		t.Run(name+" survives the relay", func(t *testing.T) {
			_, out := relay(t, yjsBytes[name])
			back := ygo.NewDoc()
			if err := ygo.ApplyUpdateV2(back, out); err != nil {
				t.Fatal(err)
			}
			var embed map[string]any
			txt := ygo.NewText(back, "t")
			rt := back.ReadTxn()
			for _, op := range txt.ToDelta() {
				if m, ok := op.Embed.(map[string]any); ok {
					embed = m
				}
			}
			rt.Close()
			if embed == nil {
				t.Fatal("the embed did not survive the relay at all")
			}
			n, ok := embed["n"].(float64)
			if !ok || !check(n) {
				t.Fatalf("n = %T %v after the relay", embed["n"], embed["n"])
			}
			if embed["keep"] != float64(1) {
				t.Fatalf("keep = %v after the relay; the rest of the embed must survive too", embed["keep"])
			}
		})
	}
}

// TestV2_NumericArrayAttribute covers the slice half of the decode-side number
// mapping, which the other tests do not reach: a V2 array attribute must come
// back with the same element types as V1.
func TestV2_NumericArrayAttribute(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 23})
	txt := ygo.NewText(d, "t")
	txn := d.WriteTxn()
	if err := txt.InsertWithAttributes(txn, 0, "x", map[string]any{"cols": []any{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}
	txn.Commit()

	fromV2 := ygo.NewDoc()
	if err := ygo.ApplyUpdateV2(fromV2, ygo.EncodeStateAsUpdateV2(d)); err != nil {
		t.Fatal(err)
	}
	cols, ok := attrsOf(t, fromV2)["cols"].([]any)
	if !ok || len(cols) != 3 {
		t.Fatalf("cols = %#v", attrsOf(t, fromV2)["cols"])
	}
	for i, c := range cols {
		if typeName(c) != "float64" {
			t.Errorf("cols[%d] from V2 is %s, want float64 like V1", i, typeName(c))
		}
	}
}
