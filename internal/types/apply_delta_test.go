package types_test

import (
	"reflect"
	"testing"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/types"
)

func TestApplyDelta_PlainInsert(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 1100})
	tx := types.NewText(d.Branch("body"))

	w := d.WriteTxn()
	if err := tx.ApplyDelta(w, []types.DeltaOp{
		{Insert: "Hello world"},
	}); err != nil {
		t.Fatal(err)
	}
	w.Commit()

	if got, want := tx.String(), "Hello world"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
}

func TestApplyDelta_InsertWithAttributes(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 1101})
	tx := types.NewText(d.Branch("body"))

	w := d.WriteTxn()
	if err := tx.ApplyDelta(w, []types.DeltaOp{
		{Insert: "Hello ", Attributes: nil},
		{Insert: "world", Attributes: types.Attrs{"bold": true}},
		{Insert: "!", Attributes: nil},
	}); err != nil {
		t.Fatal(err)
	}
	w.Commit()

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

func TestApplyDelta_RetainWithFormat(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 1102})
	tx := types.NewText(d.Branch("body"))

	w := d.WriteTxn()
	_ = tx.Insert(w, 0, "Hello world")
	w.Commit()

	// Apply: retain(7) skip "Hello, " → retain(5) format italic on "world"
	w = d.WriteTxn()
	if err := tx.ApplyDelta(w, []types.DeltaOp{
		{Retain: 6},
		{Retain: 5, Attributes: types.Attrs{"italic": true}},
	}); err != nil {
		t.Fatal(err)
	}
	w.Commit()

	delta := tx.ToDelta()
	want := []types.DeltaOp{
		{Insert: "Hello ", Attributes: nil},
		{Insert: "world", Attributes: types.Attrs{"italic": true}},
	}
	if !reflect.DeepEqual(delta, want) {
		t.Errorf("ToDelta = %+v, want %+v", delta, want)
	}
}

func TestApplyDelta_Delete(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 1103})
	tx := types.NewText(d.Branch("body"))

	w := d.WriteTxn()
	_ = tx.Insert(w, 0, "Hello world")
	w.Commit()

	// Retain 5 ("Hello"), delete 6 (" world").
	w = d.WriteTxn()
	if err := tx.ApplyDelta(w, []types.DeltaOp{
		{Retain: 5},
		{Delete: 6},
	}); err != nil {
		t.Fatal(err)
	}
	w.Commit()

	if got, want := tx.String(), "Hello"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
}

func TestApplyDelta_MixedOps(t *testing.T) {
	// Compound transformation: take "alpha beta", apply bold to
	// "alpha", insert " (note)" before "beta", delete "beta".
	d := doc.NewDocWithOptions(doc.Options{ClientID: 1104})
	tx := types.NewText(d.Branch("body"))

	w := d.WriteTxn()
	_ = tx.Insert(w, 0, "alpha beta")
	w.Commit()

	w = d.WriteTxn()
	if err := tx.ApplyDelta(w, []types.DeltaOp{
		{Retain: 5, Attributes: types.Attrs{"bold": true}}, // alpha → bold
		{Retain: 1},        // skip space
		{Insert: "(note)"}, // before "beta"
		{Delete: 4},        // remove "beta"
	}); err != nil {
		t.Fatal(err)
	}
	w.Commit()

	if got, want := tx.String(), "alpha (note)"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}

	// Verify formatting on "alpha".
	delta := tx.ToDelta()
	if len(delta) < 2 || delta[0].Insert != "alpha" || delta[0].Attributes["bold"] != true {
		t.Errorf("delta[0] = %+v, want Insert=alpha bold=true", delta[0])
	}
}

func TestApplyDelta_Embed(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 1105})
	tx := types.NewText(d.Branch("body"))

	w := d.WriteTxn()
	if err := tx.ApplyDelta(w, []types.DeltaOp{
		{Insert: "before"},
		{Embed: map[string]any{"image": "embed-val"}},
		{Insert: "after"},
	}); err != nil {
		t.Fatal(err)
	}
	w.Commit()

	delta := tx.ToDelta()
	if len(delta) != 3 {
		t.Fatalf("delta len = %d, want 3; got %+v", len(delta), delta)
	}
	if delta[0].Insert != "before" {
		t.Errorf("delta[0] = %+v", delta[0])
	}
	if !reflect.DeepEqual(delta[1].Embed, map[string]any{"image": "embed-val"}) {
		t.Errorf("delta[1] = %+v, want Embed=embed-val", delta[1])
	}
	if delta[2].Insert != "after" {
		t.Errorf("delta[2] = %+v", delta[2])
	}
}

func TestApplyDelta_ToDeltaRoundTrip(t *testing.T) {
	// Building a doc via ToDelta then ApplyDelta on a fresh doc
	// must reproduce the same delta.
	src := doc.NewDocWithOptions(doc.Options{ClientID: 1106})
	stx := types.NewText(src.Branch("body"))
	w := src.WriteTxn()
	_ = stx.Insert(w, 0, "Hello ")
	_ = stx.InsertWithAttributes(w, 6, "world", types.Attrs{"bold": true})
	_ = stx.Insert(w, 11, "!")
	w.Commit()

	srcDelta := stx.ToDelta()

	target := doc.NewDocWithOptions(doc.Options{ClientID: 1107})
	tt := types.NewText(target.Branch("body"))
	w = target.WriteTxn()
	if err := tt.ApplyDelta(w, srcDelta); err != nil {
		t.Fatal(err)
	}
	w.Commit()

	gotDelta := tt.ToDelta()
	if !reflect.DeepEqual(gotDelta, srcDelta) {
		t.Errorf("round-trip delta:\n got  %+v\n want %+v", gotDelta, srcDelta)
	}
	if got, want := tt.String(), "Hello world!"; got != want {
		t.Errorf("target String = %q, want %q", got, want)
	}
}

func TestApplyDelta_EmptyOps_NoOp(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 1108})
	tx := types.NewText(d.Branch("body"))

	w := d.WriteTxn()
	_ = tx.Insert(w, 0, "stable")
	w.Commit()

	w = d.WriteTxn()
	if err := tx.ApplyDelta(w, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.ApplyDelta(w, []types.DeltaOp{}); err != nil {
		t.Fatal(err)
	}
	w.Commit()

	if got, want := tx.String(), "stable"; got != want {
		t.Errorf("String = %q, want %q", got, want)
	}
}

func TestApplyDelta_CrossClient_Converges(t *testing.T) {
	// A creates baseline (single contiguous Item), B applies a
	// compound delta that PARTIALLY deletes A's item plus inserts
	// new content, A receives B's update — both converge.
	//
	// This test specifically stresses the DeleteSet split-on-
	// boundary path: B's wire update carries a delete-set range
	// covering clock 6..9 of A's client, but A's local view has
	// a single 10-char Item at clock 0..9. The apply must split
	// at the boundary, not tombstone the whole item.
	a := doc.NewDocWithOptions(doc.Options{ClientID: 1109})
	at := types.NewText(a.Branch("body"))
	w := a.WriteTxn()
	_ = at.Insert(w, 0, "alpha beta")
	w.Commit()

	b := doc.NewDocWithOptions(doc.Options{ClientID: 1110})
	if err := encoding.ApplyUpdate(b, encoding.EncodeStateAsUpdate(a)); err != nil {
		t.Fatal(err)
	}
	bt := types.NewText(b.Branch("body"))
	w = b.WriteTxn()
	if err := bt.ApplyDelta(w, []types.DeltaOp{
		{Retain: 5, Attributes: types.Attrs{"bold": true}}, // format "alpha"
		{Retain: 1}, // skip " "
		{Delete: 4}, // delete "beta" (partial — A has 10-char item)
		{Insert: "gamma", Attributes: types.Attrs{"italic": true}},
	}); err != nil {
		t.Fatal(err)
	}
	w.Commit()

	if err := encoding.ApplyUpdate(a, encoding.EncodeStateAsUpdate(b)); err != nil {
		t.Fatal(err)
	}

	wantString := "alpha gamma"
	if got := at.String(); got != wantString {
		t.Errorf("A String = %q, want %q (partial-delete split-on-boundary regression)", got, wantString)
	}
	if got := bt.String(); got != wantString {
		t.Errorf("B String = %q, want %q", got, wantString)
	}

	// Both should agree on formatting after sync.
	if !reflect.DeepEqual(at.ToDelta(), bt.ToDelta()) {
		t.Errorf("deltas diverged:\n A=%+v\n B=%+v", at.ToDelta(), bt.ToDelta())
	}
}

func TestApplyDelta_RetainNoAttrs_PreservesFormatting(t *testing.T) {
	// Retain without Attributes must NOT touch existing formatting.
	d := doc.NewDocWithOptions(doc.Options{ClientID: 1111})
	tx := types.NewText(d.Branch("body"))

	w := d.WriteTxn()
	_ = tx.InsertWithAttributes(w, 0, "bold", types.Attrs{"bold": true})
	_ = tx.InsertWithAttributes(w, 4, " plain", types.Attrs{})
	w.Commit()

	w = d.WriteTxn()
	// Just retain all 10 chars without attributes — formatting unchanged.
	if err := tx.ApplyDelta(w, []types.DeltaOp{
		{Retain: 10},
	}); err != nil {
		t.Fatal(err)
	}
	w.Commit()

	delta := tx.ToDelta()
	want := []types.DeltaOp{
		{Insert: "bold", Attributes: types.Attrs{"bold": true}},
		{Insert: " plain", Attributes: nil},
	}
	if !reflect.DeepEqual(delta, want) {
		t.Errorf("delta after no-attr retain = %+v, want %+v", delta, want)
	}
}

// A retain past the end pads with newlines, as yjs does, but only as many
// as fit an int: a longer one fails before the op writes anything, where
// it used to reach strings.Repeat with a negative count.
func TestApplyDelta_RetainPaddingBounded(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 1120})
	tx := types.NewText(d.Branch("body"))
	w := d.WriteTxn()
	defer w.Commit()
	for _, op := range []types.DeltaOp{
		{Retain: 1 << 63},
		{Retain: 1 << 32, Attributes: types.Attrs{"bold": true}},
	} {
		if err := tx.ApplyDelta(w, []types.DeltaOp{op}); err == nil {
			t.Errorf("ApplyDelta(%+v) = nil, want an error", op)
		}
	}
	if tx.Length() != 0 || tx.Branch().Start != nil {
		t.Errorf("text changed: length %d, start %v", tx.Length(), tx.Branch().Start)
	}
	if err := tx.ApplyDelta(w, []types.DeltaOp{{Insert: "ab"}, {Retain: 4, Attributes: types.Attrs{"bold": true}}}); err != nil {
		t.Fatal(err)
	}
	// The retain starts where the insert left the cursor, at the end, so
	// all 4 units are missing (rich-text fixture delta-retain-past-end).
	if got := tx.String(); got != "ab\n\n\n\n" {
		t.Errorf("String = %q, want %q", got, "ab\n\n\n\n")
	}
}
