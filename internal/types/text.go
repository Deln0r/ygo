package types

import (
	"fmt"
	"strings"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/utf16"
)

// Text is the user-facing wrapper around a Branch holding plain
// text. The branch's positional linked list (branch.Start) holds
// ContentString items; per types-text.md their Item.Len is in
// UTF-16 code units (matching JS Yjs and the wire format).
//
// Plain-text only in this commit. Rich-text formatting (Format
// markers, applyDelta, embeds, attributes) is tracked in
// docs/tech-debt.md.
//
// All offsets and lengths in Insert / Delete are UTF-16 code units,
// not Go bytes and not runes. Use utf16.Length(string) to convert
// a Go string to its UTF-16 length when computing positions.
type Text struct {
	branch *block.Branch
}

// NewText wraps the given branch as a Text. It tags the branch as a
// Text type (TypeRefText) so the observer dispatcher can tell a root
// Text from a root Array, whose branches both default to TypeRef zero
// otherwise. The root TypeRef is never serialized (roots are written
// by name), so this does not affect the wire format.
func NewText(branch *block.Branch) *Text {
	if branch != nil {
		branch.TypeRef = block.TypeRefText
	}
	return &Text{branch: branch}
}

// Branch returns the underlying *block.Branch for low-level access.
func (t *Text) Branch() *block.Branch { return t.branch }

// Length returns the total number of UTF-16 code units in live
// content. O(1) — reads branch.BlockLen which Item.Integrate (+)
// and TransactionMut.Delete (-) maintain.
//
// Per types-text.md §4: for our UTF-16-only port, BlockLen is
// already the count of UTF-16 units across live String items.
func (t *Text) Length() uint64 { return t.branch.BlockLen }

// String returns the concatenation of every live ContentString in
// document order as a Go UTF-8 string. O(N) over the linked list.
//
// Mirrors yrs Text::get_string (text.rs:120-132).
func (t *Text) String() string {
	var b strings.Builder
	for cur := t.branch.Start; cur != nil; cur = cur.Right {
		if cur.IsDeleted() {
			continue
		}
		if cur.Content.Kind == block.KindString {
			b.WriteString(cur.Content.Str)
		}
	}
	return b.String()
}

// Insert inserts str at the UTF-16 code-unit position idx. idx must
// be in [0, Length()]. str may contain any Unicode characters; non-BMP
// chars (e.g. emoji) contribute 2 UTF-16 units each.
//
// The text takes the formatting in effect at idx, as yjs insert(index,
// text) does: it goes in right after the content before idx, past
// tombstones and past markers that set a value already in effect, and
// before the first marker that changes one. So text typed at the end of
// a bold run is bold, and text typed at its start is not.
//
// Multiple consecutive Insert calls produce separate Items;
// commit-time TrySquash merges adjacent same-client adjacent-clock
// String items into one (yrs/src/block.rs:1987-1990).
//
// Returns an error if idx is out of range.
func (t *Text) Insert(txn *doc.TransactionMut, idx uint64, str string) error {
	if str == "" {
		return nil
	}
	length := t.Length()
	if idx > length {
		return fmt.Errorf("text: insert index %d out of range [0, %d]", idx, length)
	}
	if t.branch.HasFormatting {
		pos, err := findTextPos(txn, t.branch, idx)
		if err != nil {
			return err
		}
		attrs, keys := pos.currentAttrs()
		item, at := insertText(txn, t.branch, pos, block.Content{Kind: block.KindString, Str: str}, utf16.Length(str), attrs, keys)
		if !item.IsDeleted() {
			t.branch.Markers.ShiftAfter(at, item.Len)
			rememberMarker(t.branch, item, at)
		}
		return nil
	}

	// No format item in this text: nothing is in effect anywhere, and
	// insertText reduces to stepping over the tombstones after the
	// position. YATA convergence needs concurrent inserts at the same
	// index to attach to the same live successor as right origin.
	left, right, err := findTextPosition(t.branch, txn, idx)
	if err != nil {
		return err
	}
	for right != nil && right.IsDeleted() {
		left = right
		right = right.Right
	}
	item := integrateContent(txn, t.branch, left, right, block.Content{Kind: block.KindString, Str: str}, utf16.Length(str))
	if item.IsDeleted() {
		return nil
	}
	t.branch.Markers.ShiftAfter(idx, item.Len)
	rememberMarker(t.branch, item, idx)
	return nil
}

// Delete removes length UTF-16 code units starting at idx. Items
// straddling the [idx, idx+length) range are split via Store.SplitBlock
// at UTF-16 boundaries so the deletion lands on Item edges.
//
// Returns an error if the range exceeds the current Length.
//
// After deleting, format markers left in the gap that no longer mark
// anything are removed, as yjs deleteText does with
// cleanupFormattingGap.
//
// Mirrors yrs Text::remove_range (text.rs:361-368) → remove
// (text.rs:806-863).
func (t *Text) Delete(txn *doc.TransactionMut, idx, length uint64) error {
	total := t.Length()
	if idx+length > total {
		return fmt.Errorf("text: delete range [%d, %d) exceeds length %d", idx, idx+length, total)
	}
	if length == 0 {
		return nil
	}
	defer t.branch.Markers.ShrinkAfter(idx, length)

	// Start split happens here: findTextPosition splits any String
	// item that idx lands inside.
	_, right, err := findTextPosition(t.branch, txn, idx)
	if err != nil {
		return err
	}
	// The gap starts right after the last content before idx, ahead of
	// any format markers and tombstones in between, where yjs's cursor
	// stops. A lookup that hit a cached position can return a later item.
	gapStart := right
	for gapStart != nil && gapStart.Left != nil && (gapStart.Left.IsDeleted() || gapStart.Left.Content.Kind == block.KindFormat) {
		gapStart = gapStart.Left
	}

	remaining := length
	cur := right
	for cur != nil && remaining > 0 {
		// Format markers take no room; embeds and nested types are one
		// unit each and are deleted like text, as yjs deleteText does.
		if cur.IsDeleted() || cur.Content.Kind == block.KindFormat {
			cur = cur.Right
			continue
		}
		contentLen := cur.Len
		if remaining < contentLen {
			// End split: carve cur at `remaining` UTF-16 units, take
			// left half (the survivor of the split is what we delete).
			if right := txn.Store().SplitBlock(cur, remaining); right == nil {
				return fmt.Errorf("text: end-split failed at offset %d in item %v", remaining, cur.ID)
			}
			remaining = 0
		} else {
			remaining -= contentLen
		}
		next := cur.Right
		txn.Delete(cur)
		cur = next
	}
	cleanupFormattingGap(txn, t.branch, gapStart)
	return nil
}

// cleanupFormattingGap removes the format markers in the run of markers
// and tombstones that starts at start and ends at the next live content
// that no longer mark anything: a marker that a later marker in the
// run overrides for the same key, or one that sets the value already
// in effect before the run. Text.Delete calls it after deleting; the
// attributes in effect before the run are collected only when the run
// holds a live marker.
func cleanupFormattingGap(txn *doc.TransactionMut, branch *block.Branch, start *block.Item) {
	if start == nil {
		return
	}
	for end := start; end != nil && (end.IsDeleted() || end.Content.Kind == block.KindFormat); end = end.Right {
		if !end.IsDeleted() {
			cleanupFormattingGapFrom(txn, start, nil, currentAttributesAt(branch, start), nil)
			return
		}
	}
}

// cleanupFormattingGapFrom is yjs cleanupFormattingGap: in the run of
// markers and tombstones from start to the next live content, it deletes
// each marker that a later marker in the run overrides for the same key
// or that sets the value startAttrs already holds, comparing values
// strictly (identity for objects, as JS ===). When pos is not nil, its
// attributes follow for the markers before curr (the cursor's right): a
// deleted marker's value is taken back, a kept marker applied. It returns
// the number of markers deleted.
func cleanupFormattingGapFrom(txn *doc.TransactionMut, start, curr *block.Item, startAttrs Attrs, pos *textPos) int {
	end := start
	last := map[string]*block.Item{}
	for end != nil && (end.IsDeleted() || !end.IsCountable()) {
		if !end.IsDeleted() && end.Content.Kind == block.KindFormat {
			last[end.Content.FormatKey] = end
		}
		end = end.Right
	}
	cleanups := 0
	reachedCurr := false
	for ; start != end; start = start.Right {
		if start == curr {
			reachedCurr = true
		}
		if start.IsDeleted() || start.Content.Kind != block.KindFormat {
			continue
		}
		key, value := start.Content.FormatKey, formatValue(start)
		before := startAttrs[key]
		if last[key] != start || strictEqual(before, value) {
			txn.Delete(start)
			cleanups++
			if !reachedCurr && pos != nil && strictEqual(pos.attrs[key], value) && !strictEqual(before, value) {
				if before == nil {
					pos.deleteAttr(key)
				} else {
					pos.setAttr(key, before)
				}
			}
		}
		if !reachedCurr && pos != nil && !start.IsDeleted() {
			pos.updateAttr(key, value)
		}
	}
	return cleanups
}

// deleteText deletes length units of content from pos, or as many as
// the text holds after it, and leaves pos after them. A port of yjs
// deleteText, used by ApplyDelta: markers in the range stay, and
// cleanupFormattingGapFrom then removes the ones that no longer mark
// anything and corrects the cursor's attributes.
func deleteText(txn *doc.TransactionMut, branch *block.Branch, pos *textPos, length uint64) error {
	startAttrs, _ := pos.currentAttrs()
	start := pos.right
	for length > 0 && pos.right != nil {
		r := pos.right
		if !r.IsDeleted() {
			switch r.Content.Kind {
			case block.KindString, block.KindEmbed, block.KindType:
				if length < r.Len {
					if txn.Store().SplitBlock(r, length) == nil {
						return fmt.Errorf("text: split failed at offset %d in item %v", length, r.ID)
					}
				}
				length -= r.Len
				txn.Delete(r)
			}
		}
		pos.forward()
	}
	if start != nil {
		cleanupFormattingGapFrom(txn, start, pos.right, startAttrs, pos)
	}
	return nil
}

// findTextPosition resolves a UTF-16 cursor index into the (left,
// right) neighbour pair YATA needs. Walks branch.Start counting live
// content by length: strings in UTF-16 units, embeds and nested types
// as one unit each, the same count Length and the search markers use.
// Format markers and deleted items take no room. On a mid-block hit it
// calls Store.SplitBlock at the UTF-16 boundary.
//
// Mirrors yrs find_position (text.rs:734-804).
//
// Edge cases:
//   - idx == 0: returns (nil, branch.Start). branch.Start may be a
//     tombstone; YATA handles that fine.
//   - idx == Length(): walk completes; returns (lastSeen, nil).
//   - idx > Length(): caller should have rejected; returns clipped
//     to end (lastSeen, nil) defensively.
//
// Search-marker fast path: same shape as Array's
// findInsertPosition. Forward-only walks from the nearest marker
// turn the B4 LaTeX-trace workload from O(N) per edit into
// O(distance-from-cursor) per edit.
func findTextPosition(branch *block.Branch, txn *doc.TransactionMut, idx uint64) (*block.Item, *block.Item, error) {
	if idx == 0 {
		return nil, branch.Start, nil
	}

	startItem, startIdx := startFromMarker(branch, idx)
	counted := startIdx
	lastSeen := startItem
	cur := startItem
	if cur == nil {
		cur = branch.Start
	}
	// Marker boundary case: see findInsertPosition. Without this
	// guard a marker exactly at idx triggers a degenerate offset-0
	// split inside cur.
	if cur != nil && counted == idx {
		return cur.Left, cur, nil
	}
	for ; cur != nil; cur = cur.Right {
		lastSeen = cur
		if cur.IsDeleted() || cur.Content.Kind == block.KindFormat {
			continue
		}
		// The length comes from the content, not from cur.Len. The two
		// are equal for every live item, but reading cur.Len here made the
		// B4 editing trace 2.3 times slower (47 s against 20 s, interleaved
		// runs on one machine) with the same number of iterations; the
		// cause is below the language and was not pinned down.
		contentLen := cur.Content.Len(block.OffsetUtf16)
		if counted+contentLen == idx {
			rememberMarker(branch, cur.Right, idx)
			return cur, cur.Right, nil
		}
		if counted+contentLen > idx {
			splitOffset := idx - counted
			right := txn.Store().SplitBlock(cur, splitOffset)
			if right == nil {
				return nil, nil, fmt.Errorf("text: mid-block split failed at offset %d in item %v", splitOffset, cur.ID)
			}
			rememberMarker(branch, right, idx)
			return cur, right, nil
		}
		counted += contentLen
	}
	return lastSeen, nil, nil
}
