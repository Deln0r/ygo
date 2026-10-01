package ygo_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/Deln0r/ygo"
)

// An update handler may unsubscribe itself, and register another, from
// inside the call: Commit holds the document lock while it calls handlers,
// so registration must not need that lock.
func TestOnUpdate_HandlerChangesSubscriptions(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	text := ygo.NewText(d, "t")
	var first, second int
	var unsub func()
	unsub = d.OnUpdate(func([]byte, any) {
		first++
		unsub()
		d.OnUpdate(func([]byte, any) { second++ })
	})
	for i := 0; i < 3; i++ {
		txn := d.WriteTxn()
		if err := text.Insert(txn, uint64(i), "x"); err != nil {
			t.Fatal(err)
		}
		txn.Commit()
	}
	if first != 1 || second != 2 {
		t.Fatalf("first handler ran %d times, second %d; want 1 and 2", first, second)
	}
}

// A transaction that changes nothing emits no update, in either version;
// one that changes something emits one of each, V1 handlers first.
func TestOnUpdate_OnlyForChanges(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	m := ygo.NewMap(d, "m")
	var log []string
	d.OnUpdateV2(func([]byte, any) { log = append(log, "v2") })
	d.OnUpdate(func([]byte, any) { log = append(log, "v1") })
	d.WriteTxn().Commit()
	if len(log) != 0 {
		t.Fatalf("empty transaction emitted %v", log)
	}
	txn := d.WriteTxn()
	m.Set(txn, "k", "v")
	txn.Commit()
	if len(log) != 2 || log[0] != "v1" || log[1] != "v2" {
		t.Fatalf("got %v, want [v1 v2]", log)
	}
}

// The per-transaction updates, applied in order, rebuild the document, and
// each one holds only its own transaction: the size of an append does not
// grow with the history (the reason a third-party evaluation chose the
// other Go port: EncodeDiff against a saved state vector re-sent the whole
// block, 27 bytes growing to 1720 over 200 appends).
func TestOnUpdate_AppendsStayFlat(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 3141592653})
	text := ygo.NewText(d, "t")
	var updates [][]byte
	d.OnUpdate(func(u []byte, _ any) { updates = append(updates, u) })
	saved := ygo.EncodeStateVector(d)
	var diffs []int
	for i := 0; i < 200; i++ {
		txn := d.WriteTxn()
		if err := text.Insert(txn, uint64(i), "x"); err != nil {
			t.Fatal(err)
		}
		txn.Commit()
		diff, err := ygo.EncodeDiff(d, saved)
		if err != nil {
			t.Fatal(err)
		}
		diffs = append(diffs, len(diff))
		saved = ygo.EncodeStateVector(d)
	}
	for i, u := range updates {
		if len(u) > 20 {
			t.Fatalf("update %d is %d bytes; an append should not grow with history", i, len(u))
		}
		if diffs[i] != len(u) {
			t.Fatalf("append %d: EncodeDiff against the saved state vector is %d bytes, the update %d", i, diffs[i], len(u))
		}
	}
	peer := ygo.NewDocWithOptions(ygo.Options{ClientID: 2})
	for _, u := range updates {
		if err := ygo.ApplyUpdate(peer, u); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(ygo.EncodeStateAsUpdate(peer), ygo.EncodeStateAsUpdate(d)) {
		t.Fatal("replaying the updates did not rebuild the document")
	}
}

// ApplyUpdateWithOrigin hands its origin to the update handlers, which is
// what a sync provider's echo guard keys on.
func TestApplyUpdateWithOrigin_ReachesHandlers(t *testing.T) {
	src := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	srcMap := ygo.NewMap(src, "m") // roots before transactions: NewMap takes the lock
	txn := src.WriteTxn()
	srcMap.Set(txn, "k", "v")
	txn.Commit()

	type peer struct{ _ byte }
	from := &peer{}
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 2})
	var origins []any
	d.OnUpdate(func(_ []byte, origin any) { origins = append(origins, origin) })
	d.OnUpdateV2(func(_ []byte, origin any) { origins = append(origins, origin) })
	if err := ygo.ApplyUpdateWithOrigin(d, ygo.EncodeStateAsUpdate(src), from); err != nil {
		t.Fatal(err)
	}
	if err := ygo.ApplyUpdateV2WithOrigin(d, ygo.EncodeStateAsUpdateV2(src), "again"); err != nil {
		t.Fatal(err)
	}
	if len(origins) != 2 || origins[0] != any(from) || origins[1] != any(from) {
		t.Fatalf("origins %v; want the peer twice, and nothing for the repeated state", origins)
	}
	// A V2 update with something new carries its own origin, and the
	// default UndoManager leaves it alone.
	um := ygo.NewUndoManager(d, ygo.NewMap(d, "m"))
	defer um.Close()
	txn = src.WriteTxn()
	srcMap.Set(txn, "k2", "v2")
	txn.Commit()
	diff, err := ygo.EncodeDiffV2(src, ygo.EncodeStateVector(d))
	if err != nil {
		t.Fatal(err)
	}
	if err := ygo.ApplyUpdateV2WithOrigin(d, diff, "v2"); err != nil {
		t.Fatal(err)
	}
	if len(origins) != 4 || origins[2] != "v2" || origins[3] != "v2" {
		t.Fatalf("origins %v; want the V2 apply's origin on both events", origins)
	}
	if um.CanUndo() {
		t.Fatal("a V2 update applied with an origin was captured by the default manager")
	}
}

// An origin that cannot be a map key must not panic the commit when an
// UndoManager filters on origins; it is simply not tracked.
func TestUndoManager_UncomparableOrigin(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	m := ygo.NewMap(d, "m")
	um := ygo.NewUndoManager(d, m)
	src := ygo.NewDocWithOptions(ygo.Options{ClientID: 2})
	srcMap := ygo.NewMap(src, "m")
	txn := src.WriteTxn()
	srcMap.Set(txn, "k", "v")
	txn.Commit()
	if err := ygo.ApplyUpdateWithOrigin(d, ygo.EncodeStateAsUpdate(src), []byte("remote")); err != nil {
		t.Fatal(err)
	}
	// A comparable type holding an uncomparable value panics when hashed
	// just the same.
	txn = src.WriteTxn()
	srcMap.Set(txn, "k", "w")
	txn.Commit()
	diff, err := ygo.EncodeDiff(src, ygo.EncodeStateVector(d))
	if err != nil {
		t.Fatal(err)
	}
	if err := ygo.ApplyUpdateWithOrigin(d, diff, struct{ X any }{[]byte("remote")}); err != nil {
		t.Fatal(err)
	}
	if um.CanUndo() {
		t.Fatal("an update applied with an uncomparable origin was captured")
	}
	for _, o := range []any{[]byte("x"), struct{ X any }{[]byte("x")}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("AddTrackedOrigin accepted %#v", o)
				}
			}()
			um.AddTrackedOrigin(o)
		}()
	}
}

// Closing an UndoManager, or unsubscribing, from inside a handler that
// Commit calls must not deadlock: Commit holds the document lock there.
func TestUndoManager_CloseFromHandlers(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	m := ygo.NewMap(d, "m")
	um1 := ygo.NewUndoManager(d, m)
	um2 := ygo.NewUndoManager(d, m)
	d.OnUpdate(func([]byte, any) { um1.Close() })
	d.OnAfterTransaction(func(*ygo.TransactionMut) { um2.Close() })
	done := make(chan struct{})
	go func() {
		defer close(done)
		txn := d.WriteTxn()
		m.Set(txn, "k", "v")
		txn.Commit()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("closing an UndoManager from a commit handler deadlocked")
	}
}

// The default UndoManager tracks the nil origin, which plain ApplyUpdate
// also uses (as yjs's does), so a peer's edit is undoable unless it is
// applied with an origin or the manager tracks only its own.
func TestUndoManager_TrackedOrigins(t *testing.T) {
	remote := func(d *ygo.Doc, value string, origin any) {
		src := ygo.NewDocWithOptions(ygo.Options{ClientID: 7})
		srcMap := ygo.NewMap(src, "m")
		if err := ygo.ApplyUpdate(src, ygo.EncodeStateAsUpdate(d)); err != nil {
			t.Fatal(err)
		}
		txn := src.WriteTxn()
		srcMap.Set(txn, "k", value)
		txn.Commit()
		diff, err := ygo.EncodeDiff(src, ygo.EncodeStateVector(d))
		if err != nil {
			t.Fatal(err)
		}
		if err := ygo.ApplyUpdateWithOrigin(d, diff, origin); err != nil {
			t.Fatal(err)
		}
	}
	local := func(d *ygo.Doc, m *ygo.Map, value string, origin any) {
		txn := d.WriteTxn()
		txn.Origin = origin
		m.Set(txn, "k", value)
		txn.Commit()
	}

	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	m := ygo.NewMap(d, "m")
	um := ygo.NewUndoManager(d, m)
	remote(d, "plain", nil)
	if !um.CanUndo() {
		t.Fatal("default manager: an update applied without an origin should be captured, as in yjs")
	}
	um.Clear()
	remote(d, "tagged", "remote")
	if um.CanUndo() {
		t.Fatal("default manager captured an update applied with an origin")
	}

	um.AddTrackedOrigin("me")
	um.RemoveTrackedOrigin(nil)
	remote(d, "plain again", nil)
	local(d, m, "untagged", nil)
	if um.CanUndo() {
		t.Fatal("after RemoveTrackedOrigin(nil), nil-origin transactions are still captured")
	}
	local(d, m, "mine", "me")
	if !um.Undo() {
		t.Fatal("a transaction with the added origin was not captured")
	}
	if got := m.Get("k"); got != "untagged" {
		t.Fatalf("after undo k = %v, want untagged", got)
	}
}
