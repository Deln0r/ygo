package ygo_test

import (
	"bytes"
	"testing"

	"github.com/Deln0r/ygo"
)

// A whole-number float64 a Go caller stores as a Map or Array value is
// written as a float64 (tag 123), not as the varint lib0 writes for the
// same JavaScript number, so that a Go peer reads a float64 back, through
// V1 and V2: as a varint it would come back as an int64. A JavaScript peer
// reads the number 3 either way. An embed holding the same number follows
// lib0, and a Go peer still reads it as a float64. Kept on purpose
// (README, Strings and values).
func TestWholeFloat64_GoPeersReadAFloat64(t *testing.T) {
	src := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	m, a, x := ygo.NewMap(src, "m"), ygo.NewArray(src, "a"), ygo.NewText(src, "t")
	txn := src.WriteTxn()
	m.Set(txn, "k", float64(3))
	a.Insert(txn, 0, float64(3))
	if err := x.InsertEmbed(txn, 0, map[string]any{"n": float64(3)}); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	v1, v2 := ygo.EncodeStateAsUpdate(src), ygo.EncodeStateAsUpdateV2(src)
	if !bytes.Contains(v1, []byte{0x7b, 0x40, 0x08, 0, 0, 0, 0, 0, 0}) {
		t.Fatalf("V1 does not hold 3 as a float64 (7b4008...): %x", v1)
	}
	for name, apply := range map[string]func(*ygo.Doc) error{
		"V1": func(d *ygo.Doc) error { return ygo.ApplyUpdate(d, v1) },
		"V2": func(d *ygo.Doc) error { return ygo.ApplyUpdateV2(d, v2) },
	} {
		d := ygo.NewDoc()
		if err := apply(d); err != nil {
			t.Fatal(err)
		}
		dm, da, dt := ygo.NewMap(d, "m"), ygo.NewArray(d, "a"), ygo.NewText(d, "t")
		rtxn := d.ReadTxn()
		got := map[string]any{
			"map value":   dm.Get("k"),
			"array value": da.Get(0),
		}
		rtxn.Close()
		got["embed"] = dt.ToDelta()[0].Embed.(map[string]any)["n"]
		for what, v := range got {
			if f, ok := v.(float64); !ok || f != 3 {
				t.Errorf("%s: %s %v (%T), want float64 3", name, what, v, v)
			}
		}
	}
}
