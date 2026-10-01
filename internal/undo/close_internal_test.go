package undo

import (
	"testing"
	"time"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
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
