package doc

import (
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
// post-commit lifecycle (squash, GC, observers, update emission).
//
// Most lifecycle steps are not yet implemented — see tech-debt.md.
// Commit currently only releases the lock and marks the txn closed.
type TransactionMut struct {
	doc    *Doc
	closed bool

	// Origin is an opaque caller-supplied value attached to this
	// transaction. Mirrors yrs Origin (transaction.rs:1210-1288).
	// Visible in observer events to distinguish e.g. local edits
	// from updates applied via ApplyUpdate.
	Origin any

	// deletedIDs records items tombstoned during this transaction
	// via Delete. Used at Commit time (not yet) to build the wire
	// DeleteSet and drive squash of adjacent deleted runs. Read by
	// DeletedIDs accessor for tests and future observer dispatch.
	deletedIDs []block.ID

	// changedTypes records, per branch whose user-observable state
	// changed this transaction, which map keys changed and whether
	// positional content changed. Drives observer dispatch at Commit.
	// Read by ChangedTypes / ChangedKeys / PositionalChanged.
	changedTypes map[*block.Branch]*changedSet

	// deletedSet is the lazily-built set form of deletedIDs, used by
	// DeletedThisTxn for the observer event's deletes() predicate.
	deletedSet map[block.ID]struct{}

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

	// mergeBlocks would accumulate item IDs that should be
	// considered for try_squash at Commit. Will be added back when
	// Item.Integrate gains a MarkForMerge call site and Commit
	// gains a squash pass; both deferred (see tech-debt.md).
	// Field intentionally absent for now to keep the type lint-clean.
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
	return &TransactionMut{
		doc:         d,
		beforeState: d.store.GetStateVector(),
	}
}

// Commit runs the post-commit lifecycle and releases the write lock.
// Safe to call more than once; subsequent calls are no-ops.
//
// Lifecycle steps (mostly stubbed today; tech-debt.md tracks each):
//  1. Squash mergeBlocks against their left neighbours.
//  2. GC fully-observed deleted items if Doc.GC is true.
//  3. Fire pre-emit observers on changedTypes.
//  4. Emit the update event with V1 (or V2) bytes for the diff.
//  5. Emit subdoc events.
//  6. Fire after-commit observers.
//
// Today: only step 0 (release the lock) runs.
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
	// Garbage-collect deleted content (free payloads, merge deleted
	// runs), skipping items marked keep by an observer.
	t.gcDeleted()
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
	for client, after := range t.afterState {
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
// what yjs emits for a deleted item. Skipped when GC is disabled
// (snapshots / time-travel need the content) and for items marked keep
// (the UndoManager preserves them to support redo).
func (t *TransactionMut) gcDeleted() {
	if !t.doc.gc {
		return
	}
	bs := t.doc.store
	touched := map[uint64]uint64{} // client -> smallest GC'd clock
	for _, id := range t.deletedIDs {
		cell, ok := bs.GetBlock(id)
		if !ok {
			continue
		}
		it := cell.AsItem()
		if it == nil || !it.IsDeleted() || it.IsKeep() {
			continue
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
		if cur, ok := touched[it.ID.Client]; !ok || it.ID.Clock < cur {
			touched[it.ID.Client] = it.ID.Clock
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
		if cell.AsItem() == nil {
			if !advanceClock(&clock, cell.ClockEnd()) { // GC cell: already gone
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
// Note: the recursive-delete-of-Type-children path is not yet
// implemented (tracked in tech-debt.md). This implementation handles
// the simple case integrate uses for map-key LWW tombstoning.
func (t *TransactionMut) Delete(item *block.Item) {
	if item == nil || item.IsDeleted() {
		return
	}
	item.SetDeleted(true)
	t.deletedIDs = append(t.deletedIDs, item.ID)
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
	if t.changedTypes == nil {
		t.changedTypes = map[*block.Branch]*changedSet{}
	}
	cs := t.changedTypes[parent]
	if cs == nil {
		cs = &changedSet{keys: map[string]struct{}{}}
		t.changedTypes[parent] = cs
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
func (t *TransactionMut) PositionalChanged(b *block.Branch) bool {
	if cs := t.changedTypes[b]; cs != nil {
		return cs.positional
	}
	return false
}

// DeletedThisTxn reports whether the item at id was tombstoned during
// this transaction, the y-protocols "deletes(struct)" predicate the
// observer event logic needs. Built lazily from deletedIDs on first
// call. For map-key items (Len 1) the head-ID match is exact.
func (t *TransactionMut) DeletedThisTxn(id block.ID) bool {
	if t.deletedSet == nil {
		t.deletedSet = make(map[block.ID]struct{}, len(t.deletedIDs))
		for _, did := range t.deletedIDs {
			t.deletedSet[did] = struct{}{}
		}
	}
	_, ok := t.deletedSet[id]
	return ok
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

// DeletedIDs returns the IDs of items tombstoned during this
// transaction so far. Returned slice aliases internal state; do not
// mutate. Primarily for tests and the future delete-set emitter.
func (t *TransactionMut) DeletedIDs() []block.ID { return t.deletedIDs }

// ChangedTypes returns the branches with recorded changes in this
// transaction. Order is non-deterministic (map iteration). Primarily
// for tests and the future observer dispatcher.
func (t *TransactionMut) ChangedTypes() []*block.Branch {
	out := make([]*block.Branch, 0, len(t.changedTypes))
	for b := range t.changedTypes {
		out = append(out, b)
	}
	return out
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
