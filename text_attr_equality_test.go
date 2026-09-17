package ygo_test

import (
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/Deln0r/ygo"
)

// Attribute values can be objects and arrays, not just scalars - a mention, a
// comment thread, a column spec. Comparing two such values used to be a bare
// Go ==, which panics at runtime on maps and slices, so reformatting a range,
// reading ToDelta across two runs, or building an observer event for them
// crashed the process.
//
// Every expected delta below is what yjs 13.6.32 produced for the same
// operations, not what seemed reasonable. yjs compares attribute values with
// equalAttrs -> lib0 equalFlat: one level deep, containers inside compared by
// identity. So equal flat objects merge into one run, but objects that are
// equal only once you look inside a nested object stay separate.

func deltaJSON(t *testing.T, d *ygo.Doc, name string) any {
	t.Helper()
	txt := ygo.NewText(d, name)
	rt := d.ReadTxn()
	defer rt.Close()
	var ops []any
	for _, op := range txt.ToDelta() {
		m := map[string]any{}
		if op.Embed != nil {
			m["insert"] = op.Embed
		} else {
			m["insert"] = op.Insert
		}
		if len(op.Attributes) > 0 {
			m["attributes"] = map[string]any(op.Attributes)
		}
		ops = append(ops, m)
	}
	return normalizeJSON(t, ops)
}

func normalizeJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func mustParseJSON(t *testing.T, s string) any {
	t.Helper()
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestTextAttributes_ContainerValuesMatchYjs inserts "ab" with one attribute
// value, then "X" between them with a second value, exactly as the yjs probe
// did, and compares the resulting delta with yjs's.
func TestTextAttributes_ContainerValuesMatchYjs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		first  any
		second any
		yjs    string
	}{
		{
			name:  "equal flat objects merge",
			first: map[string]any{"x": 1}, second: map[string]any{"x": 1},
			yjs: `[{"insert":"aXb","attributes":{"meta":{"x":1}}}]`,
		},
		{
			name:  "different flat objects stay apart",
			first: map[string]any{"x": 1}, second: map[string]any{"x": 2},
			yjs: `[{"insert":"a","attributes":{"meta":{"x":1}}},{"insert":"X","attributes":{"meta":{"x":2}}},{"insert":"b","attributes":{"meta":{"x":1}}}]`,
		},
		{
			name:  "equal flat arrays merge",
			first: []any{1, 2}, second: []any{1, 2},
			yjs: `[{"insert":"aXb","attributes":{"meta":[1,2]}}]`,
		},
		{
			name:  "objects equal only below the first level stay apart",
			first: map[string]any{"x": map[string]any{"y": 1}}, second: map[string]any{"x": map[string]any{"y": 1}},
			yjs: `[{"insert":"a","attributes":{"meta":{"x":{"y":1}}}},{"insert":"X","attributes":{"meta":{"x":{"y":1}}}},{"insert":"b","attributes":{"meta":{"x":{"y":1}}}}]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := ygo.NewDocWithOptions(ygo.Options{ClientID: 41})
			txt := ygo.NewText(d, "t")
			txn := d.WriteTxn()
			if err := txt.InsertWithAttributes(txn, 0, "ab", map[string]any{"meta": tc.first}); err != nil {
				t.Fatal(err)
			}
			txn.Commit()
			txn = d.WriteTxn()
			if err := txt.InsertWithAttributes(txn, 1, "X", map[string]any{"meta": tc.second}); err != nil {
				t.Fatal(err)
			}
			txn.Commit()

			got, want := deltaJSON(t, d, "t"), mustParseJSON(t, tc.yjs)
			if !reflect.DeepEqual(got, want) {
				gb, _ := json.Marshal(got)
				t.Fatalf("delta differs from yjs:\n  ygo %s\n  yjs %s", gb, tc.yjs)
			}
		})
	}
}

// TestTextAttributes_ReformatWithEqualObjectIsANoOp: formatting a range with
// an object value it already carries must neither panic nor add format
// markers. yjs adds none; its state vector does not move.
func TestTextAttributes_ReformatWithEqualObjectIsANoOp(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 42})
	txt := ygo.NewText(d, "t")
	txn := d.WriteTxn()
	if err := txt.Insert(txn, 0, "abc"); err != nil {
		t.Fatal(err)
	}
	if err := txt.Format(txn, 0, 3, map[string]any{"meta": map[string]any{"x": 1}}); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	before := ygo.EncodeStateVector(d)

	txn = d.WriteTxn()
	if err := txt.Format(txn, 0, 3, map[string]any{"meta": map[string]any{"x": 1}}); err != nil {
		t.Fatal(err)
	}
	txn.Commit()

	if after := ygo.EncodeStateVector(d); !reflect.DeepEqual(before, after) {
		t.Fatal("reformatting with an equal object value added items; yjs adds none")
	}
	want := mustParseJSON(t, `[{"insert":"abc","attributes":{"meta":{"x":1}}}]`)
	if got := deltaJSON(t, d, "t"); !reflect.DeepEqual(got, want) {
		gb, _ := json.Marshal(got)
		t.Fatalf("delta = %s, want the single run yjs produces", gb)
	}
}

// TestTextAttributes_RemoteContainerValuesDoNotCrashObservers: the event path
// compares attribute values too, and it runs for updates arriving from a peer.
// With an observer registered, a remote update carrying two runs of an
// object-valued attribute used to panic inside event construction - a crash
// any peer could cause.
func TestTextAttributes_RemoteContainerValuesDoNotCrashObservers(t *testing.T) {
	author := ygo.NewDocWithOptions(ygo.Options{ClientID: 43})
	at := ygo.NewText(author, "t")
	txn := author.WriteTxn()
	if err := at.InsertWithAttributes(txn, 0, "ab", map[string]any{"meta": map[string]any{"x": 1}}); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	txn = author.WriteTxn()
	if err := at.InsertWithAttributes(txn, 1, "X", map[string]any{"meta": map[string]any{"x": 2}}); err != nil {
		t.Fatal(err)
	}
	txn.Commit()

	receiver := ygo.NewDoc()
	events := 0
	unobserve := ygo.NewText(receiver, "t").Observe(func(*ygo.TextEvent) { events++ })
	defer unobserve()

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("applying a remote update with object-valued attributes panicked in an observer event: %v", r)
			}
		}()
		if err := ygo.ApplyUpdate(receiver, ygo.EncodeStateAsUpdate(author)); err != nil {
			t.Fatal(err)
		}
	}()
	if events == 0 {
		t.Fatal("the observer never fired, so the event path was not exercised")
	}
}

// TestTextAttributes_RunStructureMatchesYjs covers the equality cases where
// the answer turns on identity or on JS's view of arrays as objects, and where
// JSON cannot express the values (NaN). It compares only the run structure,
// which is what the comparison decides. Every expectation is yjs 13.6.32's.
func TestTextAttributes_RunStructureMatchesYjs(t *testing.T) {
	sharedArrayWithNaN := []any{math.NaN()}
	sharedObjectWithNaN := map[string]any{"k": math.NaN()}

	for _, tc := range []struct {
		name          string
		first, second any
		yjsRuns       []string
	}{
		{
			// Go gives every empty slice one shared address; identity by pointer
			// would call these one object and merge them.
			name:  "distinct nested empty arrays stay apart",
			first: map[string]any{"x": []any{}}, second: map[string]any{"x": []any{}},
			yjsRuns: []string{"a", "X", "b"},
		},
		{
			name:  "one array reused, holding NaN, merges",
			first: sharedArrayWithNaN, second: sharedArrayWithNaN,
			yjsRuns: []string{"aXb"},
		},
		{
			name:  "distinct arrays holding NaN stay apart",
			first: []any{math.NaN()}, second: []any{math.NaN()},
			yjsRuns: []string{"a", "X", "b"},
		},
		{
			name:  "one object reused, holding NaN, merges",
			first: sharedObjectWithNaN, second: sharedObjectWithNaN,
			yjsRuns: []string{"aXb"},
		},
		{
			name:  "an array equals an object with the same numeric keys",
			first: []any{1, 2}, second: map[string]any{"0": 1, "1": 2},
			yjsRuns: []string{"aXb"},
		},
		{
			name:  "an empty array equals an empty object",
			first: []any{}, second: map[string]any{},
			yjsRuns: []string{"aXb"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := ygo.NewDocWithOptions(ygo.Options{ClientID: 61})
			txt := ygo.NewText(d, "t")
			txn := d.WriteTxn()
			if err := txt.InsertWithAttributes(txn, 0, "ab", map[string]any{"meta": tc.first}); err != nil {
				t.Fatal(err)
			}
			txn.Commit()
			txn = d.WriteTxn()
			if err := txt.InsertWithAttributes(txn, 1, "X", map[string]any{"meta": tc.second}); err != nil {
				t.Fatal(err)
			}
			txn.Commit()

			rt := d.ReadTxn()
			var runs []string
			for _, op := range txt.ToDelta() {
				runs = append(runs, op.Insert)
			}
			rt.Close()
			if !reflect.DeepEqual(runs, tc.yjsRuns) {
				t.Fatalf("runs %q, yjs produces %q", runs, tc.yjsRuns)
			}
		})
	}
}

// TestTextAttributes_NilContainerRemovesAttributeInBothFormats: a nil map as a
// format value is JSON null, which is what V1 has always sent, and null means
// "remove this attribute". The V2 path briefly turned it into an empty object
// instead, so the same Format call left V1 receivers without the attribute and
// V2 receivers with an empty one. yjs removes it.
func TestTextAttributes_NilContainerRemovesAttributeInBothFormats(t *testing.T) {
	src := ygo.NewDocWithOptions(ygo.Options{ClientID: 62})
	st := ygo.NewText(src, "t")
	txn := src.WriteTxn()
	if err := st.InsertWithAttributes(txn, 0, "abc", map[string]any{"bold": true, "meta": map[string]any{"k": 1}}); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	txn = src.WriteTxn()
	if err := st.Format(txn, 0, 3, map[string]any{"meta": map[string]any(nil)}); err != nil {
		t.Fatal(err)
	}
	txn.Commit()

	for name, transfer := range map[string]func(*ygo.Doc) error{
		"V1": func(d *ygo.Doc) error { return ygo.ApplyUpdate(d, ygo.EncodeStateAsUpdate(src)) },
		"V2": func(d *ygo.Doc) error { return ygo.ApplyUpdateV2(d, ygo.EncodeStateAsUpdateV2(src)) },
	} {
		t.Run(name, func(t *testing.T) {
			dst := ygo.NewDoc()
			if err := transfer(dst); err != nil {
				t.Fatal(err)
			}
			want := mustParseJSON(t, `[{"insert":"abc","attributes":{"bold":true}}]`)
			if got := deltaJSON(t, dst, "t"); !reflect.DeepEqual(got, want) {
				gb, _ := json.Marshal(got)
				t.Fatalf("receiver sees %s; yjs removes the attribute", gb)
			}
		})
	}
}
