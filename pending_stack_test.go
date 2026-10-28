package ygo_test

import (
	"encoding/hex"
	"testing"

	ygo "github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/internal/encoding"
)

// The same clocks queued in different splits collapse into one block, as
// yjs's merged pending update keeps them. Bytes from yjs 13.6.33: H's
// "abcd" (271828182:0..3) in a text that L (57721566:0) created; gcH is a
// document that collected it, whole is H's own state, split is a peer
// whose store holds the run cut after "a". Each live encoding waits on L
// and walls H; without merging, every split kept a covered suffix, and N
// split points kept N(N+1)/2 characters for as long as L stayed away.
// yjs's queue after the same four updates is one item of length 4.
func TestPending_SplitsOfOneRunCollapse(t *testing.T) {
	gcH, _ := hex.DecodeString("0101d689cf810100000402d689cf8101010004de85c31b010001")
	whole, _ := hex.DecodeString("0101d689cf8101000400de85c31b00046162636400")
	split, _ := hex.DecodeString("0102d689cf8101000400de85c31b00016184d689cf8101000362636401f3c8db07010001")
	lAll, _ := hex.DecodeString("0201d689cf810100000401de85c31b002101016d01700102d689cf8101010004de85c31b010001")
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 3141592653})
	for k, u := range [][]byte{gcH, whole, split, split} {
		if err := ygo.ApplyUpdate(d, u); err != nil {
			t.Fatal(err)
		}
		if k == 0 {
			continue
		}
		rtxn := d.ReadTxn()
		p := encoding.GetPending(rtxn)
		var blocks []encoding.Block
		if p != nil {
			blocks = p.Blocks[271828182]
		}
		n := p.BlockCount()
		rtxn.Close()
		if n != 1 || len(blocks) != 1 || blocks[0].Item == nil || blocks[0].Item.Len != 4 {
			t.Fatalf("after update %d: %d queued blocks %v, want one item of length 4", k, n, blocks)
		}
	}
	if err := ygo.ApplyUpdate(d, lAll); err != nil {
		t.Fatal(err)
	}
	// L's state lets every block in. What stays queued is the deletion of
	// the peer's own 16180339:0, which split carried and nobody sends;
	// yjs keeps it in pendingDs too.
	rtxn := d.ReadTxn()
	defer rtxn.Close()
	p := encoding.GetPending(rtxn)
	if n := p.BlockCount(); n != 0 {
		t.Fatalf("%d blocks still queued after L's state, want 0", n)
	}
	var left []uint64
	p.DeleteSet.Iterate(func(client uint64, ranges []encoding.Range) {
		if len(ranges) > 0 {
			left = append(left, client)
		}
	})
	if len(left) != 1 || left[0] != 16180339 {
		t.Fatalf("queued deletes from %v, want only 16180339's", left)
	}
}

// A queue holding only a delete still names what it waits for: the
// deletion of 10:0, alone, makes MissingSV name client 10 at the clock
// the document has, 0, and the peer's diff against that vector lets it
// in. A vector from the blocks alone was empty here, which the API
// documents as "nothing is queued".
func TestMissingSV_DeleteOnly(t *testing.T) {
	a := ygo.NewDocWithOptions(ygo.Options{ClientID: 10})
	ta := ygo.NewText(a, "t")
	txn := a.WriteTxn()
	ta.Insert(txn, 0, "ab")
	txn.Commit()
	b := ygo.NewDocWithOptions(ygo.Options{ClientID: 11})
	tb := ygo.NewText(b, "t")
	if err := ygo.ApplyUpdate(b, ygo.EncodeStateAsUpdate(a)); err != nil {
		t.Fatal(err)
	}
	txn = b.WriteTxn()
	tb.Delete(txn, 0, 1)
	txn.Commit()
	onlyDelete, err := ygo.EncodeDiff(b, ygo.EncodeStateVector(a))
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(onlyDelete); got != "00010a010001" {
		t.Fatalf("setup: the diff is %s, want the deletion of 10:0 alone", got)
	}
	c := ygo.NewDocWithOptions(ygo.Options{ClientID: 12})
	tc := ygo.NewText(c, "t")
	if err := ygo.ApplyUpdate(c, onlyDelete); err != nil {
		t.Fatal(err)
	}
	sv := ygo.MissingSV(c)
	if got := hex.EncodeToString(sv); got != "010a00" {
		t.Fatalf("MissingSV %s, want 010a00 (client 10 at 0)", got)
	}
	fill, err := ygo.EncodeDiff(a, sv)
	if err != nil {
		t.Fatal(err)
	}
	if err := ygo.ApplyUpdate(c, fill); err != nil {
		t.Fatal(err)
	}
	if ygo.HasPending(c) || tc.String() != "b" {
		t.Fatalf("after the fetch: pending %v, text %q, want none and \"b\"", ygo.HasPending(c), tc.String())
	}
}
