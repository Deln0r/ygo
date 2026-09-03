package encoding

import (
	"fmt"
	"sort"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/store"
)

// itemFullyKnown reports whether every clock an item covers is already
// in the store for its client (its clock range lies entirely below the
// client's next-free clock). Such an item carries no new information.
func itemFullyKnown(bs *store.BlockStore, it *block.Item) bool {
	return it.ID.Clock+it.Len <= bs.GetClock(it.ID.Client)
}

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
// Lifecycle: ApplyUpdate folds the incoming update's blocks/DS into
// Pending, then runs Pending.Drain in a loop until no further
// progress is made. Items that successfully integrate are removed
// from Pending; the leftover stays for the next ApplyUpdate call.
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
	// in the local store. yrs calls this `pending_ds`. Each Drain
	// pass re-scans these ranges; those whose target now exists
	// get applied.
	DeleteSet *IdSet
}

// NewPending returns an empty buffer.
func NewPending() *Pending {
	return &Pending{
		Blocks:    map[uint64][]Block{},
		DeleteSet: NewIdSet(),
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

// MissingSV returns the state vector describing what the local
// store is missing to satisfy every queued item's dependencies.
// Sync-protocol callers use this to request the gap from the peer
// ("send me everything since (client, clock) for each entry").
//
// For each pending item we walk Origin and RightOrigin; for each
// reference that points outside the current BlockStore, we record
// (clientID, clock+1) — the receiver wants every clock starting at
// that target, and the SV convention is "the smallest clock NOT
// yet seen". Parent-by-ID references contribute the same way.
//
// A block queued because of a hole in its OWN client sequence
// contributes that client's currently-known clock, which is the
// smallest one not yet seen for it. Without this, a document stuck
// purely on such a hole reported an empty vector and could not name
// what it needed — the first item in a root type has no Origin,
// RightOrigin or Parent-by-ID to walk.
//
// Entries merge by taking the larger clock, so when one client has
// several unmet needs the vector names the highest of them: it is a
// request that may have to be repeated, not a complete manifest.
//
// Returns an empty map when nothing is missing.
func (p *Pending) MissingSV(bs *store.BlockStore) store.StateVector {
	out := make(store.StateVector)
	if p == nil {
		return out
	}
	track := func(ref block.ID) {
		if bs.Contains(ref) {
			return
		}
		want := ref.Clock + 1
		if cur, ok := out[ref.Client]; !ok || want > cur {
			out[ref.Client] = want
		}
	}
	for client, list := range p.Blocks {
		for _, b := range list {
			// A hole in the block's own client sequence is a dependency as
			// real as an Origin reference, and for a first-in-a-root item it
			// is the ONLY one - all three references below are nil or
			// non-ID there. Leaving it out made MissingSV report an empty
			// vector on a document that was demonstrably stuck, so the
			// documented recovery path (ask a peer for what is missing)
			// could not name the thing it needed.
			// Block.ID is only populated for GC and Skip blocks; an item
			// carries its own. Reading the wrong one here made this whole
			// branch silently dead - it compared 0 against the known clock
			// and never fired.
			start := b.ID
			if b.Kind == WireBlockItem && b.Item != nil {
				start = b.Item.ID
			}
			if known := bs.GetClock(client); start.Clock > known {
				// The smallest clock not yet seen for this client, which is
				// exactly what the store already has: everything from there
				// up to the queued block is what we are waiting for.
				if cur, ok := out[client]; !ok || known > cur {
					out[client] = known
				}
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
	return out
}

// addBlock appends b to the per-client queue, keeping the queue
// sorted by clock ascending so causal-prefix integration sees the
// earliest unsatisfied item first.
func (p *Pending) addBlock(client uint64, b Block) {
	list := p.Blocks[client]
	clock := blockStartClock(b)
	idx := sort.Search(len(list), func(i int) bool {
		return blockStartClock(list[i]) >= clock
	})
	// Skip exact-clock duplicates — re-applying the same update
	// shouldn't double-queue.
	if idx < len(list) && blockStartClock(list[idx]) == clock {
		return
	}
	list = append(list, Block{})
	copy(list[idx+1:], list[idx:])
	list[idx] = b
	p.Blocks[client] = list
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

// itemMissingDep reports whether any of an Item's wire-level
// dependency references point to a clock the local store has not
// seen yet. Used both during the first Apply pass and during
// Drain to decide if an item is still stuck.
func itemMissingDep(bs *store.BlockStore, it *block.Item) bool {
	if it == nil {
		return false
	}
	if it.Origin != nil && !bs.Contains(*it.Origin) {
		return true
	}
	if it.RightOrigin != nil && !bs.Contains(*it.RightOrigin) {
		return true
	}
	if it.Parent.Kind == block.ParentID && !bs.Contains(it.Parent.ID) {
		return true
	}
	// A hole in the item's OWN client sequence is a missing dependency
	// too, even when every explicit reference resolves. An item that
	// starts above the clock we have for its client (a delta published
	// against a state vector we never received, or simply an update
	// delivered out of order) has an implicit dependency on the blocks
	// filling that hole: yrs `Update::integrate` and yjs
	// `integrateStructs` both hold such a struct back and record the
	// gap in `missing`.
	//
	// Integrating it anyway pushes the store clock past the hole, and
	// PushBlock's precondition (store.go: "item.ID.Clock ==
	// s.GetClock(...)") is broken. The damage is silent and permanent:
	// the blocks that would have filled the hole arrive later, read as
	// clock-below-known, and are dropped as already-known. Measured
	// against yjs 13.6.32 before the fix, one client editing two roots
	// and publishing only the second delta lost the first root's text
	// outright when the deltas arrived newest-first.
	if it.ID.Clock > bs.GetClock(it.ID.Client) {
		return true
	}
	return false
}

// foldUpdate merges every block and delete-set entry from u into p.
// Items that the local store already has are silently dropped (no
// need to retry them).
func (p *Pending) foldUpdate(u *Update, bs *store.BlockStore) {
	for client, list := range u.Blocks {
		for _, b := range list {
			switch b.Kind {
			case WireBlockItem:
				// Keep unless the item is fully known. A partially
				// known item (squashed peer block overlapping the
				// state-vector boundary) is queued and sliced at
				// Drain time, not dropped on a first-clock check.
				if b.Item != nil && itemFullyKnown(bs, b.Item) {
					continue
				}
				p.addBlock(client, b)
			case WireBlockGC, WireBlockSkip:
				if bs.Contains(b.ID) {
					continue
				}
				p.addBlock(client, b)
			}
		}
	}
	if u.DeleteSet != nil {
		u.DeleteSet.Iterate(func(client uint64, ranges []Range) {
			for _, r := range ranges {
				p.DeleteSet.Insert(client, r.Start, r.Length)
			}
		})
	}
}

// Drain attempts to integrate every queued block whose dependencies
// are now satisfied. Returns the number of blocks that successfully
// integrated this pass. Apply callers loop on Drain until it
// returns 0, which means the queue has reached its fixed point for
// this transaction.
//
// The order of work within a pass: iterate clients in ascending
// clientID, items within a client in clock-ascending order. A block
// with a still-missing dep is skipped (left in the queue) — the
// causal-prefix property of yrs updates means a later clock for
// the same client may yet integrate (depends on a different chain),
// so we do not short-circuit on first failure within a client.
func (p *Pending) Drain(txn *doc.TransactionMut) int {
	bs := txn.Store()
	progress := 0

	// Block integration pass.
	clients := make([]uint64, 0, len(p.Blocks))
	for c := range p.Blocks {
		clients = append(clients, c)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] < clients[j] })

	for _, c := range clients {
		list := p.Blocks[c]
		remaining := list[:0]
		for _, b := range list {
			switch b.Kind {
			case WireBlockGC:
				if bs.Contains(b.ID) {
					continue
				}
				if b.ID.Clock > bs.GetClock(c) {
					// Gap ahead of what we have for this client;
					// PushGC has PushBlock's monotonicity
					// precondition, so queue rather than punch a
					// hole. Same reasoning as itemMissingDep.
					remaining = append(remaining, b)
					continue
				}
				bs.PushGC(c, b.ID.Clock, b.ID.Clock+b.Len-1)
				progress++
			case WireBlockSkip:
				// Skip blocks reserve clock space without semantics;
				// we drop them once they reach the front of the queue
				// (matches Apply's current handling).
				progress++
			case WireBlockItem:
				it := b.Item
				if it == nil {
					continue
				}
				known := bs.GetClock(c)
				if it.ID.Clock+it.Len <= known {
					continue // fully known now
				}
				if it.ID.Clock < known {
					// Partial overlap: drop the known prefix and keep
					// the unknown right half. Origin now points just
					// below `known`, a clock the store has, so the
					// left dependency is satisfied.
					if err := sliceWireItemRight(it, known-it.ID.Clock); err != nil {
						// Non-splittable content at a partial boundary
						// should not occur (single-element kinds have
						// Len 1); drop rather than spin.
						continue
					}
				}
				if itemMissingDep(bs, it) {
					remaining = append(remaining, b)
					continue
				}
				if err := block.Repair(it, txn); err != nil {
					// Repair failed (typically ParentIDUnresolved
					// when nested types not yet supported) — leave
					// queued; future code may resolve it.
					remaining = append(remaining, b)
					continue
				}
				bs.PushBlock(it)
				if dropped := it.Integrate(txn, 0); dropped {
					txn.Delete(it)
				}
				progress++
			}
		}
		if len(remaining) == 0 {
			delete(p.Blocks, c)
		} else {
			p.Blocks[c] = remaining
		}
	}

	// Delete-set pass: rebuild a per-client remaining list whose
	// ranges still point at unseen IDs. Each accepted range goes
	// through applyDeleteRange which splits items at the range
	// boundaries before tombstoning, so a wire delete-set that
	// covers a SUBSET of an existing item's clocks tombstones only
	// the matching middle slice instead of the whole item.
	if p.DeleteSet != nil {
		remaining := NewIdSet()
		p.DeleteSet.Iterate(func(client uint64, ranges []Range) {
			for _, r := range ranges {
				deleted, leftover := applyDeleteRange(txn, client, r.Start, r.End())
				progress += deleted
				if leftover.Length > 0 {
					remaining.Insert(client, leftover.Start, leftover.Length)
				}
			}
		})
		p.DeleteSet = remaining
	}
	return progress
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
		if cell.AsItem() == nil {
			// GC cell — already tombstoned, just step past.
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
