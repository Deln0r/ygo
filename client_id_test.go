package ygo_test

import (
	"math"
	"sync"
	"testing"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/internal/encoding"
)

// Generated client IDs are 32-bit, as yjs 13 and 14 draw them
// (random.uint32): every one in [1, 2^32-1], and the draws cover the
// whole range, the top bit set in about half of them.
func TestNewDoc_ClientIDIs32Bit(t *testing.T) {
	const n = 20000
	high := 0
	for range n {
		id := ygo.NewDoc().ClientID()
		if id == 0 || id > math.MaxUint32 {
			t.Fatalf("generated client ID %d is outside [1, 2^32-1]", id)
		}
		if id >= 1<<31 {
			high++
		}
	}
	// Binomial(20000, 1/2): the standard deviation is about 71.
	if high < n/2-1000 || high > n/2+1000 {
		t.Fatalf("%d of %d IDs have the top bit set, want about half", high, n)
	}
}

func insertText(t *testing.T, d *ygo.Doc, txt *ygo.Text, idx uint64, s string) {
	t.Helper()
	txn := d.WriteTxn()
	if err := txt.Insert(txn, idx, s); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
}

func applyAll(t *testing.T, from, to *ygo.Doc) {
	t.Helper()
	if err := ygo.ApplyUpdate(to, ygo.EncodeStateAsUpdate(from)); err != nil {
		t.Fatal(err)
	}
}

// A replica that receives an update advancing its own client's clock
// takes a new random ID, as yjs cleanupTransactions does, so its next
// edits cannot collide with the other replica's. Before, both kept
// writing items with the same (client, clock), each skipped the other's
// as already known, and they diverged.
func TestDoc_NewClientIDWhenAnUpdateUsesIt(t *testing.T) {
	a := ygo.NewDocWithOptions(ygo.Options{ClientID: 7})
	b := ygo.NewDocWithOptions(ygo.Options{ClientID: 7})
	ta, tb := ygo.NewText(a, "t"), ygo.NewText(b, "t")
	insertText(t, a, ta, 0, "from a")
	applyAll(t, a, b)
	id := b.ClientID()
	if id == 7 {
		t.Fatal("b kept client ID 7 after an update that wrote under it")
	}
	if id == 0 || id > math.MaxUint32 {
		t.Fatalf("new client ID %d is outside [1, 2^32-1]", id)
	}
	insertText(t, b, tb, 0, "b ")
	insertText(t, a, ta, ta.Length(), " a")
	if got := ygo.EncodeStateVector(b); got == nil {
		t.Fatal("no state vector")
	}
	applyAll(t, a, b)
	applyAll(t, b, a)
	if ta.String() != tb.String() {
		t.Fatalf("replicas diverged: %q and %q", ta.String(), tb.String())
	}
	if b.ClientID() != id {
		t.Fatalf("b changed its ID again to %d on an update that did not write under %d", b.ClientID(), id)
	}
}

// The ID stays when nothing but this replica writes under it: local
// edits, an update from another client, an update carrying only what
// the replica already has of its own, and undo and redo.
func TestDoc_ClientIDStaysWithoutACollision(t *testing.T) {
	a := ygo.NewDocWithOptions(ygo.Options{ClientID: 9})
	other := ygo.NewDocWithOptions(ygo.Options{ClientID: 10})
	ta, to := ygo.NewText(a, "t"), ygo.NewText(other, "t")
	um := ygo.NewUndoManagerWithOptions(a, ygo.UndoManagerOptions{CaptureTimeout: -1}, ta)
	insertText(t, a, ta, 0, "mine")
	insertText(t, other, to, 0, "theirs")
	applyAll(t, other, a)
	applyAll(t, a, a)
	um.Undo()
	um.Redo()
	if a.ClientID() != 9 {
		t.Fatalf("client ID changed to %d without another writer", a.ClientID())
	}
}

// The new ID is in place when the colliding transaction's update is
// emitted, as yjs changes it before emitting 'update'; observers and
// after-transaction handlers of that transaction still see the old one.
func TestDoc_NewClientIDBeforeTheUpdateEvent(t *testing.T) {
	a := ygo.NewDocWithOptions(ygo.Options{ClientID: 7})
	b := ygo.NewDocWithOptions(ygo.Options{ClientID: 7})
	ta, tb := ygo.NewText(a, "t"), ygo.NewText(b, "t")
	insertText(t, a, ta, 0, "x")
	var inObserver, inAfter, inUpdate uint64
	tb.Observe(func(*ygo.TextEvent) { inObserver = b.ClientID() })
	b.OnAfterTransaction(func(*ygo.TransactionMut) { inAfter = b.ClientID() })
	b.OnUpdate(func([]byte, any) { inUpdate = b.ClientID() })
	applyAll(t, a, b)
	if inObserver != 7 || inAfter != 7 {
		t.Fatalf("observer saw %d and after-transaction handler %d, want the old ID 7", inObserver, inAfter)
	}
	if inUpdate == 7 || inUpdate != b.ClientID() {
		t.Fatalf("update handler saw %d, want the new ID %d", inUpdate, b.ClientID())
	}
}

// Blocks under the replica's own ID that wait on a missing dependency do
// not change the ID while they wait; the update that lets them integrate
// does.
func TestDoc_NewClientIDWhenPendingBlocksIntegrate(t *testing.T) {
	x := ygo.NewDocWithOptions(ygo.Options{ClientID: 3})
	a := ygo.NewDocWithOptions(ygo.Options{ClientID: 7})
	b := ygo.NewDocWithOptions(ygo.Options{ClientID: 7})
	tx, ta := ygo.NewText(x, "t"), ygo.NewText(a, "t")
	ygo.NewText(b, "t")
	insertText(t, x, tx, 0, "base")
	applyAll(t, x, a)
	insertText(t, a, ta, 4, "+a")
	// a's own edit, without x's base it depends on.
	onlyA, err := ygo.EncodeDiff(a, ygo.EncodeStateVector(x))
	if err != nil {
		t.Fatal(err)
	}
	if err := ygo.ApplyUpdate(b, onlyA); err != nil {
		t.Fatal(err)
	}
	if !ygo.HasPending(b) {
		t.Fatal("a's edit integrated without its dependency")
	}
	if b.ClientID() != 7 {
		t.Fatalf("ID changed to %d while the colliding blocks were pending", b.ClientID())
	}
	applyAll(t, x, b)
	if ygo.HasPending(b) {
		t.Fatal("still pending after the dependency arrived")
	}
	if b.ClientID() == 7 {
		t.Fatal("b kept client ID 7 after the pending blocks under it integrated")
	}
}

// Every path that applies data made elsewhere changes the ID: V2 updates
// and the loader for bytes ygo stored itself (snapshots restore with it)
// open their own non-local transactions.
func TestDoc_NewClientIDOnEveryUpdatePath(t *testing.T) {
	src := ygo.NewDocWithOptions(ygo.Options{ClientID: 7})
	insertText(t, src, ygo.NewText(src, "t"), 0, "x")
	for _, tc := range []struct {
		name  string
		apply func(d *ygo.Doc) error
	}{
		{"V2", func(d *ygo.Doc) error { return ygo.ApplyUpdateV2(d, ygo.EncodeStateAsUpdateV2(src)) }},
		{"with origin", func(d *ygo.Doc) error { return ygo.ApplyUpdateWithOrigin(d, ygo.EncodeStateAsUpdate(src), "peer") }},
		{"stored", func(d *ygo.Doc) error { return encoding.ApplyStoredUpdate(d, ygo.EncodeStateAsUpdate(src)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := ygo.NewDocWithOptions(ygo.Options{ClientID: 7})
			ygo.NewText(d, "t")
			if err := tc.apply(d); err != nil {
				t.Fatal(err)
			}
			if d.ClientID() == 7 {
				t.Fatal("kept client ID 7 after an update that wrote under it")
			}
		})
	}
}

// A subdocument's ID is its own: an update that writes under it changes
// the subdocument's ID and leaves the parent's.
func TestDoc_SubdocumentTakesItsOwnNewClientID(t *testing.T) {
	p := ygo.NewDocWithOptions(ygo.Options{ClientID: 5})
	m := ygo.NewMap(p, "m")
	txn := p.WriteTxn()
	sub := m.SetDoc(txn, "child")
	txn.Commit()
	sid := sub.ClientID()
	other := ygo.NewDocWithOptions(ygo.Options{ClientID: sid})
	insertText(t, other, ygo.NewText(other, "t"), 0, "x")
	ygo.NewText(sub, "t")
	applyAll(t, other, sub)
	if sub.ClientID() == sid {
		t.Fatal("the subdocument kept its ID after an update that wrote under it")
	}
	if p.ClientID() != 5 {
		t.Fatalf("the parent's ID changed to %d", p.ClientID())
	}
}

// ClientID is safe to read from another goroutine while commits change
// it (run with -race).
func TestDoc_ClientIDReadDuringChanges(t *testing.T) {
	d := ygo.NewDoc()
	ygo.NewText(d, "t")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = d.ClientID()
			}
		}
	}()
	changed := 0
	for range 50 {
		id := d.ClientID()
		src := ygo.NewDocWithOptions(ygo.Options{ClientID: id})
		insertText(t, src, ygo.NewText(src, "t"), 0, "x")
		applyAll(t, src, d)
		if d.ClientID() != id {
			changed++
		}
	}
	close(stop)
	wg.Wait()
	if changed != 50 {
		t.Fatalf("the ID changed %d times in 50 collisions", changed)
	}
}
