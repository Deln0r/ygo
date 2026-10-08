package ygo_test

import (
	"encoding/hex"
	"testing"

	"github.com/Deln0r/ygo"
)

func syncDocs(t *testing.T, from, to *ygo.Doc) {
	t.Helper()
	u, err := ygo.EncodeDiff(from, ygo.EncodeStateVector(to))
	if err != nil {
		t.Fatal(err)
	}
	if err := ygo.ApplyUpdate(to, u); err != nil {
		t.Fatal(err)
	}
}

func writeTxn(d *ygo.Doc, f func(*ygo.TransactionMut)) {
	txn := d.WriteTxn()
	f(txn)
	txn.Commit()
}

// When the commit-time merge folds a key's current winner into the item to
// its left, the map points at the merged item, as yjs tryToMergeWithLefts
// does; it used to keep pointing at the absorbed one, and a later write on
// the key resolved against a dead item, so the replicas diverged for good.
// Bytes from yjs 13.6.33 for the same edits.
func TestMap_WinnerAfterMerge(t *testing.T) {
	const state = "0202cdcc83da0b002101016d016102a8cdcc83da0b0101770866726f6d4869676801d689cf810100a1cdcc83da0b010102cdcc83da0b010002d689cf8101010001"
	d0 := ygo.NewDocWithOptions(ygo.Options{ClientID: 3141592653})
	d1 := ygo.NewDocWithOptions(ygo.Options{ClientID: 271828182})
	m0, m1 := ygo.NewMap(d0, "m"), ygo.NewMap(d1, "m")
	writeTxn(d0, func(x *ygo.TransactionMut) { m0.Set(x, "a", int64(1)) })
	writeTxn(d0, func(x *ygo.TransactionMut) { m0.Set(x, "a", int64(2)) })
	writeTxn(d0, func(x *ygo.TransactionMut) { m0.Delete(x, "a") })
	syncDocs(t, d0, d1)
	writeTxn(d0, func(x *ygo.TransactionMut) { m0.Set(x, "a", "fromHigh") })
	writeTxn(d1, func(x *ygo.TransactionMut) { m1.Set(x, "a", "fromLow") })
	syncDocs(t, d1, d0)
	syncDocs(t, d0, d1)
	if a0, a1 := m0.Get("a"), m1.Get("a"); a0 != "fromHigh" || a1 != "fromHigh" {
		t.Errorf("a = %v / %v, want fromHigh on both, as yjs", a0, a1)
	}
	for i, d := range []*ygo.Doc{d0, d1} {
		if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != state {
			t.Errorf("doc %d state\n got  %s\n want %s", i, got, state)
		}
	}
}

// When a key's winner is split, its right piece becomes the winner, as
// yjs splitItem sets _map; the map kept the left piece, so the next local
// Set took the wrong left neighbour, lost to the key's history and was
// deleted at once. Bytes from yjs 13.6.33 for the same edits.
func TestMap_WinnerAfterSplit(t *testing.T) {
	const state = "0302cdcc83da0b002101016d016101a1cdcc83da0b000101d689cf810100a8cdcc83da0b010177015901acd6b74300a1cdcc83da0b000102cdcc83da0b010002acd6b743010001"
	d0 := ygo.NewDocWithOptions(ygo.Options{ClientID: 3141592653})
	d1 := ygo.NewDocWithOptions(ygo.Options{ClientID: 271828182})
	d2 := ygo.NewDocWithOptions(ygo.Options{ClientID: 141421356})
	m0, m1, m2 := ygo.NewMap(d0, "m"), ygo.NewMap(d1, "m"), ygo.NewMap(d2, "m")
	writeTxn(d0, func(x *ygo.TransactionMut) { m0.Set(x, "a", int64(1)) })
	syncDocs(t, d0, d2)
	writeTxn(d0, func(x *ygo.TransactionMut) { m0.Set(x, "a", int64(2)) })
	writeTxn(d0, func(x *ygo.TransactionMut) { m0.Delete(x, "a") })
	syncDocs(t, d0, d1)
	writeTxn(d2, func(x *ygo.TransactionMut) { m2.Set(x, "a", "X") })
	syncDocs(t, d2, d1)
	writeTxn(d1, func(x *ygo.TransactionMut) { m1.Set(x, "a", "Y") })
	if got := m1.Get("a"); got != "Y" {
		t.Errorf("a = %v after the local write, want Y, as yjs", got)
	}
	if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d1)); got != state {
		t.Errorf("state\n got  %s\n want %s", got, state)
	}
}
