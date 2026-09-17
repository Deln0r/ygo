package ygo_test

import (
	"bytes"
	"slices"
	"testing"

	"github.com/Deln0r/ygo"
)

// Every attribute of a formatting edit becomes its own format marker, and the
// order of those markers is part of the encoded update. They used to be
// emitted in Go map order, which is randomised, so the same edit produced a
// different update on each run: the documents were equivalent, but anything
// comparing or hashing update bytes saw a change, and the cross-language
// fixture check (regenerate the Go-encoded fixtures, diff them) failed on the
// v1.20.0 release commit after passing on the commit before it.

func manyAttrs() ygo.Attrs {
	return ygo.Attrs{
		"header": 1, "align": "center", "bold": true, "italic": true,
		"color": "#f00", "size": 12, "font": "serif", "link": "https://example.com",
	}
}

func TestFormatMarkers_SameEditEncodesToSameBytes(t *testing.T) {
	edits := []struct {
		name string
		edit func(*ygo.Text, *ygo.TransactionMut) error
	}{
		{"InsertWithAttributes", func(txt *ygo.Text, txn *ygo.TransactionMut) error {
			return txt.InsertWithAttributes(txn, 0, "Title\n", manyAttrs())
		}},
		{"Format", func(txt *ygo.Text, txn *ygo.TransactionMut) error {
			if err := txt.Insert(txn, 0, "hello world"); err != nil {
				return err
			}
			return txt.Format(txn, 6, 5, manyAttrs())
		}},
		{"ApplyDelta", func(txt *ygo.Text, txn *ygo.TransactionMut) error {
			if err := txt.ApplyDelta(txn, []ygo.DeltaOp{
				{Insert: "hello ", Attributes: manyAttrs()},
				{Insert: "world"},
			}); err != nil {
				return err
			}
			return txt.ApplyDelta(txn, []ygo.DeltaOp{
				{Retain: 6},
				{Retain: 5, Attributes: manyAttrs()},
			})
		}},
	}
	for _, e := range edits {
		t.Run(e.name, func(t *testing.T) {
			encode := func() (v1, v2 []byte) {
				d := ygo.NewDocWithOptions(ygo.Options{ClientID: 7})
				txt := ygo.NewText(d, "x")
				txn := d.WriteTxn()
				if err := e.edit(txt, txn); err != nil {
					t.Fatal(err)
				}
				txn.Commit()
				return ygo.EncodeStateAsUpdate(d), ygo.EncodeStateAsUpdateV2(d)
			}
			wantV1, wantV2 := encode()
			for run := 1; run < 64; run++ {
				v1, v2 := encode()
				if !bytes.Equal(v1, wantV1) {
					t.Fatalf("run %d: V1 update differs from run 0\n got %x\nwant %x", run, v1, wantV1)
				}
				if !bytes.Equal(v2, wantV2) {
					t.Fatalf("run %d: V2 update differs from run 0\n got %x\nwant %x", run, v2, wantV2)
				}
			}
		})
	}
}

// The order is ascending by key, for the opening markers and for the closing
// ones, at both ends of a formatted range. Pinned so a later change cannot swap
// one fixed order for another without a test noticing; the Go-encoded fixtures
// pin the bytes as well. In V1 each marker writes its key as a plain string and
// a client's items are written in clock order, so the keys appear in the update
// in the order the markers were created.
func TestFormatMarkers_EmittedInKeyOrder(t *testing.T) {
	attrs := func() ygo.Attrs { return ygo.Attrs{"header": 1, "align": "center"} }
	edits := []struct {
		name string
		edit func(*ygo.Text, *ygo.TransactionMut) error
	}{
		{"InsertWithAttributes", func(txt *ygo.Text, txn *ygo.TransactionMut) error {
			return txt.InsertWithAttributes(txn, 0, "Title\n", attrs())
		}},
		{"Format", func(txt *ygo.Text, txn *ygo.TransactionMut) error {
			if err := txt.Insert(txn, 0, "hello world"); err != nil {
				return err
			}
			return txt.Format(txn, 6, 5, attrs())
		}},
	}
	want := []string{"align", "header", "align", "header"}
	for _, e := range edits {
		t.Run(e.name, func(t *testing.T) {
			d := ygo.NewDocWithOptions(ygo.Options{ClientID: 7})
			txt := ygo.NewText(d, "x")
			txn := d.WriteTxn()
			if err := e.edit(txt, txn); err != nil {
				t.Fatal(err)
			}
			txn.Commit()

			u := ygo.EncodeStateAsUpdate(d)
			var got []string
			for i := range u {
				for _, k := range []string{"align", "header"} {
					if bytes.HasPrefix(u[i:], []byte(k)) {
						got = append(got, k)
					}
				}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("marker keys in update order = %v, want %v (opening then closing, ascending)\nupdate %x", got, want, u)
			}
		})
	}
}
