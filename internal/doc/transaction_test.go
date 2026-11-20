package doc

import (
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Deln0r/ygo/internal/block"
)

func TestReadTxn_BasicLifecycle(t *testing.T) {
	d := NewDoc()
	txn := d.ReadTxn()

	if txn.Doc() != d {
		t.Error("Doc() should return the originating Doc")
	}
	if txn.Store() != d.store {
		t.Error("Store() should return the doc's BlockStore")
	}

	txn.Close()
	// Idempotent
	txn.Close()
}

func TestWriteTxn_BasicLifecycle(t *testing.T) {
	d := NewDoc()
	txn := d.WriteTxn()

	if txn.Doc() != d {
		t.Error("Doc() should return the originating Doc")
	}
	if txn.Store() != d.store {
		t.Error("Store() should return the doc's BlockStore")
	}
	txn.Origin = "test-origin"
	if got := txn.Origin; got != "test-origin" {
		t.Errorf("Origin not preserved: got %v", got)
	}

	txn.Commit()
	// Idempotent
	txn.Commit()
}

func TestWriteTxn_BlocksConcurrentReadTxn(t *testing.T) {
	d := NewDoc()
	w := d.WriteTxn()
	defer w.Commit()

	var entered atomic.Bool
	done := make(chan struct{})
	go func() {
		r := d.ReadTxn()
		entered.Store(true)
		r.Close()
		close(done)
	}()

	// Give the goroutine a chance to attempt the lock.
	time.Sleep(50 * time.Millisecond)
	if entered.Load() {
		t.Fatal("ReadTxn entered while WriteTxn was still open")
	}

	w.Commit() // Release the lock; goroutine should proceed.
	select {
	case <-done:
		// Expected.
	case <-time.After(time.Second):
		t.Fatal("ReadTxn did not proceed after WriteTxn.Commit")
	}
	if !entered.Load() {
		t.Error("ReadTxn never entered (timeout in select?)")
	}
}

func TestReadTxn_AllowsConcurrentReadTxn(t *testing.T) {
	d := NewDoc()
	r1 := d.ReadTxn()
	defer r1.Close()

	done := make(chan struct{})
	go func() {
		r2 := d.ReadTxn()
		r2.Close()
		close(done)
	}()

	select {
	case <-done:
		// Expected: two read txns coexist.
	case <-time.After(time.Second):
		t.Fatal("Concurrent ReadTxn was blocked; sync.RWMutex semantics broken")
	}
}

func TestWriteTxn_StoreMutationVisibleToReadTxn(t *testing.T) {
	d := NewDoc()

	// Push a block under the write lock.
	w := d.WriteTxn()
	w.Store().PushBlock(&block.Item{
		ID:      block.ID{Client: d.ClientID(), Clock: 0},
		Len:     5,
		Content: block.Content{Kind: block.KindString, Str: "hello"},
	})
	w.Commit()

	// Read from a fresh ReadTxn after release.
	r := d.ReadTxn()
	defer r.Close()
	it := r.Store().GetItem(block.ID{Client: d.ClientID(), Clock: 0})
	if it == nil {
		t.Fatal("Block pushed inside WriteTxn not visible in subsequent ReadTxn")
	}
	if it.Content.Str != "hello" {
		t.Errorf("Content.Str=%q want %q", it.Content.Str, "hello")
	}
}

// TestInvalidatePositionalMarkers_OnlyStale: an applied update resets the
// markers of the types whose positions moved since the last reset, not of
// every type changed earlier in the transaction (MergeUpdatesV2 applies
// all its inputs in one, and the reset per input was quadratic).
func TestInvalidatePositionalMarkers_OnlyStale(t *testing.T) {
	d := NewDoc()
	a, b := d.Branch("a"), d.Branch("b")
	w := d.WriteTxn()
	defer w.Commit()
	w.AddChangedType(a, nil)
	w.AddChangedType(b, nil)
	w.AddChangedType(a, nil)
	if !slices.Equal(w.staleMarkers, []*block.Branch{a, b}) {
		t.Fatalf("stale %v, want a, b once each", w.staleMarkers)
	}
	w.InvalidatePositionalMarkers()
	if len(w.staleMarkers) != 0 {
		t.Fatalf("stale after the reset: %v", w.staleMarkers)
	}
	key := "k"
	w.AddChangedType(a, &key) // a key, not a position
	w.AddChangedType(b, nil)
	if !slices.Equal(w.staleMarkers, []*block.Branch{b}) {
		t.Fatalf("stale %v, want b alone", w.staleMarkers)
	}
	if !w.PositionalChanged(a) || !w.PositionalChanged(b) {
		t.Fatal("the transaction no longer reports a positional change")
	}
}

// TestFollowUp_CommitIsRunByItsOpener: a follow-up committed by hand is
// left to the Commit that opened it, which runs it once, after the
// transaction that opened it; committing it would run it twice and release
// the lock twice.
func TestFollowUp_CommitIsRunByItsOpener(t *testing.T) {
	d := NewDoc()
	var order []any
	d.OnAfterTransaction(func(tr *TransactionMut) {
		order = append(order, tr.Origin)
		if tr.Origin == "root" {
			f := OpenFollowUp(tr)
			f.Origin = "follow-up"
			f.Commit()
		}
	})
	w := d.WriteTxn()
	w.Origin = "root"
	w.Commit()
	if fmt.Sprint(order) != "[root follow-up]" {
		t.Fatalf("after-transaction order %v, want [root follow-up]", order)
	}
	// The lock is free again: a second transaction opens and commits.
	w = d.WriteTxn()
	w.Commit()
	if f := OpenFollowUp(w); f != nil {
		t.Fatal("a follow-up opened after its opener's Commit finished")
	}
}
