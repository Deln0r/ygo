package ygo_test

import (
	"bytes"
	"encoding/hex"
	"math"
	"testing"

	"github.com/Deln0r/ygo"
)

// TestFormat_BigIntEquality: formatting a range with an attribute equal to
// the one it carries writes nothing, as yjs's formatText skips it, and the
// comparison is JS ===. Probed against yjs 13.6.33: 5n over 5n and -0 over
// 0 write nothing; 5 over 5n and 2^62+1 over 2^62 write a new pair of
// markers. Before v1.33.0 ygo compared a BigInt as a float64 number.
func TestFormat_BigIntEquality(t *testing.T) {
	cases := []struct {
		name          string
		first, second any
		writes        bool
	}{
		{"BigInt over the same BigInt", ygo.BigInt(5), ygo.BigInt(5), false},
		{"number over a BigInt", ygo.BigInt(5), int64(5), true},
		{"BigInt over a number", int64(5), ygo.BigInt(5), true},
		{"2^62+1 over 2^62", ygo.BigInt(1 << 62), ygo.BigInt(1<<62 + 1), true},
		{"0 over -0", math.Copysign(0, -1), float64(0), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
			x := ygo.NewText(d, "t")
			txn := d.WriteTxn()
			if err := x.InsertWithAttributes(txn, 0, "ab", ygo.Attrs{"x": c.first}); err != nil {
				t.Fatal(err)
			}
			txn.Commit()
			before := ygo.EncodeStateVector(d)
			txn = d.WriteTxn()
			if err := x.Format(txn, 0, 2, ygo.Attrs{"x": c.second}); err != nil {
				t.Fatal(err)
			}
			txn.Commit()
			wrote := !bytes.Equal(before, ygo.EncodeStateVector(d))
			if wrote != c.writes {
				t.Fatalf("format wrote markers: %v, want %v (yjs)", wrote, c.writes)
			}
		})
	}
}

// TestFormat_UndefinedValueReadsAsNil pins the one place undefined is not
// kept: a format attribute whose value is undefined itself. yjs keeps it
// (toDelta gives {x: undefined}) and treats it as null at most lookups but
// not all; ygo's text code does not model the difference, so it reads the
// marker as null, x is absent, and the marker is written back as null (7e).
// Bytes: yjs 13.6.33, insert "ab", format(0, 2, {x: undefined}).
func TestFormat_UndefinedValueReadsAsNil(t *testing.T) {
	b, err := hex.DecodeString("00020002024101010201000504004600860a05746162787801024100010100000103007f7e00")
	if err != nil {
		t.Fatal(err)
	}
	d := ygo.NewDoc()
	if err := ygo.ApplyUpdateV2(d, b); err != nil {
		t.Fatal(err)
	}
	delta := ygo.NewText(d, "t").ToDelta()
	if len(delta) != 1 || delta[0].Insert != "ab" {
		t.Fatalf("delta %#v", delta)
	}
	if v, ok := delta[0].Attributes["x"]; ok {
		t.Fatalf("attribute x = %#v, want it absent, read as a null marker", v)
	}
}

// TestFormat_UndefinedFromGoIsNil: a Go caller's top-level ygo.Undefined{}
// as a format value or an embed is stored as nil, what every reader of the
// update gets, so the writer's document is not the only one holding it.
func TestFormat_UndefinedFromGoIsNil(t *testing.T) {
	d := ygo.NewDoc()
	x := ygo.NewText(d, "t")
	txn := d.WriteTxn()
	if err := x.InsertWithAttributes(txn, 0, "ab", ygo.Attrs{"x": ygo.Undefined{}, "y": map[string]any{"a": ygo.Undefined{}}}); err != nil {
		t.Fatal(err)
	}
	if err := x.InsertEmbed(txn, 2, ygo.Undefined{}); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	readers := map[string]*ygo.Doc{"writer": d}
	for name, enc := range map[string]func(*ygo.Doc) []byte{"V1 reader": ygo.EncodeStateAsUpdate, "V2 reader": ygo.EncodeStateAsUpdateV2} {
		r := ygo.NewDoc()
		apply := ygo.ApplyUpdate
		if name == "V2 reader" {
			apply = ygo.ApplyUpdateV2
		}
		if err := apply(r, enc(d)); err != nil {
			t.Fatal(err)
		}
		readers[name] = r
	}
	for name, r := range readers {
		delta := ygo.NewText(r, "t").ToDelta()
		if len(delta) != 2 {
			t.Fatalf("%s: delta %#v", name, delta)
		}
		if _, ok := delta[0].Attributes["x"]; ok {
			t.Errorf("%s: attribute x present: %#v", name, delta[0].Attributes["x"])
		}
		if delta[1].Embed != nil {
			t.Errorf("%s: embed = %#v, want nil", name, delta[1].Embed)
		}
	}
	// Nested undefined is data: the writer and a V2 reader both keep it.
	for _, name := range []string{"writer", "V2 reader"} {
		y, _ := ygo.NewText(readers[name], "t").ToDelta()[0].Attributes["y"].(map[string]any)
		if _, ok := y["a"].(ygo.Undefined); !ok {
			t.Errorf("%s: nested undefined in attribute y = %#v, want kept", name, y)
		}
	}
}
