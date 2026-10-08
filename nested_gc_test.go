package ygo_test

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Deln0r/ygo"
)

// TestNestedTypeGC_CrossLanguage verifies that deleting a populated
// nested shared type produces byte-identical V1 update bytes to yjs: the
// reference item becomes a ContentDeleted marker and every child collapses
// into a garbage-collected (ref 0) struct run. Op sequences here mirror
// testdata/gen/gen-nested-gc.mjs, keyed by scenario name.
func TestNestedTypeGC_CrossLanguage(t *testing.T) {
	path := filepath.Join("testdata", "nested-gc-fixtures.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("testdata/nested-gc-fixtures.json not present; run testdata/gen/gen-nested-gc.mjs")
		}
		t.Fatal(err)
	}
	var fx struct {
		Scenarios []struct {
			Name      string `json:"name"`
			ClientID  uint64 `json:"client_id"`
			UpdateHex string `json:"update_hex"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(data, &fx); err != nil {
		t.Fatal(err)
	}

	// builders replay the same logical operations as the JS generator.
	builders := map[string]func(d *ygo.Doc){
		"map_in_map": func(d *ygo.Doc) {
			root := ygo.NewMap(d, "root")
			txn := d.WriteTxn()
			child := root.SetMap(txn, "child")
			child.Set(txn, "a", "x")
			child.Set(txn, "b", "y")
			child.Set(txn, "c", "z")
			txn.Commit()
			txn = d.WriteTxn()
			root.Delete(txn, "child")
			txn.Commit()
		},
		"array_in_map": func(d *ygo.Doc) {
			root := ygo.NewMap(d, "root")
			txn := d.WriteTxn()
			child := root.SetArray(txn, "list")
			child.Insert(txn, 0, "p")
			child.Insert(txn, 1, "q")
			child.Insert(txn, 2, "r")
			txn.Commit()
			txn = d.WriteTxn()
			root.Delete(txn, "list")
			txn.Commit()
		},
		"map_in_map_2level": func(d *ygo.Doc) {
			root := ygo.NewMap(d, "root")
			txn := d.WriteTxn()
			outer := root.SetMap(txn, "outer")
			inner := outer.SetMap(txn, "inner")
			inner.Set(txn, "a", "x")
			inner.Set(txn, "b", "y")
			outer.Set(txn, "k", "v")
			txn.Commit()
			txn = d.WriteTxn()
			root.Delete(txn, "outer")
			txn.Commit()
		},
		"map_in_array": func(d *ygo.Doc) {
			root := ygo.NewArray(d, "arr")
			txn := d.WriteTxn()
			m := root.InsertMap(txn, 0)
			m.Set(txn, "a", "x")
			m.Set(txn, "b", "y")
			txn.Commit()
			txn = d.WriteTxn()
			root.Delete(txn, 0, 1)
			txn.Commit()
		},
	}

	for _, sc := range fx.Scenarios {
		sc := sc
		t.Run(sc.Name, func(t *testing.T) {
			build, ok := builders[sc.Name]
			if !ok {
				t.Fatalf("no Go builder for scenario %q", sc.Name)
			}
			d := ygo.NewDocWithOptions(ygo.Options{ClientID: sc.ClientID})
			build(d)
			got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d))
			if got != sc.UpdateHex {
				t.Errorf("scenario %s mismatch:\n go = %s\n js = %s", sc.Name, got, sc.UpdateHex)
			}
		})
	}
}

// A collected nested type keeps no reference to its old items, as yjs
// ContentType.gc empties the type: a wrapper still held reads empty.
func TestNestedGC_RetainedWrapperReadsEmpty(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	m := ygo.NewMap(d, "m")
	txn := d.WriteTxn()
	child := m.SetMap(txn, "child")
	child.Set(txn, "a", int64(1))
	list := m.SetArray(txn, "list")
	list.Push(txn, int64(1), int64(2))
	txn.Commit()

	txn = d.WriteTxn()
	m.Delete(txn, "child")
	m.Delete(txn, "list")
	txn.Commit()

	if got := child.Get("a"); got != nil || child.Len() != 0 {
		t.Errorf("collected map still reads a=%v, len %d", got, child.Len())
	}
	if got := list.ToSlice(); len(got) != 0 || list.Len() != 0 {
		t.Errorf("collected array still reads %v, len %d", got, list.Len())
	}
	// Reads come back empty anyway (the items inside were deleted with
	// the type); what GC adds is dropping the references, so the old
	// items can be freed while the wrapper lives.
	for name, br := range map[string]*ygo.Branch{"map": child.Branch(), "array": list.Branch()} {
		if br.Start != nil || len(br.Map) != 0 {
			t.Errorf("collected %s still references its items: start=%v, %d map entries", name, br.Start != nil, len(br.Map))
		}
	}
}

// A type deleted in a transaction is not among the transaction's changed
// types, even if it changed earlier in it, as yjs ContentType.delete drops
// it from transaction.changed.
func TestDeletedTypeLeavesChangedTypes(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	m := ygo.NewMap(d, "m")
	txn := d.WriteTxn()
	child := m.SetMap(txn, "child")
	txn.Commit()

	var changed []*ygo.Branch
	d.OnAfterTransaction(func(t *ygo.TransactionMut) { changed = t.ChangedTypes() })
	txn = d.WriteTxn()
	child.Set(txn, "k", "v")
	m.Delete(txn, "child")
	txn.Commit()

	for _, b := range changed {
		if b == child.Branch() {
			t.Fatal("the deleted child is still listed as changed")
		}
	}
	if len(changed) != 1 || changed[0] != m.Branch() {
		t.Fatalf("changed types %d, want only the parent map", len(changed))
	}
}

// Undoing the deletion of a nested map restores a new map, as yjs does:
// the old map's observer stays silent, the parent reports the key added
// back (yjs 13.6.33: delete, then add with no old value), and the new
// map holds the restored keys.
func TestUndoNestedMap_Observers(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	m := ygo.NewMap(d, "m")
	txn := d.WriteTxn()
	txn.Origin = "setup"
	old := m.SetMap(txn, "child")
	old.Set(txn, "a", int64(1))
	txn.Commit()
	um := ygo.NewUndoManager(d, m)
	defer um.Close()

	var log []string
	old.Observe(func(*ygo.MapEvent) { log = append(log, "old child observer") })
	m.Observe(func(e *ygo.MapEvent) {
		for k, c := range e.Keys {
			log = append(log, fmt.Sprintf("root %s %s old=%T", k, c.Action, c.OldValue))
		}
	})
	txn = d.WriteTxn()
	m.Delete(txn, "child")
	txn.Commit()
	if !um.Undo() {
		t.Fatal("undo did nothing")
	}
	want := []string{"root child delete old=*types.Map", "root child add old=<nil>"}
	if fmt.Sprint(log) != fmt.Sprint(want) {
		t.Fatalf("events %q, want %q", log, want)
	}
	restored, ok := m.Get("child").(*ygo.Map)
	if !ok || restored.Branch() == old.Branch() {
		t.Fatalf("restored child %T is not a new map", m.Get("child"))
	}
	if got := restored.Get("a"); got != int64(1) {
		t.Fatalf("restored a = %v", got)
	}
}

// Clear releases what the dropped steps kept in time for the GC of the
// transaction it is called from, even from a commit handler, and after
// Close too. Bytes from yjs 13.6.33 for the same sequence: p={x:1}, a
// tracked delete of x, an untracked delete of p.
func TestUndoClear_ReleasesBeforeThatGC(t *testing.T) {
	const (
		collected = "010201002101016d01700100010101010002"
		kept      = "010201002701016d017001280001000178017d010101010002"
	)
	for _, variant := range []string{"clear in a handler", "clear then close", "no clear"} {
		d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
		m := ygo.NewMap(d, "m")
		txn := d.WriteTxn()
		txn.Origin = "setup"
		p := m.SetMap(txn, "p")
		p.Set(txn, "x", int64(1))
		txn.Commit()
		um := ygo.NewUndoManager(d, m)
		if variant == "clear in a handler" {
			d.OnAfterTransaction(func(t *ygo.TransactionMut) {
				if t.Origin == "other" {
					um.Clear()
				}
			})
		}
		txn = d.WriteTxn()
		p.Delete(txn, "x")
		txn.Commit()
		if variant == "clear then close" {
			um.Clear()
			um.Close()
		}
		txn = d.WriteTxn()
		txn.Origin = "other"
		m.Delete(txn, "p")
		txn.Commit()
		want := collected
		if variant == "no clear" {
			want = kept
		}
		if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != want {
			t.Errorf("%s: state\n got  %s\n want %s", variant, got, want)
		}
	}
}

// A Clear called from a commit handler that runs before the manager's own
// handler releases what the dropped step kept before the manager keeps
// what the commit deleted, as yjs clear releases at once: the parent both
// steps keep stays kept, and undo restores it. The state after undo is
// yjs 13.6.33's for the same edits with the clear made before the delete
// (testdata scenario undo-clear-then-tracked-delete).
func TestUndoClear_FromEarlierHandlerBeforeCapture(t *testing.T) {
	const want = "0105cdcc83da0b002701016d0170012800cdcc83da0b000178017d012800cdcc83da0b000179017d02a7cdcc83da0b00012800cdcc83da0b030179017d0201cdcc83da0b010003"
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 3141592653})
	m := ygo.NewMap(d, "m")
	txn := d.WriteTxn()
	txn.Origin = "setup"
	p := m.SetMap(txn, "p")
	txn.Commit()
	txn = d.WriteTxn()
	txn.Origin = "setup"
	p.Set(txn, "x", int64(1))
	p.Set(txn, "y", int64(2))
	txn.Commit()
	var um *ygo.UndoManager
	d.OnAfterTransaction(func(t *ygo.TransactionMut) {
		if t.Origin == "c" {
			um.Clear()
		}
	})
	um = ygo.NewUndoManager(d, m)
	um.AddTrackedOrigin("c")
	txn = d.WriteTxn()
	p.Delete(txn, "x")
	txn.Commit()
	txn = d.WriteTxn()
	txn.Origin = "c"
	m.Delete(txn, "p")
	txn.Commit()
	if !um.Undo() {
		t.Fatal("Undo found nothing to restore")
	}
	if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != want {
		t.Errorf("state after undo\n got  %s\n want %s", got, want)
	}
}

// OnSubdocs fires after GC and the update handlers, where yjs emits
// 'subdocs': a Clear from it releases what the commit's capture kept only
// after that commit's GC, so the deleted entries keep their content, as in
// yjs. Bytes from yjs 13.6.33: m.x=1 and m.s=Doc{guid:"S"} from client 1,
// a manager on m, both deleted in one transaction, the manager cleared
// from the subdocs listener.
func TestUndoClear_FromOnSubdocsAfterGC(t *testing.T) {
	const (
		setup = "010201002801016d0178017d012901016d01730153760000"
		want  = "010201002801016d0178017d012901016d0173015376000101010002"
	)
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	u, _ := hex.DecodeString(setup)
	if err := ygo.ApplyUpdateWithOrigin(d, u, "setup"); err != nil {
		t.Fatal(err)
	}
	m := ygo.NewMap(d, "m")
	um := ygo.NewUndoManager(d, m)
	var order []string
	d.OnUpdate(func([]byte, any) { order = append(order, "update") })
	d.OnSubdocs(func(ev ygo.SubdocsEvent) {
		order = append(order, "subdocs")
		if len(ev.Removed) > 0 {
			um.Clear()
		}
	})
	txn := d.WriteTxn()
	m.Delete(txn, "x")
	m.Delete(txn, "s")
	txn.Commit()
	if got := strings.Join(order, ","); got != "update,subdocs" {
		t.Errorf("handler order %s, want update,subdocs", got)
	}
	if um.CanUndo() {
		t.Error("CanUndo after Clear")
	}
	if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != want {
		t.Errorf("state\n got  %s\n want %s", got, want)
	}
}
