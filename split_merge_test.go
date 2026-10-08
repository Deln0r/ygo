package ygo_test

import (
	"encoding/hex"
	"testing"

	"github.com/Deln0r/ygo"
)

// Pieces of an item a transaction splits are merged again at commit when
// they end up adjacent, as yjs does with transaction._mergeStructs: client
// 1 types "a", then "b" (one struct "ab"), and client 2's "X" typed after
// "a" splits it on integration and lands after "b". Bytes from yjs 13.6.33.
func TestCommit_RemergesSplitPieces(t *testing.T) {
	const state = "0201020084010001580101000401017402616200"
	a := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	b := ygo.NewDocWithOptions(ygo.Options{ClientID: 2})
	ta, tb := ygo.NewText(a, "t"), ygo.NewText(b, "t")
	insert := func(d *ygo.Doc, t2 *ygo.Text, idx uint64, s string) {
		txn := d.WriteTxn()
		if err := t2.Insert(txn, idx, s); err != nil {
			t.Fatal(err)
		}
		txn.Commit()
	}
	insert(a, ta, 0, "a")
	if err := ygo.ApplyUpdate(b, ygo.EncodeStateAsUpdate(a)); err != nil {
		t.Fatal(err)
	}
	insert(a, ta, 1, "b")
	insert(b, tb, 1, "X")
	syncDocs(t, b, a)
	if got := ta.String(); got != "abX" {
		t.Fatalf("text %q, want abX", got)
	}
	if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(a)); got != state {
		t.Errorf("state\n got  %s\n want %s", got, state)
	}
}

// A deleted item that arrives in the commit after a kept tombstone is
// collected before anything merges, so it cannot merge into the kept
// tombstone and keep its content: yjs runs GC before the merge. Bytes from
// yjs 13.6.33 (client 1 with GC off inserts 3 after [1,2] and deletes it;
// client 2 deleted [1,2] under an UndoManager and then receives that).
func TestCommit_CollectsBeforeMergingIntoKept(t *testing.T) {
	const (
		event = "01010102810101010101010201"
		state = "0102010008010161027d017d02810101010101010003"
	)
	a := ygo.NewDocWithOptions(ygo.Options{ClientID: 1, DisableGC: true})
	aa := ygo.NewArray(a, "a")
	b := ygo.NewDocWithOptions(ygo.Options{ClientID: 2})
	ba := ygo.NewArray(b, "a")
	ygo.NewUndoManagerWithOptions(b, ygo.UndoManagerOptions{CaptureTimeout: -1}, ba)
	writeTxn(a, func(x *ygo.TransactionMut) { aa.InsertRange(x, 0, []any{int64(1), int64(2)}) })
	if err := ygo.ApplyUpdateWithOrigin(b, ygo.EncodeStateAsUpdate(a), "remote"); err != nil {
		t.Fatal(err)
	}
	writeTxn(a, func(x *ygo.TransactionMut) { aa.InsertRange(x, 2, []any{int64(3)}) })
	writeTxn(a, func(x *ygo.TransactionMut) { aa.Delete(x, 2, 1) })
	writeTxn(b, func(x *ygo.TransactionMut) { ba.Delete(x, 0, 2) })
	var got string
	b.OnUpdate(func(u []byte, _ any) { got = hex.EncodeToString(u) })
	u, err := ygo.EncodeDiff(a, ygo.EncodeStateVector(b))
	if err != nil {
		t.Fatal(err)
	}
	if err := ygo.ApplyUpdateWithOrigin(b, u, "remote"); err != nil {
		t.Fatal(err)
	}
	if got != event {
		t.Errorf("update event\n got  %s\n want %s", got, event)
	}
	if s := hex.EncodeToString(ygo.EncodeStateAsUpdate(b)); s != state {
		t.Errorf("state\n got  %s\n want %s", s, state)
	}
}

// MergeUpdates carries deleted content, as yjs mergeUpdates does, so a
// receiver with GC off (version history, snapshots) keeps it; the merge
// used to run through a document with GC on and collect it. Bytes from
// yjs 13.6.33: a GC-off document writes "abc" and deletes "b"; the
// receiver applies the merge of the two update events.
func TestMergeUpdates_KeepsDeletedContent(t *testing.T) {
	const receiver = "01030100040101740161840100016284010101630101010101"
	var ups [][]byte
	for _, u := range []string{"01010100040101740361626300", "000101010101"} {
		b, _ := hex.DecodeString(u)
		ups = append(ups, b)
	}
	merged, err := ygo.MergeUpdates(ups)
	if err != nil {
		t.Fatal(err)
	}
	r := ygo.NewDocWithOptions(ygo.Options{ClientID: 9, DisableGC: true})
	if err := ygo.ApplyUpdate(r, merged); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(r)); got != receiver {
		t.Errorf("receiver state\n got  %s\n want %s", got, receiver)
	}
}
