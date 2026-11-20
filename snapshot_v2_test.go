package ygo_test

import (
	"math"
	"reflect"
	"testing"

	"github.com/Deln0r/ygo"
)

// TestRestoreSnapshot_ManyStructsInFewBytes: a snapshot is restored through
// V2, and its bytes, which ygo wrote itself, are decoded without the bound
// the wire decoder puts on struct counts. 500 empty nested arrays are 500
// structs in fewer bytes; through the wire bound the restore fell back to
// V1 and an undefined format value came back as null.
func TestRestoreSnapshot_ManyStructsInFewBytes(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1, DisableGC: true})
	arr := ygo.NewArray(d, "a")
	txt := ygo.NewText(d, "t")
	w := d.WriteTxn()
	for i := uint64(0); i < 500; i++ {
		arr.InsertArray(w, i)
	}
	if err := txt.InsertWithAttributes(w, 0, "x", ygo.Attrs{"b": ygo.Undefined{}}); err != nil {
		t.Fatal(err)
	}
	w.Commit()
	r, err := ygo.RestoreSnapshot(d, ygo.CreateSnapshot(d))
	if err != nil {
		t.Fatal(err)
	}
	if n := ygo.NewArray(r, "a").Len(); n != 500 {
		t.Fatalf("restored %d arrays, want 500", n)
	}
	delta := ygo.NewText(r, "t").ToDelta()
	if len(delta) != 1 || delta[0].Attributes["b"] != (ygo.Undefined{}) {
		t.Fatalf("restored delta %#v, want x with b undefined", delta)
	}
}

// TestRestoreSnapshot_KeepsWhatV2Carries: since the restore goes through V2,
// as yjs createDocFromSnapshot does, an embed keeps what V1's JSON could not
// carry: a byte array, a BigInt, NaN, -0 and an undefined inside an object.
func TestRestoreSnapshot_KeepsWhatV2Carries(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1, DisableGC: true})
	txt := ygo.NewText(d, "t")
	w := d.WriteTxn()
	embeds := []any{
		[]byte{1, 2},
		map[string]any{"n": math.NaN(), "z": math.Copysign(0, -1), "u": ygo.Undefined{}, "big": ygo.BigInt(1 << 60)},
	}
	for i, e := range embeds {
		if err := txt.InsertEmbed(w, uint64(i), e); err != nil {
			t.Fatal(err)
		}
	}
	w.Commit()
	r, err := ygo.RestoreSnapshot(d, ygo.CreateSnapshot(d))
	if err != nil {
		t.Fatal(err)
	}
	got := ygo.NewText(r, "t").ToDelta()
	if len(got) != 2 {
		t.Fatalf("restored delta %#v", got)
	}
	if b, ok := got[0].Embed.([]byte); !ok || !reflect.DeepEqual(b, []byte{1, 2}) {
		t.Errorf("byte array embed restored as %#v", got[0].Embed)
	}
	m, _ := got[1].Embed.(map[string]any)
	if n, ok := m["n"].(float64); !ok || !math.IsNaN(n) {
		t.Errorf("NaN restored as %#v", m["n"])
	}
	if z, ok := m["z"].(float64); !ok || z != 0 || !math.Signbit(z) {
		t.Errorf("-0 restored as %#v", m["z"])
	}
	if m["u"] != (ygo.Undefined{}) {
		t.Errorf("nested undefined restored as %#v", m["u"])
	}
	if m["big"] != ygo.BigInt(1<<60) {
		t.Errorf("BigInt restored as %#v", m["big"])
	}
}
