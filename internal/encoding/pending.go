package encoding

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/store"
)

// sliceWireItemRight trims a decoded wire Item to keep only the clocks
// at or after offset, discarding the [0, offset) prefix the receiver
// already has. After commit-time squash a peer may emit one item
// spanning clocks the receiver partly knows; this keeps the unknown
// right half so it integrates, instead of the whole item being dropped
// on a first-clock-known check (which loses the new tail and breaks
// convergence).
//
// The content is split (left half discarded), clock and length advance
// by offset, and Origin is repointed at the last clock of the dropped
// prefix so Repair re-resolves Left. Caller re-runs Repair before
// integrating. Mirrors the slice yrs performs in Update::integrate.
func sliceWireItemRight(it *block.Item, offset uint64) error {
	if offset == 0 || offset >= it.Len {
		return fmt.Errorf("encoding: slice offset %d out of range for item len %d", offset, it.Len)
	}
	right, err := it.Content.Split(offset)
	if err != nil {
		return err
	}
	it.Content = right
	newClock := it.ID.Clock + offset
	originID := block.ID{Client: it.ID.Client, Clock: newClock - 1}
	it.Origin = &originID
	it.ID.Clock = newClock
	it.Len -= offset
	it.Left = nil // re-resolved by Repair from the new Origin
	return nil
}

// Pending buffers items and delete-set entries that arrived before
// their causal dependencies (Origin / RightOrigin / Parent-by-ID
// referring to clocks the local BlockStore has not yet seen).
//
// yrs's equivalent state lives in `Store::pending` (`store.rs` field
// `pending: Option<PendingUpdate>`) plus `pending_ds`
// (`Option<DeleteSet>`). We collapse both into a single Pending
// struct attached to the Doc via the opaque `pendingState any` slot.
//
// Lifecycle (yjs readUpdateV2): Update.Apply integrates the incoming
// update's blocks by dependency stack (integrateStack), queues what is
// left, applies the update's delete set and then the queued one, and,
// when the store has passed a clock in Missing, integrates the queued
// blocks once more and applies the queued delete set again. Items that
// integrate leave the queue; the leftover stays for the next ApplyUpdate
// call.
//
// Concurrency: Pending is mutated only under the doc's write lock
// (acquired by the surrounding TransactionMut). No internal mutex.
type Pending struct {
	// Blocks groups queued wire-level Block records by clientID.
	// Per-client entries are kept in clock-ascending order to
	// preserve causal-prefix ordering — yrs relies on this when
	// dropping a contiguous prefix that has become satisfied.
	Blocks map[uint64][]Block

	// DeleteSet stores ranges whose target IDs are not yet present
	// in the local store. yrs calls this `pending_ds`. Each Apply
	// re-scans these ranges; those whose target now exists get
	// applied.
	DeleteSet *IdSet

	// Missing is yjs's pendingStructs.missing: for each client the
	// queued blocks wait on, the clock past which the store has to get
	// before a retry can integrate anything (integrateStack). It decides
	// whether the queue is retried; MissingSV, what to ask a peer for, is
	// computed separately.
	Missing map[uint64]uint64
}

// NewPending returns an empty buffer.
func NewPending() *Pending {
	return &Pending{
		Blocks:    map[uint64][]Block{},
		DeleteSet: NewIdSet(),
		Missing:   map[uint64]uint64{},
	}
}

// IsEmpty reports whether the buffer holds zero queued items.
func (p *Pending) IsEmpty() bool {
	if p == nil {
		return true
	}
	if len(p.Blocks) > 0 {
		return false
	}
	if p.DeleteSet == nil {
		return true
	}
	empty := true
	p.DeleteSet.Iterate(func(_ uint64, ranges []Range) {
		if len(ranges) > 0 {
			empty = false
		}
	})
	return empty
}

// BlockCount returns the total number of queued blocks across all
// clients. Useful for tests and observability ("how many items are
// stuck waiting?").
func (p *Pending) BlockCount() int {
	if p == nil {
		return 0
	}
	n := 0
	for _, list := range p.Blocks {
		n += len(list)
	}
	return n
}

// MissingSV returns the state vector to ask a peer with: for every
// client the queue waits on, the clock the local store has for it, so
// a peer's EncodeDiff against the vector sends everything from there on.
// The clients are those of a queued block with a hole before it in its
// own sequence, those a queued block refers to (Origin, RightOrigin,
// Parent-by-ID) without the store having the target, and those with a
// queued delete range.
//
// It names what the store has, not the clock of the missing target: a
// vector naming the target's clock + 1, as this did before, tells the
// peer the target itself is known, and the diff leaves it out. A
// first-in-a-root item has no reference to walk, so its own hole is the
// only way to name it; a queue holding only deletes still needs the
// clients they delete from.
//
// Returns an empty map when nothing is missing.
func (p *Pending) MissingSV(bs *store.BlockStore) store.StateVector {
	out := make(store.StateVector)
	if p == nil {
		return out
	}
	need := func(client uint64) {
		out[client] = bs.GetClock(client)
	}
	track := func(ref block.ID) {
		if !bs.Contains(ref) {
			need(ref.Client)
		}
	}
	for client, list := range p.Blocks {
		for _, b := range list {
			if blockStartClock(b) > bs.GetClock(client) {
				need(client)
			}
			if b.Kind != WireBlockItem || b.Item == nil {
				continue
			}
			it := b.Item
			if it.Origin != nil {
				track(*it.Origin)
			}
			if it.RightOrigin != nil {
				track(*it.RightOrigin)
			}
			if it.Parent.Kind == block.ParentID {
				track(it.Parent.ID)
			}
		}
	}
	if p.DeleteSet != nil {
		p.DeleteSet.Iterate(func(client uint64, ranges []Range) {
			if len(ranges) > 0 {
				need(client)
			}
		})
	}
	return out
}

// blockLen is how many clocks a queued block covers, whichever kind it is.
func blockLen(b Block) uint64 {
	if b.Kind == WireBlockItem {
		if b.Item == nil {
			return 0
		}
		return b.Item.Len
	}
	return b.Len
}

// addBlock queues b for client (mergeBlocks with one block).
func (p *Pending) addBlock(client uint64, b Block) {
	p.mergeBlocks(map[uint64][]Block{client: {b}})
}

// mergeBlocks adds blocks to the queue and normalizes each client it
// touches the way yjs mergeUpdatesV2 merges one client's structs, since
// yjs's queue is a merged update: by start clock, a longer block before a
// shorter one at the same start; a block inside what the ones before it
// cover is dropped; one that straddles their end is cut to its uncovered
// part, its origin then the client's own previous clock, as yjs
// sliceStruct leaves it. Without this, the same clocks queued in
// different splits (peers split a run at different points) piled up:
// each walled again and none ever left.
//
// Skip blocks are never queued: they carry nothing, the next block of
// their client meets the same hole without them, and a queued Skip could
// keep out a real block for the same clocks that arrives later.
func (p *Pending) mergeBlocks(blocks map[uint64][]Block) {
	for client, add := range blocks {
		list := p.Blocks[client]
		touched := false
		for _, b := range add {
			if b.Kind == WireBlockSkip || blockLen(b) == 0 || (b.Kind == WireBlockItem && b.Item == nil) {
				continue
			}
			list = append(list, b)
			touched = true
		}
		if !touched {
			continue
		}
		sort.SliceStable(list, func(a, b int) bool {
			sa, sb := blockStartClock(list[a]), blockStartClock(list[b])
			if sa != sb {
				return sa < sb
			}
			return blockLen(list[a]) > blockLen(list[b])
		})
		out := make([]Block, 0, len(list))
		var covered uint64
		for k, b := range list {
			start, n := blockStartClock(b), blockLen(b)
			if k > 0 && start+n <= covered {
				continue
			}
			if k > 0 && start < covered {
				off := covered - start
				if b.Kind == WireBlockItem {
					if err := sliceWireItemRight(b.Item, off); err != nil {
						continue // unsplittable: the wider block before it covers the overlap
					}
					// The cut keeps a substring or subslice of what the
					// block was decoded with. Queued, that would hold the
					// whole decoded payload for its uncovered tail: runs of
					// N characters starting one clock apart, each walled,
					// kept N² bytes for clocks already known.
					ownPayload(&b.Item.Content)
				} else {
					b.ID.Clock += off
					b.Len -= off
				}
				start, n = covered, n-off
			}
			out = append(out, b)
			covered = start + n
		}
		if len(out) == 0 {
			delete(p.Blocks, client)
		} else {
			p.Blocks[client] = out
		}
	}
}

// ownPayload gives c storage of its own, so a cut piece no longer keeps
// the rest of the payload it was cut from reachable.
func ownPayload(c *block.Content) {
	switch c.Kind {
	case block.KindString:
		c.Str = strings.Clone(c.Str)
	case block.KindAny:
		c.Anys = slices.Clone(c.Anys)
	case block.KindJSON:
		c.JSONStrs = slices.Clone(c.JSONStrs)
	}
}

func blockStartClock(b Block) uint64 {
	switch b.Kind {
	case WireBlockItem:
		if b.Item != nil {
			return b.Item.ID.Clock
		}
	case WireBlockGC, WireBlockSkip:
		return b.ID.Clock
	}
	return 0
}

// crossClientMissing is yjs Item.getMissing's first half: the client of
// the first Origin, RightOrigin or Parent-by-ID of ANOTHER client whose
// clock the store does not have yet. A reference to the block's own
// client is not checked: integrateStack only gets here once the store
// has every clock of that client below the block, as yjs relies on its
// offset check. GC and Skip blocks depend on nothing.
func crossClientMissing(bs *store.BlockStore, b Block) (uint64, bool) {
	if b.Kind != WireBlockItem || b.Item == nil {
		return 0, false
	}
	it := b.Item
	own := it.ID.Client
	if r := it.Origin; r != nil && r.Client != own && r.Clock >= bs.GetClock(r.Client) {
		return r.Client, true
	}
	if r := it.RightOrigin; r != nil && r.Client != own && r.Clock >= bs.GetClock(r.Client) {
		return r.Client, true
	}
	if it.Parent.Kind == block.ParentID {
		if r := it.Parent.ID; r.Client != own && r.Clock >= bs.GetClock(r.Client) {
			return r.Client, true
		}
	}
	return 0, false
}

// collectedDep reports whether an item whose references are all in the
// store hangs off something garbage collection removed: its origin or
// right origin lies in a GC run, or its parent by ID is a GC run or an
// item that no longer holds a type (a collected type keeps only a
// ContentDeleted marker). Mirrors the cases yjs Item.getMissing turns
// into a null parent.
func collectedDep(txn *doc.TransactionMut, it *block.Item) bool {
	if it.Origin != nil && txn.GetItem(*it.Origin) == nil {
		return true
	}
	if it.RightOrigin != nil && txn.GetItem(*it.RightOrigin) == nil {
		return true
	}
	if it.Parent.Kind == block.ParentID {
		p := txn.GetItem(it.Parent.ID)
		if p == nil || p.Content.Kind != block.KindType || p.Content.Branch == nil {
			return true
		}
	}
	return false
}

// foldDeleteSet queues u's delete set in p, in the order the update
// wrote its clients, which applyDeletes keeps for an update's own
// delete set.
func (p *Pending) foldDeleteSet(u *Update) {
	if u.DeleteSet == nil {
		return
	}
	for _, client := range u.DeleteSet.ClientsInOrder() {
		for _, r := range u.DeleteSet.ClientRanges(client) {
			p.DeleteSet.Insert(client, r.Start, r.Length)
		}
	}
}

// stackEntry is a block integrateStack had to put aside: refs[client][i].
type stackEntry struct {
	client uint64
	i      int
}

// integrateStack is yjs integrateStructs over refs, each client's blocks
// in clock order. It returns what could not be integrated, from the
// first block of each walled client on, without Skips, and the missing
// vector (Pending.Missing) for it.
//
// Clients go from the highest ID down. A block whose client has a hole
// before it, or that waits on a block of a client this call has none
// left of, walls: every block on the stack and the rest of their
// clients wait for a later update. A block that waits on a client this
// call still has blocks of is put on the stack, and that client's NEXT
// block is taken, whatever it is, until the one waited on integrates;
// then the stack unwinds. So the order of integration, and with it the
// order of the transaction's deletions, of the map keys it sets first
// and of the types it changes first, is yjs's: a pass that skips the
// waiting block and comes back for it is not.
//
// The checks run in yjs's order, on the block as decoded: the hole
// (offset < 0), then the missing reference, then, for a block the store
// does not wholly have, whether what it hangs off was collected, then the
// cut of the known prefix. A wholly known block is not integrated, but a
// missing reference still walls its client, as in yjs.
func integrateStack(txn *doc.TransactionMut, refs map[uint64][]Block) (map[uint64][]Block, map[uint64]uint64) {
	bs := txn.Store()
	rest := map[uint64][]Block{}
	missing := map[uint64]uint64{}
	total := 0
	clients := make([]uint64, 0, len(refs))
	for c, list := range refs {
		if len(list) > 0 {
			clients = append(clients, c)
			total += len(list)
		}
	}
	if len(clients) == 0 {
		return rest, missing
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] < clients[j] })
	next := map[uint64]int{}
	walled := map[uint64]bool{}
	var stack []stackEntry

	updateMissing := func(client, clock uint64) {
		if m, ok := missing[client]; !ok || clock < m {
			missing[client] = clock
		}
	}
	nextTarget := func() (uint64, bool) {
		for len(clients) > 0 {
			c := clients[len(clients)-1]
			if !walled[c] && next[c] < len(refs[c]) {
				return c, true
			}
			clients = clients[:len(clients)-1]
		}
		return 0, false
	}
	wall := func() {
		from := map[uint64]int{}
		var order []uint64
		for _, e := range stack {
			if f, ok := from[e.client]; !ok || e.i < f {
				if !ok {
					order = append(order, e.client)
				}
				from[e.client] = e.i
			}
		}
		for _, c := range order {
			if walled[c] {
				continue
			}
			walled[c] = true
			for _, b := range refs[c][from[c]:] {
				if b.Kind != WireBlockSkip {
					rest[c] = append(rest[c], b)
				}
			}
		}
		stack = stack[:0]
	}

	cur, ok := nextTarget()
	if !ok {
		return rest, missing
	}
	head := stackEntry{cur, next[cur]}
	next[cur]++
	// No valid update needs more steps: each block is taken once, and the
	// stack, bounded by the number of clients, unwinds through each at most
	// once more. Past the bound the remaining blocks stay queued.
	limit := total*(len(refs)+2) + 16
	for steps := 0; ; steps++ {
		if steps > limit {
			for c, list := range refs {
				if walled[c] {
					continue
				}
				for _, b := range list[min(next[c], len(list)):] {
					if b.Kind != WireBlockSkip {
						rest[c] = append(rest[c], b)
					}
				}
				walled[c] = true
			}
			break
		}
		b := refs[head.client][head.i]
		if b.Kind != WireBlockSkip {
			state := bs.GetClock(head.client)
			start := blockStartClock(b)
			if start > state {
				// A hole before it in its own client.
				stack = append(stack, head)
				updateMissing(head.client, start-1)
				wall()
			} else if m, has := crossClientMissing(bs, b); has {
				stack = append(stack, head)
				if !walled[m] && next[m] < len(refs[m]) {
					head = stackEntry{m, next[m]}
					next[m]++
					continue
				}
				// It waits on an update that has not come yet.
				updateMissing(m, bs.GetClock(m))
				wall()
			} else if start+blockLen(b) > state {
				if !integrateBlock(txn, b, state-start) {
					rest[head.client] = append(rest[head.client], b)
				}
			}
		}
		switch {
		case len(stack) > 0:
			head = stack[len(stack)-1]
			stack = stack[:len(stack)-1]
		case !walled[cur] && next[cur] < len(refs[cur]):
			head = stackEntry{cur, next[cur]}
			next[cur]++
		default:
			if cur, ok = nextTarget(); !ok {
				return rest, missing
			}
			head = stackEntry{cur, next[cur]}
			next[cur]++
		}
	}
	return rest, missing
}

// integrateBlock integrates b, of which the store already has the first
// offset clocks (yjs Item.integrate / GC.integrate with an offset). An
// item hanging off something collected (decided on its references as
// decoded, as yjs getMissing does before integrate cuts it) becomes a GC
// run of its unknown part. Returns false when an item cannot be
// repaired; it then stays queued.
func integrateBlock(txn *doc.TransactionMut, b Block, offset uint64) bool {
	bs := txn.Store()
	switch b.Kind {
	case WireBlockGC:
		start := b.ID.Clock + offset
		bs.PushGC(b.ID.Client, start, b.ID.Clock+b.Len-1)
		return true
	case WireBlockItem:
		it := b.Item
		collected := collectedDep(txn, it)
		if offset > 0 {
			if err := sliceWireItemRight(it, offset); err != nil {
				// Non-splittable content at a partial boundary should not
				// occur (single-element kinds have Len 1); drop rather than
				// spin.
				return true
			}
		}
		if collected {
			// What the item hangs off was garbage-collected: it
			// integrates as a GC run of its length, so the client's clock
			// moves on (yjs Item.getMissing sets the parent to null,
			// Item.integrate writes a GC).
			bs.PushGC(it.ID.Client, it.ID.Clock, it.ID.Clock+it.Len-1)
			return true
		}
		if err := block.Repair(it, txn); err != nil {
			return false
		}
		bs.PushBlock(it)
		if dropped := it.Integrate(txn, 0); dropped {
			txn.Delete(it)
		}
		return true
	}
	return true
}

// applyDeletes applies the delete set and keeps the ranges that still
// point at unseen IDs. An update's own delete set (wireOrder) goes client
// by client in the order the update wrote them, as yjs
// readAndApplyDeleteSet reads it; the queued one from the highest client
// ID down, the order yjs re-encodes its pending delete set in
// (writeDeleteSet). The order of the deletions is the order an
// UndoManager restores them in. Each range goes through applyDeleteRange,
// which splits items at the range boundaries before tombstoning, so a
// wire delete-set that covers a SUBSET of an existing item's clocks
// tombstones only the matching middle slice instead of the whole item.
// Returns the number of items deleted.
func (p *Pending) applyDeletes(txn *doc.TransactionMut, wireOrder bool) int {
	if p.DeleteSet == nil {
		return 0
	}
	deleted := 0
	remaining := NewIdSet()
	var clients []uint64
	if wireOrder {
		clients = p.DeleteSet.ClientsInOrder()
	} else {
		p.DeleteSet.Iterate(func(client uint64, _ []Range) {
			clients = append(clients, client)
		})
		for i, j := 0, len(clients)-1; i < j; i, j = i+1, j-1 {
			clients[i], clients[j] = clients[j], clients[i]
		}
	}
	for _, client := range clients {
		for _, r := range p.DeleteSet.ClientRanges(client) {
			n, leftover := applyDeleteRange(txn, client, r.Start, r.End())
			deleted += n
			if leftover.Length > 0 {
				remaining.Insert(client, leftover.Start, leftover.Length)
			}
		}
	}
	p.DeleteSet = remaining
	return deleted
}

// applyDeleteRange tombstones every live Item covering any clock
// in [start, end) for the given client. Splits items at start /
// end boundaries via txn.MaterializeCleanStart / MaterializeCleanEnd
// so the deletion lands precisely on the range. Returns the count
// of items actually tombstoned plus the leftover range that could
// not be applied (because a clock was missing from the store —
// caller should re-queue).
//
// Mirrors yrs Store::integrate_delete_set inner loop. Per
// docs/tech-debt.md "DeleteSet apply does not split items at
// range boundaries" — resolves the gap that broke cross-client
// convergence when a peer's delete partially covers an item the
// receiver has as a single contiguous block.
func applyDeleteRange(txn *doc.TransactionMut, client, start, end uint64) (int, Range) {
	bs := txn.Store()
	deleted := 0
	clock := start
	for clock < end {
		cell, ok := bs.GetBlock(block.ID{Client: client, Clock: clock})
		if !ok {
			// Unseen — re-queue from here.
			return deleted, Range{Start: clock, Length: end - clock}
		}
		if it := cell.AsItem(); it == nil || it.IsDeleted() {
			// GC cell or an item already deleted: step past it whole.
			// Cutting a deleted item would leave its pieces apart for
			// good (nothing in this transaction re-merges them), so the
			// store and every later encoding would differ from yjs,
			// whose readAndApplyDeleteSet only splits live items.
			if !advanceClock(&clock, cell.ClockEnd()) {
				break
			}
			continue
		}

		// Materialize an Item starting at exactly clock. If the
		// existing cell begins before clock, this splits the cell
		// and returns the right half. Then check if the item
		// extends past end-1; if so, materialize a clean end to
		// split off the unaffected tail. Re-fetch after split.
		item := txn.MaterializeCleanStart(block.ID{Client: client, Clock: clock})
		if item == nil {
			// Shouldn't happen after GetBlock succeeded; defensive.
			if !advanceClock(&clock, cell.ClockEnd()) {
				break
			}
			continue
		}
		if item.ID.Clock+item.Len-1 >= end {
			// Split at end-1 to keep our target item bounded.
			_ = txn.MaterializeCleanEnd(block.ID{Client: client, Clock: end - 1})
			// Re-fetch the item starting at clock — the split
			// produced a new left half ending exactly at end-1.
			item = txn.GetItem(block.ID{Client: client, Clock: clock})
			if item == nil {
				return deleted, Range{}
			}
		}

		if !item.IsDeleted() {
			txn.Delete(item)
			deleted++
		}
		if !advanceClock(&clock, item.ID.Clock+item.Len-1) {
			break
		}
	}
	return deleted, Range{}
}

// advanceClock moves *clock past lastInclusive (the inclusive upper
// clock of the cell just processed) and reports whether it advanced. It
// returns false WITHOUT moving when stepping forward would not make
// progress: either lastInclusive is below the current clock (a
// zero-length item, whose ID.Clock+Len-1 underflows below clock) or it
// is the very top of the uint64 space (a malformed Skip reaching
// MaxUint64, where +1 wraps to 0 and would restart the scan). Callers
// break instead of spinning or wrapping back to clock 0. Well-formed
// updates always advance, so this never fires for valid input.
func advanceClock(clock *uint64, lastInclusive uint64) bool {
	if lastInclusive < *clock || lastInclusive == ^uint64(0) {
		return false
	}
	*clock = lastInclusive + 1
	return true
}
