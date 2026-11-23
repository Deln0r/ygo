package encoding

import "container/heap"

// mergeReaders keeps the readers of mergeStreams in the order yjs's loop has
// them: each step it drops the readers that ran out and sorts the rest with
// Array.prototype.sort and compareStructs, keeping the order the step
// before left, and the first one writes. The comparator is consistent on
// the readers' current structs unless two of them are a GC and an item at
// one clock of one client (it says each goes first); while it is, V8's sort
// is the stable sort of the order before, and the readers are a heap ordered
// by (compareStructs, position), O(log n) a step. Where such a pair exists,
// the order is an array sorted by the port of V8's sort (v8Sort), as yjs
// sorts it.
type mergeReaders struct {
	all      []*mergeReader
	heap     readerHeap
	order    []*mergeReader // the order while a tie needs V8's sort; nil otherwise
	low      int64          // next position for a reader put first
	ties     map[structKey]*[2]int
	tied     int // keys holding both a GC and an item
	frontR   *mergeReader
	lastDone int
}

type mergeReader struct {
	idx   int
	cur   structCursor
	pos   int64
	index int // in heap
	key   structKey
	kind  int // 0 GC, 1 item, -1 none
}

type structKey struct{ client, clock uint64 }

func (r *mergeReader) curr() *Block { return r.cur.curr() }

func newMergeReaders(streams [][]Block) *mergeReaders {
	rs := &mergeReaders{lastDone: -1}
	for i, st := range streams {
		r := &mergeReader{idx: i, cur: structCursor{list: st}, pos: int64(i), kind: -1}
		rs.all = append(rs.all, r)
		rs.track(r)
		if r.curr() != nil {
			rs.heap = append(rs.heap, r)
		} else {
			rs.lastDone = i
		}
	}
	if rs.tied > 0 {
		// The first sort meets a tie: the array yjs sorts is the readers in
		// their order, which a heap ordered by (key, position) cannot stand
		// for while the comparator is not consistent.
		rs.order = append([]*mergeReader(nil), rs.heap...)
		rs.heap = rs.heap[:0]
		return rs
	}
	for i, r := range rs.heap {
		r.index = i
	}
	heap.Init(&rs.heap)
	return rs
}

// track records r's current struct in the tie count.
func (rs *mergeReaders) track(r *mergeReader) {
	if r.kind >= 0 {
		if c := rs.ties[r.key]; c != nil {
			before := c[0] > 0 && c[1] > 0
			c[r.kind]--
			if before && !(c[0] > 0 && c[1] > 0) {
				rs.tied--
			}
			if c[0] == 0 && c[1] == 0 {
				delete(rs.ties, r.key)
			}
		}
		r.kind = -1
	}
	b := r.curr()
	if b == nil {
		return
	}
	k := 0
	if b.Kind == WireBlockItem {
		k = 1
	}
	if rs.ties == nil {
		rs.ties = map[structKey]*[2]int{}
	}
	r.key = structKey{blockClient(b), blockStartClock(*b)}
	r.kind = k
	c := rs.ties[r.key]
	if c == nil {
		c = &[2]int{}
		rs.ties[r.key] = c
	}
	before := c[0] > 0 && c[1] > 0
	c[k]++
	if !before && c[0] > 0 && c[1] > 0 {
		rs.tied++
	}
}

// next advances r (the reader in front) and returns its new current struct.
func (rs *mergeReaders) next(r *mergeReader) *Block {
	b := r.cur.next()
	rs.track(r)
	return b
}

// front drops the readers that ran out, sorts as yjs sorts, and returns the
// reader in front.
func (rs *mergeReaders) front() (*mergeReader, bool) {
	// The reader in front before is the only one that moved: it stays first
	// in the array yjs sorts, so it takes the first position.
	if f := rs.frontR; f != nil {
		f.pos = rs.low - 1
		rs.low--
		if f.curr() == nil {
			rs.lastDone = f.idx
		}
		if rs.order == nil {
			if f.curr() == nil {
				heap.Remove(&rs.heap, f.index)
			} else {
				heap.Fix(&rs.heap, f.index)
			}
		} else if f.curr() == nil {
			rs.order = rs.order[1:]
		}
		rs.frontR = nil
	}
	if rs.tied > 0 {
		if rs.order == nil {
			// The array yjs sorts: the reader that moved first (lowest
			// position), the others in the order the last sort left, which
			// had no tie, so (key, position) is that order.
			rs.order = append([]*mergeReader(nil), rs.heap...)
			sortByPos(rs.order)
			rs.heap = rs.heap[:0]
		}
		v8Sort(rs.order, func(x, y *mergeReader) int { return compareStructs(x.curr(), y.curr()) })
		if len(rs.order) == 0 {
			return nil, false
		}
		rs.frontR = rs.order[0]
		return rs.frontR, true
	}
	if rs.order != nil {
		// Consistent again: the stable sort of the array as it stands.
		for i, r := range rs.order {
			r.pos = int64(i)
			r.index = i
		}
		rs.heap = append(rs.heap[:0], rs.order...)
		rs.order = nil
		rs.low = 0
		heap.Init(&rs.heap)
	}
	if len(rs.heap) == 0 {
		return nil, false
	}
	rs.frontR = rs.heap[0]
	return rs.frontR, true
}

func sortByPos(rs []*mergeReader) {
	// Positions are distinct; the heap's order is (key, position), and the
	// array before the sort is the moved reader first, then (key, position).
	if len(rs) < 2 {
		return
	}
	first := 0
	for i, r := range rs {
		if r.pos < rs[first].pos {
			first = i
		}
	}
	rs[0], rs[first] = rs[first], rs[0]
	rest := rs[1:]
	h := readerHeap(rest)
	sorted := make([]*mergeReader, 0, len(rest))
	heap.Init(&h)
	for h.Len() > 0 {
		sorted = append(sorted, heap.Pop(&h).(*mergeReader))
	}
	copy(rest, sorted)
}

// readerHeap orders readers by compareStructs of their current struct, then
// position.
type readerHeap []*mergeReader

func (h readerHeap) Len() int { return len(h) }
func (h readerHeap) Less(i, j int) bool {
	if c := compareStructs(h[i].curr(), h[j].curr()); c != 0 {
		return c < 0
	}
	return h[i].pos < h[j].pos
}
func (h readerHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}
func (h *readerHeap) Push(x any) {
	r := x.(*mergeReader)
	r.index = len(*h)
	*h = append(*h, r)
}
func (h *readerHeap) Pop() any {
	old := *h
	r := old[len(old)-1]
	*h = old[:len(old)-1]
	return r
}
