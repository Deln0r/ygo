package encoding

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/Deln0r/ygo/internal/block"
)

// TestPending_mergeBlocks_KeepsTheTailAtOneClock: two blocks can start at the
// same clock and cover different amounts of it, because peers split a
// client's run at different points. The queue keeps what came first and cuts
// the later block to what it adds, as yjs mergeUpdatesV2 merges its queue:
// the tail is never lost, and a block that adds nothing is dropped.
func TestPending_mergeBlocks_KeepsTheTailAtOneClock(t *testing.T) {
	type piece struct {
		clock uint64
		str   string
	}
	for _, tc := range []struct {
		name  string
		order []string
		want  []piece
	}{
		{"shorter first", []string{"ab", "abcde"}, []piece{{10, "ab"}, {12, "cde"}}},
		{"longer first", []string{"abcde", "ab"}, []piece{{10, "abcde"}}},
		{"the same twice", []string{"abcde", "abcde"}, []piece{{10, "abcde"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPending()
			for _, s := range tc.order {
				p.mergeBlocks(map[uint64][]Block{9: {strItem(9, 10, s, nil)}})
			}
			list := p.Blocks[9]
			if len(list) != len(tc.want) {
				t.Fatalf("queued %d blocks, want %d", len(list), len(tc.want))
			}
			for i, w := range tc.want {
				if it := list[i].Item; it.ID.Clock != w.clock || it.Content.Str != w.str {
					t.Fatalf("block %d = %d %q, want %d %q", i, it.ID.Clock, it.Content.Str, w.clock, w.str)
				}
			}
		})
	}
}

// TestMergeStructs_GCRuns: GC runs from the two readers merge when they
// touch or overlap; a gap between them is a Skip; one reader's consecutive
// runs stay apart unless a merge came first (the reader is not advanced
// after a merge, as in yjs).
func TestMergeStructs_GCRuns(t *testing.T) {
	gc := func(clock, n uint64) Block {
		return Block{Kind: WireBlockGC, ID: block.ID{Client: 9, Clock: clock}, Len: n}
	}
	skip := func(clock, n uint64) Block {
		return Block{Kind: WireBlockSkip, ID: block.ID{Client: 9, Clock: clock}, Len: n}
	}
	for _, tc := range []struct {
		name string
		a, b []Block
		want []Block
	}{
		{"touching runs merge", []Block{gc(0, 5)}, []Block{gc(5, 5)}, []Block{gc(0, 10)}},
		{"a gap is a Skip", []Block{gc(0, 5)}, []Block{gc(10, 5)}, []Block{gc(0, 5), skip(5, 5), gc(10, 5)}},
		{"a covered run is dropped", []Block{gc(10, 5)}, []Block{gc(10, 2)}, []Block{gc(10, 5)}},
		{"a straddling run is cut and merged", []Block{gc(10, 2)}, []Block{gc(11, 5)}, []Block{gc(10, 6)}},
		{"one reader's runs stay apart", nil, []Block{gc(0, 2), gc(2, 2)}, []Block{gc(0, 2), gc(2, 2)}},
		{"after a merge the same reader's next run merges", []Block{gc(2, 2)}, []Block{gc(4, 2), gc(6, 2)}, []Block{gc(2, 6)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeStructs(tc.a, tc.b)
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i].Kind != tc.want[i].Kind || got[i].ID != tc.want[i].ID || got[i].Len != tc.want[i].Len {
					t.Fatalf("struct %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestMergeStructs_GCAndItemAtOneClock: yjs's comparator puts each of a GC
// run and an item at one clock before the other, and V8 sorting the two
// readers swaps them, so the second reader wins in either order.
func TestMergeStructs_GCAndItemAtOneClock(t *testing.T) {
	g := Block{Kind: WireBlockGC, ID: block.ID{Client: 9, Clock: 1}, Len: 3}
	it := strItem(9, 1, "abc", nil)
	if got := mergeStructs([]Block{it}, []Block{g}); len(got) != 1 || got[0].Kind != WireBlockGC {
		t.Fatalf("item then GC: %+v, want the GC run", got)
	}
	if got := mergeStructs([]Block{g}, []Block{it}); len(got) != 1 || got[0].Kind != WireBlockItem {
		t.Fatalf("GC then item: %+v, want the item", got)
	}
}

// TestPending_mergeBlocks_QueuedItemsAsRead: a queued item with an origin
// has no parent and no key, as yjs reads it back from its queue's bytes; a
// first item (no origins) keeps them.
func TestPending_mergeBlocks_QueuedItemsAsRead(t *testing.T) {
	key := "k"
	first := strItem(9, 0, "ab", nil)
	first.Item.Parent = block.Parent{Kind: block.ParentID, ID: block.ID{Client: 5, Clock: 0}}
	first.Item.ParentSub = &key
	next := strItem(9, 5, "cd", &block.ID{Client: 9, Clock: 4})
	next.Item.Parent = block.Parent{Kind: block.ParentID, ID: block.ID{Client: 5, Clock: 0}}
	next.Item.ParentSub = &key
	p := NewPending()
	p.mergeBlocks(map[uint64][]Block{9: {first, next}})
	list := p.Blocks[9]
	if list[0].Item.Parent.Kind != block.ParentID || list[0].Item.ParentSub == nil {
		t.Fatal("the first item lost its parent or key")
	}
	if list[1].Item.Parent != (block.Parent{}) || list[1].Item.ParentSub != nil {
		t.Fatalf("the item with an origin kept parent %+v and key %v", list[1].Item.Parent, list[1].Item.ParentSub)
	}
}

// TestMergeQueue_MatchesTheWholeMerge: merging only the clients a leftover
// shares with the queue gives what yjs's loop over the whole queue gives,
// on random queues and leftovers of GC runs and items that start at the
// same clocks, touch and overlap, across clients the two share in part.
func TestMergeQueue_MatchesTheWholeMerge(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	letters := "abcdefghijklmnopqrstuvwxyz"
	list := func() []Block {
		var out []Block
		clock := uint64(rng.IntN(3))
		for range 1 + rng.IntN(3) {
			n := uint64(1 + rng.IntN(4))
			out = append(out, Block{}) // kind and client set below
			out[len(out)-1] = Block{Kind: WireBlockGC, ID: block.ID{Clock: clock}, Len: n}
			if rng.IntN(2) == 0 {
				out[len(out)-1] = Block{Kind: WireBlockItem, Item: &block.Item{
					ID:      block.ID{Clock: clock},
					Content: block.Content{Kind: block.KindString, Str: letters[clock%20 : clock%20+n]},
					Len:     n,
				}}
			}
			clock += n + uint64(rng.IntN(2))
		}
		return out
	}
	side := func() map[uint64][]Block {
		m := map[uint64][]Block{}
		for _, c := range []uint64{1, 2, 3, 4, 5, 6} {
			if rng.IntN(2) == 0 {
				continue
			}
			l := list()
			for i := range l {
				if l[i].Kind == WireBlockItem {
					l[i].Item.ID.Client = c
				} else {
					l[i].ID.Client = c
				}
			}
			m[c] = l
		}
		return m
	}
	key := func(b Block) string {
		if b.Kind == WireBlockItem {
			return fmt.Sprintf("I%d:%d:%s", b.Item.ID.Client, b.Item.ID.Clock, b.Item.Content.Str)
		}
		return fmt.Sprintf("G%d:%d+%d", b.ID.Client, b.ID.Clock, b.Len)
	}
	for run := range 20000 {
		q, r := side(), side()
		if len(q) == 0 || len(r) == 0 {
			continue
		}
		var whole []string
		for _, b := range mergeStructs(clientStream(q), clientStream(r)) {
			if b.Kind != WireBlockSkip {
				whole = append(whole, key(b))
			}
		}
		mergeQueue(q, r)
		var part []string
		for _, b := range clientStream(q) {
			part = append(part, key(b))
		}
		if !slices.Equal(whole, part) {
			t.Fatalf("run %d:\nwhole %v\n part %v", run, whole, part)
		}
	}
}

// TestSliceStruct_ReadMark: a piece cut from a store item read back without
// its key (it has an origin) is written without the key bit; a piece of a
// first item (no origins), which yjs reads with its key, keeps it, though
// the piece itself now has an origin.
func TestSliceStruct_ReadMark(t *testing.T) {
	key := "k"
	first := Block{Kind: WireBlockItem, read: true, Item: &block.Item{
		ID: block.ID{Client: 9, Clock: 0}, Len: 3, ParentSub: &key,
		Content: block.Content{Kind: block.KindDeleted, DeletedLen: 3},
		Parent:  block.Parent{Kind: block.ParentNamed, Named: "m"},
	}}
	s, ok := sliceStruct(first, 1)
	if !ok || s.read || s.Item.Info()&block.InfoHasParentSub == 0 {
		t.Fatalf("piece of a first item: read %v, info %#x; want the key bit kept", s.read, s.Item.Info())
	}
	next := first
	next.Item = &block.Item{ID: block.ID{Client: 9, Clock: 3}, Len: 3, ParentSub: &key,
		Origin: &block.ID{Client: 9, Clock: 2}, Content: block.Content{Kind: block.KindDeleted, DeletedLen: 3}}
	s, ok = sliceStruct(next, 1)
	if !ok || !s.read {
		t.Fatalf("piece of an item with an origin: read %v, want it read back without its key", s.read)
	}
}

// TestPending_mergeBlocks_RawQueueIsMergedWhole: a first leftover with
// overlapping blocks is queued as it came, and the next merge takes the
// whole queue through yjs's loop, also the clients the leftover lacks.
func TestPending_mergeBlocks_RawQueueIsMergedWhole(t *testing.T) {
	p := NewPending()
	p.mergeBlocks(map[uint64][]Block{9: {strItem(9, 0, "abcde", nil), strItem(9, 2, "cd", nil)}})
	if len(p.Blocks[9]) != 2 {
		t.Fatalf("first leftover queued as %d blocks, want it as it came (2)", len(p.Blocks[9]))
	}
	p.mergeBlocks(map[uint64][]Block{7: {strItem(7, 3, "x", nil)}})
	if len(p.Blocks[9]) != 1 || p.Blocks[9][0].Item.Content.Str != "abcde" {
		t.Fatalf("client 9 after the next merge: %d blocks, want the covered one dropped", len(p.Blocks[9]))
	}
	if len(p.Blocks[7]) != 1 {
		t.Fatalf("client 7: %d blocks, want 1", len(p.Blocks[7]))
	}
}
