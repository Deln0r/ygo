package block

// Repair pre-resolves an Item's transient pointers and parent
// reference so it is ready for Integrate. Called once per item in the
// update-apply path before push_block / Integrate.
//
// Specifically:
//
//   - Origin (immutable ID) → Left (live *Item), via
//     ctx.MaterializeCleanEnd. The store splits the underlying block
//     if Origin lands mid-block.
//   - RightOrigin (immutable ID) → Right (live *Item), via
//     ctx.MaterializeCleanStart.
//   - Parent (tagged union) → resolved ParentBranch:
//   - ParentBranch already resolved → no-op.
//   - ParentNamed("name") → ctx.GetOrCreateBranch(name).
//   - ParentUnknown → inherit from a resolved neighbour
//     (left first, then right) per yrs Item::repair lines 1394-1404.
//   - ParentID (nested type by item ID) → not yet implemented;
//     returns ErrParentIDUnresolved.
//
// Mirrors yrs/src/block.rs:1368-1431 Item::repair, omitting the
// rich-text / weak-link extras since we do not yet carry those
// content variants.
//
// Concurrency: caller must hold the doc's write lock (ctx is a
// TransactionMut or equivalent).
func Repair(it *Item, ctx IntegrateContext) error {
	if it.Origin != nil {
		it.Left = ctx.MaterializeCleanEnd(*it.Origin)
	}
	if it.RightOrigin != nil {
		it.Right = ctx.MaterializeCleanStart(*it.RightOrigin)
	}
	switch it.Parent.Kind {
	case ParentBranch:
		if it.Parent.Branch != nil {
			return nil
		}
		// Branch pointer null but kind is Branch — degenerate; try
		// to recover from neighbours below.
		fallthrough
	case ParentUnknown:
		switch {
		case it.Left != nil && it.Left.Parent.IsResolved():
			it.Parent = it.Left.Parent
			if it.ParentSub == nil {
				it.ParentSub = it.Left.ParentSub
			}
		case it.Right != nil && it.Right.Parent.IsResolved():
			it.Parent = it.Right.Parent
			if it.ParentSub == nil {
				it.ParentSub = it.Right.ParentSub
			}
		}
	case ParentNamed:
		b := ctx.GetOrCreateBranch(it.Parent.Named)
		if b == nil {
			return ErrParentUnresolved
		}
		it.Parent = Parent{Kind: ParentBranch, Branch: b}
	case ParentID:
		// Nested-type parent: look up the parent Item and pull its
		// embedded Branch out of the KindType Content. The parent
		// Item must already be in the store; missing parents land
		// in encoding.Pending which retries Repair on every
		// integration pass (per docs/yrs-port-notes/nested-types.md §4 and
		// gotcha 8). itemMissingDep over in encoding/pending.go
		// already covers ParentID misses.
		//
		// Mirrors yrs Item::repair `TypePtr::ID` arm
		// (block.rs:1399-1413).
		parent := ctx.GetItem(it.Parent.ID)
		if parent == nil {
			return ErrParentIDUnresolved
		}
		if parent.Content.Kind != KindType || parent.Content.Branch == nil {
			return ErrParentIDNotType
		}
		it.Parent = Parent{Kind: ParentBranch, Branch: parent.Content.Branch}
	}
	return nil
}

// Errors returned by Repair.
var (
	ErrParentUnresolved   repairError = "block: parent could not be resolved"
	ErrParentIDUnresolved repairError = "block: parent-by-ID points to an Item not yet in the store (queue in pending)"
	ErrParentIDNotType    repairError = "block: parent-by-ID points to an Item whose Content is not KindType"
)

type repairError string

func (e repairError) Error() string { return string(e) }
