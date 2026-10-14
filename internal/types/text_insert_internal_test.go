package types

import (
	"reflect"
	"testing"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
)

// A search marker can point at the first content item after a run of
// format markers, so a lookup through it returns a position past those
// markers. yjs uses no search markers once a text has formatting, so Insert
// on such a text must not take its position from one: here a marker on
// "def" at index 3 sits right after the marker that ends the bold run, and
// yjs puts X before that marker, bold. Expected values are yjs 13.6.33's
// for insert(0, 'abc', {bold: true}); insert(3, 'def', {}); insert(3, 'X')
// on client 7: X has origin 7:3 and right origin 7:4 (the closing marker).
func TestText_InsertIgnoresMarkerPastFormatting(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 7})
	txt := NewText(d.Branch("t"))
	w := d.WriteTxn()
	if err := txt.InsertWithAttributes(w, 0, "abc", Attrs{"bold": true}); err != nil {
		t.Fatal(err)
	}
	if err := txt.InsertWithAttributes(w, 3, "def", Attrs{}); err != nil {
		t.Fatal(err)
	}
	w.Commit()

	var def *block.Item
	for it := txt.branch.Start; it != nil; it = it.Right {
		if it.Content.Kind == block.KindString && it.Content.Str == "def" {
			def = it
		}
	}
	if def == nil || def.Left == nil || def.Left.Content.Kind != block.KindFormat {
		t.Fatalf("setup: want \"def\" right after a format marker")
	}
	txt.branch.Markers.Invalidate()
	rememberMarker(txt.branch, def, 3)

	w = d.WriteTxn()
	if err := txt.Insert(w, 3, "X"); err != nil {
		t.Fatal(err)
	}
	w.Commit()

	want := []DeltaOp{{Insert: "abcX", Attributes: Attrs{"bold": true}}, {Insert: "def"}}
	if got := txt.ToDelta(); !reflect.DeepEqual(got, want) {
		t.Errorf("ToDelta = %+v, want %+v", got, want)
	}
	var x *block.Item
	for it := txt.branch.Start; it != nil; it = it.Right {
		if it.Content.Kind == block.KindString && it.Content.Str == "X" {
			x = it
		}
	}
	if x == nil || x.Origin == nil || x.RightOrigin == nil {
		t.Fatalf("X item missing or without origins: %+v", x)
	}
	if *x.Origin != (block.ID{Client: 7, Clock: 3}) || *x.RightOrigin != (block.ID{Client: 7, Clock: 4}) {
		t.Errorf("X origin %v right origin %v, want 7:3 and 7:4", *x.Origin, *x.RightOrigin)
	}
}
