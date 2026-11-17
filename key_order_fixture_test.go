package ygo_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/internal/encoding"
)

// keyOrderScenario is one entry of testdata/key-order-fixtures.json
// (testdata/gen/gen-key-order.mjs, yjs 13.6.33).
type keyOrderScenario struct {
	Name    string   `json:"name"`
	Updates []string `json:"updates"`
	V1      string   `json:"v1"`
	V2      string   `json:"v2"`
}

func loadKeyOrderFixtures(t *testing.T) []keyOrderScenario {
	t.Helper()
	raw, err := os.ReadFile("testdata/key-order-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Scenarios []keyOrderScenario `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Scenarios) == 0 {
		t.Fatal("no scenarios")
	}
	return f.Scenarios
}

// TestKeyOrder_Fixtures: objects a peer wrote with their keys out of sorted
// order come out of ygo in that order, as yjs writes them: ygo applies
// yjs's updates (some waiting for a dependency, one splitting an item
// between objects, one restoring a format value around new content) and
// its state equals yjs's in V1 and V2, encoded twice; it reads yjs's V2
// state back to the same bytes; and a document applying the updates merged
// has yjs's state.
// Objects from writers that are not JavaScript, with their keys out of
// JavaScript's enumeration order or a key repeated, come out as yjs writes
// them after reading them. ygo sorted the keys before.
func TestKeyOrder_Fixtures(t *testing.T) {
	for _, s := range loadKeyOrderFixtures(t) {
		t.Run(s.Name, func(t *testing.T) {
			var updates [][]byte
			d := ygo.NewDoc()
			for _, u := range s.Updates {
				b := mustHex(t, u)
				updates = append(updates, b)
				if err := ygo.ApplyUpdate(d, b); err != nil {
					t.Fatal(err)
				}
			}
			if ygo.HasPending(d) {
				t.Fatal("blocks still pending")
			}
			for round := range 2 {
				if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != s.V1 {
					t.Fatalf("round %d V1:\n got %s\nwant %s", round, got, s.V1)
				}
				if got := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(d)); got != s.V2 {
					t.Fatalf("round %d V2:\n got %s\nwant %s", round, got, s.V2)
				}
			}
			r := ygo.NewDoc()
			if err := ygo.ApplyUpdateV2(r, mustHex(t, s.V2)); err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(r)); got != s.V1 {
				t.Fatalf("read V2, wrote V1:\n got %s\nwant %s", got, s.V1)
			}
			if got := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(r)); got != s.V2 {
				t.Fatalf("read V2, wrote V2:\n got %s\nwant %s", got, s.V2)
			}
			// A merge carries deleted content (GC off, as yjs mergeUpdates),
			// so it is compared through a document that applies it.
			merged, err := ygo.MergeUpdates(updates)
			if err != nil {
				t.Fatal(err)
			}
			m := ygo.NewDoc()
			if err := ygo.ApplyUpdate(m, merged); err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(m)); got != s.V1 {
				t.Fatalf("merged, applied:\n got %s\nwant %s", got, s.V1)
			}
		})
	}
}

// A value brought back by undo is a copy of the deleted content and keeps
// its key order.
func TestKeyOrder_UndoKeepsTheOrder(t *testing.T) {
	var s keyOrderScenario
	for _, c := range loadKeyOrderFixtures(t) {
		if c.Name == "map value in insertion order" {
			s = c
		}
	}
	d := ygo.NewDoc()
	m := ygo.NewMap(d, "m")
	if err := ygo.ApplyUpdate(d, mustHex(t, s.Updates[0])); err != nil {
		t.Fatal(err)
	}
	um := ygo.NewUndoManagerWithOptions(d, ygo.UndoManagerOptions{CaptureTimeout: -1}, m)
	txn := d.WriteTxn()
	m.Delete(txn, "k")
	txn.Commit()
	if !um.Undo() {
		t.Fatal("nothing to undo")
	}
	// {b: 1, a: 2} as yjs writes it: object, two keys, "b" 1, "a" 2. The
	// restored item is a new one; the deleted original still holds the
	// bytes it was read with, so the live item's content is checked.
	want := []byte{0x76, 0x02, 0x01, 'b', 0x7d, 0x01, 0x01, 'a', 0x7d, 0x02}
	it := m.Branch().Map["k"]
	if it == nil || it.IsDeleted() {
		t.Fatal("undo did not bring k back")
	}
	if got := encoding.EncodeContent(nil, it.Content); !bytes.Contains(got, want) {
		t.Errorf("the restored value is %x, want {b, a} in that order", got)
	}
}

// A marker ygo writes to restore a format value around new content keeps
// the value's key order: ygo, as the second client, makes the same edit
// yjs's second client made, after applying the first client's update, and
// its state equals yjs's. ygo wrote the restored value with its keys
// sorted before.
func TestKeyOrder_RestoringMarkerFromGo(t *testing.T) {
	for name, edit := range map[string]func(*ygo.TransactionMut, *ygo.Text) error{
		"insert without the link inside a link": func(txn *ygo.TransactionMut, x *ygo.Text) error {
			return x.InsertWithAttributes(txn, 2, "X", ygo.Attrs{"link": nil})
		},
		"link removed from the middle of a link": func(txn *ygo.TransactionMut, x *ygo.Text) error {
			return x.Format(txn, 1, 2, ygo.Attrs{"link": nil})
		},
		"link removed over where a link starts": func(txn *ygo.TransactionMut, x *ygo.Text) error {
			return x.Format(txn, 1, 2, ygo.Attrs{"link": nil})
		},
	} {
		t.Run(name, func(t *testing.T) {
			var s keyOrderScenario
			for _, c := range loadKeyOrderFixtures(t) {
				if c.Name == name {
					s = c
				}
			}
			if s.Name == "" {
				t.Fatalf("no fixture %q", name)
			}
			d := ygo.NewDocWithOptions(ygo.Options{ClientID: 2})
			x := ygo.NewText(d, "t")
			if err := ygo.ApplyUpdate(d, mustHex(t, s.Updates[0])); err != nil {
				t.Fatal(err)
			}
			txn := d.WriteTxn()
			if err := edit(txn, x); err != nil {
				t.Fatal(err)
			}
			txn.Commit()
			if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != s.V1 {
				t.Fatalf("V1:\n got %s\nwant %s", got, s.V1)
			}
			if got := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(d)); got != s.V2 {
				t.Fatalf("V2:\n got %s\nwant %s", got, s.V2)
			}
		})
	}
}
