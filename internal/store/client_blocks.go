package store

import (
	"fmt"
	"sort"

	"github.com/Deln0r/ygo/internal/block"
)

// ClientBlockList is the dense, clock-ordered list of every block
// produced by a single client. Invariants enforced:
//
//  1. Per-client clock density: the union of cell ranges equals
//     [0, Clock()) with no gaps and no overlaps.
//  2. Sorted by clock: list[i].ClockStart < list[i+1].ClockStart.
//  3. Inclusive ranges: ClockStart and ClockEnd are both inclusive;
//     Len = End - Start + 1.
//
// See docs/yrs-port-notes/store.md "Internal invariants".
type ClientBlockList struct {
	list []BlockCell
	// cells is the owning store's cell count (BlockStore.Cells), nil for
	// a list no store owns.
	cells *int
}

// count adds n to the owning store's cell count.
func (l *ClientBlockList) count(n int) {
	if l.cells != nil {
		*l.cells += n
	}
}

// NewClientBlockList returns an empty list. Capacity is reserved for
// the common case of a few-dozen blocks per client.
func NewClientBlockList() *ClientBlockList {
	return &ClientBlockList{list: make([]BlockCell, 0, 16)}
}

// Len returns the number of cells in the list.
func (l *ClientBlockList) Len() int { return len(l.list) }

// Get returns the cell at index i. Returns the zero BlockCell and false
// if i is out of range.
func (l *ClientBlockList) Get(i int) (BlockCell, bool) {
	if i < 0 || i >= len(l.list) {
		return BlockCell{}, false
	}
	return l.list[i], true
}

// Clock returns the next free clock for this client. Equal to
// list[len-1].ClockEnd() + 1, or 0 for an empty list. Constant-time.
//
// Mirrors yrs ClientBlockList::clock (block_store.rs:24-34). Used for
// state vector materialization and append-precondition checks.
func (l *ClientBlockList) Clock() uint64 {
	if len(l.list) == 0 {
		return 0
	}
	return l.list[len(l.list)-1].ClockEnd() + 1
}

// FindPivot returns the index of the cell whose [ClockStart, ClockEnd]
// inclusive range contains the given clock. Returns (0, false) if the
// list is empty or the clock is past Clock().
//
// Implementation uses sort.Search rather than yrs's hand-rolled
// interpolation+binary search (block_store.rs:42-68). The interpolation
// is a micro-optimisation; sort.Search costs at most log2(N) extra
// comparisons and avoids the divide-by-zero edge case yrs has when the
// list contains a single block ending at clock 0.
func (l *ClientBlockList) FindPivot(clock uint64) (int, bool) {
	if len(l.list) == 0 {
		return 0, false
	}
	i := sort.Search(len(l.list), func(i int) bool {
		return l.list[i].ClockEnd() >= clock
	})
	if i == len(l.list) || l.list[i].ClockStart() > clock {
		return 0, false
	}
	return i, true
}

// Push appends a cell to the list. Caller must guarantee
// cell.ClockStart() == l.Clock() to preserve invariant 1; in tests,
// follow with CheckInvariants.
//
// Mirrors yrs ClientBlockList::push (block_store.rs:84-87).
func (l *ClientBlockList) Push(cell BlockCell) {
	l.list = append(l.list, cell)
	l.count(1)
}

// Insert places cell at the given index, shifting later cells right.
// Used by split_block and materialize for the new right-half cell;
// caller must compute index via FindPivot and validate that the insert
// keeps invariants 1-3.
//
// Mirrors yrs ClientBlockList::insert (block_store.rs:89-93).
func (l *ClientBlockList) Insert(index int, cell BlockCell) {
	l.list = append(l.list, BlockCell{})
	copy(l.list[index+1:], l.list[index:])
	l.list[index] = cell
	l.count(1)
}

// SquashFrom merges adjacent same-client item cells at and after
// startIdx into their left neighbour wherever Item.TrySquash succeeds,
// dropping the absorbed cell. Returns the number of cells removed.
// startIdx is clamped to >= 1 (index 0 has no left neighbour).
//
// This is the commit-time block-squash primitive: it collapses runs of
// same-client adjacent-clock items (the classic per-character
// Text.Insert pattern) into single items, matching yjs's on-commit
// merging and removing the V1 per-item wire overhead. Clock density is
// preserved because TrySquash extends the surviving cell to cover the
// absorbed cell's clocks.
func (l *ClientBlockList) SquashFrom(startIdx int) int {
	if startIdx < 1 {
		startIdx = 1
	}
	if startIdx >= len(l.list) {
		return 0
	}
	// One pass: w is the last kept cell, and every cell after it either
	// merges into it or is kept next to it.
	w := startIdx - 1
	for r := startIdx; r < len(l.list); r++ {
		prev, cur := l.list[w], l.list[r]
		switch {
		case prev.Kind == CellKindItem && cur.Kind == CellKindItem &&
			prev.Item != nil && cur.Item != nil && squashItems(prev.Item, cur.Item):
			// cur merged into prev and left the linked list.
		case prev.Kind == CellKindGC && cur.Kind == CellKindGC:
			// Collapse two adjacent garbage-collected runs into one,
			// as yjs GC.mergeWith does.
			l.list[w] = CellOfGC(prev.GC.Start, cur.GC.End)
		default:
			w++
			if w != r {
				l.list[w] = cur
			}
		}
	}
	removed := len(l.list) - (w + 1)
	clear(l.list[w+1:])
	l.list = l.list[:w+1]
	l.count(-removed)
	return removed
}

// squashItems merges other into its left neighbour i (Item.TrySquash)
// and moves the search markers that pointed at other onto i, in both
// lists: MarkerList and a late-typed text's TextMarkers (yjs mergeWith).
func squashItems(i, other *block.Item) bool {
	before := i.Len
	if !i.TrySquash(other) {
		return false
	}
	if br := i.Parent.Branch; br != nil {
		if br.Markers != nil {
			br.Markers.Absorbed(other, i, before)
		}
		br.TextMarkers.OnMerge(other, i, before)
	}
	return true
}

// MergeWithLefts merges the cell at pos into its left neighbour, and the
// result into its own left neighbour, while that succeeds (yjs
// tryToMergeWithLefts). Returns the number of merges. The merged cells
// leave the list in one move, as yjs splices them out once.
func (l *ClientBlockList) MergeWithLefts(pos int) int {
	if pos < 1 || pos >= len(l.list) {
		return 0
	}
	j := l.mergeChain(pos)
	merged := pos - j
	if merged > 0 {
		l.list = append(l.list[:j+1], l.list[pos+1:]...)
		l.count(-merged)
	}
	return merged
}

// mergeChain merges the cell at pos into its left neighbour, that one into
// its own, and so on while a merge succeeds (yjs's order), leaving the
// absorbed cells in place; it returns the index of the cell that absorbed
// them (pos when none merged). Whether two cells merge depends only on the
// two cells as they were, so the chain's extent is found first and the
// cells are then merged left to right: each payload is appended once to the
// survivor's, where merging right to left copied the growing tail into
// every cell on the way.
func (l *ClientBlockList) mergeChain(pos int) int {
	j := pos
	for j >= 1 && l.canMerge(j-1, j) {
		j--
	}
	for k := j + 1; k <= pos; k++ {
		l.mergePair(j, k)
	}
	return j
}

// canMerge reports whether mergePair(i, j) would merge.
func (l *ClientBlockList) canMerge(i, j int) bool {
	prev, cur := l.list[i], l.list[j]
	switch {
	case prev.Kind == CellKindItem && cur.Kind == CellKindItem && prev.Item != nil && cur.Item != nil:
		return prev.Item.CanSquash(cur.Item)
	case prev.Kind == CellKindGC && cur.Kind == CellKindGC:
		return true
	}
	return false
}

// MergeSession runs a series of yjs tryToMergeWithLefts calls on the list as
// yjs runs them on its array, which splices the merged structs out at once,
// but leaves the absorbed cells in place, each recording the cell that
// absorbed it, and removes them all in one pass at Close: splicing at every
// merge copied the rest of the list each time. Indices are physical; the
// methods see the list as yjs would at that point (a cell's left neighbour
// is the nearest cell not absorbed). The list must not be changed otherwise
// until Close.
type MergeSession struct {
	l        *ClientBlockList
	absorber map[int]int // absorbed index -> index of the cell it went into
	skip     map[int]int // absorbed index -> an index right of it, toward the next live cell
	minDead  int         // -1 while nothing is absorbed
	chain    []int       // scratch for MergeWithLefts
	steps    int         // pointer hops in live and liveFrom (tests)
}

// BeginMerges starts a MergeSession on l.
func (l *ClientBlockList) BeginMerges() *MergeSession {
	return &MergeSession{l: l, minDead: -1}
}

func (m *MergeSession) dead(i int) bool {
	_, ok := m.absorber[i]
	return ok
}

// live returns the cell that holds i now: i, or the cell it was absorbed
// into, followed to one that was not. Every cell on the way is pointed at
// it, so a later lookup is short.
func (m *MergeSession) live(i int) int {
	root := i
	for {
		a, ok := m.absorber[root]
		if !ok {
			break
		}
		root = a
		m.steps++
	}
	for k := i; k != root; {
		next := m.absorber[k]
		m.absorber[k] = root
		k = next
	}
	return root
}

// liveFrom returns the first cell at or right of k that is not absorbed
// (len when none), pointing the absorbed cells passed at it.
func (m *MergeSession) liveFrom(k int) int {
	root := k
	for root < len(m.l.list) && m.dead(root) {
		root = m.skip[root]
		m.steps++
	}
	for k < root {
		next := m.skip[k]
		m.skip[k] = root
		k = next
	}
	return root
}

// Len is the physical length of the list.
func (m *MergeSession) Len() int { return len(m.l.list) }

// ClockStart is the first clock of the live cell at i.
func (m *MergeSession) ClockStart(i int) uint64 { return m.l.list[i].ClockStart() }

// Find returns the live cell holding clock (yjs findIndexSS on its array).
func (m *MergeSession) Find(clock uint64) (int, bool) {
	l := m.l.list
	// Cells keep their own first clock, absorbed or not, so starts stay
	// sorted; the cell starting at or before clock, followed to its
	// holder, is the one holding it.
	i := sort.Search(len(l), func(i int) bool { return l[i].ClockStart() > clock }) - 1
	if i < 0 {
		return 0, false
	}
	j := m.live(i)
	if l[j].ClockEnd() < clock {
		return 0, false
	}
	return j, true
}

// Next returns the live cell right of the live cell i (yjs i+1), or false.
func (m *MergeSession) Next(i int) (int, bool) {
	k := m.liveFrom(i + 1)
	if k >= len(m.l.list) {
		return 0, false
	}
	return k, true
}

// Left returns the live cell left of the live cell i (yjs i-1), or false
// for the first.
func (m *MergeSession) Left(i int) (int, bool) {
	if i < 1 {
		return 0, false
	}
	return m.live(i - 1), true
}

// MergeWithLefts merges the live cell pos into its left neighbour, and the
// result into its own, while that succeeds (yjs tryToMergeWithLefts), and
// returns the number of merges and the cell that absorbed them (pos when
// none).
func (m *MergeSession) MergeWithLefts(pos int) (int, int) {
	// The chain's extent first, then the merges left to right (mergeChain).
	chain := append(m.chain[:0], pos)
	for cur := pos; ; {
		left, ok := m.Left(cur)
		if !ok || !m.l.canMerge(left, cur) {
			break
		}
		chain = append(chain, left)
		cur = left
	}
	m.chain = chain
	survivor := chain[len(chain)-1]
	if len(chain) == 1 {
		return 0, survivor
	}
	if m.absorber == nil {
		m.absorber, m.skip = map[int]int{}, map[int]int{}
	}
	for k := len(chain) - 2; k >= 0; k-- {
		c := chain[k]
		m.l.mergePair(survivor, c)
		m.absorber[c] = survivor
		m.skip[c] = c + 1
		if m.minDead < 0 || c < m.minDead {
			m.minDead = c
		}
	}
	return len(chain) - 1, survivor
}

// Close removes the absorbed cells, moving each stretch of kept cells
// between them once.
func (m *MergeSession) Close() {
	if m.minDead < 0 {
		return
	}
	l := m.l
	n := len(l.list)
	gone := make([]int, 0, len(m.absorber))
	for i := range m.absorber {
		gone = append(gone, i)
	}
	sort.Ints(gone)
	w := gone[0]
	for k, d := range gone {
		next := n
		if k+1 < len(gone) {
			next = gone[k+1]
		}
		w += copy(l.list[w:], l.list[d+1:next])
	}
	clear(l.list[w:])
	l.list = l.list[:w]
	l.count(w - n)
	m.absorber, m.skip, m.minDead = nil, nil, -1
}

// mergePair merges the cell at j into the cell at i, its left neighbour
// in the list as merges see it, if they can merge: two items that squash
// (TrySquash), or two GC runs (yjs GC.mergeWith).
func (l *ClientBlockList) mergePair(i, j int) bool {
	prev, cur := l.list[i], l.list[j]
	switch {
	case prev.Kind == CellKindItem && cur.Kind == CellKindItem &&
		prev.Item != nil && cur.Item != nil && squashItems(prev.Item, cur.Item):
		return true
	case prev.Kind == CellKindGC && cur.Kind == CellKindGC:
		l.list[i] = CellOfGC(prev.GC.Start, cur.GC.End)
		return true
	}
	return false
}

// MergeLeftsDownTo is yjs's merge of a transaction's new structs: from the
// last cell down to first, each cell merges with its lefts (MergeWithLefts),
// and the walk goes on left of where the merges stopped. A chain can run on
// left of first. The absorbed cells leave the list in one pass at the end,
// from the leftmost of them, so the list is moved once however many chains
// merge, and each chain's payloads are appended once (mergeChain). Returns
// the number of cells removed.
func (l *ClientBlockList) MergeLeftsDownTo(first int) int {
	if first < 1 {
		first = 1
	}
	n := len(l.list)
	if first >= n {
		return 0
	}
	// The absorbed cells of each chain, [lo, hi], found right to left.
	// A chain only reads cells left of its head, so an absorbed cell is
	// never read again.
	var absorbed [][2]int
	for i := n - 1; i >= first; {
		j := l.mergeChain(i)
		if j < i {
			absorbed = append(absorbed, [2]int{j + 1, i})
		}
		i = j - 1
	}
	if len(absorbed) == 0 {
		return 0
	}
	w := absorbed[len(absorbed)-1][0]
	for k := len(absorbed) - 1; k >= 0; k-- {
		next := n
		if k > 0 {
			next = absorbed[k-1][0]
		}
		w += copy(l.list[w:], l.list[absorbed[k][1]+1:next])
	}
	clear(l.list[w:])
	l.list = l.list[:w]
	l.count(w - n)
	return n - w
}

// ReplaceWithGC converts the cell at index i into a GC cell covering the
// same clock range, discarding the live Item. No-op if i is out of range
// or the cell is already a GC cell.
//
// Used by nested-type collection: when a shared type is deleted, each of
// its children collapses to a garbage-collected range (yjs's parentGCd
// path in ContentType.gc), encoded as a ref-0 GC struct. The cells merge
// with their neighbours only where yjs merges structs (the transaction's
// delete set and merge candidates, see doc.TransactionMut.mergeDeleted).
func (l *ClientBlockList) ReplaceWithGC(i int) {
	if i < 0 || i >= len(l.list) {
		return
	}
	c := l.list[i]
	if c.Kind == CellKindGC {
		return
	}
	l.list[i] = CellOfGC(c.ClockStart(), c.ClockEnd())
}

// CheckInvariants validates structural invariants 1-3 across the list.
// Returns the first error found, or nil if everything is consistent.
//
// Test-only utility; production code should not call this on the hot
// path. Recommended pattern: defer l.CheckInvariants() in any test that
// performs Push / Insert / SquashLeft to catch regressions immediately.
func (l *ClientBlockList) CheckInvariants() error {
	var nextClock uint64 = 0
	for i, c := range l.list {
		if c.ClockEnd() < c.ClockStart() {
			return fmt.Errorf("cell %d: inverted range [%d, %d]", i, c.ClockStart(), c.ClockEnd())
		}
		if c.ClockStart() != nextClock {
			return fmt.Errorf("cell %d: gap or overlap (expected start %d, got %d)", i, nextClock, c.ClockStart())
		}
		nextClock = c.ClockEnd() + 1
	}
	return nil
}
