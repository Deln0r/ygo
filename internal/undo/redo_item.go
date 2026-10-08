package undo

import (
	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
)

// followRedoneAt maps the position id through the Redone chain to the
// live item holding the same content, and returns that item with the
// clock id now has in it. Each hop keeps the offset from the start of
// the item containing the position, as yjs followRedone does. Returns
// nil when a hop leads nowhere.
func followRedoneAt(txn *doc.TransactionMut, id block.ID) (*block.Item, uint64) {
	next := id
	for {
		it := txn.GetItem(next)
		if it == nil {
			return nil, 0
		}
		if it.Redone == nil {
			return it, next.Clock
		}
		next = block.ID{Client: it.Redone.Client, Clock: it.Redone.Clock + (next.Clock - it.ID.Clock)}
	}
}

// deletedByAStack reports whether an entry still on the undo or redo
// stack deleted id: undoing or redoing that entry would bring it back.
func (um *UndoManager) deletedByAStack(id block.ID) bool {
	um.mu.Lock()
	defer um.mu.Unlock()
	for _, stack := range [][]*StackItem{um.undoStack, um.redoStack} {
		for _, si := range stack {
			if si.Deletions.Contains(id.Client, id.Clock) {
				return true
			}
		}
	}
	return false
}

// redoItem resurrects a previously deleted item by inserting a fresh
// item with a copy of its content, linked back to the original via
// Item.Redone: yjs redoItem, for map entries (redoMapItem) and
// positional items (redoSequenceItem). A shared type comes back as a
// new empty copy, its content restored into it item by item, and an
// item whose parent type was deleted is restored into the parent's live
// copy. Returns the restored item, or nil when restoration is not
// possible (a parent that cannot be restored, a conflicting later write
// on the key, or Move and subdocument content).
//
// Caller holds the doc write lock via txn.
func (um *UndoManager) redoItem(txn *doc.TransactionMut, item *block.Item, si *StackItem) *block.Item {
	if item == nil {
		return nil
	}

	// Already redone: follow the chain to the current live item.
	if item.Redone != nil {
		return txn.MaterializeCleanStart(*item.Redone)
	}

	if item.Parent.Kind != block.ParentBranch || item.Parent.Branch == nil {
		return nil
	}
	parent := item.Parent.Branch
	if pi := parent.Item; pi != nil && pi.IsDeleted() {
		// The parent type was deleted too. As yjs redoItem: restore it
		// first when this step deleted it as well, otherwise give up
		// unless it was restored already; then restore into its live
		// copy, the end of its Redone chain, even if that copy has been
		// deleted again (the item is then restored deleted, and the step
		// still counts).
		if pi.Redone == nil {
			if !um.inStepToRedo(pi, si) || um.redoItem(txn, pi, si) == nil {
				return nil
			}
		}
		for pi.Redone != nil {
			next := txn.MaterializeCleanStart(*pi.Redone)
			if next == nil {
				return nil
			}
			pi = next
		}
		if pi.Content.Kind != block.KindType || pi.Content.Branch == nil {
			return nil
		}
		parent = pi.Content.Branch
	}

	// Move and subdocument content are not restored.
	if !item.Content.CopyableForUndo() {
		return nil
	}

	if item.ParentSub != nil {
		return um.redoMapItem(txn, item, parent, si)
	}
	return redoSequenceItem(txn, item, parent)
}

// redoMapItem resurrects a map-keyed item: the current tail under the
// key becomes the left neighbour (mirroring Map.Set), right is nil.
func (um *UndoManager) redoMapItem(txn *doc.TransactionMut, item *block.Item, parent *block.Branch, si *StackItem) *block.Item {
	// A later write on the key that this undo does not account for is a
	// change someone else made; restoring over it would erase it, so the
	// item is not restored, as in yjs redoItem. Writes this step inserted,
	// writes an undo or redo stack deleted, and writes already redone
	// elsewhere are stepped over.
	left := item
	for left != nil && left.Right != nil && (left.Right.Redone != nil ||
		si.Insertions.Contains(left.Right.ID.Client, left.Right.ID.Clock) ||
		um.deletedByAStack(left.Right.ID)) {
		left = left.Right
		for left != nil && left.Redone != nil {
			left = txn.MaterializeCleanStart(*left.Redone)
		}
	}
	if left != nil && left.Right != nil {
		return nil
	}
	if left == nil || left.Parent.Branch != parent {
		// A left from another parent would carry a misleading origin
		// (yjs #757); attach to the key's current winner instead.
		left = parent.Map[*item.ParentSub]
	}
	var origin *block.ID
	if left != nil {
		lid := left.LastID()
		origin = &lid
	}

	clientID := txn.Doc().ClientID()
	clock := txn.Store().GetClock(clientID)
	nextID := block.ID{Client: clientID, Clock: clock}

	keyCopy := *item.ParentSub
	redone := &block.Item{
		ID:        nextID,
		Len:       1,
		Origin:    origin,
		Left:      left,
		Content:   item.Content.Copy(),
		Parent:    block.Parent{Kind: block.ParentBranch, Branch: parent},
		ParentSub: &keyCopy,
		Flags:     block.FlagCountable,
	}
	return integrateRedone(txn, item, redone)
}

// integrateRedone links item to its restored copy and integrates the
// copy, as the end of yjs redoItem does: item.redone is set first, the
// copy is kept, and the copy is returned even when integration deletes
// it at once (its parent is deleted), so the step counts as a change.
func integrateRedone(txn *doc.TransactionMut, item, redone *block.Item) *block.Item {
	id := redone.ID
	item.Redone = &id
	keepItem(redone, true)
	txn.Store().PushBlock(redone)
	if dropped := redone.Integrate(txn, 0); dropped {
		txn.Delete(redone)
	}
	return redone
}

// inStepToRedo reports whether this step restores it: deleted by the
// step (si.Deletions), not inserted by it, and in scope. yjs's redoitems.
func (um *UndoManager) inStepToRedo(it *block.Item, si *StackItem) bool {
	return si.Deletions.Contains(it.ID.Client, it.ID.Clock) &&
		!si.Insertions.Contains(it.ID.Client, it.ID.Clock) &&
		um.itemInScope(it)
}

// redoSequenceItem resurrects a positional (Array / Text) item at its
// original slot. Following yjs redoItem's ParentSub == nil branch: the
// left neighbour is traced through any redone chains until it lands in
// the same parent, and the deleted item itself anchors the right side.
func redoSequenceItem(txn *doc.TransactionMut, item *block.Item, parent *block.Branch) *block.Item {
	// Trace the left neighbour to its current live representative in
	// the same parent. A neighbour that was itself undone/redone is
	// followed through its Redone chain.
	left := item.Left
	for left != nil {
		trace := left
		for trace != nil && !sameParentBranch(trace, parent) {
			if trace.Redone == nil {
				trace = nil
			} else {
				trace = txn.MaterializeCleanStart(*trace.Redone)
			}
		}
		if trace != nil && sameParentBranch(trace, parent) {
			left = trace
			break
		}
		left = left.Left
	}

	// The right anchor starts at the deleted item itself (yjs: right =
	// item). For a same-parent item the trace resolves immediately.
	right := item
	for right != nil {
		trace := right
		for trace != nil && !sameParentBranch(trace, parent) {
			if trace.Redone == nil {
				trace = nil
			} else {
				trace = txn.MaterializeCleanStart(*trace.Redone)
			}
		}
		if trace != nil && sameParentBranch(trace, parent) {
			right = trace
			break
		}
		right = right.Right
	}

	var origin, rightOrigin *block.ID
	if left != nil {
		lid := left.LastID()
		origin = &lid
	}
	if right != nil {
		rid := right.ID
		rightOrigin = &rid
	}

	clientID := txn.Doc().ClientID()
	clock := txn.Store().GetClock(clientID)
	nextID := block.ID{Client: clientID, Clock: clock}

	// A restored item counts toward its parent's length only when its
	// content does, as yjs derives it from the content. A format marker
	// restored as countable would add a phantom character to Text.Length
	// and shift every index after it.
	var flags uint16
	if item.Content.IsCountable() {
		flags = block.FlagCountable
	}
	redone := &block.Item{
		ID:          nextID,
		Len:         item.Len,
		Origin:      origin,
		Left:        left,
		RightOrigin: rightOrigin,
		Right:       right,
		Content:     item.Content.Copy(),
		Parent:      block.Parent{Kind: block.ParentBranch, Branch: parent},
		Flags:       flags,
	}
	return integrateRedone(txn, item, redone)
}

// sameParentBranch reports whether it sits directly under parent.
func sameParentBranch(it *block.Item, parent *block.Branch) bool {
	return it != nil && it.Parent.Kind == block.ParentBranch && it.Parent.Branch == parent
}
