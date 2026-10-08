package ygo_test

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/Deln0r/ygo"
)

// deltaString renders a text delta compactly: "d2 iX r1".
func deltaString(ops []ygo.DeltaOp) string {
	var parts []string
	for _, op := range ops {
		switch {
		case op.Insert != "":
			parts = append(parts, "i"+op.Insert)
		case op.Delete > 0:
			parts = append(parts, fmt.Sprintf("d%d", op.Delete))
		case op.Retain > 0:
			parts = append(parts, fmt.Sprintf("r%d", op.Retain))
		}
	}
	return strings.Join(parts, " ")
}

// A text item deleted whole by an update's delete set and then split by a
// block that was waiting in the pending queue for that update: every piece
// counts as deleted in the transaction, for the observer delta and for GC,
// as yjs tests transaction.deleteSet by range. Bytes from yjs 13.6.33:
// client 3 writes "abc"; client 2 inserts "Z" at 3 and then "W" at 1;
// client 3 deletes 0..3; the receiver (client 99) gets "abc", then "W"
// (queued for "Z"), then mergeUpdates("Z", the delete).
func TestApply_SplitOfDeletedItemCountsAsDeleted(t *testing.T) {
	for _, c := range []struct {
		gc    bool
		state string
	}{
		{true, "02020300010101740181030002020200840302015ac40300030101570103010003"},
		{false, "02020300040101740161840300026263020200840302015ac40300030101570103010003"},
	} {
		d := ygo.NewDocWithOptions(ygo.Options{ClientID: 99, DisableGC: !c.gc})
		txt := ygo.NewText(d, "t")
		var last string
		txt.Observe(func(e *ygo.TextEvent) { last = deltaString(e.Delta) })
		for _, u := range []string{"01010300040101740361626300", "01010201c403000301015700", "01010200840302015a0103010003"} {
			b, _ := hex.DecodeString(u)
			if err := ygo.ApplyUpdate(d, b); err != nil {
				t.Fatal(err)
			}
		}
		if last != "d1 iW d2 iZ" {
			t.Errorf("gc=%v: delta %q, want yjs's \"d1 iW d2 iZ\"", c.gc, last)
		}
		if got := txt.String(); got != "WZ" {
			t.Errorf("gc=%v: text %q", c.gc, got)
		}
		if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != c.state {
			t.Errorf("gc=%v: state\n got  %s\n want %s", c.gc, got, c.state)
		}
	}
}

// The same with the pieces the other way round: the queued block arrives
// first and the update holding the deleted item's neighbour and the
// delete comes second (the shape of a sync-step-2 diff). Bytes from yjs:
// client 300 writes "abc", client 200 inserts "Y" at 3 and "X" at 2,
// client 300 deletes 0..3; the receiver (client 100) has "abc" and gets
// "X" (queued), then mergeUpdates("Y", the delete).
func TestApply_SplitOfDeletedItemAfterQueuedBlock(t *testing.T) {
	const state = "0202ac0200010101740281ac02010102c8010084ac02020159c4ac0201ac0202015801ac02010003"
	c := ygo.NewDocWithOptions(ygo.Options{ClientID: 300})
	ct := ygo.NewText(c, "t")
	txn := c.WriteTxn()
	if err := ct.Insert(txn, 0, "abc"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	r := ygo.NewDocWithOptions(ygo.Options{ClientID: 100})
	txt := ygo.NewText(r, "t")
	if err := ygo.ApplyUpdate(r, ygo.EncodeStateAsUpdate(c)); err != nil {
		t.Fatal(err)
	}
	var last string
	txt.Observe(func(e *ygo.TextEvent) { last = deltaString(e.Delta) })
	for _, u := range []string{"0101c80101c4ac0201ac0202015800", "0101c8010084ac0202015901ac02010003"} {
		b, _ := hex.DecodeString(u)
		if err := ygo.ApplyUpdateWithOrigin(r, b, "remote"); err != nil {
			t.Fatal(err)
		}
	}
	if last != "d2 iX d1 iY" {
		t.Errorf("delta %q, want yjs's \"d2 iX d1 iY\"", last)
	}
	if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(r)); got != state {
		t.Errorf("state\n got  %s\n want %s", got, state)
	}
}

// The older shape of the same gap, already in 1.23.0: a block waits for an
// insert, the insert's deletion waits in the pending delete set, then the
// insert arrives; the waiting block splits the item the queued deletion
// deleted. Bytes from yjs 13.6.33 (receiver client 99).
func TestApply_SplitAfterPendingDelete(t *testing.T) {
	const state = "02020200010101740281020101010100c40201020201580102010003"
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 99})
	txt := ygo.NewText(d, "t")
	for _, u := range []string{"01010100c402010202015800", "000102010003", "01010200040101740361626300"} {
		b, _ := hex.DecodeString(u)
		if err := ygo.ApplyUpdate(d, b); err != nil {
			t.Fatal(err)
		}
	}
	if got := txt.String(); got != "X" {
		t.Errorf("text %q, want X", got)
	}
	if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != state {
		t.Errorf("state\n got  %s\n want %s", got, state)
	}
}

// An update's delete set applies in the order its clients are written, as
// yjs readAndApplyDeleteSet reads it, not sorted: yjs and ygo write them
// from the highest client down, but any order is valid on the wire. The
// order of the deletions is the order an undo of the tracked update
// restores them in. Bytes from yjs 13.6.33: client 271828182 sets m.a,
// client 141421356 sets m.b, a delete-set-only update lists the lower
// client first; the receiver (client 3141592653) applies it tracked and
// undoes.
func TestApply_DeleteSetInWireOrder(t *testing.T) {
	const state = "0302cdcc83da0b00a8acd6b74300017d02a8d689cf810100017d0101d689cf8101002801016d0161017d0101acd6b743002801016d0162017d0202d689cf8101010001acd6b743010001"
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 3141592653})
	m := ygo.NewMap(d, "m")
	for _, u := range []string{"0101d689cf8101002801016d0161017d0100", "0101acd6b743002801016d0162017d0200"} {
		b, _ := hex.DecodeString(u)
		if err := ygo.ApplyUpdateWithOrigin(d, b, "setup"); err != nil {
			t.Fatal(err)
		}
	}
	um := ygo.NewUndoManagerWithOptions(d, ygo.UndoManagerOptions{CaptureTimeout: -1, TrackedOrigins: map[any]struct{}{"tracked": {}}}, m)
	b, _ := hex.DecodeString("0002acd6b743010001d689cf8101010001")
	if err := ygo.ApplyUpdateWithOrigin(d, b, "tracked"); err != nil {
		t.Fatal(err)
	}
	if !um.Undo() {
		t.Fatal("Undo found nothing")
	}
	if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != state {
		t.Errorf("state\n got  %s\n want %s", got, state)
	}
}

// A V1 delete set that lists a client twice deletes the ranges of both
// entries, as yjs applies each entry it reads; the second used to replace
// the first. Bytes from yjs 13.6.33: client 1 pushes [10], [20], [30]; the
// delete set lists client 1 with [0,1) and again with [2,1); yjs ends with
// [20].
func TestApply_DeleteSetClientListedTwice(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 99})
	a := ygo.NewArray(d, "a")
	for _, u := range []string{"0101010008010161037d0a7d147d1e00", "00020101000101010201"} {
		b, _ := hex.DecodeString(u)
		if err := ygo.ApplyUpdate(d, b); err != nil {
			t.Fatal(err)
		}
	}
	if got := fmt.Sprint(a.ToSlice()); got != "[20]" {
		t.Errorf("array %s, want [20]", got)
	}
}
