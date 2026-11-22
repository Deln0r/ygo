package ygo_test

import (
	"bytes"
	"testing"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/internal/doc"
)

// gcSubtreeDoc builds client 1's map m with p = [C], C = [1, [9], 2], deletes
// C under an UndoManager and clears it, so C's subtree is deleted and no
// longer kept, then makes one unrelated change (key x).
func gcSubtreeDoc(t *testing.T) (*ygo.Doc, *ygo.Map) {
	t.Helper()
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	m := ygo.NewMap(d, "m")
	txn := d.WriteTxn()
	p := m.SetArray(txn, "p")
	c := p.InsertArray(txn, 0)
	c.InsertRange(txn, 0, []any{float64(1)})
	c.InsertArray(txn, 1).InsertRange(txn, 0, []any{float64(9)})
	c.InsertRange(txn, 2, []any{float64(2)})
	txn.Commit()
	um := ygo.NewUndoManager(d, m)
	txn = d.WriteTxn()
	p.Delete(txn, 0, 1)
	txn.Commit()
	um.Clear()
	return d, m
}

// A type deleted in a follow-up of another transaction (the transactions
// yjs opens while one is being cleaned up) gets its merge candidates merged
// by the follow-up's own commit, after the follow-up's collection, as when
// it is deleted in a transaction of its own: the outer transaction's commit
// must not take them first, when the subtree is not collected yet.
func TestGCMerge_FollowUpOwnsItsCandidates(t *testing.T) {
	own, m := gcSubtreeDoc(t)
	txn := own.WriteTxn()
	m.Set(txn, "x", float64(1))
	txn.Commit()
	txn = own.WriteTxn()
	txn.Origin = "other"
	m.Delete(txn, "p")
	txn.Commit()

	viaFollowUp, m2 := gcSubtreeDoc(t)
	opened := false
	off := viaFollowUp.OnAfterTransaction(func(tm *doc.TransactionMut) {
		if opened || doc.Opener(tm) != nil {
			return
		}
		opened = true
		if f := doc.OpenFollowUp(tm); f != nil {
			f.Origin = "other"
			m2.Delete(f, "p")
		}
	})
	txn = viaFollowUp.WriteTxn()
	m2.Set(txn, "x", float64(1))
	txn.Commit()
	off()
	if !opened {
		t.Fatal("the follow-up was not opened")
	}
	if got, want := ygo.EncodeStateAsUpdate(viaFollowUp), ygo.EncodeStateAsUpdate(own); !bytes.Equal(got, want) {
		t.Fatalf("state via a follow-up:\n got  %x\n want %x (the same delete in a transaction of its own)", got, want)
	}
}
