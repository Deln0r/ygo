package block

// IntegrateContext is the subset of TransactionMut that Item.Integrate
// consumes. Defined here in the block package so Item.Integrate can be
// declared without an import cycle on the doc package.
//
// All methods are called under the TransactionMut write lock; no
// internal synchronization is needed.
type IntegrateContext interface {
	// GetItem returns the Item containing the given ID, or nil if the
	// store has no record of that ID or the cell at that clock is
	// a GC tombstone.
	GetItem(id ID) *Item

	// MaterializeCleanStart returns an Item that starts exactly at
	// id.Clock, splitting the underlying block in the store if id
	// lands mid-block. Returns nil if id is in a GC cell or unknown.
	//
	// Wraps yrs's `store.blocks.get_item_clean_start(id).map(|slc|
	// store.materialize(slc))`.
	MaterializeCleanStart(id ID) *Item

	// MaterializeCleanEnd returns an Item ending exactly at id.Clock
	// (inclusive), splitting if needed. Returns nil under the same
	// conditions as MaterializeCleanStart.
	//
	// Wraps yrs's `store.blocks.get_item_clean_end(id).map(|slc|
	// store.materialize(slc))`.
	MaterializeCleanEnd(id ID) *Item

	// Delete tombstones an Item: sets FlagDeleted, records the ID in
	// the transaction's delete set, and schedules observer events on
	// the owning branch.
	Delete(item *Item)

	// AddChangedType marks a Branch (with optional map-key
	// discriminator) as having user-observable changes; observers
	// fire on the recorded set at Commit time.
	AddChangedType(parent *Branch, parentSub *string)

	// GetOrCreateBranch returns the root branch with the given name,
	// creating it lazily if absent. Used by Repair to resolve
	// ParentNamed references arriving from wire updates.
	GetOrCreateBranch(name string) *Branch
}

// Integrate inserts this Item into its parent branch, running the
// YATA conflict-resolution algorithm to find the right position
// among any concurrent inserts. The Item must already be present in
// the block store (typically via store.PushBlock) before calling
// Integrate; Integrate only wires the doubly-linked list and updates
// parent fields.
//
// Preconditions:
//   - i.Origin / i.RightOrigin (immutable) are set if the item is
//     not anchored at the parent boundary.
//   - i.Left / i.Right have been pre-resolved by a Repair pass that
//     looked up the Origin / RightOrigin in the store. (Until our
//     Repair pass ships, callers must do this manually; see
//     tech-debt.md.)
//   - i.Parent.Kind is ParentBranch with a resolved *Branch. The
//     ParentNamed and ParentID forms are not yet handled — callers
//     must pre-resolve via the types layer's root-type registry.
//
// Return value mirrors yrs: true means "the item is now wired into
// the parent, but the caller must immediately Delete it." Reasons:
//   - parent is deleted, or
//   - this is a map-key insert that lost the LWW race
//     (parent_sub.is_some() && right.is_some()).
//
// false means "successfully integrated, do nothing further."
//
// See docs/yrs-port-notes/integrate.md for the line-by-line mapping
// to yrs/src/block.rs:562-846.
//
// Limitations vs yrs (tracked in tech-debt.md):
//   - offset > 0 (the Update partial-integrate path) is not handled.
//   - Named/ID parent variants return false without wiring (caller
//     bug — pre-resolve to ParentBranch).
//   - Move integration recursion is skipped.
//   - Subdoc registration (ContentDoc) is skipped.
//   - Weak-link (info.IsLinked) bookkeeping is skipped.
//   - Format-marker integration is a no-op (matches yrs).
func (i *Item) Integrate(ctx IntegrateContext, offset uint64) bool {
	if offset > 0 {
		// TODO: port the offset > 0 path (block.rs:569-583). Used
		// when Update.integrate is applying a partial item; not
		// reachable from direct inserts yet.
		return false
	}

	if i.Parent.Kind != ParentBranch {
		// Named/ID parent variants require root-type resolution we
		// don't yet have. Yrs returns true (drop) for Unknown; we
		// extend the same drop to unresolved Named/ID as a safety
		// pending the types layer. Tracked in tech-debt.md.
		return true
	}
	parent := i.Parent.Branch
	if parent == nil {
		return true
	}

	left := i.Left
	right := i.Right

	rightIsNullOrHasLeft := right == nil || right.Left != nil
	leftHasOtherRightThanSelf := left != nil && left.Right != i.Right

	if (left == nil && rightIsNullOrHasLeft) || leftHasOtherRightThanSelf {
		// Find the first conflicting item.
		var o *Item
		switch {
		case left != nil:
			o = left.Right
		case i.ParentSub != nil:
			// Walk down parent.Map[sub].Left to find conflict zone start.
			o = parent.Map[*i.ParentSub]
			for o != nil && o.Left != nil {
				o = o.Left
			}
		default:
			o = parent.Start
		}

		// YATA conflict-resolution loop. Pointer-identity equality is
		// sound here because the store guarantees a single *Item per
		// (Client, Clock) within a transaction. See
		// docs/yrs-port-notes/integrate.md §7.3.
		newLeft := i.Left
		conflictingItems := map[*Item]struct{}{}
		itemsBeforeOrigin := map[*Item]struct{}{}

		for o != nil {
			if o == i.Right {
				break
			}
			itemsBeforeOrigin[o] = struct{}{}
			conflictingItems[o] = struct{}{}
			if originEqual(i.Origin, o.Origin) {
				// Case 1: same origin. Tiebreaker by (Client, Clock):
				// lower client comes first, so if o's client is
				// strictly less than ours, o stays to our left.
				if o.ID.Client < i.ID.Client {
					newLeft = o
					conflictingItems = map[*Item]struct{}{}
				} else if originEqual(i.RightOrigin, o.RightOrigin) {
					// We share both origins; the lower ID wins
					// leftmost position. Since we are to the left of
					// o (by ID descent reached this branch), break.
					break
				}
			} else {
				// Case 2: different origin. If o's origin is already
				// in items_before_origin but not in conflicting_items,
				// move past o.
				var oOrigin *Item
				if o.Origin != nil {
					oOrigin = ctx.GetItem(*o.Origin)
				}
				if oOrigin != nil {
					if _, before := itemsBeforeOrigin[oOrigin]; before {
						if _, conflicting := conflictingItems[oOrigin]; !conflicting {
							newLeft = o
							conflictingItems = map[*Item]struct{}{}
						}
					} else {
						break
					}
				} else {
					break
				}
			}
			o = o.Right
		}
		i.Left = newLeft
	}

	// Inherit parent_sub from left, then right, if we don't have one.
	if i.ParentSub == nil {
		if i.Left != nil && i.Left.ParentSub != nil {
			i.ParentSub = i.Left.ParentSub
		} else if i.Right != nil && i.Right.ParentSub != nil {
			i.ParentSub = i.Right.ParentSub
		}
	}

	// Reconnect left and right.
	if i.Left != nil {
		// Item lands between left and left's old right.
		i.Right = i.Left.Right
		i.Left.Right = i
	} else {
		// No left: lands at the head of parent's positional list or
		// at the head of the map-key conflict chain.
		var r *Item
		if i.ParentSub != nil {
			// Walk down to the leftmost item under this key.
			r = parent.Map[*i.ParentSub]
			for r != nil && r.Left != nil {
				r = r.Left
			}
		} else {
			r = parent.Start
			parent.Start = i
		}
		i.Right = r
	}

	if i.Right != nil {
		i.Right.Left = i
	} else if i.ParentSub != nil {
		// We are the new rightmost item under this key.
		if parent.Map == nil {
			parent.Map = map[string]*Item{}
		}
		parent.Map[*i.ParentSub] = i
		if i.Left != nil {
			// The previous map-key winner becomes a tombstone.
			ctx.Delete(i.Left)
		}
	}

	// Adjust parent length counters.
	if i.ParentSub == nil && !i.IsDeleted() && i.IsCountable() {
		parent.BlockLen += i.Len
		// content_len(encoding) for non-string content equals Len.
		// String content matches Len in our ASCII-only mode; the
		// proper UTF-16 distinction arrives with the Text type.
		parent.ContentLen += i.Len
	}

	// Content-specific integrate actions.
	switch i.Content.Kind {
	case KindDeleted:
		// Content that arrives already deleted is tombstoned and its
		// range goes into the transaction's delete set, as yjs
		// ContentDeleted.integrate does (addToDeleteSet + markDeleted),
		// so the transaction's update and GC merge see it.
		ctx.Delete(i)
	case KindMove, KindDoc, KindFormat:
		// Defer per tech-debt.md (Move integration, subdoc
		// registration, format searchmarker).
	case KindType:
		// Nested-type init: wire the embedded Branch's back-
		// reference to this Item so subsequent extractValue calls
		// and Repair lookups can locate the owning Item from the
		// Branch alone. Per docs/yrs-port-notes/nested-types.md
		// gotcha 2.
		if i.Content.Branch != nil {
			i.Content.Branch.Item = i
		}
	}

	ctx.AddChangedType(parent, i.ParentSub)

	// Auto-delete if parent itself is gone, or if this is a map-key
	// insert that lost the LWW race.
	parentDeleted := parent.Item != nil && parent.Item.IsDeleted()
	if parentDeleted || (i.ParentSub != nil && i.Right != nil) {
		return true
	}

	// Positional inserts may have shifted user-facing indices in
	// ways the per-branch search-marker cache can't describe (this
	// path is taken by both local API mutations AND remote ApplyUpdate
	// integrations; the API side has already-correct ShiftAfter
	// hooks in Array.InsertRange / Text.Insert, but the apply side
	// has no idea which markers to shift). Invalidate the cache; the
	// API-side caller refills it with a single marker after Integrate
	// returns, so within-txn locality (the B4 workload) still wins.
	if i.ParentSub == nil {
		parent.Markers.Invalidate()
	}
	return false
}

// originEqual reports whether two *ID pointers name the same origin
// or are both nil. Used by Integrate's YATA loop.
func originEqual(a, b *ID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// TrySquash attempts to merge `other` into the receiver, producing a
// single combined Item that covers both ID ranges. Returns true on
// success; the caller must then drop `other` from its container.
//
// Squash refuses if any of the following fails:
//   - Same client.
//   - Adjacent clocks: i.LastClock + 1 == other.ID.Clock.
//   - Same deleted state.
//   - other.Origin == i.LastID (i.e. other was inserted with i as
//     its left neighbour at insertion time).
//   - other.RightOrigin == i.RightOrigin (immutable invariants line up).
//   - i.Right == other (current linked-list pointers agree).
//   - Neither is in a Move range or has been redone.
//   - Neither is weak-linked.
//   - Content.TrySquash succeeds for the kind-specific payload.
//
// Mirrors yrs/src/block.rs:848-876 ItemPtr::try_squash.
func (i *Item) TrySquash(other *Item) bool {
	if i.ID.Client != other.ID.Client {
		return false
	}
	if i.ID.Clock+i.Len != other.ID.Clock {
		return false
	}
	if i.IsDeleted() != other.IsDeleted() {
		return false
	}
	// other.Origin must equal i.LastID()
	if other.Origin == nil || !other.Origin.Equal(i.LastID()) {
		return false
	}
	if !originEqual(i.RightOrigin, other.RightOrigin) {
		return false
	}
	if i.Right != other {
		return false
	}
	if i.Moved != nil || other.Moved != nil {
		return false
	}
	if i.Redone != nil || other.Redone != nil {
		return false
	}
	// Items the UndoManager marked to keep (resurrected via redoItem)
	// must not merge: undo/redo tracks them by exact identity, and a
	// merge would let one Undo delete a neighbour. Live, never-undone
	// items are not kept and squash freely.
	if i.IsKeep() || other.IsKeep() {
		return false
	}
	if i.IsLinked() || other.IsLinked() {
		return false
	}
	if !i.Content.TrySquash(&other.Content) {
		return false
	}
	// Merge: extend i to cover other's clocks; rewire neighbours.
	i.Len += other.Len
	i.Right = other.Right
	if other.Right != nil {
		other.Right.Left = i
	}
	return true
}
