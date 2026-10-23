package undo

import (
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/types"
)

// TestClose_CommitInFlight drives the interleaving that deadlocked: a commit
// holds the document lock and is about to run the manager's handler (which
// takes um.mu) while Close, on another goroutine, unsubscribes (which takes
// the document lock). Close used to unsubscribe with um.mu still held. The
// time bound only detects the hang; with the fix Close returns at once.
func TestClose_CommitInFlight(t *testing.T) {
	d := doc.NewDoc()
	m := types.NewMap(d.Branch("m"))

	// Registered before the manager, so it runs first in the commit: it
	// parks the committing goroutine, document lock held, until released.
	inCommit := make(chan struct{})
	release := make(chan struct{})
	parked := false
	d.OnAfterTransaction(func(*doc.TransactionMut) {
		if !parked {
			parked = true
			close(inCommit)
			<-release
		}
	})
	um := NewUndoManager(d, []*block.Branch{m.Branch()})
	unsubscribing := make(chan struct{})
	unsubscribe := um.unsubscribe
	um.unsubscribe = func() {
		close(unsubscribing)
		unsubscribe()
	}

	committed := make(chan struct{})
	go func() {
		txn := d.WriteTxn()
		m.Set(txn, "k", int64(1))
		txn.Commit()
		close(committed)
	}()
	<-inCommit

	closed := make(chan struct{})
	go func() {
		um.Close()
		close(closed)
	}()
	<-unsubscribing
	close(release)

	for _, c := range []struct {
		name string
		ch   chan struct{}
	}{{"Close", closed}, {"the commit", committed}} {
		select {
		case <-c.ch:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s did not return: Close and a commit deadlocked", c.name)
		}
	}
	if um.CanUndo() {
		t.Error("a manager closed during the commit captured it")
	}
}

// TestClear_DuringCapture drives a Clear into the window between a
// capture keeping its deletions and publishing its step, as a Clear on
// another goroutine can land while a commit runs. The release of the
// cleared step must not take away the keep the new step needs on the
// parent both share: undo restores the parent with its remaining key, as
// yjs does when the clear comes before the capture.
func TestClear_DuringCapture(t *testing.T) {
	d := doc.NewDoc()
	m := types.NewMap(d.Branch("m"))
	txn := d.WriteTxn()
	txn.Origin = "setup"
	p := m.SetMap(txn, "p")
	p.Set(txn, "x", int64(1))
	p.Set(txn, "y", int64(2))
	txn.Commit()
	um := NewUndoManager(d, []*block.Branch{m.Branch()})
	txn = d.WriteTxn()
	p.Delete(txn, "x")
	txn.Commit()

	cleared := false
	um.afterKeep = func() {
		if !cleared {
			cleared = true
			um.Clear()
		}
	}
	txn = d.WriteTxn()
	m.Delete(txn, "p")
	txn.Commit()
	if !cleared {
		t.Fatal("the hook never ran")
	}
	if !um.Undo() {
		t.Fatal("Undo found nothing to restore")
	}
	restored, ok := m.Get("p").(*types.Map)
	if !ok {
		t.Fatalf("p after undo = %v, want a map", m.Get("p"))
	}
	if got := restored.Get("y"); got != int64(2) {
		t.Fatalf("p.y after undo = %v, want 2", got)
	}
}

// TestClear_DuringCaptureRekeepsAncestors: the step captured while Clear
// ran deletes a child whose parent the cleared step had also kept. The
// release un-keeps the parent; re-keeping the surviving step must keep it
// again although the child itself is still kept, so a later untracked
// deletion of the parent does not collect what the step needs. The state
// is yjs 13.6.33's for the clear made before the capture.
func TestClear_DuringCaptureRekeepsAncestors(t *testing.T) {
	const want = "010301002701016d017001280001000178017d01280001000179017d020101010003"
	d := doc.NewDocWithOptions(doc.Options{ClientID: 1})
	m := types.NewMap(d.Branch("m"))
	txn := d.WriteTxn()
	txn.Origin = "setup"
	p := m.SetMap(txn, "p")
	txn.Commit()
	txn = d.WriteTxn()
	txn.Origin = "setup"
	p.Set(txn, "x", int64(1))
	p.Set(txn, "y", int64(2))
	txn.Commit()
	um := NewUndoManager(d, []*block.Branch{m.Branch()})
	txn = d.WriteTxn()
	p.Delete(txn, "x")
	txn.Commit()

	cleared := false
	um.afterKeep = func() {
		if !cleared {
			cleared = true
			um.Clear()
		}
	}
	txn = d.WriteTxn()
	p.Delete(txn, "y")
	txn.Commit()
	txn = d.WriteTxn()
	txn.Origin = "remote"
	m.Delete(txn, "p")
	txn.Commit()
	if !cleared {
		t.Fatal("the hook never ran")
	}
	if got := hex.EncodeToString(encoding.EncodeStateAsUpdate(d)); got != want {
		t.Errorf("state\n got  %s\n want %s", got, want)
	}
}

// TestUndo_ConcurrentCommitIsAnOrdinaryEdit: Undo sets its flag and then
// waits for the doc lock; a commit on another goroutine that holds the
// lock runs the handler meanwhile. That commit is an ordinary tracked edit
// (it goes on the undo stack and drops the redo history), not the undo's
// own step, as in either serial order of yjs.
func TestUndo_ConcurrentCommitIsAnOrdinaryEdit(t *testing.T) {
	d := doc.NewDoc()
	m := types.NewMap(d.Branch("m"))
	um := NewUndoManager(d, []*block.Branch{m.Branch()}, Options{CaptureTimeout: -1})
	txn := d.WriteTxn()
	m.Set(txn, "a", int64(1))
	txn.Commit()

	// The state Undo is in while it waits: step popped, flag set.
	um.mu.Lock()
	um.undoing = true
	um.mu.Unlock()
	txn = d.WriteTxn()
	m.Set(txn, "b", int64(2))
	txn.Commit()
	um.mu.Lock()
	um.undoing = false
	undo, redo := len(um.undoStack), len(um.redoStack)
	um.mu.Unlock()
	if undo != 2 || redo != 0 {
		t.Fatalf("stacks after a concurrent edit: undo %d redo %d, want 2 and 0", undo, redo)
	}
}

// TestUndo_LinearizableWithConcurrentCommit: a commit holding the doc lock
// when Undo starts lands wholly before the undo: it joins the top step
// (within the capture timeout), and that step is the one undone, as in
// yjs when the edit comes first. It must not join the step below.
func TestUndo_LinearizableWithConcurrentCommit(t *testing.T) {
	d := doc.NewDoc()
	m := types.NewMap(d.Branch("m"))
	um := NewUndoManager(d, []*block.Branch{m.Branch()}, Options{CaptureTimeout: time.Hour})
	txn := d.WriteTxn()
	m.Set(txn, "a", int64(1))
	txn.Commit()
	um.StopCapturing()
	txn = d.WriteTxn()
	m.Set(txn, "b", int64(2))
	txn.Commit()

	txn = d.WriteTxn() // another goroutine's edit, holding the lock
	m.Set(txn, "c", int64(3))
	done := make(chan bool)
	go func() { done <- um.Undo() }()
	time.Sleep(20 * time.Millisecond) // Undo is (most likely) waiting for the lock
	txn.Commit()
	if !<-done {
		t.Fatal("first Undo found nothing")
	}
	keys := func() string {
		r := d.ReadTxn()
		defer r.Close()
		out := ""
		for _, k := range []string{"a", "b", "c"} {
			if m.Has(k) {
				out += k
			}
		}
		return out
	}
	if got := keys(); got != "a" {
		t.Fatalf("after the first Undo: %q, want a (b and c undone together)", got)
	}
	if !um.Undo() || keys() != "" {
		t.Fatalf("after the second Undo: %q, want nothing", keys())
	}
	if um.Undo() {
		t.Fatal("a third Undo found something")
	}
}

// TestClear_FromUpdateHandlerBeforeFormatCleanup: a Clear from the update
// handler of a remote transaction that asks for a format cleanup releases
// what the dropped step kept before the cleanup's observers run. yjs clear
// releases at once, inside the remote's update emit, before the cleanup's
// lifecycle. The cleanup is then captured as a step of its own.
func TestClear_FromUpdateHandlerBeforeFormatCleanup(t *testing.T) {
	a := doc.NewDocWithOptions(doc.Options{ClientID: 3141592653})
	b := doc.NewDocWithOptions(doc.Options{ClientID: 271828182})
	ta, tb := types.NewText(a.Branch("t")), types.NewText(b.Branch("t"))
	txn := a.WriteTxn()
	txn.Origin = "setup"
	ta.Insert(txn, 0, "xyz")
	txn.Commit()
	if err := encoding.ApplyUpdate(b, encoding.EncodeStateAsUpdate(a)); err != nil {
		t.Fatal(err)
	}
	um := NewUndoManager(a, []*block.Branch{ta.Branch()})
	txn = a.WriteTxn()
	ta.Delete(txn, 2, 1)
	txn.Commit()
	var z *block.Item
	for it := ta.Branch().Start; it != nil; it = it.Right {
		if it.IsDeleted() {
			z = it
		}
	}
	if z == nil || !z.IsKeep() {
		t.Fatal("the tracked delete of z kept nothing")
	}
	txn = a.WriteTxn()
	txn.Origin = "setup"
	ta.Format(txn, 0, 2, types.Attrs{"bold": true})
	txn.Commit()
	txn = b.WriteTxn()
	tb.Format(txn, 0, 2, types.Attrs{"bold": true})
	txn.Commit()

	var keptAtEvent []bool
	ta.Observe(func(*types.TextEvent) { keptAtEvent = append(keptAtEvent, z.IsKeep()) })
	updates := 0
	a.OnUpdate(func(_ []byte, origin any) {
		updates++
		if origin == "remote" {
			um.Clear()
		}
	})
	if err := encoding.ApplyUpdateWithOrigin(a, encoding.EncodeStateAsUpdate(b), "remote"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(keptAtEvent) != "[true false]" {
		t.Fatalf("z kept at the remote and the cleanup event: %v, want [true false]", keptAtEvent)
	}
	if updates != 2 {
		t.Fatalf("%d updates, want the remote one and the cleanup", updates)
	}
	um.mu.Lock()
	steps := len(um.undoStack)
	um.mu.Unlock()
	if steps != 1 {
		t.Fatalf("%d steps after the Clear, want the cleanup's", steps)
	}
}
