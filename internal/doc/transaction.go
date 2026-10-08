package doc

import (
	"sort"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/store"
)

// Transaction is a read-only transaction holding the doc's read lock
// for its lifetime. Created by Doc.ReadTxn; released by Close.
//
// Mirrors yrs Transaction<'doc>. The lock is held until Close runs;
// Go has no Drop trait, so Close must be called explicitly. Use a
// `defer txn.Close()` immediately after acquisition.
//
// A Transaction value must not be retained past its Close call. yrs
// enforces this with a 'doc lifetime parameter; we document the
// contract and trust callers (see tech-debt.md).
type Transaction struct {
	doc    *Doc
	closed bool
}

// ReadTxn acquires the doc's read lock and returns a Transaction. The
// caller MUST call Close (typically via defer) to release the lock.
//
// Multiple read transactions may coexist; they block any concurrent
// WriteTxn until all read transactions close.
func (d *Doc) ReadTxn() *Transaction {
	d.mu.RLock()
	return &Transaction{doc: d}
}

// Close releases the read lock. Safe to call more than once; only the
// first call unlocks.
func (t *Transaction) Close() {
	if t.closed {
		return
	}
	t.closed = true
	t.doc.mu.RUnlock()
}

// Doc returns the Doc this transaction was created from.
func (t *Transaction) Doc() *Doc { return t.doc }

// Store returns the doc's BlockStore for read access. Mutations
// through this pointer would race with WriteTxn writers; do not
// mutate from within a read transaction.
func (t *Transaction) Store() *store.BlockStore { return t.doc.store }

// PendingState returns the opaque pending-update state stored on
// the doc, read-only. Concrete type is *encoding.Pending; see
// TransactionMut.PendingState for the rationale.
func (t *Transaction) PendingState() any { return t.doc.pendingState }

// TransactionMut is a write transaction holding the doc's write lock
// for its lifetime. Created by Doc.WriteTxn; released by Commit.
//
// Mirrors yrs TransactionMut<'doc>. Accumulates change-tracking state
// during the transaction; consumes it at Commit time to run the
// post-commit lifecycle (observers, squash, after-transaction handlers,
// GC, update events); see Commit.
type TransactionMut struct {
	doc    *Doc
	closed bool

	// Origin is an opaque caller-supplied value attached to this
	// transaction. Mirrors yrs Origin (transaction.rs:1210-1288).
	// Visible in observer events to distinguish e.g. local edits
	// from updates applied via ApplyUpdate.
	Origin any

	// deletedIDs records the head ID of every item tombstoned during this
	// transaction via Delete. Subdocument tracking reads it; GC, the
	// observers' deletes() predicate and the wire delete set go by
	// deletedRanges, because a later split in the same transaction cuts a
	// deleted item into pieces whose heads are not here.
	deletedIDs []block.ID

	// deletedRanges holds the exact clock range of every Delete, taken
	// when the item is tombstoned. Commit-time squash may later merge the
	// tombstone with a neighbouring one, after which the store item no
	// longer says which clocks this transaction deleted.
	deletedRanges []DeletedRange

	// changedTypes records, per branch whose user-observable state
	// changed this transaction, which map keys changed and whether
	// positional content changed. Drives observer dispatch at Commit.
	// Read by ChangedTypes / ChangedKeys / PositionalChanged.
	changedTypes map[*block.Branch]*changedSet

	// changedOrder holds the branches of changedTypes in the order they
	// were first changed. Observers fire in this order, as yjs fires them
	// in the insertion order of transaction.changed.
	changedOrder []*block.Branch

	// deletedByClient is deletedRanges per client, sorted and merged,
	// built lazily by DeletedThisTxn and gcDeleted and dropped by Delete.
	deletedByClient map[uint64][]clockRange

	// beforeState is the per-client clock head snapshot taken when
	// this transaction acquired the write lock. afterState is the
	// same snapshot taken immediately before observer dispatch in
	// Commit. UndoManager (and any future change-tracking observer)
	// derives the per-transaction insertion ranges as
	// afterState - beforeState. Read by BeforeState / AfterState
	// accessors.
	beforeState store.StateVector
	afterState  store.StateVector

	// subdocsAdded / subdocsRemoved / subdocsLoaded accumulate the
	// subdocument lifecycle changes produced this transaction, surfaced
	// to observers via the SubdocsAdded / SubdocsRemoved / SubdocsLoaded
	// accessors. Populated by trackSubdocs in Commit.
	subdocsAdded   []string
	subdocsRemoved []string
	subdocsLoaded  []string

	// There is no mergeBlocks list: squashNewBlocks walks every client
	// whose clock moved in this transaction, from its start state on,
	// and gcDeleted merges around the deleted items.
}

// WriteTxn acquires the doc's write lock and returns a TransactionMut.
// The caller MUST call Commit (typically via defer) to release the
// lock and run the commit lifecycle.
//
// WriteTxn blocks until all concurrent ReadTxn / WriteTxn close.
// Calling WriteTxn while already holding a write lock on this Doc
// from the same goroutine deadlocks (Go RWMutex is not re-entrant),
// matching yrs's transact_mut behaviour (transact.rs:255 explicit
// "this will hang forever" comment).
func (d *Doc) WriteTxn() *TransactionMut {
	d.mu.Lock()
	t := &TransactionMut{
		doc:         d,
		beforeState: d.store.GetStateVector(),
	}
	// Work queued while no transaction ran (QueueUnderLock), such as an
	// UndoManager releasing what a cleared step kept, runs before this
	// transaction can keep or delete anything.
	d.runQueued(t)
	return t
}

// Commit runs the post-commit lifecycle and releases the write lock.
// Safe to call more than once; subsequent calls are no-ops.
//
// Lifecycle, in order, all with the write lock held:
//  1. Snapshot the after-state.
//  2. Shared-type observers (Observe / ObserveDeep), on the layout as
//     the transaction left it.
//  3. Squash: merge this transaction's new blocks into their left
//     neighbours.
//  4. Record subdocument changes.
//  5. After-transaction handlers (the UndoManager, sync clients), which
//     may mark deleted items kept. Functions queued with
//     QueueUnderLock run before each handler and once more after the
//     last (and when a write transaction opens).
//  6. GC: free deleted content (unless GC is off or the item is kept)
//     and merge deleted runs.
//  7. Update handlers (OnUpdate, OnUpdateV2), on the final layout.
//  8. Subdocument handlers (OnSubdocs), as yjs emits 'subdocs' last.
//
// yjs runs its merge after GC rather than before the after-transaction
// handlers; docs/tech-debt.md ("Commit-time squash runs before
// after-transaction handlers") has the one case where that shows.
func (t *TransactionMut) Commit() {
	if t.closed {
		return
	}
	t.closed = true
	// Snapshot the post-mutation state vector before any handlers fire
	// so observers see a stable afterState. Together with beforeState
	// captured in WriteTxn, this is the data UndoManager needs to
	// compute per-transaction insertion ranges.
	t.afterState = t.doc.store.GetStateVector()
	// Dispatch shared-type observe / observeDeep events FIRST, before
	// any commit-time mutation rewrites the item layout. This mirrors
	// yjs, which computes type-event deltas during cleanup before
	// merging structs: squash would merge a new item into its
	// pre-existing left neighbour (e.g. appending "!" onto "hello"),
	// after which the merged item's clock predates beforeState and the
	// added text reads as a retain, losing the insert. Running here,
	// the items are still un-squashed and un-GC'd, so adds()/deletes()
	// and a delete's oldValue content are all correct.
	if TypeEventHook != nil {
		TypeEventHook(t)
	}
	// Commit-time block squash: merge same-client adjacent-clock items
	// created this transaction into their predecessors. Clocks are
	// preserved, so afterState (captured above) stays valid.
	t.squashNewBlocks()
	// Collect subdocument lifecycle changes (added / removed / loaded)
	// so observers see them, before handlers fire.
	t.trackSubdocs()
	// AfterTransaction handlers fire here, while the write lock is
	// still held. They observe a finalised TransactionMut state and
	// must not start a new ReadTxn / WriteTxn on the same doc. The
	// UndoManager runs here and marks tombstoned items it tracks with
	// keep, which the GC pass below must see, so observers run BEFORE
	// gcDeleted.
	t.doc.fireAfterTransactionHandlers(t)
	// Work queued since the last handler (QueueUnderLock), such as an
	// UndoManager releasing what a step cleared from a handler kept, so
	// this GC already sees it.
	t.doc.runQueued(t)
	// Garbage-collect deleted content (free payloads, merge deleted
	// runs), skipping items marked keep by an observer.
	t.gcDeleted()
	// Update events last, as yjs emits 'update' / 'updateV2' after GC
	// and the struct merge: the bytes describe the final layout.
	t.doc.fireUpdateHandlers(t)
	t.doc.fireSubdocsHandlers(t)
	t.doc.mu.Unlock()
}

// trackSubdocs records the subdocument lifecycle changes this
// transaction produced: ContentDoc items created this transaction are
// "added" (and "loaded" when their options carry autoLoad or the
// subdoc was explicitly loaded), and ContentDoc items tombstoned this
// transaction are "removed". Mirrors yjs's subdocsAdded / subdocsLoaded
// / subdocsRemoved sets. Each added subdoc is registered on the doc so
// the in-memory handle is reachable via Subdoc(guid).
func (t *TransactionMut) trackSubdocs() {
	bs := t.doc.store
	// Clients from the highest ID down, the order an update integrates
	// them in, so Added lists subdocuments in yjs's order rather than Go's
	// random map order.
	clients := make([]uint64, 0, len(t.afterState))
	for c := range t.afterState {
		clients = append(clients, c)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] > clients[j] })
	for _, client := range clients {
		after := t.afterState[client]
		clock := t.beforeState[client]
		for clock < after {
			// Walk cell by cell, not clock by clock: a Skip block can
			// cover a huge clock range in a single GC cell, and advancing
			// one clock at a time over it (GetItem returns nil there) is
			// O(range) — a malformed update with a multi-billion-clock
			// Skip would hang here. GetBlock locates the covering cell so
			// we jump past it in one step.
			cell, ok := bs.GetBlock(block.ID{Client: client, Clock: clock})
			if !ok {
				break // no cell covers this clock; the contiguous run ended
			}
			it := cell.AsItem()
			if it == nil {
				if !advanceClock(&clock, cell.ClockEnd()) { // skip a GC / Skip cell whole
					break
				}
				continue
			}
			if it.Content.Kind == block.KindDoc {
				guid := it.Content.DocGuid
				// A ContentDoc added and tombstoned in the SAME transaction
				// is a net no-op: yjs's ContentDoc.delete removes the doc
				// from subdocsAdded (and never reaches subdocsRemoved) when
				// it was added this txn. Surface it in neither Added nor
				// Loaded, and drop the handle SetDoc eagerly registered so
				// the registry does not retain a cancelled subdoc; the
				// removed-scan below skips it symmetrically.
				if it.IsDeleted() {
					t.doc.RemoveSubdoc(guid)
					if !advanceClock(&clock, it.ID.Clock+it.Len-1) {
						break
					}
					continue
				}
				t.subdocsAdded = append(t.subdocsAdded, guid)
				sub := t.doc.Subdoc(guid) // create + register the handle
				if optBool(it.Content.DocOpts, "autoLoad") {
					sub.Load()
				}
				if sub.ShouldLoad() {
					t.subdocsLoaded = append(t.subdocsLoaded, guid)
				}
			}
			if !advanceClock(&clock, it.ID.Clock+it.Len-1) {
				break
			}
		}
	}
	for _, id := range t.deletedIDs {
		it := bs.GetItem(id)
		if it == nil || it.Content.Kind != block.KindDoc {
			continue
		}
		// Mirror yjs ContentDoc.delete: only report Removed when the
		// subdoc was NOT added this same transaction. A subdoc created in
		// a prior transaction has clock < beforeState[client] and so is
		// correctly still reported; one created this txn falls in
		// [beforeState, afterState) and cancels out (already excluded from
		// Added above).
		before := t.beforeState[id.Client]
		after := t.afterState[id.Client]
		if id.Clock >= before && id.Clock < after {
			continue // added and removed this txn: net no-op
		}
		t.subdocsRemoved = append(t.subdocsRemoved, it.Content.DocGuid)
		// Drop the cached handle so Subdocs / Subdoc / GetDoc stop
		// returning a stale instance for a now-tombstoned reference.
		t.doc.RemoveSubdoc(it.Content.DocGuid)
	}
}

// optBool reads a boolean option from a ContentDoc options map.
func optBool(opts map[string]any, key string) bool {
	if opts == nil {
		return false
	}
	v, _ := opts[key].(bool)
	return v
}

// SubdocsAdded returns the GUIDs of subdocuments first surfaced during
// this transaction (local SetDoc or a decoded remote ContentDoc).
func (t *TransactionMut) SubdocsAdded() []string { return t.subdocsAdded }

// SubdocsRemoved returns the GUIDs of subdocuments whose reference was
// tombstoned during this transaction.
func (t *TransactionMut) SubdocsRemoved() []string { return t.subdocsRemoved }

// SubdocsLoaded returns the GUIDs of subdocuments marked to load during
// this transaction (autoLoad option or an explicit Load call).
func (t *TransactionMut) SubdocsLoaded() []string { return t.subdocsLoaded }

// gcDeleted frees the content of items tombstoned during this
// transaction and merges adjacent deleted runs, matching yjs's
// commit-time GC (tryGcDeleteSet + tryMergeDeleteSet). A deleted item's
// payload is replaced with a ContentDeleted marker of the same length,
// so the wire form becomes ContentDeleted (ref 1), byte-aligned with
// what yjs emits for a deleted item. Freeing is skipped when GC is
// disabled (snapshots / time-travel need the content) and for items
// marked keep (the UndoManager preserves them to support redo); the
// merge runs either way.
func (t *TransactionMut) gcDeleted() {
	bs := t.doc.store
	touched := map[uint64]uint64{} // client -> smallest deleted clock
	// Every cell inside the deleted ranges, as yjs tryGcDeleteSet walks
	// the structs of transaction.deleteSet: a deleted item that a later
	// integration split has pieces whose heads no Delete call saw.
	t.deletedRangesOf(0)
	clients := make([]uint64, 0, len(t.deletedByClient))
	for c := range t.deletedByClient {
		clients = append(clients, c)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] < clients[j] })
	for _, c := range clients {
		for _, r := range t.deletedByClient[c] {
			// Each cell is looked up afresh: collecting a type replaces
			// its children's cells with GC runs, possibly further along
			// this walk.
			for clock := r.start; clock < r.end; {
				cell, ok := bs.GetBlock(block.ID{Client: c, Clock: clock})
				if !ok {
					break
				}
				if it := cell.AsItem(); it != nil && it.IsDeleted() {
					t.gcDeletedItem(it, touched)
				}
				if !advanceClock(&clock, cell.ClockEnd()) {
					break
				}
			}
		}
	}
	// Merge adjacent deleted/GC'd cells per affected client, starting at
	// the first GC'd clock so the run collapses (and can also absorb a
	// deleted left neighbour). TrySquash already refuses to merge kept
	// items, so tracked tombstones stay distinct.
	for c, minClock := range touched {
		list := bs.GetClient(c)
		if list == nil {
			continue
		}
		startIdx, ok := list.FindPivot(minClock)
		if !ok {
			startIdx = 1
		}
		list.SquashFrom(startIdx)
	}
}

// gcDeletedItem records it for the merge pass and, with GC on and it not
// kept, frees its content (collapsing a shared type's subtree).
func (t *TransactionMut) gcDeletedItem(it *block.Item, touched map[uint64]uint64) {
	// The merge pass below runs whether or not the content is
	// collected: yjs tryMergeDeleteSet merges the deleted ranges
	// with GC off too, so a block cut by deletions rejoins.
	if cur, ok := touched[it.ID.Client]; !ok || it.ID.Clock < cur {
		touched[it.ID.Client] = it.ID.Clock
	}
	if !t.doc.gc || it.IsKeep() {
		return
	}
	// A deleted shared type collapses its whole subtree: every child
	// becomes a garbage-collected range (yjs ContentType.gc under
	// parentGCd), while the reference item itself becomes a
	// ContentDeleted marker below. Collect the children before the
	// content swap destroys the branch pointer.
	if it.Content.Kind == block.KindType && it.Content.Branch != nil {
		t.gcNestedChildren(it.Content.Branch, touched)
	}
	if it.Content.Kind != block.KindDeleted {
		it.Content = block.Content{Kind: block.KindDeleted, DeletedLen: it.Len}
		// ContentDeleted is not countable; clear the flag so the
		// item honours the invariant at block/item.go (a deleted
		// marker never contributes to parent length totals).
		it.SetCountable(false)
	}
}

// gcNestedChildren recursively replaces the children of a deleted shared
// type with GC cells, mirroring yjs ContentType.gc: every child collapses
// to a garbage-collected range (ref-0 GC struct on the wire), and nested
// types recurse depth-first. Children are reached two ways, matching the
// store's layout: positional items via Branch.Start (walk Right), and
// keyed items via Branch.Map's winner plus its overwritten left-chain
// (walk Left). touched records the smallest affected clock per client so
// the caller's merge pass collapses the adjacent GC cells into one run.
func (t *TransactionMut) gcNestedChildren(br *block.Branch, touched map[uint64]uint64) {
	bs := t.doc.store
	gcItem := func(it *block.Item) {
		// Depth-first: collapse this child's own subtree first, so a
		// nested-in-nested type is fully collected.
		if it.Content.Kind == block.KindType && it.Content.Branch != nil {
			t.gcNestedChildren(it.Content.Branch, touched)
		}
		list := bs.GetClient(it.ID.Client)
		if list == nil {
			return
		}
		idx, ok := list.FindPivot(it.ID.Clock)
		if !ok {
			return
		}
		list.ReplaceWithGC(idx)
		if cur, ok := touched[it.ID.Client]; !ok || it.ID.Clock < cur {
			touched[it.ID.Client] = it.ID.Clock
		}
	}
	for it := br.Start; it != nil; it = it.Right {
		gcItem(it)
	}
	for _, winner := range br.Map {
		for it := winner; it != nil; it = it.Left {
			gcItem(it)
		}
	}
	// The collected type keeps no reference to its old items, as yjs
	// ContentType.gc empties _start and _map: a wrapper someone still
	// holds reads an empty type, and the items can be freed.
	br.Start = nil
	br.Map = map[string]*block.Item{}
	br.MapOrder = nil
	br.BlockLen = 0
	br.ContentLen = 0
	br.Markers.Invalidate()
}

// squashNewBlocks collapses runs of same-client adjacent-clock items
// produced during this transaction into single items, the classic
// per-character Text.Insert pattern. Mirrors the merge-blocks step of
// yjs's commit lifecycle; removes the V1 per-item overhead that
// otherwise inflates document size by roughly 12x on fine-grained text
// workloads. Squash starts at the first new cell so it can also merge
// into the prior tail.
func (t *TransactionMut) squashNewBlocks() {
	for c, after := range t.afterState {
		before := t.beforeState[c]
		if after <= before {
			continue // no new clocks for this client
		}
		list := t.doc.store.GetClient(c)
		if list == nil {
			continue
		}
		startIdx, ok := list.FindPivot(before)
		if !ok {
			startIdx = 0
		}
		list.SquashFrom(startIdx)
	}
}

// DeleteRange tombstones the clock range [start, end) for client,
// splitting items at the range boundaries so a merged or partially
// covered item is sliced and only the matching part is tombstoned.
// Items already deleted are skipped. Clocks the store has not seen are
// skipped (the range may extend past this replica's state). Returns the
// number of items newly tombstoned.
//
// This is the split-aware delete shared by the wire delete-set path and
// the UndoManager. After commit-time squash a single item can span
// several clocks that belong to distinct logical edits; deleting by
// range with boundary splitting is what keeps undo and remote deletes
// correct in the presence of merged blocks.
func (t *TransactionMut) DeleteRange(client, start, end uint64) int {
	deleted := 0
	clock := start
	for clock < end {
		cell, ok := t.doc.store.GetBlock(block.ID{Client: client, Clock: clock})
		if !ok {
			return deleted // unseen tail
		}
		if it := cell.AsItem(); it == nil || it.IsDeleted() {
			// GC cell or already deleted: nothing to do, and cutting a
			// deleted item would leave its pieces apart for good.
			if !advanceClock(&clock, cell.ClockEnd()) {
				break
			}
			continue
		}
		item := t.MaterializeCleanStart(block.ID{Client: client, Clock: clock})
		if item == nil {
			if !advanceClock(&clock, cell.ClockEnd()) {
				break
			}
			continue
		}
		if item.ID.Clock+item.Len-1 >= end {
			_ = t.MaterializeCleanEnd(block.ID{Client: client, Clock: end - 1})
			item = t.GetItem(block.ID{Client: client, Clock: clock})
			if item == nil {
				return deleted
			}
		}
		if !item.IsDeleted() {
			t.Delete(item)
			deleted++
		}
		if !advanceClock(&clock, item.ID.Clock+item.Len-1) {
			break
		}
	}
	return deleted
}

// advanceClock moves *clock past lastInclusive (the inclusive upper
// clock of the cell just processed) and reports whether it advanced. It
// returns false WITHOUT moving when a step would not make progress —
// lastInclusive below the current clock (a zero-length item underflows
// ID.Clock+Len-1 below clock) or at the top of the uint64 space (a
// malformed Skip reaching MaxUint64, where +1 wraps to 0). Callers stop
// the scan rather than spinning or wrapping back to clock 0. Well-formed
// updates always advance, so this never fires for valid input.
func advanceClock(clock *uint64, lastInclusive uint64) bool {
	if lastInclusive < *clock || lastInclusive == ^uint64(0) {
		return false
	}
	*clock = lastInclusive + 1
	return true
}

// BeforeState returns the per-client clock-head snapshot taken when
// this transaction acquired the write lock. Read-only; do not mutate
// the returned map.
//
// Together with AfterState, the per-transaction insertion ranges are
// `afterState[client] - beforeState[client]` per client.
func (t *TransactionMut) BeforeState() store.StateVector { return t.beforeState }

// AfterState returns the per-client clock-head snapshot taken at the
// start of Commit. Populated only after Commit runs; before Commit the
// returned map is empty.
func (t *TransactionMut) AfterState() store.StateVector { return t.afterState }

// Doc returns the Doc this transaction was created from.
func (t *TransactionMut) Doc() *Doc { return t.doc }

// Store returns the doc's BlockStore. Safe to mutate from within
// this transaction; the write lock prevents concurrent access.
func (t *TransactionMut) Store() *store.BlockStore { return t.doc.store }

// IntegrateContext implementation. These methods make TransactionMut
// satisfy block.IntegrateContext so Item.Integrate can route store
// access and change-tracking through the active transaction.

// GetItem looks up the Item containing id in the doc's BlockStore.
// Returns nil for GC cells or unknown IDs.
func (t *TransactionMut) GetItem(id block.ID) *block.Item {
	return t.doc.store.GetItem(id)
}

// MaterializeCleanStart returns an Item starting exactly at id.Clock,
// splitting the underlying block in the store if id lands mid-block.
// Returns nil if id is in a GC cell or unknown.
func (t *TransactionMut) MaterializeCleanStart(id block.ID) *block.Item {
	slc, ok := t.doc.store.GetItemCleanStart(id)
	if !ok {
		return nil
	}
	return t.doc.store.Materialize(slc)
}

// MaterializeCleanEnd returns an Item ending exactly at id.Clock
// (inclusive), splitting if needed.
func (t *TransactionMut) MaterializeCleanEnd(id block.ID) *block.Item {
	slc, ok := t.doc.store.GetItemCleanEnd(id)
	if !ok {
		return nil
	}
	return t.doc.store.Materialize(slc)
}

// Delete tombstones an Item and records its ID for inclusion in the
// transaction's eventual delete-set emission. The Item must already
// be in the store.
//
// Deleting a shared type's item deletes the items inside it too
// (deleteTypeContent), as yjs ContentType.delete does.
func (t *TransactionMut) Delete(item *block.Item) {
	if item == nil || item.IsDeleted() {
		return
	}
	item.SetDeleted(true)
	t.deletedIDs = append(t.deletedIDs, item.ID)
	t.deletedRanges = append(t.deletedRanges, DeletedRange{ID: item.ID, Len: item.Len})
	t.deletedByClient = nil
	if item.Parent.IsResolved() {
		// Record the parent (and changed key) so observers fire on
		// deletions, mirroring yjs addChangedTypeToTransaction in
		// Item.delete.
		t.AddChangedType(item.Parent.Branch, item.ParentSub)
		// Tombstoning subtracts from the parent's countable totals,
		// mirroring yrs's branch.block_len -= len adjustment.
		if item.IsCountable() && item.ParentSub == nil {
			parent := item.Parent.Branch
			if item.Len <= parent.BlockLen {
				parent.BlockLen -= item.Len
			}
			if item.Len <= parent.ContentLen {
				parent.ContentLen -= item.Len
			}
		}
	}
	if item.Content.Kind == block.KindType && item.Content.Branch != nil {
		t.deleteTypeContent(item.Content.Branch)
	}
}

// deleteTypeContent deletes every live item of a shared type whose own
// item was just deleted, and forgets the changes recorded on the type in
// this transaction, as yjs ContentType.delete does: the items inside go
// into the transaction's delete set (and so into its update and an
// UndoManager's step), and a deleted type reports no event. Positional
// items go in document order, map entries in the order their keys were
// first set (yjs _map.forEach).
func (t *TransactionMut) deleteTypeContent(br *block.Branch) {
	for it := br.Start; it != nil; it = it.Right {
		if !it.IsDeleted() {
			t.Delete(it)
		}
	}
	for _, k := range br.MapKeys() {
		if it := br.Map[k]; it != nil && !it.IsDeleted() {
			t.Delete(it)
		}
	}
	br.Markers.Invalidate()
	if _, ok := t.changedTypes[br]; ok {
		delete(t.changedTypes, br)
		for i, b := range t.changedOrder {
			if b == br {
				t.changedOrder = append(t.changedOrder[:i:i], t.changedOrder[i+1:]...)
				break
			}
		}
	}
}

// AddChangedType records that a Branch saw user-observable changes
// during this transaction, tracking the changed map keys (parentSub
// != nil) and whether positional content changed (parentSub == nil)
// separately. Drives observer dispatch at Commit. Mirrors yjs
// transaction.changed (Map<AbstractType, Set<string|null>>).
func (t *TransactionMut) AddChangedType(parent *block.Branch, parentSub *string) {
	if parent == nil {
		return
	}
	// As yjs addChangedTypeToTransaction: a nested type created in this
	// transaction reports through its parent's event, and a deleted one
	// reports nothing, so neither is recorded. Root types always are.
	if it := parent.Item; it != nil && (it.ID.Clock >= t.beforeState[it.ID.Client] || it.IsDeleted()) {
		return
	}
	if t.changedTypes == nil {
		t.changedTypes = map[*block.Branch]*changedSet{}
	}
	cs := t.changedTypes[parent]
	if cs == nil {
		cs = &changedSet{keys: map[string]struct{}{}}
		t.changedTypes[parent] = cs
		t.changedOrder = append(t.changedOrder, parent)
	}
	if parentSub != nil {
		cs.keys[*parentSub] = struct{}{}
	} else {
		cs.positional = true
	}
}

// ChangedKeys returns the set of map keys changed on branch this
// transaction. Returns nil for a branch with no recorded key changes.
func (t *TransactionMut) ChangedKeys(b *block.Branch) map[string]struct{} {
	if cs := t.changedTypes[b]; cs != nil {
		return cs.keys
	}
	return nil
}

// PositionalChanged reports whether branch saw positional (Array /
// Text) content changes this transaction.
// InvalidatePositionalMarkers drops the search markers of every type whose
// positional content this transaction changed. The type API keeps its
// markers in step with its own edits; deletions that do not go through
// it (an update's delete set, an UndoManager step) leave them pointing at
// old indices, and yjs resets them there too (_callObserver for a remote
// transaction, popStackItem for undo).
func (t *TransactionMut) InvalidatePositionalMarkers() {
	for _, b := range t.changedOrder {
		if cs := t.changedTypes[b]; cs != nil && cs.positional {
			b.Markers.Invalidate()
		}
	}
}

func (t *TransactionMut) PositionalChanged(b *block.Branch) bool {
	if cs := t.changedTypes[b]; cs != nil {
		return cs.positional
	}
	return false
}

// DeletedThisTxn reports whether the clock at id was deleted during this
// transaction, the deletes(struct) predicate of observer events. Like
// yjs's isDeleted(transaction.deleteSet, id) it goes by the deleted clock
// ranges, not by item identity: an item deleted whole and then split by
// a later integration in the same transaction counts as deleted in every
// piece.
func (t *TransactionMut) DeletedThisTxn(id block.ID) bool {
	ranges := t.deletedRangesOf(id.Client)
	i := sort.Search(len(ranges), func(i int) bool { return ranges[i].end > id.Clock })
	return i < len(ranges) && ranges[i].start <= id.Clock
}

// clockRange is the half-open clock range [start, end) of one client.
type clockRange struct{ start, end uint64 }

// deletedRangesOf returns the clock ranges this transaction deleted for
// client, sorted and merged.
func (t *TransactionMut) deletedRangesOf(client uint64) []clockRange {
	if t.deletedByClient == nil {
		t.deletedByClient = map[uint64][]clockRange{}
		for _, r := range t.deletedRanges {
			t.deletedByClient[r.ID.Client] = append(t.deletedByClient[r.ID.Client], clockRange{r.ID.Clock, r.ID.Clock + r.Len})
		}
		for c, rs := range t.deletedByClient {
			sort.Slice(rs, func(i, j int) bool { return rs[i].start < rs[j].start })
			merged := rs[:1]
			for _, r := range rs[1:] {
				if last := &merged[len(merged)-1]; r.start <= last.end {
					if r.end > last.end {
						last.end = r.end
					}
					continue
				}
				merged = append(merged, r)
			}
			t.deletedByClient[c] = merged
		}
	}
	return t.deletedByClient[client]
}

// changedSet is the per-branch record of what changed this
// transaction: which map keys, and whether positional content moved.
type changedSet struct {
	keys       map[string]struct{}
	positional bool
}

// TypeEventHook, when set, is invoked during Commit (after observers'
// AfterTransaction handlers, before GC) to dispatch shared-type
// observe events. The types layer installs it in an init, bridging
// the doc -> types direction without an import cycle (types imports
// doc, not the reverse). nil in tests that never register an observer.
var TypeEventHook func(t *TransactionMut)

// GetOrCreateBranch returns the root branch with the given name from
// the doc's root-branch registry. Used by block.Repair to resolve
// ParentNamed references arriving from wire updates.
//
// We do not call Doc.Branch here because Doc.Branch acquires the
// write lock, which we already hold inside this transaction. Touch
// the registry directly under the existing lock instead.
func (t *TransactionMut) GetOrCreateBranch(name string) *block.Branch {
	if b, ok := t.doc.rootBranches[name]; ok {
		return b
	}
	b := &block.Branch{Name: name, Map: map[string]*block.Item{}}
	t.doc.rootBranches[name] = b
	return b
}

// DeletedRange is the clock range one Delete tombstoned: Len clocks
// starting at ID.
type DeletedRange struct {
	ID  block.ID
	Len uint64
}

// DeletedRanges returns the exact clock ranges tombstoned during this
// transaction so far, one per Delete, as they were when deleted.
// Returned slice aliases internal state; do not mutate.
func (t *TransactionMut) DeletedRanges() []DeletedRange { return t.deletedRanges }

// DeletedIDs returns the IDs of items tombstoned during this
// transaction so far. Returned slice aliases internal state; do not
// mutate. Primarily for tests and the future delete-set emitter.
func (t *TransactionMut) DeletedIDs() []block.ID { return t.deletedIDs }

// ChangedTypes returns the branches changed this transaction, in the
// order they were first changed.
func (t *TransactionMut) ChangedTypes() []*block.Branch {
	return append([]*block.Branch(nil), t.changedOrder...)
}

// PendingState returns the opaque pending-update state stored on
// the doc. Returns nil when no encoding-layer state has been
// installed yet. The concrete type is *encoding.Pending; the doc
// package does not depend on encoding so this stays any-typed at
// the boundary. Callers (encoding.ApplyUpdate) type-assert.
func (t *TransactionMut) PendingState() any { return t.doc.pendingState }

// SetPendingState replaces the opaque pending-update state on the
// doc. Pass nil to drop pending state entirely (e.g. when the
// queue drains to empty and the encoding layer wants to release
// the allocation).
func (t *TransactionMut) SetPendingState(s any) { t.doc.pendingState = s }

// Compile-time check that TransactionMut satisfies the
// block.IntegrateContext interface. If this line stops compiling,
// the integrate-context contract has shifted.
var _ block.IntegrateContext = (*TransactionMut)(nil)
