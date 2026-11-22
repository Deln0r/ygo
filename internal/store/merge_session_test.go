package store

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/Deln0r/ygo/internal/block"
)

// randomMergeList builds a dense list of GC runs (which always merge with
// each other) and items that merge with nothing (no origin), seeded.
func randomMergeList(r *rand.Rand, n int) *ClientBlockList {
	l := &ClientBlockList{}
	clock := uint64(0)
	for i := 0; i < n; i++ {
		ln := uint64(1 + r.Intn(3))
		if r.Intn(3) == 0 {
			it := &block.Item{ID: block.ID{Client: 1, Clock: clock}, Len: ln, Content: block.Content{Kind: block.KindDeleted, DeletedLen: ln}}
			l.list = append(l.list, CellOfItem(it))
		} else {
			l.list = append(l.list, CellOfGC(clock, clock+ln-1))
		}
		clock += ln
	}
	return l
}

func describeList(l *ClientBlockList) string {
	out := ""
	for _, c := range l.list {
		if c.Kind == CellKindGC {
			out += fmt.Sprintf("G%d-%d ", c.GC.Start, c.GC.End)
		} else {
			out += fmt.Sprintf("I%d+%d ", c.Item.ID.Clock, c.Item.Len)
		}
	}
	return out
}

// TestMergeSession_SameAsImmediateMerges: a series of tryToMergeWithLefts
// calls through a MergeSession, which leaves absorbed cells in place until
// Close, leaves the list as the same calls on a list that drops each merged
// cell at once (yjs's array splice). Each call names a live cell by its
// position among the live cells, as yjs's indices do, and a clock lookup
// finds the same cell both ways.
func TestMergeSession_SameAsImmediateMerges(t *testing.T) {
	for seed := int64(1); seed <= 300; seed++ {
		r := rand.New(rand.NewSource(seed))
		n := 2 + r.Intn(40)
		a := randomMergeList(rand.New(rand.NewSource(seed)), n)
		b := randomMergeList(rand.New(rand.NewSource(seed)), n)
		ms := b.BeginMerges()
		// The live cells of b in order, as a cannot be read by position.
		liveAt := func(k int) int {
			for i := range b.list {
				if !ms.dead(i) {
					if k == 0 {
						return i
					}
					k--
				}
			}
			return -1
		}
		end := a.list[len(a.list)-1].ClockEnd()
		for op := 0; op < 1+r.Intn(15); op++ {
			if len(a.list) < 2 {
				break
			}
			if r.Intn(2) == 0 {
				pos := 1 + r.Intn(len(a.list)-1)
				ma := a.MergeWithLefts(pos)
				mb, _ := ms.MergeWithLefts(liveAt(pos))
				if ma != mb {
					t.Fatalf("seed %d op %d: merges %d immediate, %d in the session", seed, op, ma, mb)
				}
			} else {
				clock := uint64(r.Int63n(int64(end) + 1))
				ia, oka := a.FindPivot(clock)
				ib, okb := ms.Find(clock)
				if oka != okb || (oka && liveAt(ia) != ib) {
					t.Fatalf("seed %d op %d: clock %d found at %d (%v) immediate, physical %d (%v) in the session", seed, op, clock, ia, oka, ib, okb)
				}
				if oka {
					na, okna := ia+1, ia+1 < len(a.list)
					nb, oknb := ms.Next(ib)
					if okna != oknb || (okna && liveAt(na) != nb) {
						t.Fatalf("seed %d op %d: next of %d differs", seed, op, ia)
					}
				}
			}
		}
		ms.Close()
		if da, db := describeList(a), describeList(b); da != db {
			t.Fatalf("seed %d:\n immediate %s\n session   %s", seed, da, db)
		}
		if a.Len() != b.Len() {
			t.Fatalf("seed %d: lengths %d and %d", seed, a.Len(), b.Len())
		}
	}
}

// TestMergeSession_CandidatesOverOneRunAreLinear: candidates over one long
// run of GC cells, worked last to first as mergeSplits works them (yjs
// _mergeStructs: a type's already-deleted children), merge the run with the
// first and then find each cell, and its right neighbour, in a bounded
// number of hops each: lookups that walked absorber chains and absorbed
// runs one cell at a time took K hops per candidate.
func TestMergeSession_CandidatesOverOneRunAreLinear(t *testing.T) {
	const k = 20000
	l := &ClientBlockList{}
	l.list = append(l.list, CellOfItem(&block.Item{ID: block.ID{Client: 1, Clock: 0}, Len: 1, Content: block.Content{Kind: block.KindDeleted, DeletedLen: 1}}))
	for c := uint64(1); c <= k; c++ {
		l.list = append(l.list, CellOfGC(c, c))
	}
	ms := l.BeginMerges()
	for c := uint64(k); c >= 1; c-- {
		idx, ok := ms.Find(c)
		if !ok {
			t.Fatalf("clock %d not found", c)
		}
		if next, ok := ms.Next(idx); ok {
			if merged, _ := ms.MergeWithLefts(next); merged > 1 {
				continue
			}
		}
		if idx > 0 {
			ms.MergeWithLefts(idx)
		}
	}
	steps := ms.steps
	ms.Close()
	if l.Len() != 2 {
		t.Fatalf("%d cells, want the item and one GC run", l.Len())
	}
	if steps > 8*k {
		t.Fatalf("%d hops for %d candidates, want O(K)", steps, k)
	}
}
