package encoding

import (
	"math"
	"slices"

	"github.com/Deln0r/ygo/internal/block"
)

// mergeStructs is the struct loop of yjs mergeUpdatesV2 (utils/updates.js)
// for two updates, ported step for step, because its choices decide which
// encoding of the same clocks survives and where items end:
//
//   - every step the two readers are put in order, client descending, clock
//     ascending; at an equal clock the order of the step before is kept, and
//     a GC and an item at one clock are swapped (the comparator says each
//     goes first, and V8 sorting two elements compares the second with the
//     first once and swaps them when it says so);
//   - the reader in front writes; a struct of the other reader that what was
//     written covers is skipped, one it overlaps is cut to what it adds
//     (sliceStruct);
//   - GC runs merge with the GC run written before them; items never do
//     (yjs Item.mergeWith needs `this.right === right`, null for structs
//     read from an update); after a merge the reader is not advanced, so its
//     next GC run can merge as well;
//   - one reader's consecutive structs are written as they are, without
//     merging;
//   - a gap within a client is written as a Skip.
//
// So the winner at an equal start is the reader that was in front after the
// step before, and that carries from client to client: queuing "abcde" and
// then receiving "abcdef" for the same clocks keeps "abcde" and cuts "f",
// unless the arriving update also held a higher client, which puts it in
// front. yjs keeps its pending structs as one update and merges each
// leftover into it this way (readUpdateV2), and writes its state as the
// merge of the store part and the queue diffed against the state vector
// (encodeStateAsUpdateV2); both go through here. Only two readers are ever
// merged there: the pending delete set yjs merges in as well carries no
// structs and is dropped before the sort.
//
// a and b list structs client by client, clients descending, clocks
// ascending within a client, without Skips. The result holds the structs to
// write, Skips included; an item that had to be cut is a copy with its own
// payload, every other entry is the input's.
func mergeStructs(a, b []Block) []Block {
	out, _ := mergeStructsLast(a, b)
	return out
}

// mergeStructsLast is mergeStructs that also reports which input ran out
// last (0 for a, 1 for b), which decides the order the two readers meet the
// next client in (mergeQueue).
func mergeStructsLast(a, b []Block) (out []Block, lastDone int) {
	ca, cb := &structCursor{list: a}, &structCursor{list: b}
	decs := []*structCursor{ca, cb}
	out = make([]Block, 0, len(a)+len(b)+1)
	var cw Block // yjs currWrite
	have := false
	end := func() uint64 { return blockStartClock(cw) + blockLen(cw) }
	for {
		n := 0
		for _, d := range decs {
			if d.curr() != nil {
				decs[n] = d
				n++
			} else if d == cb {
				lastDone = 1
			} else {
				lastDone = 0
			}
		}
		decs = decs[:n]
		if n == 0 {
			break
		}
		if n == 2 && compareStructs(decs[1].curr(), decs[0].curr()) < 0 {
			decs[0], decs[1] = decs[1], decs[0]
		}
		cd := decs[0]
		firstClient := blockClient(cd.curr())
		if have {
			curr := cd.curr()
			iterated := false
			// Skip what is already written (yjs compares the clock alone with
			// a client at least the written one's).
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
					gap := blockStartClock(*curr) - end()
					cw = Block{Kind: WireBlockSkip, ID: block.ID{Client: firstClient, Clock: end()}, Len: gap}
				}
			default:
				c := *curr
				if diff := end() - blockStartClock(c); diff > 0 {
					if cw.Kind == WireBlockSkip {
						cw.Len -= min(diff, cw.Len)
					} else if s, ok := sliceStruct(c, diff); ok {
						c = s
					} else {
						// Content that cannot be cut where the written struct
						// ends (single-element kinds never straddle; a malformed
						// length can): what is written covers it.
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
	return out, lastDone
}

// mergeQueue is mergeStructs of the queue (q) and a leftover (r), client by
// client, for the clients the two share: a client only one of them has
// comes out of yjs's loop as it went in (one reader's structs, in order and
// apart, are written as they are), so only the shared ones are merged, and
// the cost is that of the leftover and the clients it touches, not of the
// whole queue.
//
// What the loop carries from client to client is which reader is in front
// at a tie. The reader in front when the two first meet at a client is the
// one that reached it later: the other was waiting there while it wrote a
// higher client. A reader reaches client c when it has written the client
// before c in its own list, so the later one is the reader whose previous
// client is lower; with no previous client a reader starts at c; when both
// start there the queue is in front (yjs merges [queue, leftover]); when
// both come from the same client, the one that ran out of it last. q's
// lists are well-formed (clock-ascending, without overlap), as a merge
// leaves them; a list of r only one reader has goes through yjs's loop
// alone, which leaves a well-formed one as it is. The result replaces q's
// entries for the clients r has.
func mergeQueue(q, r map[uint64][]Block) {
	rc := make([]uint64, 0, len(r))
	for c := range r {
		rc = append(rc, c)
	}
	slices.Sort(rc)
	// between[i]: the queue has a client strictly between rc[i] and
	// rc[i+1]. That is all the order needs: the leftover's previous client
	// for rc[i] is rc[i+1], and the queue's is lower (the queue arrives
	// later) exactly when one lies between. One pass over the queue's
	// clients, before the leftover's own join it, and none for a leftover
	// of one client.
	between := make([]bool, len(rc))
	shared := make([]bool, len(rc)) // rc[i] is a queue client too
	for i, c := range rc {
		_, shared[i] = q[c]
	}
	if len(rc) > 1 {
		for x := range q {
			if j, found := slices.BinarySearch(rc, x); !found && j > 0 && j < len(rc) {
				between[j-1] = true
			}
		}
	}
	lastQ := true // the queue ran out of the previous shared client last
	for i := len(rc) - 1; i >= 0; i-- {
		c := rc[i]
		rl := r[c]
		ql := q[c]
		if !shared[i] {
			if wellFormed(rl) {
				q[c] = rl
			} else {
				q[c] = mergeStructs(rl, nil)
			}
			continue
		}
		var qFront bool
		switch {
		case i+1 == len(rc):
			// The leftover starts here: the queue either starts here too
			// (yjs merges [queue, leftover]) or comes from a higher client.
			qFront = true
		case between[i]:
			qFront = true
		default:
			// The queue comes from rc[i+1] too (both ran out of it, the
			// one that did last arrives later), or from a client above it.
			qFront = shared[i+1] && lastQ
		}
		var merged []Block
		var last int
		if qFront {
			merged, last = mergeStructsLast(ql, rl)
			lastQ = last == 0
		} else {
			merged, last = mergeStructsLast(rl, ql)
			lastQ = last == 1
		}
		q[c] = merged
	}
}

// wellFormed reports whether list is what yjs's loop passes through
// unchanged when it is one reader's: clock-ascending, no overlap, no
// Skips.
func wellFormed(list []Block) bool {
	var next uint64
	for i, b := range list {
		if b.Kind == WireBlockSkip {
			return false
		}
		if i > 0 && blockStartClock(b) < next {
			return false
		}
		next = blockStartClock(b) + blockLen(b)
	}
	return true
}

// structCursor is yjs LazyStructReader over a list of structs.
type structCursor struct {
	list []Block
	i    int
}

func (c *structCursor) curr() *Block {
	if c.i < len(c.list) {
		return &c.list[c.i]
	}
	return nil
}

func (c *structCursor) next() *Block {
	c.i++
	return c.curr()
}

func blockClient(b *Block) uint64 {
	if b.Kind == WireBlockItem {
		return b.Item.ID.Client
	}
	return b.ID.Client
}

// compareStructs is mergeUpdatesV2's comparator: client descending, then
// clock ascending; at one clock 0 for the same kind of struct, and -1 for
// different kinds whichever comes first (a Skip goes last, but readers
// filter Skips).
func compareStructs(a, b *Block) int {
	ca, cb := blockClient(a), blockClient(b)
	if ca != cb {
		if cb > ca {
			return 1
		}
		return -1
	}
	ka, kb := blockStartClock(*a), blockStartClock(*b)
	switch {
	case ka < kb:
		return -1
	case ka > kb:
		return 1
	case a.Kind == b.Kind:
		return 0
	case a.Kind == WireBlockSkip:
		return 1
	default:
		return -1
	}
}

// sliceStruct is yjs sliceStruct: b from diff clocks on. An item becomes a
// copy whose origin is the clock before, keeping b's right origin, parent
// and key, with storage of its own for the cut payload; ok is false when
// the content cannot be cut there.
func sliceStruct(b Block, diff uint64) (Block, bool) {
	if b.Kind != WireBlockItem {
		b.ID.Clock += diff
		b.Len -= diff
		return b, true
	}
	// A store item read back without its key gives a piece without it; a
	// first item (no origins) keeps its key, and the piece, which has an
	// origin now, is written with the key bit, as yjs writes it.
	read := b.read && (b.Item.Origin != nil || b.Item.RightOrigin != nil)
	cp := *b.Item
	if err := sliceWireItemRight(&cp, diff); err != nil {
		return Block{}, false
	}
	ownPayload(&cp.Content)
	return Block{Kind: WireBlockItem, Item: &cp, read: read}, true
}

// mergeWith is yjs GC.mergeWith and Skip.mergeWith (extend by the next run
// of the same kind); structs read from an update are items that never merge.
func mergeWith(cw *Block, right Block) bool {
	if cw.Kind != right.Kind || cw.Kind == WireBlockItem {
		return false
	}
	cw.Len += right.Len
	return true
}

// clientStream lists blocks by client as a merge reads an update: clients
// descending, each client's blocks in their order, without Skips (empty
// blocks stay, as yjs's reader keeps them).
func clientStream(blocks map[uint64][]Block) []Block {
	clients := make([]uint64, 0, len(blocks))
	n := 0
	for c, list := range blocks {
		clients = append(clients, c)
		n += len(list)
	}
	slices.Sort(clients)
	out := make([]Block, 0, n)
	for i := len(clients) - 1; i >= 0; i-- {
		for _, b := range blocks[clients[i]] {
			if b.Kind == WireBlockSkip || (b.Kind == WireBlockItem && b.Item == nil) {
				continue
			}
			out = append(out, b)
		}
	}
	return out
}

// asRead gives an item the fields yjs's update reader gives it: one with an
// origin or a right origin is read without its parent and key (they are
// not written for it), so a struct that passes through an update in yjs
// (the queue, the store part of a state merge, a struct written from an
// offset) has none. Changes it in place and returns it.
func asRead(it *block.Item) *block.Item {
	if it.Origin == nil && it.RightOrigin == nil {
		return it
	}
	it.Parent = block.Parent{}
	it.ParentSub = nil
	return it
}

// asWritten is a client's leftover as yjs writes it into its queue:
// writeStructs starts at findIndexSS(structs, the first struct's clock),
// which passes over empty structs at the start when a later struct covers
// that clock (the last struct when it starts there, else one found by a
// pivoted binary search), and writes the struct it finds from that clock.
// yjs's list has a Skip at every gap (an update's own, or the merge's in
// its queue), so the search runs over list with those put back. Where
// findIndexSS finds nothing yjs throws and rejects the update; the list is
// then kept as it is. list holds no Skips.
func asWritten(list []Block) []Block {
	if len(list) < 2 {
		return list
	}
	v := make([]Block, 0, len(list)+2)
	real := make([]int, 0, len(list)+2) // index into list, -1 for a Skip put back
	next := blockStartClock(list[0])
	for i, b := range list {
		if start := blockStartClock(b); start > next {
			v = append(v, Block{Kind: WireBlockSkip, ID: block.ID{Client: blockClient(&b), Clock: next}, Len: start - next})
			real = append(real, -1)
		}
		v = append(v, b)
		real = append(real, i)
		next = max(next, blockStartClock(b)+blockLen(b))
	}
	clock := blockStartClock(list[0])
	k, ok := findIndexSS(v, clock)
	if !ok {
		return list
	}
	i := real[k]
	if i < 0 { // a Skip: what follows it is written
		i = real[k+1]
	}
	out := list[i:]
	if off := clock - min(clock, blockStartClock(out[0])); off > 0 {
		// A struct that starts before the first clock (only in a list that
		// is not clock-ordered) is written from it.
		if s, ok := sliceStruct(out[0], off); ok {
			if s.Kind == WireBlockItem {
				asRead(s.Item)
			}
			out = append([]Block{s}, out[1:]...)
		}
	}
	return out
}

// findIndexSS is yjs findIndexSS (utils/StructStore.js) over structs,
// float pivot included; ok is false where it throws or reads past the
// list.
func findIndexSS(structs []Block, clock uint64) (int, bool) {
	left, right := 0, len(structs)-1
	mid := structs[right]
	if blockStartClock(mid) == clock {
		return right, true
	}
	idx := math.Floor(float64(clock) / (float64(blockStartClock(mid)) + float64(blockLen(mid)) - 1) * float64(right))
	for left <= right {
		if math.IsNaN(idx) || idx < 0 || idx >= float64(len(structs)) {
			return 0, false // structs[idx] is undefined in yjs: a TypeError
		}
		mi := int(idx)
		mid = structs[mi]
		if mc := blockStartClock(mid); mc <= clock {
			if clock < mc+blockLen(mid) {
				return mi, true
			}
			left = mi + 1
		} else {
			right = mi - 1
		}
		idx = math.Floor(float64(left+right) / 2)
	}
	return 0, false
}
