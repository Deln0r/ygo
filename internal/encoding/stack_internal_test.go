package encoding

import (
	"strings"
	"testing"
	"unsafe"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
)

func strItem(client, clock uint64, s string, origin *block.ID) Block {
	return Block{Kind: WireBlockItem, Item: &block.Item{
		ID:      block.ID{Client: client, Clock: clock},
		Origin:  origin,
		Content: block.Content{Kind: block.KindString, Str: s},
		Len:     uint64(len(s)),
		Parent:  block.Parent{Kind: block.ParentNamed, Named: "t"},
		Flags:   block.FlagCountable,
	}}
}

// A malformed update whose blocks wait on each other in a cycle (20:0 on
// 10:0, 10:0 on 20:1, which comes after 20:0) cannot be integrated: the
// stack follows 20:0 into 10, 10:0 into 20, meets the hole before 20:1 and
// walls. The run ends and every block stays queued.
func TestIntegrateStack_CycleWallsAndEnds(t *testing.T) {
	d := doc.NewDoc()
	txn := d.WriteTxn()
	defer txn.Commit()
	refs := map[uint64][]Block{
		20: {strItem(20, 0, "x", &block.ID{Client: 10, Clock: 0}), strItem(20, 1, "y", &block.ID{Client: 20, Clock: 0})},
		10: {strItem(10, 0, "z", &block.ID{Client: 20, Clock: 1})},
	}
	rest, missing := integrateStack(txn, refs)
	if len(rest[20]) != 2 || len(rest[10]) != 1 {
		t.Fatalf("queued %d blocks of 20 and %d of 10, want 2 and 1", len(rest[20]), len(rest[10]))
	}
	if got := txn.Store().GetClock(20) + txn.Store().GetClock(10); got != 0 {
		t.Fatalf("integrated %d clocks of a cycle", got)
	}
	if m, ok := missing[20]; !ok || m != 0 {
		t.Fatalf("missing %v, want 20 from the hole before 20:1 (clock 0)", missing)
	}
}

// Skips are not queued, so a real block for the same clocks that comes
// later is not kept out by one.
func TestPending_mergeBlocks_DropsSkips(t *testing.T) {
	p := NewPending()
	p.mergeBlocks(map[uint64][]Block{9: {
		strItem(9, 0, "a", nil),
		{Kind: WireBlockSkip, ID: block.ID{Client: 9, Clock: 1}, Len: 2},
		strItem(9, 3, "d", nil),
	}})
	p.mergeBlocks(map[uint64][]Block{9: {strItem(9, 1, "bc", nil)}})
	var got []uint64
	for _, b := range p.Blocks[9] {
		if b.Kind != WireBlockItem {
			t.Fatalf("queued a %v block", b.Kind)
		}
		got = append(got, b.Item.ID.Clock, b.Item.Len)
	}
	if want := []uint64{0, 1, 1, 2, 3, 1}; len(got) != len(want) || got[2] != 1 || got[3] != 2 {
		t.Fatalf("queued (clock, len) %v, want %v", got, want)
	}
}

// A queued block cut to its uncovered part keeps storage of its own: a
// substring of the decoded text would keep the whole of it reachable, and
// runs of N characters starting one clock apart then held N² bytes.
func TestPending_mergeBlocks_CutPieceOwnsItsText(t *testing.T) {
	long := strings.Repeat("y", 1000)
	p := NewPending()
	p.mergeBlocks(map[uint64][]Block{9: {strItem(9, 0, "aaaaaaaaaa", nil), strItem(9, 5, long, nil)}})
	list := p.Blocks[9]
	if len(list) != 2 || list[1].Item.ID.Clock != 10 || list[1].Item.Len != 995 {
		t.Fatalf("queued %v, want the 1000-character block cut to 10..1004", list)
	}
	cut := unsafe.StringData(list[1].Item.Content.Str)
	start := uintptr(unsafe.Pointer(unsafe.StringData(long)))
	if at := uintptr(unsafe.Pointer(cut)); at >= start && at < start+uintptr(len(long)) {
		t.Fatal("the cut piece still points into the decoded text")
	}
}
