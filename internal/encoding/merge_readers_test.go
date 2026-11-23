package encoding

import (
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"testing"

	"github.com/Deln0r/ygo/internal/block"
)

// mergeStreamsReference is mergeStreams with yjs's reader handling as it is
// written: every step the finished readers are dropped from the array and
// the rest sorted by the port of V8's sort.
func mergeStreamsReference(streams [][]Block) []Block {
	out, _ := mergeStreamsReferenceCount(streams)
	return out
}

// mergeStreamsReferenceCount is mergeStreamsReference that also counts the
// comparisons yjs's sorting makes.
func mergeStreamsReferenceCount(streams [][]Block) ([]Block, int64) {
	cmps := int64(0)
	decs := make([]*structCursor, len(streams))
	for i, st := range streams {
		decs[i] = &structCursor{list: st}
	}
	var out []Block
	var cw Block
	have := false
	end := func() uint64 { return blockStartClock(cw) + blockLen(cw) }
	for {
		decs = slices.DeleteFunc(decs, func(d *structCursor) bool { return d.curr() == nil })
		v8Sort(decs, func(x, y *structCursor) int {
			cmps++
			return compareStructs(x.curr(), y.curr())
		})
		if len(decs) == 0 {
			break
		}
		cd := decs[0]
		firstClient := blockClient(cd.curr())
		if have {
			curr := cd.curr()
			iterated := false
			for curr != nil && blockStartClock(*curr)+blockLen(*curr) <= end() && blockClient(curr) >= blockClient(&cw) {
				curr = cd.next()
				iterated = true
			}
			if curr == nil || blockClient(curr) != firstClient || (iterated && blockStartClock(*curr) > end()) {
				continue
			}
			switch {
			case firstClient != blockClient(&cw):
				out = append(out, cw)
				cw = *curr
				cd.next()
			case end() < blockStartClock(*curr):
				if cw.Kind == WireBlockSkip {
					cw.Len = blockStartClock(*curr) + blockLen(*curr) - cw.ID.Clock
				} else {
					out = append(out, cw)
					cw = Block{Kind: WireBlockSkip, ID: block.ID{Client: firstClient, Clock: end()}, Len: blockStartClock(*curr) - end()}
				}
			default:
				c := *curr
				if diff := end() - blockStartClock(c); diff > 0 {
					if cw.Kind == WireBlockSkip {
						cw.Len -= min(diff, cw.Len)
					} else if s, ok := sliceStruct(c, diff); ok {
						c = s
					} else {
						cd.next()
						continue
					}
				}
				if !mergeWith(&cw, c) {
					out = append(out, cw)
					cw = c
					cd.next()
				}
			}
		} else {
			cw = *cd.curr()
			have = true
			cd.next()
		}
		for next := cd.curr(); next != nil && blockClient(next) == firstClient && blockStartClock(*next) == end() && next.Kind != WireBlockSkip; next = cd.next() {
			out = append(out, cw)
			cw = *next
		}
	}
	if have {
		out = append(out, cw)
	}
	return out, cmps
}

func describeBlocks(bs []Block) string {
	s := ""
	for _, b := range bs {
		k := "G"
		switch b.Kind {
		case WireBlockItem:
			k = "I"
		case WireBlockSkip:
			k = "S"
		}
		s += fmt.Sprintf("%s%d:%d+%d ", k, blockClient(&b), blockStartClock(b), blockLen(b))
	}
	return s
}

func deletedItem(client, clock, ln uint64) Block {
	it := &block.Item{ID: block.ID{Client: client, Clock: clock}, Len: ln, Content: block.Content{Kind: block.KindDeleted, DeletedLen: ln}}
	return Block{Kind: WireBlockItem, Item: it}
}

func gcRun(client, clock, ln uint64) Block {
	return Block{Kind: WireBlockGC, ID: block.ID{Client: client, Clock: clock}, Len: ln}
}

// randomStreams gives n readers' streams of GC runs and deleted items over
// the clients, with overlapping clocks and holes.
func randomStreams(r *rand.Rand, n int, clients []uint64) [][]Block {
	streams := make([][]Block, n)
	for i := range streams {
		var st []Block
		for _, c := range clients {
			if r.Intn(3) == 0 {
				continue
			}
			clock := uint64(r.Intn(3))
			for k := 0; k < 1+r.Intn(4); k++ {
				ln := uint64(1 + r.Intn(3))
				if r.Intn(2) == 0 {
					st = append(st, gcRun(c, clock, ln))
				} else {
					st = append(st, deletedItem(c, clock, ln))
				}
				clock += ln
				if r.Intn(4) == 0 {
					clock += uint64(1 + r.Intn(2))
				}
			}
		}
		streams[i] = st
	}
	return streams
}

// TestMergeStreams_SameAsSortingEveryStep: the readers' heap, and the array
// it falls back to where a GC and an item tie (kept for a few sorts after
// the tie), put the same reader in front at every step as sorting the
// whole array every step, as yjs does, on seeded random streams of GC runs
// and deleted items over a few clients and overlapping clocks, with up to
// 12 readers over 3 clients and up to 40 over 6.
func TestMergeStreams_SameAsSortingEveryStep(t *testing.T) {
	for seed := int64(1); seed <= 4000; seed++ {
		r := rand.New(rand.NewSource(seed))
		n, clients := 1+r.Intn(12), []uint64{3, 2, 1}
		if seed > 3000 {
			n, clients = 1+r.Intn(40), []uint64{6, 5, 4, 3, 2, 1}
		}
		streams := randomStreams(r, n, clients)
		got, _, err := mergeStreams(streams, -1)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		want := mergeStreamsReference(streams)
		if g, w := describeBlocks(got), describeBlocks(want); g != w {
			t.Fatalf("seed %d, %d readers:\n got  %s\n want %s", seed, n, g, w)
		}
	}
}

// heldTieStreams: a GC and an item tied at client 1's clock 0, the lowest
// client, so they wait while n readers of higher clients merge; each of
// those makes yjs sort every reader again.
func heldTieStreams(n int) [][]Block {
	streams := make([][]Block, 0, n+2)
	for c := n + 1; c >= 2; c-- {
		streams = append(streams, []Block{gcRun(uint64(c), 0, 1)})
	}
	return append(streams, []Block{gcRun(1, 0, 1)}, []Block{deletedItem(1, 0, 1)})
}

// TestMergeStreams_HeldTieSortingIsBounded: such a merge gives yjs's result,
// and its sorting stops at the budget with ErrUpdateTooDense.
func TestMergeStreams_HeldTieSortingIsBounded(t *testing.T) {
	streams := heldTieStreams(300)
	want, cmps := mergeStreamsReferenceCount(streams)
	got, _, err := mergeStreams(streams, -1)
	if err != nil {
		t.Fatal(err)
	}
	if g, w := describeBlocks(got), describeBlocks(want); g != w {
		t.Fatalf("got  %s\nwant %s", g, w)
	}
	if _, _, err := mergeStreams(streams, cmps/2); !errors.Is(err, ErrUpdateTooDense) {
		t.Fatalf("a budget of half yjs's %d comparisons: err %v, want ErrUpdateTooDense", cmps, err)
	}
}

// TestMergeStreams_BudgetCoversLeavingTheArray: a tie at the highest client
// resolves first, the array stays a few sorts and is left while 200 other
// readers merge; the work counted to the end, leaving included, is what the
// budget bounds, so one comparison less stops the merge.
func TestMergeStreams_BudgetCoversLeavingTheArray(t *testing.T) {
	streams := [][]Block{{gcRun(500, 0, 1)}, {deletedItem(500, 0, 1)}}
	for c := uint64(200); c >= 1; c-- {
		streams = append(streams, []Block{gcRun(c, 0, 1)})
	}
	_, _, work, err := mergeStreamsWork(streams, -1)
	if err != nil || work == 0 {
		t.Fatalf("work %d, err %v", work, err)
	}
	if _, _, err := mergeStreams(streams, work); err != nil {
		t.Fatalf("a budget of the work done: %v", err)
	}
	if _, _, err := mergeStreams(streams, work-1); !errors.Is(err, ErrUpdateTooDense) {
		t.Fatalf("a budget one under the work done (%d): err %v, want ErrUpdateTooDense", work, err)
	}
}

// TestMergeStreams_TiesThatComeAndGoCostWhatYjsSorts: n pairs of readers,
// each pair tied at its client's clock 2 once the first reader has written
// clock 0, one pair after the other. Building the array for every tie would
// cost n log n each time; staying an array a few sorts keeps the sorting
// within twice what yjs's own sorting compares.
func TestMergeStreams_TiesThatComeAndGoCostWhatYjsSorts(t *testing.T) {
	var streams [][]Block
	for c := uint64(400); c >= 1; c-- {
		streams = append(streams, []Block{deletedItem(c, 0, 1), deletedItem(c, 2, 1)}, []Block{gcRun(c, 2, 1)})
	}
	want, cmps := mergeStreamsReferenceCount(streams)
	got, _, err := mergeStreams(streams, 2*cmps)
	if err != nil {
		t.Fatalf("yjs sorts with %d comparisons: %v", cmps, err)
	}
	if g, w := describeBlocks(got), describeBlocks(want); g != w {
		t.Fatalf("got  %s\nwant %s", g, w)
	}
}
