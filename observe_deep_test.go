package ygo_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/Deln0r/ygo"
)

// eventPath returns the Path of whichever event type e is.
func eventPath(e any) []any {
	switch ev := e.(type) {
	case *ygo.MapEvent:
		return ev.Path
	case *ygo.ArrayEvent:
		return ev.Path
	case *ygo.TextEvent:
		return ev.Path
	}
	return nil
}

// TestObserveDeep_Path checks that a deep observer on a root map fires
// for changes to nested types, with the path from the root to the
// changed type, matching yjs@13.6.31 observeDeep.
func TestObserveDeep_Path(t *testing.T) {
	d := ygo.NewDoc()
	root := ygo.NewMap(d, "root")
	txn := d.WriteTxn()
	child := root.SetMap(txn, "child")
	list := root.SetArray(txn, "list")
	txn.Commit()

	var paths [][]any
	root.ObserveDeep(func(evs []any) {
		for _, e := range evs {
			paths = append(paths, eventPath(e))
		}
	})

	txn = d.WriteTxn()
	child.Set(txn, "k", "v")
	txn.Commit()

	txn = d.WriteTxn()
	list.Push(txn, "x")
	txn.Commit()

	want := [][]any{{"child"}, {"list"}}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
}

// TestObserveDeep_NestedIndex checks an array-index path segment: a map
// nested inside an array element resolves to a numeric path segment.
func TestObserveDeep_NestedIndex(t *testing.T) {
	d := ygo.NewDoc()
	root := ygo.NewArray(d, "root")
	txn := d.WriteTxn()
	root.Push(txn, "a", "b") // indices 0,1
	inner := root.InsertMap(txn, 2)
	txn.Commit()

	var paths [][]any
	root.ObserveDeep(func(evs []any) {
		for _, e := range evs {
			paths = append(paths, eventPath(e))
		}
	})

	txn = d.WriteTxn()
	inner.Set(txn, "x", "y")
	txn.Commit()

	want := [][]any{{2}}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
}

// TestObserveDeep_FiresOnSelf confirms a deep observer also fires for a
// change to the observed type itself, with an empty path.
func TestObserveDeep_FiresOnSelf(t *testing.T) {
	d := ygo.NewDoc()
	m := ygo.NewMap(d, "m")
	fired := 0
	var gotPath []any
	m.ObserveDeep(func(evs []any) {
		fired++
		gotPath = eventPath(evs[0])
	})
	txn := d.WriteTxn()
	m.Set(txn, "k", "v")
	txn.Commit()
	if fired != 1 {
		t.Fatalf("fired %d, want 1", fired)
	}
	if len(gotPath) != 0 {
		t.Errorf("path = %v, want empty for self change", gotPath)
	}
}

// A deep observer that keeps the events it was given must keep the paths it
// was given. The same change reaches every observing ancestor; each gets its
// own copy with the path relative to itself, so delivery to one ancestor does
// not rewrite what another one kept (yjs shares one event object and moves its
// path from one ancestor to the next, yjs #768).
func TestObserveDeep_KeptEventsKeepTheirPath(t *testing.T) {
	d := ygo.NewDoc()
	root := ygo.NewMap(d, "root")
	txn := d.WriteTxn()
	child := root.SetMap(txn, "child")
	grand := child.SetMap(txn, "grand")
	txn.Commit()

	var fromRoot, fromChild []any
	root.ObserveDeep(func(evs []any) { fromRoot = append(fromRoot, evs...) })
	child.ObserveDeep(func(evs []any) { fromChild = append(fromChild, evs...) })

	txn = d.WriteTxn()
	grand.Set(txn, "k", 1)
	txn.Commit()

	if len(fromRoot) != 1 || len(fromChild) != 1 {
		t.Fatalf("got %d events at the root and %d at the child, want one each", len(fromRoot), len(fromChild))
	}
	if got, want := eventPath(fromRoot[0]), []any{"child", "grand"}; !reflect.DeepEqual(got, want) {
		t.Errorf("kept root event path = %v, want %v", got, want)
	}
	if got, want := eventPath(fromChild[0]), []any{"grand"}; !reflect.DeepEqual(got, want) {
		t.Errorf("kept child event path = %v, want %v", got, want)
	}
}

// Observers of different types fire in the order the types were first
// changed, on every run. They used to follow Go map iteration order.
func TestObservers_OrderIsStable(t *testing.T) {
	keys := []string{"q", "d", "x", "b", "m", "a", "z", "k"}
	want := ""
	for run := 0; run < 50; run++ {
		d := ygo.NewDoc()
		root := ygo.NewMap(d, "root")
		txn := d.WriteTxn()
		children := map[string]*ygo.Map{}
		for _, k := range keys {
			children[k] = root.SetMap(txn, k)
		}
		txn.Commit()

		var order []string
		for _, k := range keys {
			k := k
			children[k].Observe(func(*ygo.MapEvent) { order = append(order, k) })
		}
		txn = d.WriteTxn()
		for _, k := range keys {
			children[k].Set(txn, "v", 1)
		}
		txn.Commit()

		got := fmt.Sprint(order)
		if run == 0 {
			want = fmt.Sprint(keys)
		}
		if got != want {
			t.Fatalf("run %d: observers fired in order %s, want %s", run, got, want)
		}
	}
}

// Which types get a deep call is settled before the first deep callback runs.
// A deep observer that a callback registers on a type that had none starts
// with the next transaction, as in yjs 13.6.33, which queues the observing
// types before calling any of them.
func TestObserveDeep_RegisteredDuringDeliveryStartsNextTransaction(t *testing.T) {
	d := ygo.NewDoc()
	root := ygo.NewMap(d, "root")
	txn := d.WriteTxn()
	child := root.SetMap(txn, "child")
	txn.Commit()

	var calls []string
	registered := false
	child.ObserveDeep(func([]any) {
		calls = append(calls, "deep child")
		if !registered {
			registered = true
			root.ObserveDeep(func([]any) { calls = append(calls, "deep root") })
		}
	})
	for i := 1; i <= 2; i++ {
		txn = d.WriteTxn()
		child.Set(txn, "k", i)
		txn.Commit()
		calls = append(calls, fmt.Sprintf("-- commit %d", i))
	}

	want := []string{"deep child", "-- commit 1", "deep child", "deep root", "-- commit 2"}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("calls = %q, want %q (yjs 13.6.33)", calls, want)
	}
}
