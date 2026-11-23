package encoding

import (
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
		v8Sort(decs, func(x, y *structCursor) int { return compareStructs(x.curr(), y.curr()) })
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
	return out
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

// TestMergeStreams_SameAsSortingEveryStep: the readers' heap, and the array
// it falls back to where a GC and an item tie, put the same reader in front
// at every step as sorting the whole array every step, as yjs does, on
// seeded random streams of GC runs and deleted items over a few clients and
// overlapping clocks.
func TestMergeStreams_SameAsSortingEveryStep(t *testing.T) {
	for seed := int64(1); seed <= 3000; seed++ {
		r := rand.New(rand.NewSource(seed))
		n := 1 + r.Intn(12)
		streams := make([][]Block, n)
		for i := range streams {
			var st []Block
			for _, c := range []uint64{3, 2, 1} {
				if r.Intn(3) == 0 {
					continue
				}
				clock := uint64(r.Intn(3))
				for k := 0; k < 1+r.Intn(4); k++ {
					ln := uint64(1 + r.Intn(3))
					if r.Intn(2) == 0 {
						st = append(st, Block{Kind: WireBlockGC, ID: block.ID{Client: c, Clock: clock}, Len: ln})
					} else {
						it := &block.Item{ID: block.ID{Client: c, Clock: clock}, Len: ln, Content: block.Content{Kind: block.KindDeleted, DeletedLen: ln}}
						st = append(st, Block{Kind: WireBlockItem, Item: it})
					}
					clock += ln
					if r.Intn(4) == 0 {
						clock += uint64(1 + r.Intn(2))
					}
				}
			}
			streams[i] = st
		}
		got, _ := mergeStreams(streams)
		want := mergeStreamsReference(streams)
		if g, w := describeBlocks(got), describeBlocks(want); g != w {
			t.Fatalf("seed %d, %d readers:\n got  %s\n want %s", seed, n, g, w)
		}
	}
}
