package undo

import (
	"sync"
	"time"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
)

// DefaultCaptureTimeout groups subsequent edits into the same StackItem
// when they land within this window of the previous capture. Matches
// the upstream yjs default of 500 milliseconds.
const DefaultCaptureTimeout = 500 * time.Millisecond

// Options configures an UndoManager. All fields are optional; a zero
// value is equivalent to passing no Options at all.
type Options struct {
	// CaptureTimeout groups bursty edits into a single StackItem.
	// Zero means "use DefaultCaptureTimeout"; a negative value
	// disables grouping (every transaction becomes its own
	// StackItem).
	CaptureTimeout time.Duration

	// TrackedOrigins is the set of TransactionMut.Origin values
	// that qualify as "this UndoManager should record this edit".
	// nil means default-track-local: a single entry of the untyped
	// nil origin (matching the default a local edit produces).
	TrackedOrigins map[any]struct{}

	// IgnoreRemoteMapChanges, when true, suppresses StackItem
	// capture for transactions whose only effect on the scope is
	// a Map operation overwriting a key from a non-tracked origin.
	// Off by default; the implementation hook lands with full
	// nested-type support in a follow-up.
	IgnoreRemoteMapChanges bool
}

// UndoManager records local mutations under a scope of root branches
// and replays them on Undo / Redo. See package doc for semantics.
//
// An UndoManager is safe for concurrent reads of CanUndo / CanRedo;
// Undo, Redo, StopCapturing, and Clear must be serialised by the
// caller (mirroring the Doc write-lock contract).
type UndoManager struct {
	doc *doc.Doc

	// scope is the set of root branches whose mutations qualify.
	// Nested-type ancestry walks up Branch.Item.Parent to find a
	// matching scope entry.
	scope []*block.Branch

	captureTimeout         time.Duration
	trackedOrigins         map[any]struct{}
	ignoreRemoteMapChanges bool //nolint:unused // wired in nested-type follow-up

	mu        sync.Mutex
	undoStack []*StackItem
	redoStack []*StackItem

	// undoing / redoing flip true while Undo or Redo runs an
	// internally-issued transaction; the AfterTransaction handler
	// uses these flags to route the new StackItem to the opposite
	// stack instead of the usual undoStack.
	undoing bool
	redoing bool

	// lastChange is the wall-clock time of the previous captured
	// transaction. Used together with captureTimeout to decide
	// whether the next transaction extends the top StackItem or
	// opens a new one. Zero means "no prior change".
	lastChange time.Time

	// nowFn returns the current wall-clock time. Indirected so tests
	// can drive grouping deterministically without sleeping.
	nowFn func() time.Time

	unsubscribe func()
	closed      bool
}

// NewUndoManager registers an UndoManager on doc, watching the given
// scope of root branches. Returns the manager; call Close when done
// to unregister the handler.
//
// Panics if scope is empty (a no-scope UndoManager would silently
// drop every transaction; almost always a programming error).
func NewUndoManager(d *doc.Doc, scope []*block.Branch, opts ...Options) *UndoManager {
	if len(scope) == 0 {
		panic("undo: NewUndoManager requires at least one scope branch")
	}
	var opt Options
	if len(opts) > 0 {
		opt = opts[0]
	}

	captureTimeout := opt.CaptureTimeout
	if captureTimeout == 0 {
		captureTimeout = DefaultCaptureTimeout
	}

	trackedOrigins := opt.TrackedOrigins
	if trackedOrigins == nil {
		trackedOrigins = map[any]struct{}{nil: {}}
	}

	um := &UndoManager{
		doc:                    d,
		scope:                  append([]*block.Branch(nil), scope...),
		captureTimeout:         captureTimeout,
		trackedOrigins:         trackedOrigins,
		ignoreRemoteMapChanges: opt.IgnoreRemoteMapChanges,
		nowFn:                  time.Now,
	}
	um.unsubscribe = d.OnAfterTransaction(um.onAfterTransaction)
	return um
}

// onAfterTransaction is the Doc-level hook. Called under the Doc
// write lock; mutations to UndoManager state happen here. We take
// the local mu inside this callback so external readers of
// CanUndo / CanRedo see consistent state even between transactions.
func (um *UndoManager) onAfterTransaction(mut *doc.TransactionMut) {
	// Closed flag is written by Close from arbitrary goroutines (a
	// mobile UI thread closing while a background sync client commits
	// remote transactions), so the read must hold the lock.
	um.mu.Lock()
	closed := um.closed
	um.mu.Unlock()
	if closed {
		return
	}

	// Origin filter: skip transactions whose Origin is not in
	// trackedOrigins. The receiver itself is implicitly tracked so
	// Undo / Redo's own transactions roundtrip back to the opposite
	// stack.
	if !um.isTrackedOrigin(mut.Origin) {
		return
	}

	// Build the StackItem for this transaction, filtering every
	// touched item by scope. We resolve items through the store
	// rather than trusting changedTypes, which the types layer does
	// not yet populate (see internal/doc AddChangedType note).
	store := mut.Store()
	si := newStackItem()

	// Insertions: per-client newly-created clocks (afterState minus
	// beforeState), keeping only items whose parent is in scope.
	before := mut.BeforeState()
	after := mut.AfterState()
	for client, end := range after {
		start := before[client]
		clock := start
		for clock < end {
			// Walk cell by cell (not clock by clock): a Skip block covers
			// a huge range in one GC cell, and a malformed update can drive
			// afterState arbitrarily high, so a per-clock scan would hang.
			cell, ok := store.GetBlock(block.ID{Client: client, Clock: clock})
			if !ok {
				break
			}
			it := cell.AsItem()
			if it == nil {
				if !advanceClock(&clock, cell.ClockEnd()) {
					break
				}
				continue
			}
			if um.itemInScope(it) {
				// Record only the portion of this item inside the
				// new-clocks window [start, end). Commit-time squash
				// may have merged the new item with older neighbours,
				// so the item's own range can extend past the window;
				// recording its full range would make Undo delete
				// content from earlier transactions.
				recStart := it.ID.Clock
				if recStart < start {
					recStart = start
				}
				recEnd := it.ID.Clock + it.Len
				if recEnd > end {
					recEnd = end
				}
				si.Insertions.Insert(client, recStart, recEnd-recStart)
			}
			if !advanceClock(&clock, it.ID.Clock+it.Len-1) {
				break
			}
		}
	}

	// Deletions: every item tombstoned this transaction whose parent
	// is in scope. DeletedIDs reports the head ID; we read the full
	// Len off the resolved item.
	for _, id := range mut.DeletedIDs() {
		it := store.GetItem(id)
		if it == nil {
			continue
		}
		if um.itemInScope(it) {
			si.Deletions.Insert(it.ID.Client, it.ID.Clock, it.Len)
			// Mark the tombstoned item to keep so commit-time GC does
			// not free the content this manager needs to resurrect it
			// on undo (redoItem copies the original content). Runs
			// before gcDeleted, which skips kept items.
			it.SetKeep(true)
		}
	}

	// Nothing in scope changed: this transaction is invisible to us.
	if si.Insertions.ClientCount() == 0 && si.Deletions.ClientCount() == 0 {
		return
	}

	um.mu.Lock()
	defer um.mu.Unlock()
	if um.closed {
		return // closed concurrently while this capture was being built
	}

	now := um.nowFn()
	undoing := um.undoing
	redoing := um.redoing
	target := &um.undoStack
	if undoing {
		target = &um.redoStack
	}

	if !undoing && !redoing {
		// Fresh local edit: throw away any pending redo history.
		um.redoStack = um.redoStack[:0]
	}

	if !undoing && !redoing &&
		len(*target) > 0 &&
		!um.lastChange.IsZero() &&
		um.captureTimeout >= 0 &&
		now.Sub(um.lastChange) < um.captureTimeout {
		// Group with the previous StackItem.
		(*target)[len(*target)-1].merge(si)
	} else {
		*target = append(*target, si)
	}

	if !undoing && !redoing {
		um.lastChange = now
	}
}

// CanUndo reports whether there is anything on the undo stack.
func (um *UndoManager) CanUndo() bool {
	um.mu.Lock()
	defer um.mu.Unlock()
	return len(um.undoStack) > 0
}

// CanRedo reports whether there is anything on the redo stack.
func (um *UndoManager) CanRedo() bool {
	um.mu.Lock()
	defer um.mu.Unlock()
	return len(um.redoStack) > 0
}

// StopCapturing prevents the next change from grouping with the
// current top StackItem. Useful before a logical "boundary" event
// such as a save, a programmatic batch, or a user-visible step end.
func (um *UndoManager) StopCapturing() {
	um.mu.Lock()
	defer um.mu.Unlock()
	um.lastChange = time.Time{}
}

// Clear empties both stacks. After Clear, CanUndo and CanRedo both
// return false. Existing keep flags on items previously held against
// GC are released by the GC pass when the items become reachable
// again (not yet wired in this skeleton; tracked in tech-debt).
func (um *UndoManager) Clear() {
	um.mu.Lock()
	defer um.mu.Unlock()
	um.undoStack = um.undoStack[:0]
	um.redoStack = um.redoStack[:0]
	um.lastChange = time.Time{}
}

// Close unregisters this UndoManager from the doc and releases its
// stacks. After Close, all methods become no-ops (in particular Undo
// and Redo return false even if they would otherwise have anything
// to do).
func (um *UndoManager) Close() {
	um.mu.Lock()
	defer um.mu.Unlock()
	if um.closed {
		return
	}
	um.closed = true
	um.undoStack = nil
	um.redoStack = nil
	if um.unsubscribe != nil {
		um.unsubscribe()
		um.unsubscribe = nil
	}
}

// Undo pops the top of the undo stack and replays it against the doc:
// items inserted during the captured window are deleted, items deleted
// during the window are resurrected via redoItem. Returns true if a
// StackItem was applied, false if the stack was empty or the manager
// is closed.
//
// The replay runs in its own WriteTxn with Origin set to the manager,
// so the resulting AfterTransaction is routed to the redo stack rather
// than appended to the undo stack.
//
// Undo must not be called from inside an active transaction on the
// same doc, and concurrent Undo / Redo calls must be serialised by the
// caller.
func (um *UndoManager) Undo() bool {
	um.mu.Lock()
	if um.closed || len(um.undoStack) == 0 {
		um.mu.Unlock()
		return false
	}
	si := um.undoStack[len(um.undoStack)-1]
	um.undoStack = um.undoStack[:len(um.undoStack)-1]
	um.undoing = true
	um.mu.Unlock()

	um.applyStackItem(si)

	um.mu.Lock()
	um.undoing = false
	um.mu.Unlock()
	return true
}

// Redo is the mirror of Undo, replaying the top of the redo stack.
// Returns true if a StackItem was applied.
func (um *UndoManager) Redo() bool {
	um.mu.Lock()
	if um.closed || len(um.redoStack) == 0 {
		um.mu.Unlock()
		return false
	}
	si := um.redoStack[len(um.redoStack)-1]
	um.redoStack = um.redoStack[:len(um.redoStack)-1]
	um.redoing = true
	um.mu.Unlock()

	um.applyStackItem(si)

	um.mu.Lock()
	um.redoing = false
	um.mu.Unlock()
	return true
}

// applyStackItem runs the deletion-of-insertions and resurrection-of-
// deletions for one StackItem inside a fresh WriteTxn. The umorigin
// marker on the transaction makes the resulting AfterTransaction route
// to the opposite stack.
func (um *UndoManager) applyStackItem(si *StackItem) {
	txn := um.doc.WriteTxn()
	txn.Origin = um
	defer txn.Commit()

	// Delete everything that was inserted during the captured window.
	// Two cases per item:
	//   - resurrected by an earlier Undo/Redo (Redone chain set): the
	//     live representative is a kept, never-squashed item; follow the
	//     chain and delete it whole.
	//   - live, never-undone: commit-time squash may have merged it with
	//     neighbours, so delete only this range's slice via the
	//     split-aware DeleteRange (deleting the whole merged item would
	//     remove adjacent edits that belong to other undo steps).
	bs := txn.Store()
	si.Insertions.Iterate(func(client uint64, ranges []encoding.Range) {
		for _, r := range ranges {
			clock := r.Start
			for clock < r.End() {
				cell, ok := bs.GetBlock(block.ID{Client: client, Clock: clock})
				if !ok {
					break
				}
				it := cell.AsItem()
				if it == nil {
					if !advanceClock(&clock, cell.ClockEnd()) {
						break
					}
					continue
				}
				if it.Redone != nil {
					live := followRedone(txn, it)
					if live != nil && !live.IsDeleted() {
						txn.Delete(live)
					}
					if !advanceClock(&clock, it.ID.Clock+it.Len-1) {
						break
					}
					continue
				}
				end := r.End()
				if itEnd := it.ID.Clock + it.Len; itEnd < end {
					end = itEnd
				}
				txn.DeleteRange(client, clock, end)
				if !advanceClock(&clock, end-1) {
					break
				}
			}
		}
	})

	// Resurrect everything that was deleted during the captured window.
	si.Deletions.Iterate(func(client uint64, ranges []encoding.Range) {
		for _, r := range ranges {
			clock := r.Start
			for clock < r.End() {
				cell, ok := bs.GetBlock(block.ID{Client: client, Clock: clock})
				if !ok {
					break
				}
				it := cell.AsItem()
				if it == nil {
					if !advanceClock(&clock, cell.ClockEnd()) {
						break
					}
					continue
				}
				redoItem(txn, it)
				if !advanceClock(&clock, it.ID.Clock+it.Len-1) {
					break
				}
			}
		}
	})
}

// advanceClock moves *clock past lastInclusive (the inclusive upper
// clock of the cell just processed) and reports whether it advanced. It
// returns false WITHOUT moving when a step would not make progress: a
// zero-length item (ID.Clock+Len-1 underflows below clock) or a
// malformed Skip whose range reaches MaxUint64 (where +1 wraps to 0 and
// would restart the scan from clock 0). Callers break rather than spin.
// Well-formed updates always advance, so this never fires for valid
// input.
func advanceClock(clock *uint64, lastInclusive uint64) bool {
	if lastInclusive < *clock || lastInclusive == ^uint64(0) {
		return false
	}
	*clock = lastInclusive + 1
	return true
}

// isTrackedOrigin reports whether origin is in trackedOrigins. The
// UndoManager itself is always tracked so its own Undo / Redo
// transactions cycle back into the opposite stack.
func (um *UndoManager) isTrackedOrigin(origin any) bool {
	if origin == um {
		return true
	}
	_, ok := um.trackedOrigins[origin]
	return ok
}

// itemInScope reports whether an item's parent branch lies under the
// configured scope. Items with an unresolved or non-branch parent are
// out of scope.
func (um *UndoManager) itemInScope(it *block.Item) bool {
	if it.Parent.Kind != block.ParentBranch || it.Parent.Branch == nil {
		return false
	}
	return um.isInScope(it.Parent.Branch)
}

// isInScope walks up the parent chain of b. The chain terminates at
// a root branch (Branch.Item == nil) or at a non-branch parent.
func (um *UndoManager) isInScope(b *block.Branch) bool {
	for cur := b; cur != nil; {
		for _, s := range um.scope {
			if cur == s {
				return true
			}
		}
		if cur.Item == nil {
			return false
		}
		if cur.Item.Parent.Kind != block.ParentBranch || cur.Item.Parent.Branch == nil {
			return false
		}
		cur = cur.Item.Parent.Branch
	}
	return false
}
