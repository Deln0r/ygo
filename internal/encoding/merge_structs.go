package encoding

import (
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
	// prevQ[i]: the queue's lowest client above rc[i], the one the queue
	// reader writes just before it reaches rc[i]; one pass over the queue,
	// before the leftover's own clients join it.
	prevQ := make([]uint64, len(rc))
	hasPrevQ := make([]bool, len(rc))
	for x := range q {
		for i, c := range rc {
			if x > c && (!hasPrevQ[i] || x < prevQ[i]) {
				prevQ[i], hasPrevQ[i] = x, true
			}
		}
	}
	lastQ := true // the queue ran out of the previous shared client last
	for i := len(rc) - 1; i >= 0; i-- {
		c := rc[i]
		rl := r[c]
		ql, inQ := q[c]
		if !inQ {
			if wellFormed(rl) {
				q[c] = rl
			} else {
				q[c] = mergeStructs(rl, nil)
			}
			continue
		}
		hasPrevR := i+1 < len(rc)
		var qFront bool
		switch {
		case !hasPrevQ[i] && !hasPrevR:
			qFront = true
		case !hasPrevQ[i]:
			qFront = false
		case !hasPrevR:
			qFront = true
		case prevQ[i] != rc[i+1]:
			qFront = prevQ[i] < rc[i+1]
		default:
			// Both come from the client before, which they share.
			qFront = lastQ
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
// descending, each client's blocks in their order, without Skips or empty
// blocks.
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
			if b.Kind == WireBlockSkip || blockLen(b) == 0 || (b.Kind == WireBlockItem && b.Item == nil) {
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
