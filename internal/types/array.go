package types

import (
	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
)

// Array is the user-facing wrapper around a positional Branch. The
// branch's Start linked list holds the document-order sequence; we
// count live (non-deleted, countable) items to translate user-facing
// positions to *Item references.
//
// Construct via NewArray; usually obtained as
// types.NewArray(d.Branch("name")).
//
// Array shares its underlying Branch with Map's possible usage of
// the same name — Map uses Branch.Map (string-keyed), Array uses
// Branch.Start (positional). The two surfaces operate on disjoint
// fields of the same Branch and may technically coexist, though in
// practice each root branch is used as one type.
type Array struct {
	branch *block.Branch
}

// NewArray wraps the given branch as an Array.
func NewArray(branch *block.Branch) *Array {
	return &Array{branch: branch}
}

// Branch returns the underlying *block.Branch for low-level access.
func (a *Array) Branch() *block.Branch { return a.branch }

// Len returns the number of live elements. O(1) — reads
// branch.BlockLen, which Item.Integrate (+countable.Len) and
// TransactionMut.Delete (-countable.Len) maintain.
//
// Per types-array.md finding 5: Map keys are excluded from
// BlockLen; the counter only tracks positional, countable, live
// items.
func (a *Array) Len() uint64 { return a.branch.BlockLen }

// Insert inserts a single value at idx. Convenience for
// InsertRange with a one-element slice.
//
// idx must be in [0, Len]; out-of-range inserts append at the end
// (matching JS Y.Array.insert tolerant behaviour).
func (a *Array) Insert(txn *doc.TransactionMut, idx uint64, value any) {
	a.InsertRange(txn, idx, []any{value})
}

// Push appends one or more values to the end of the array.
// Equivalent to InsertRange(txn, Len(), values).
func (a *Array) Push(txn *doc.TransactionMut, values ...any) {
	a.InsertRange(txn, a.Len(), values)
}

// InsertRange inserts every value at idx in order, packed into a
// single ContentAny Item with Len == len(values). Per types-array.md
// finding 3, this matches yrs's RangePrelim squash-on-insert.
//
// Mid-block inserts trigger Store.SplitBlock to materialize a clean
// boundary at idx.
func (a *Array) InsertRange(txn *doc.TransactionMut, idx uint64, values []any) {
	if len(values) == 0 {
		return
	}

	left, right := findInsertPosition(a.branch, txn, idx)

	var origin, rightOrigin *block.ID
	if left != nil {
		lid := left.LastID()
		origin = &lid
	}
	if right != nil {
		// Per types-array.md finding 4: array Items carry
		// right_origin = right.ID (Map sets it nil).
		rid := right.ID
		rightOrigin = &rid
	}

	clientID := txn.Doc().ClientID()
	clock := txn.Store().GetClock(clientID)

	anys := make([]block.Any, len(values))
	copy(anys, values)

	item := &block.Item{
		ID:          block.ID{Client: clientID, Clock: clock},
		Len:         uint64(len(values)),
		Origin:      origin,
		Left:        left,
		RightOrigin: rightOrigin,
		Right:       right,
		Content:     block.Content{Kind: block.KindAny, Anys: anys},
		Parent:      block.Parent{Kind: block.ParentBranch, Branch: a.branch},
		// ParentSub: nil — positional, not map-key.
		Flags: block.FlagCountable,
	}

	txn.Store().PushBlock(item)
	if dropped := item.Integrate(txn, 0); dropped {
		txn.Delete(item)
		return
	}
	// Shift markers past the insertion point so they keep pointing
	// at the right user-facing index.
	a.branch.Markers.ShiftAfter(idx, item.Len)
	// Remember the just-inserted item so the next call near idx
	// short-circuits the walk.
	rememberMarker(a.branch, item, idx)
}

// Get returns the value at idx, or nil if idx is out of range or
// the underlying content kind is not unpacked by extractValueAt.
//
// O(N) over the linked list. Search markers help only when callers
// alternate Get with Insert/Delete near the same area; pure Get
// loops still walk from start (no marker write occurs here).
func (a *Array) Get(idx uint64) any {
	var counted uint64 = 0
	for cur := a.branch.Start; cur != nil; cur = cur.Right {
		if cur.IsDeleted() || !cur.IsCountable() {
			continue
		}
		if counted+cur.Len > idx {
			offset := idx - counted
			return extractValueAt(cur.Content, offset)
		}
		counted += cur.Len
	}
	return nil
}

// Delete removes length elements starting at idx. Items that
// straddle the [idx, idx+length) range are split via SplitBlock so
// the deletion lands on Item boundaries.
//
// length == 0 is a no-op. Out-of-range deletions clip to Len().
func (a *Array) Delete(txn *doc.TransactionMut, idx, length uint64) {
	if length == 0 {
		return
	}
	end := idx + length
	if end > a.Len() {
		end = a.Len()
	}
	if idx >= end {
		return
	}
	defer a.branch.Markers.ShrinkAfter(idx, end-idx)

	var counted uint64 = 0
	cur := a.branch.Start
	for cur != nil && counted < end {
		if cur.IsDeleted() || !cur.IsCountable() {
			cur = cur.Right
			continue
		}

		blockStart := counted
		blockEnd := counted + cur.Len

		if blockEnd <= idx {
			counted = blockEnd
			cur = cur.Right
			continue
		}
		if blockStart >= end {
			break
		}

		// Block overlaps the delete range. Split at start boundary
		// if needed.
		if blockStart < idx {
			right := txn.Store().SplitBlock(cur, idx-blockStart)
			if right == nil {
				// Should not happen for valid offset; guard anyway.
				cur = cur.Right
				continue
			}
			counted = idx
			cur = right
			blockStart = counted
			blockEnd = counted + cur.Len
		}

		// Split at end boundary if the block extends past `end`.
		if blockEnd > end {
			if right := txn.Store().SplitBlock(cur, end-blockStart); right == nil {
				// Defensive.
				return
			}
			blockEnd = end
		}

		// Now [blockStart, blockEnd) ⊆ [idx, end). Delete cur and
		// advance.
		next := cur.Right
		txn.Delete(cur)
		counted = blockEnd
		cur = next
	}
}

// Range visits live elements in document order. The callback returns
// false to stop early. For ContentAny items with Len > 1 (squashed
// runs from InsertRange / Push) each element is yielded individually.
func (a *Array) Range(fn func(idx uint64, value any) bool) {
	var idx uint64
	for cur := a.branch.Start; cur != nil; cur = cur.Right {
		if cur.IsDeleted() || !cur.IsCountable() {
			continue
		}
		for offset := uint64(0); offset < cur.Len; offset++ {
			v := extractValueAt(cur.Content, offset)
			if !fn(idx, v) {
				return
			}
			idx++
		}
	}
}

// ToSlice materializes the array into a fresh []any. Convenience
// for tests; production callers should prefer Range to avoid the
// allocation.
func (a *Array) ToSlice() []any {
	out := make([]any, 0, a.Len())
	a.Range(func(_ uint64, v any) bool {
		out = append(out, v)
		return true
	})
	return out
}

// findInsertPosition resolves the user-facing index to the (left,
// right) neighbour pair that the YATA integrate algorithm needs.
//
// Walks branch.Start counting live, countable items. If idx lands
// mid-block, calls Store.SplitBlock to materialize a clean boundary
// and returns the (truncated-left, new-right) halves.
//
// Edge cases:
//   - idx == 0: left = nil, right = branch.Start (which may itself
//     be a deleted item; YATA preserves links through tombstones).
//   - idx == Len(): left = last item in linked list (live or
//     deleted, since YATA needs the immediate-list neighbour, not
//     the last live one), right = nil.
//   - idx > Len(): clipped to Len() (treated as append).
//
// Per types-array.md finding 2: this is the eager-split flavour —
// we split immediately rather than deferring via a rel cursor.
//
// Search-marker fast path: if branch.Markers has a marker whose
// Index is closer to idx than the linear-from-start walk would
// be, start the walk from that marker instead. Successful walks
// add a fresh marker at the landing position to amortize the
// cost across subsequent calls near the same area (the B4 trace
// motivator).
func findInsertPosition(branch *block.Branch, txn *doc.TransactionMut, idx uint64) (*block.Item, *block.Item) {
	if idx == 0 {
		return nil, branch.Start
	}

	startItem, startIdx := startFromMarker(branch, idx)

	counted := startIdx
	lastSeen := startItem
	cur := startItem
	if cur == nil {
		cur = branch.Start
	}
	// Marker boundary case: marker said "cur starts at counted" and
	// caller wants idx == counted, so insert immediately before cur.
	// Without this guard the walk falls into the
	// `counted+cur.Len > idx` branch and tries to split cur at
	// offset 0 — corrupts the linked list.
	if cur != nil && counted == idx {
		return cur.Left, cur
	}
	for ; cur != nil; cur = cur.Right {
		lastSeen = cur
		if cur.IsDeleted() || !cur.IsCountable() {
			continue
		}
		if counted+cur.Len == idx {
			rememberMarker(branch, cur.Right, idx)
			return cur, cur.Right
		}
		if counted+cur.Len > idx {
			splitOffset := idx - counted
			right := txn.Store().SplitBlock(cur, splitOffset)
			if right == nil {
				return cur, cur.Right
			}
			rememberMarker(branch, right, idx)
			return cur, right
		}
		counted += cur.Len
	}

	return lastSeen, nil
}

// startFromMarker picks the best starting point for a position
// walk: either the nearest marker (if it's closer to idx than
// branch.Start) or branch.Start itself. Returns the starting Item
// and its known starting index.
//
// Backward walks (target idx < marker.Index) are skipped — the
// shared types' walk loops are forward-only (cur = cur.Right).
// Reverse-direction walking would need a separate Left-traversal
// path that the current callers don't have; an opportunistic
// future enhancement.
func startFromMarker(branch *block.Branch, idx uint64) (*block.Item, uint64) {
	if branch.Markers == nil {
		return nil, 0
	}
	mk := branch.Markers.Nearest(idx)
	if mk == nil || mk.Item == nil {
		return nil, 0
	}
	// Forward-only: marker.Index must be <= idx and closer than 0.
	if mk.Index > idx || mk.Index == 0 {
		return nil, 0
	}
	branch.Markers.Touch(mk)
	return mk.Item, mk.Index
}

// rememberMarker records the (item, idx) pair so the next call
// with a target near idx can short-circuit the walk. Allocates
// branch.Markers lazily.
func rememberMarker(branch *block.Branch, item *block.Item, idx uint64) {
	if item == nil || idx == 0 {
		return
	}
	if branch.Markers == nil {
		branch.Markers = block.NewMarkerList()
	}
	branch.Markers.Add(item, idx)
}

// extractValueAt returns the offset-th element of c. For
// ContentAny (the variant we emit from Insert/InsertRange) this is
// Anys[offset]. ContentString uses byte indexing as a placeholder
// pending the Text type's UTF-16 storage decision; ContentBinary
// returns the whole []byte at offset 0 (it's a single-element
// variant). ContentType returns the wrapped nested *Map/*Array/*Text
// at offset 0 (Len == 1 always for KindType items). Other kinds
// return nil.
func extractValueAt(c block.Content, offset uint64) any {
	switch c.Kind {
	case block.KindAny:
		if offset < uint64(len(c.Anys)) {
			return c.Anys[offset]
		}
	case block.KindString:
		if offset < uint64(len(c.Str)) {
			return string(c.Str[offset])
		}
	case block.KindBinary:
		if offset == 0 {
			return c.Bytes
		}
	case block.KindType:
		if offset != 0 || c.Branch == nil {
			return nil
		}
		switch c.Branch.TypeRef {
		case block.TypeRefMap:
			return NewMap(c.Branch)
		case block.TypeRefArray:
			return NewArray(c.Branch)
		case block.TypeRefText:
			return NewText(c.Branch)
		}
	}
	return nil
}

// InsertMap inserts a freshly-constructed nested Map at idx and
// returns the wrapper. Per docs/yrs-port-notes/nested-types.md §6.
//
// Like InsertRange, idx is clamped to [0, Len]; the new Map element
// occupies one slot (Len contribution = 1).
func (a *Array) InsertMap(txn *doc.TransactionMut, idx uint64) *Map {
	inner := &block.Branch{
		TypeRef: block.TypeRefMap,
		Map:     map[string]*block.Item{},
	}
	a.insertNested(txn, idx, inner)
	return &Map{branch: inner}
}

// InsertArray inserts a freshly-constructed nested Array at idx.
func (a *Array) InsertArray(txn *doc.TransactionMut, idx uint64) *Array {
	inner := &block.Branch{TypeRef: block.TypeRefArray}
	a.insertNested(txn, idx, inner)
	return &Array{branch: inner}
}

// InsertText inserts a freshly-constructed nested Text at idx
// (plain-text only).
func (a *Array) InsertText(txn *doc.TransactionMut, idx uint64) *Text {
	inner := &block.Branch{TypeRef: block.TypeRefText}
	a.insertNested(txn, idx, inner)
	return &Text{branch: inner}
}

// insertNested is the shared scaffolding for InsertMap/InsertArray/
// InsertText: build an Item with KindType content at position idx
// and integrate. Item.Integrate KindType arm wires Branch.Item back
// to the Item.
func (a *Array) insertNested(txn *doc.TransactionMut, idx uint64, inner *block.Branch) {
	left, right := findInsertPosition(a.branch, txn, idx)

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

	item := &block.Item{
		ID:          block.ID{Client: clientID, Clock: clock},
		Len:         1,
		Origin:      origin,
		Left:        left,
		RightOrigin: rightOrigin,
		Right:       right,
		Content:     block.Content{Kind: block.KindType, Branch: inner},
		Parent:      block.Parent{Kind: block.ParentBranch, Branch: a.branch},
		Flags:       block.FlagCountable,
	}

	txn.Store().PushBlock(item)
	if dropped := item.Integrate(txn, 0); dropped {
		txn.Delete(item)
	}
}
