package types_test

import (
	"math"
	"reflect"
	"testing"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/types"
)

func TestText_InsertWithAttributes_OpenAndClose(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 700})
	tx := types.NewText(d.Branch("body"))

	wtxn := d.WriteTxn()
	if err := tx.InsertWithAttributes(wtxn, 0, "bold", types.Attrs{"weight": "bold"}); err != nil {
		t.Fatal(err)
	}
	wtxn.Commit()

	if got, want := tx.String(), "bold"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
	// Length counts the four characters, not the format markers.
	if got, want := tx.Length(), uint64(4); got != want {
		t.Errorf("Length = %d, want %d", got, want)
	}

	delta := tx.ToDelta()
	want := []types.DeltaOp{
		{Insert: "bold", Attributes: types.Attrs{"weight": "bold"}},
	}
	if !reflect.DeepEqual(delta, want) {
		t.Errorf("ToDelta = %+v, want %+v", delta, want)
	}
}

func TestText_InsertWithAttributes_InheritedThenPlain(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 701})
	tx := types.NewText(d.Branch("body"))

	wtxn := d.WriteTxn()
	// Insert plain text first.
	_ = tx.Insert(wtxn, 0, "Hello ")
	// Insert bold text at end.
	_ = tx.InsertWithAttributes(wtxn, 6, "world", types.Attrs{"bold": true})
	// Insert plain text at end again.
	_ = tx.Insert(wtxn, 11, "!")
	wtxn.Commit()

	if got, want := tx.String(), "Hello world!"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}

	delta := tx.ToDelta()
	want := []types.DeltaOp{
		{Insert: "Hello ", Attributes: nil},
		{Insert: "world", Attributes: types.Attrs{"bold": true}},
		{Insert: "!", Attributes: nil},
	}
	if !reflect.DeepEqual(delta, want) {
		t.Errorf("ToDelta = %+v, want %+v", delta, want)
	}
}

func TestText_Format_AppliesToExistingRange(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 702})
	tx := types.NewText(d.Branch("body"))

	wtxn := d.WriteTxn()
	_ = tx.Insert(wtxn, 0, "Hello, world!")
	wtxn.Commit()

	wtxn = d.WriteTxn()
	if err := tx.Format(wtxn, 7, 5, types.Attrs{"italic": true}); err != nil {
		t.Fatal(err)
	}
	wtxn.Commit()

	delta := tx.ToDelta()
	want := []types.DeltaOp{
		{Insert: "Hello, ", Attributes: nil},
		{Insert: "world", Attributes: types.Attrs{"italic": true}},
		{Insert: "!", Attributes: nil},
	}
	if !reflect.DeepEqual(delta, want) {
		t.Errorf("ToDelta = %+v, want %+v", delta, want)
	}
}

func TestText_Format_ClearAttribute(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 703})
	tx := types.NewText(d.Branch("body"))

	wtxn := d.WriteTxn()
	_ = tx.InsertWithAttributes(wtxn, 0, "bold text", types.Attrs{"bold": true})
	wtxn.Commit()

	// Strip bold from positions 0..4 ("bold").
	wtxn = d.WriteTxn()
	_ = tx.Format(wtxn, 0, 4, types.Attrs{"bold": nil})
	wtxn.Commit()

	delta := tx.ToDelta()
	want := []types.DeltaOp{
		{Insert: "bold", Attributes: nil},
		{Insert: " text", Attributes: types.Attrs{"bold": true}},
	}
	if !reflect.DeepEqual(delta, want) {
		t.Errorf("ToDelta = %+v, want %+v", delta, want)
	}
}

// A range that does not fit is an error, including ranges whose end
// overflows uint64: idx+length wrapped around to a small number and let
// such a call through.
func TestText_Format_RangePastTheEndIsAnError(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 704})
	tx := types.NewText(d.Branch("body"))

	wtxn := d.WriteTxn()
	_ = tx.Insert(wtxn, 0, "abc")
	wtxn.Commit()

	wtxn = d.WriteTxn()
	for _, r := range []struct{ idx, length uint64 }{
		{3, 1}, {2, 2}, {math.MaxUint64, 2}, {1, math.MaxUint64},
	} {
		if err := tx.Format(wtxn, r.idx, r.length, types.Attrs{"bold": true}); err == nil {
			t.Errorf("Format(%d, %d) on a 3-unit text returned no error", r.idx, r.length)
		}
	}
	wtxn.Commit()

	if got, want := tx.ToDelta(), []types.DeltaOp{{Insert: "abc"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("a rejected Format changed the text: %+v", got)
	}
}

func TestText_InsertEmbed_AppearsInDelta(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 704})
	tx := types.NewText(d.Branch("body"))

	wtxn := d.WriteTxn()
	_ = tx.Insert(wtxn, 0, "before after")
	wtxn.Commit()

	wtxn = d.WriteTxn()
	embedVal := "embed-payload"
	if err := tx.InsertEmbed(wtxn, 6, embedVal); err != nil {
		t.Fatal(err)
	}
	wtxn.Commit()

	delta := tx.ToDelta()
	if len(delta) != 3 {
		t.Fatalf("delta len = %d, want 3; got %+v", len(delta), delta)
	}
	if delta[0].Insert != "before" {
		t.Errorf("delta[0] = %+v, want Insert=before", delta[0])
	}
	if delta[1].Embed != embedVal {
		t.Errorf("delta[1] = %+v, want Embed=%v", delta[1], embedVal)
	}
	if delta[2].Insert != " after" {
		t.Errorf("delta[2] = %+v, want Insert=' after'", delta[2])
	}
}

func TestText_Range_VisitsAllChunks(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 705})
	tx := types.NewText(d.Branch("body"))

	wtxn := d.WriteTxn()
	_ = tx.Insert(wtxn, 0, "hi ")
	_ = tx.InsertWithAttributes(wtxn, 3, "bold", types.Attrs{"bold": true})
	_ = tx.Insert(wtxn, 7, " bye")
	wtxn.Commit()

	type chunk struct {
		v     any
		attrs types.Attrs
	}
	var seen []chunk
	tx.Range(func(_ types.ChunkKind, v any, a types.Attrs) bool {
		seen = append(seen, chunk{v: v, attrs: a})
		return true
	})

	if len(seen) != 3 {
		t.Fatalf("Range visited %d chunks, want 3", len(seen))
	}
	if seen[0].v != "hi " {
		t.Errorf("chunk[0] = %v, want 'hi '", seen[0].v)
	}
	if seen[1].v != "bold" {
		t.Errorf("chunk[1] = %v, want 'bold'", seen[1].v)
	}
	if seen[1].attrs["bold"] != true {
		t.Errorf("chunk[1] attrs = %v, want bold:true", seen[1].attrs)
	}
	if seen[2].v != " bye" {
		t.Errorf("chunk[2] = %v, want ' bye'", seen[2].v)
	}
}

func TestText_WireRoundTrip_PreservesFormatting(t *testing.T) {
	src := doc.NewDocWithOptions(doc.Options{ClientID: 800})
	tx := types.NewText(src.Branch("body"))
	wtxn := src.WriteTxn()
	_ = tx.Insert(wtxn, 0, "Hello ")
	_ = tx.InsertWithAttributes(wtxn, 6, "world", types.Attrs{"bold": true})
	_ = tx.Insert(wtxn, 11, "!")
	wtxn.Commit()

	wantDelta := tx.ToDelta()

	update := encoding.EncodeStateAsUpdate(src)

	target := doc.NewDoc()
	if err := encoding.ApplyUpdate(target, update); err != nil {
		t.Fatal(err)
	}
	tt := types.NewText(target.Branch("body"))
	if got := tt.String(); got != "Hello world!" {
		t.Errorf("String = %q, want Hello world!", got)
	}
	if got := tt.ToDelta(); !reflect.DeepEqual(got, wantDelta) {
		t.Errorf("Delta mismatch:\n got  %+v\n want %+v", got, wantDelta)
	}
}

func TestText_CrossClient_FormattingConverges(t *testing.T) {
	a := doc.NewDocWithOptions(doc.Options{ClientID: 900})
	at := types.NewText(a.Branch("body"))
	wtxn := a.WriteTxn()
	_ = at.Insert(wtxn, 0, "Hello world")
	wtxn.Commit()

	b := doc.NewDocWithOptions(doc.Options{ClientID: 901})
	if err := encoding.ApplyUpdate(b, encoding.EncodeStateAsUpdate(a)); err != nil {
		t.Fatal(err)
	}
	bt := types.NewText(b.Branch("body"))
	wtxn = b.WriteTxn()
	if err := bt.Format(wtxn, 6, 5, types.Attrs{"bold": true}); err != nil {
		t.Fatal(err)
	}
	wtxn.Commit()

	// A receives B's update.
	if err := encoding.ApplyUpdate(a, encoding.EncodeStateAsUpdate(b)); err != nil {
		t.Fatal(err)
	}

	wantDelta := []types.DeltaOp{
		{Insert: "Hello ", Attributes: nil},
		{Insert: "world", Attributes: types.Attrs{"bold": true}},
	}
	for label, tx := range map[string]*types.Text{"A": at, "B": bt} {
		if got := tx.ToDelta(); !reflect.DeepEqual(got, wantDelta) {
			t.Errorf("%s delta = %+v, want %+v", label, got, wantDelta)
		}
	}
}

func TestText_LengthExcludesFormatMarkers(t *testing.T) {
	// Format markers must not contribute to Text.Length — they have
	// IsCountable=false. The marker exists in branch.Start but
	// branch.BlockLen excludes it.
	d := doc.NewDocWithOptions(doc.Options{ClientID: 1000})
	tx := types.NewText(d.Branch("body"))
	wtxn := d.WriteTxn()
	_ = tx.InsertWithAttributes(wtxn, 0, "abc", types.Attrs{"bold": true})
	wtxn.Commit()

	if got, want := tx.Length(), uint64(3); got != want {
		t.Errorf("Length = %d, want 3 (excludes format markers)", got)
	}
}

func TestText_EmptyAttrs_FallsThroughToInsert(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 1001})
	tx := types.NewText(d.Branch("body"))
	wtxn := d.WriteTxn()
	if err := tx.InsertWithAttributes(wtxn, 0, "plain", nil); err != nil {
		t.Fatal(err)
	}
	wtxn.Commit()

	delta := tx.ToDelta()
	want := []types.DeltaOp{{Insert: "plain"}}
	if !reflect.DeepEqual(delta, want) {
		t.Errorf("ToDelta = %+v, want %+v", delta, want)
	}
}
