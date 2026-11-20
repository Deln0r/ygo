package ygo_test

import (
	"bytes"
	"encoding/hex"
	"errors"
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

// TestFormat_UndefinedValueKept: a format attribute whose value is
// undefined itself is a marker of its own, as in yjs: toDelta gives
// {x: Undefined{}}, and the state is written back byte for byte (tag 7f).
// Bytes: yjs 13.6.33, insert "ab", format(0, 2, {x: undefined}).
func TestFormat_UndefinedValueKept(t *testing.T) {
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
	if v, ok := delta[0].Attributes["x"]; !ok || v != (ygo.Undefined{}) {
		t.Fatalf("attribute x = %#v (present %v), want Undefined{}", v, ok)
	}
	if got := ygo.EncodeStateAsUpdateV2(d); !bytes.Equal(got, b) {
		t.Fatalf("state %x, want yjs's %x", got, b)
	}
}

// TestFormat_UndefinedFromGo: a Go caller's top-level ygo.Undefined{} as a
// format value is kept by the writer and by a V2 reader; V1, which is JSON,
// cannot carry it (yjs cannot write the document in V1 at all) and a V1
// reader gets null. Nested undefined is data and stays in V2. An undefined
// embed is refused before anything changes.
func TestFormat_UndefinedFromGo(t *testing.T) {
	d := ygo.NewDoc()
	x := ygo.NewText(d, "t")
	txn := d.WriteTxn()
	if err := x.InsertWithAttributes(txn, 0, "ab", ygo.Attrs{"x": ygo.Undefined{}, "y": map[string]any{"a": ygo.Undefined{}}}); err != nil {
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
		if len(delta) != 1 {
			t.Fatalf("%s: delta %#v", name, delta)
		}
		v, ok := delta[0].Attributes["x"]
		if name == "V1 reader" {
			if ok {
				t.Errorf("V1 reader: attribute x = %#v, want it read as a null marker", v)
			}
			continue
		}
		if v != (ygo.Undefined{}) {
			t.Errorf("%s: attribute x = %#v (present %v), want Undefined{}", name, v, ok)
		}
		y, _ := delta[0].Attributes["y"].(map[string]any)
		if _, ok := y["a"].(ygo.Undefined); !ok {
			t.Errorf("%s: nested undefined in attribute y = %#v, want kept", name, y)
		}
	}

	before := ygo.EncodeStateAsUpdateV2(d)
	txn = d.WriteTxn()
	err := x.InsertEmbed(txn, 1, ygo.Undefined{})
	err2 := x.InsertEmbedWithAttributes(txn, 1, ygo.Undefined{}, ygo.Attrs{"bold": true})
	txn.Commit()
	if !errors.Is(err, ygo.ErrUndefinedEmbed) || !errors.Is(err2, ygo.ErrUndefinedEmbed) {
		t.Fatalf("InsertEmbed(Undefined{}) = %v, %v; want ErrUndefinedEmbed", err, err2)
	}
	if after := ygo.EncodeStateAsUpdateV2(d); !bytes.Equal(before, after) {
		t.Fatal("a refused undefined embed changed the document")
	}
}
