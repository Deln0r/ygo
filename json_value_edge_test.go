package ygo_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Deln0r/ygo"
)

// A Go value nested deeper than the JSON reader takes (10000 levels) is
// stored as nil, as both formats wrote it before, not as text a V1
// receiver, ygo included, would refuse.
func TestJSONValue_TooDeepIsStoredAsNil(t *testing.T) {
	var v any = "<&>"
	for range 10001 {
		v = []any{v}
	}
	d := ygo.NewDoc()
	x := ygo.NewText(d, "t")
	txn := d.WriteTxn()
	if err := x.InsertEmbed(txn, 0, v); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	for name, apply := range map[string]func(*ygo.Doc) error{
		"V1": func(r *ygo.Doc) error { return ygo.ApplyUpdate(r, ygo.EncodeStateAsUpdate(d)) },
		"V2": func(r *ygo.Doc) error { return ygo.ApplyUpdateV2(r, ygo.EncodeStateAsUpdateV2(d)) },
	} {
		r := ygo.NewDoc()
		if err := apply(r); err != nil {
			t.Fatalf("%s: ygo refuses its own update: %v", name, err)
		}
		if got := ygo.NewText(r, "t").ToDelta(); len(got) != 1 || got[0].Embed != nil {
			t.Fatalf("%s: delta %#v, want one nil embed", name, got)
		}
	}
}

// A format value whose JSON is null (a json.RawMessage("null"), a nil
// pointer) removes the attribute in the writer's document, as it does for
// every peer reading it as null.
func TestJSONValue_NullFormatValueRemovesTheAttribute(t *testing.T) {
	for name, null := range map[string]any{
		"RawMessage null": json.RawMessage("null"),
		"nil pointer":     (*int)(nil),
	} {
		t.Run(name, func(t *testing.T) {
			d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
			x := ygo.NewText(d, "t")
			txn := d.WriteTxn()
			if err := x.Insert(txn, 0, "abcd"); err != nil {
				t.Fatal(err)
			}
			if err := x.Format(txn, 0, 4, ygo.Attrs{"bold": true}); err != nil {
				t.Fatal(err)
			}
			if err := x.Format(txn, 0, 2, ygo.Attrs{"bold": null}); err != nil {
				t.Fatal(err)
			}
			txn.Commit()
			local := fmt.Sprint(x.ToDelta())
			if local != fmt.Sprint([]ygo.DeltaOp{{Insert: "ab"}, {Insert: "cd", Attributes: ygo.Attrs{"bold": true}}}) {
				t.Fatalf("writer's delta %s", local)
			}
			for format, apply := range map[string]func(*ygo.Doc) error{
				"V1": func(r *ygo.Doc) error { return ygo.ApplyUpdate(r, ygo.EncodeStateAsUpdate(d)) },
				"V2": func(r *ygo.Doc) error { return ygo.ApplyUpdateV2(r, ygo.EncodeStateAsUpdateV2(d)) },
			} {
				r := ygo.NewDoc()
				if err := apply(r); err != nil {
					t.Fatal(err)
				}
				if got := fmt.Sprint(ygo.NewText(r, "t").ToDelta()); got != local {
					t.Fatalf("%s peer's delta %s, writer's %s", format, got, local)
				}
			}
		})
	}
}

// Two values stored as different JSON are different values: formatting with
// the second after the first writes it.
func TestJSONValue_DifferentJSONIsADifferentValue(t *testing.T) {
	d := ygo.NewDoc()
	x := ygo.NewText(d, "t")
	txn := d.WriteTxn()
	if err := x.Insert(txn, 0, "abc"); err != nil {
		t.Fatal(err)
	}
	if err := x.Format(txn, 0, 3, ygo.Attrs{"meta": json.RawMessage(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	before := ygo.EncodeStateVector(d)
	txn = d.WriteTxn()
	if err := x.Format(txn, 0, 3, ygo.Attrs{"meta": json.RawMessage(`{"a":2}`)}); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	if bytes.Equal(before, ygo.EncodeStateVector(d)) {
		t.Fatal("formatting with different JSON wrote nothing")
	}
	got := x.ToDelta()
	if len(got) != 1 || string(got[0].Attributes["meta"].(json.RawMessage)) != `{"a":2}` {
		t.Fatalf("delta %#v, want meta {\"a\":2}", got)
	}
}
