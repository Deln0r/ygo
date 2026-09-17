package types

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/utf16"
)

// Attrs is the format-attribute map carried by KindFormat markers
// and exposed in Range / ToDelta output. Per
// docs/yrs-port-notes/types-text-rich.md §9.
//
// Nil values inside Attrs mean "clear this attribute" — they encode
// to the Any-Null tag on the wire and are interpreted by
// currentAttributes as removing the key. To distinguish "key not
// present" from "key present with nil value", check via the second
// return of a map lookup.
type Attrs = map[string]any

// DeltaOp is a single op from a Quill-style delta. Exactly one of
// Insert / Retain / Delete is non-zero for any given op. Used by
// ToDelta to expose the doc and by ApplyDelta to ingest one (the
// ApplyDelta API ships in a follow-up commit; see tech-debt.md).
type DeltaOp struct {
	// Insert is the text to insert at the cursor. Mutually exclusive
	// with Embed, Retain, Delete.
	Insert string

	// Embed is a single embedded value (image, mention, etc.). Mutually
	// exclusive with Insert, Retain, Delete. The Embed value's runtime
	// type matches whatever Y.encodeStateAsUpdate-produced JS clients
	// would round-trip through Any — typically a map[string]any.
	Embed any

	// Retain advances the cursor by N UTF-16 units, optionally
	// applying Attributes to the retained range. Mutually exclusive
	// with Insert, Embed, Delete.
	Retain uint64

	// Delete removes N UTF-16 units from the cursor. Mutually exclusive
	// with Insert, Embed, Retain.
	Delete uint64

	// Attributes applies to Insert, Embed, or Retain. Nil-valued keys
	// inside Attributes mean "clear this attribute" on the affected
	// range; absent keys preserve the current value.
	Attributes Attrs
}

// InsertWithAttributes inserts str at idx, opening format markers
// before the text for any attribute whose value differs from the
// current formatting, and closing them afterward to restore the
// prior state.
//
// nil attrs is equivalent to Insert(idx, str). An empty map (zero
// keys) is also equivalent.
//
// Per docs/yrs-port-notes/types-text-rich.md §5, the algorithm
// mirrors yrs Text::insert_with_attributes (text.rs:280-295) +
// JS Y.Text._insertWithAttributes (ytype.js:330-350).
func (t *Text) InsertWithAttributes(txn *doc.TransactionMut, idx uint64, str string, attrs Attrs) error {
	if str == "" {
		return nil
	}
	if len(attrs) == 0 {
		return t.Insert(txn, idx, str)
	}

	length := t.Length()
	if idx > length {
		return fmt.Errorf("text: insert index %d out of range [0, %d]", idx, length)
	}

	left, right, err := findTextPosition(t.branch, txn, idx)
	if err != nil {
		return err
	}
	// Skip past tombstones AND existing format markers at the cursor
	// so the new content lands at the natural end of the position
	// (after any close markers that would otherwise leave the new
	// text inside the prior formatting range).
	for right != nil && (right.IsDeleted() || right.Content.Kind == block.KindFormat) {
		left = right
		right = right.Right
	}

	currentAttrs := currentAttributesAt(t.branch, right)
	keys := sortedAttrKeys(attrs)

	// Emit opening markers for every attr whose target value differs
	// from current. Skip keys that already match.
	for _, key := range keys {
		value := attrs[key]
		if attrValuesEqual(currentAttrs[key], value) {
			continue
		}
		marker := buildFormatMarker(txn, left, right, t.branch, key, value)
		// The new marker is now the left neighbour for subsequent
		// inserts at this cursor.
		left = marker
		// right stays — markers sit between left and the next item.
	}

	// Insert the text itself.
	if err := insertStringBetween(txn, t.branch, left, right, str); err != nil {
		return err
	}

	// Compute the post-insert cursor for closing markers: walk to
	// just past the string item we just inserted.
	postLeft := postInsertLeftOf(t.branch, idx+utf16.Length(str))
	postRight := neighbourRightOf(postLeft)

	// Emit closing markers to restore the previous formatting where
	// our applied attrs differ. For each key we opened, emit a
	// marker carrying its previous value (which may be nil = clear).
	for _, key := range keys {
		value := attrs[key]
		prev := currentAttrs[key]
		if attrValuesEqual(prev, value) {
			continue
		}
		closeMarker := buildFormatMarker(txn, postLeft, postRight, t.branch, key, prev)
		postLeft = closeMarker
	}

	return nil
}

// Format applies attrs to the range [idx, idx+length). It is a port of
// yjs YText.format / formatText: the cursor stops at idx before any
// format markers there, markers that already set a requested value are
// stepped over, an opening marker is written for every attribute that
// changes, markers inside the range for a requested key are deleted
// (they would otherwise re-establish the old value mid-range), and
// markers after the range restore what was in effect there.
//
// A nil value in attrs clears that attribute over the range. length == 0
// and empty attrs are no-ops. A range past the end of the text is an
// error; yjs instead appends newlines for the missing length, a Quill
// convention this port does not follow.
func (t *Text) Format(txn *doc.TransactionMut, idx, length uint64, attrs Attrs) error {
	if length == 0 || len(attrs) == 0 {
		return nil
	}
	total := t.Length()
	if idx > total || length > total-idx {
		return fmt.Errorf("text: format range of %d at %d exceeds length %d", length, idx, total)
	}
	pos, err := findTextPos(txn, t.branch, idx)
	if err != nil {
		return err
	}
	return formatText(txn, t.branch, pos, length, attrs)
}

// textPos is a cursor between two items of a text, carrying the
// formatting in effect at that point. It mirrors yjs
// ItemTextListPosition.
type textPos struct {
	left, right *block.Item
	index       uint64
	attrs       Attrs
}

// forward steps the cursor over right, which must not be nil. A live
// format marker updates the formatting in effect; live content advances
// the index. Deleted items change neither.
func (p *textPos) forward() {
	r := p.right
	if !r.IsDeleted() {
		if r.Content.Kind == block.KindFormat {
			updateCurrentAttrs(p.attrs, r.Content.FormatKey, formatValue(r))
		} else {
			p.index += r.Len
		}
	}
	p.left = r
	p.right = r.Right
}

// findTextPos walks from the start of the text to index, splitting a
// string item the index falls inside. Like yjs findNextPosition it
// stops as soon as index units of content are behind it, so format
// markers right after the index stay ahead of the cursor. It does not
// use search markers: the formatting in effect at the cursor needs
// every marker before it.
func findTextPos(txn *doc.TransactionMut, branch *block.Branch, index uint64) (*textPos, error) {
	pos := &textPos{right: branch.Start, attrs: Attrs{}}
	for pos.right != nil && index > 0 {
		r := pos.right
		if !r.IsDeleted() && r.Content.Kind != block.KindFormat && index < r.Len {
			if txn.Store().SplitBlock(r, index) == nil {
				return nil, fmt.Errorf("text: split failed at offset %d in item %v", index, r.ID)
			}
		}
		if !r.IsDeleted() && r.Content.Kind != block.KindFormat {
			index -= r.Len
		}
		pos.forward()
	}
	return pos, nil
}

// negatedAttrs holds the values to restore after a formatted range, in
// the order they were recorded. Setting an existing key keeps its place
// and deleting then setting moves it last, as with a JavaScript Map.
type negatedAttrs struct {
	keys   []string
	values map[string]any
}

func (n *negatedAttrs) set(key string, value any) {
	if n.values == nil {
		n.values = map[string]any{}
	}
	if _, ok := n.values[key]; !ok {
		n.keys = append(n.keys, key)
	}
	n.values[key] = value
}

func (n *negatedAttrs) get(key string) (any, bool) {
	v, ok := n.values[key]
	return v, ok
}

func (n *negatedAttrs) remove(key string) {
	if _, ok := n.values[key]; !ok {
		return
	}
	delete(n.values, key)
	for i, k := range n.keys {
		if k == key {
			n.keys = append(n.keys[:i], n.keys[i+1:]...)
			return
		}
	}
}

// minimizeAttributeChanges steps over deleted items and over format
// markers that already set the value attrs asks for, so no marker is
// written for a change that is already in effect. An attribute missing
// from attrs counts as nil. Mirrors yjs minimizeAttributeChanges.
func minimizeAttributeChanges(pos *textPos, attrs Attrs) {
	for pos.right != nil {
		r := pos.right
		if !r.IsDeleted() && (r.Content.Kind != block.KindFormat || !attrValuesEqual(attrs[r.Content.FormatKey], formatValue(r))) {
			return
		}
		pos.forward()
	}
}

// insertAttributes writes an opening marker for every attribute whose
// requested value differs from the one in effect, and returns the values
// that were in effect so they can be restored after the range. Mirrors
// yjs insertAttributes, with keys in ascending order.
func insertAttributes(txn *doc.TransactionMut, branch *block.Branch, pos *textPos, attrs Attrs) *negatedAttrs {
	negated := &negatedAttrs{}
	for _, key := range sortedAttrKeys(attrs) {
		value := attrs[key]
		current := pos.attrs[key]
		if attrValuesEqual(current, value) {
			continue
		}
		negated.set(key, current)
		pos.right = buildFormatMarker(txn, pos.left, pos.right, branch, key, value)
		pos.forward()
	}
	return negated
}

// formatText applies attrs to length units of content from pos. Mirrors
// yjs formatText: after the opening markers it walks the range, deleting
// markers for requested keys, and keeps walking past the range while
// values remain to restore and the next item is a marker or deleted, so
// a marker that already restores a value is reused instead of doubled.
func formatText(txn *doc.TransactionMut, branch *block.Branch, pos *textPos, length uint64, attrs Attrs) error {
	minimizeAttributeChanges(pos, attrs)
	negated := insertAttributes(txn, branch, pos, attrs)
walk:
	for pos.right != nil {
		r := pos.right
		if length == 0 && (len(negated.keys) == 0 || (!r.IsDeleted() && r.Content.Kind != block.KindFormat)) {
			break
		}
		if !r.IsDeleted() {
			if r.Content.Kind == block.KindFormat {
				key, value := r.Content.FormatKey, formatValue(r)
				if requested, ok := attrs[key]; ok {
					if attrValuesEqual(requested, value) {
						negated.remove(key)
					} else {
						if length == 0 {
							break walk
						}
						negated.set(key, value)
					}
					txn.Delete(r)
				} else {
					pos.attrs[key] = value
				}
			} else {
				if length < r.Len {
					if txn.Store().SplitBlock(r, length) == nil {
						return fmt.Errorf("text: split failed at offset %d in item %v", length, r.ID)
					}
				}
				length -= r.Len
			}
		}
		pos.forward()
	}
	insertNegatedAttributes(txn, branch, pos, negated)
	return nil
}

// insertNegatedAttributes restores the values recorded in negated after a
// formatted range. Deleted items are stepped over, and so are markers
// that already set a recorded value, which drop that value from the list.
// Mirrors yjs insertNegatedAttributes.
func insertNegatedAttributes(txn *doc.TransactionMut, branch *block.Branch, pos *textPos, negated *negatedAttrs) {
	for pos.right != nil {
		r := pos.right
		if !r.IsDeleted() {
			if r.Content.Kind != block.KindFormat {
				break
			}
			value, ok := negated.get(r.Content.FormatKey)
			if !ok || !attrValuesEqual(value, formatValue(r)) {
				break
			}
			negated.remove(r.Content.FormatKey)
		}
		pos.forward()
	}
	for _, key := range negated.keys {
		pos.right = buildFormatMarker(txn, pos.left, pos.right, branch, key, negated.values[key])
		pos.forward()
	}
}

// formatValue returns the value a format marker sets; nil clears.
func formatValue(it *block.Item) any {
	if len(it.Content.Anys) > 0 {
		return it.Content.Anys[0]
	}
	return nil
}

// InsertEmbed inserts a single embedded value at idx. The value
// must be a type representable through lib0 Any encoding —
// typically map[string]any or one of the scalar Any types.
//
// Embeds count as one UTF-16 position in Text.Length. They are
// emitted by Range / ToDelta as an Embed op rather than a string
// chunk.
func (t *Text) InsertEmbed(txn *doc.TransactionMut, idx uint64, value any) error {
	total := t.Length()
	if idx > total {
		return fmt.Errorf("text: embed index %d out of range [0, %d]", idx, total)
	}
	left, right, err := findTextPosition(t.branch, txn, idx)
	if err != nil {
		return err
	}
	for right != nil && right.IsDeleted() {
		left = right
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

	item := &block.Item{
		ID:          block.ID{Client: clientID, Clock: clock},
		Len:         1,
		Origin:      origin,
		Left:        left,
		RightOrigin: rightOrigin,
		Right:       right,
		Content:     block.Content{Kind: block.KindEmbed, Anys: []block.Any{value}},
		Parent:      block.Parent{Kind: block.ParentBranch, Branch: t.branch},
		Flags:       block.FlagCountable,
	}
	txn.Store().PushBlock(item)
	if dropped := item.Integrate(txn, 0); dropped {
		txn.Delete(item)
	}
	return nil
}

// ChunkKind discriminates the variants emitted by Text.Range.
type ChunkKind uint8

const (
	// ChunkString is a contiguous run of UTF-8 text from a KindString
	// item; the value passed to fn is a string.
	ChunkString ChunkKind = iota
	// ChunkEmbed is a single embedded value from a KindEmbed item;
	// the value passed to fn is whatever the embed payload's runtime
	// type is (typically map[string]any or a primitive Any).
	ChunkEmbed
)

// Range walks the document and invokes fn for each (kind, value, attrs)
// chunk. attrs reflects the current formatting at this chunk's start.
//
// Stops early when fn returns false. Returns immediately for an
// empty doc.
//
// Per docs/yrs-port-notes/types-text-rich.md §8 — companion to ToDelta.
// Explicit ChunkKind discriminator means a string-typed embed payload
// is not misidentified as a text chunk by callers that switch on
// the runtime type of the value.
func (t *Text) Range(fn func(kind ChunkKind, value any, attrs Attrs) bool) {
	current := Attrs{}
	for cur := t.branch.Start; cur != nil; cur = cur.Right {
		if cur.IsDeleted() {
			continue
		}
		switch cur.Content.Kind {
		case block.KindFormat:
			var value any
			if len(cur.Content.Anys) > 0 {
				value = cur.Content.Anys[0]
			}
			updateCurrentAttrs(current, cur.Content.FormatKey, value)
		case block.KindString:
			if cur.Content.Str == "" {
				continue
			}
			if !fn(ChunkString, cur.Content.Str, copyAttrs(current)) {
				return
			}
		case block.KindEmbed:
			var v any
			if len(cur.Content.Anys) > 0 {
				v = cur.Content.Anys[0]
			}
			if !fn(ChunkEmbed, v, copyAttrs(current)) {
				return
			}
		}
	}
}

// ApplyDelta walks ops in order and dispatches each to the
// appropriate primitive (InsertWithAttributes / InsertEmbed /
// Format / Delete) while maintaining a cursor through the text.
//
// Op semantics (mirrors JS Y.Text.applyDelta from
// testdata/gen/node_modules/yjs/src/types/YText.js):
//
//   - Insert string with optional Attributes — insert text at
//     cursor, advance cursor by len(text)
//   - Insert non-string (Embed) with optional Attributes — insert a
//     single embed at cursor, format it with Attributes, advance
//     cursor by 1
//   - Retain N with optional Attributes — if Attributes is non-nil,
//     apply Format(cursor, N, Attributes); advance cursor by N
//   - Delete N — delete N units at cursor; cursor unchanged
//     (the text shifts left under the cursor)
//
// All ops execute inside the supplied txn so the entire delta is
// atomic from peers' perspective. Errors abort mid-delta — the
// txn caller must decide whether to commit the partial result or
// roll back (the doc layer's TransactionMut has no Rollback, so
// the partial mutation persists; this mirrors yrs's behaviour).
//
// Per docs/yrs-port-notes/types-text-rich.md §7. Empty ops slice
// is a no-op; nil ops also a no-op.
func (t *Text) ApplyDelta(txn *doc.TransactionMut, ops []DeltaOp) error {
	var cursor uint64
	for i, op := range ops {
		switch {
		case op.Insert != "":
			if err := t.InsertWithAttributes(txn, cursor, op.Insert, op.Attributes); err != nil {
				return fmt.Errorf("ApplyDelta op[%d] insert: %w", i, err)
			}
			cursor += utf16.Length(op.Insert)
		case op.Embed != nil:
			if err := t.InsertEmbed(txn, cursor, op.Embed); err != nil {
				return fmt.Errorf("ApplyDelta op[%d] embed: %w", i, err)
			}
			if len(op.Attributes) > 0 {
				if err := t.Format(txn, cursor, 1, op.Attributes); err != nil {
					return fmt.Errorf("ApplyDelta op[%d] embed attributes: %w", i, err)
				}
			}
			cursor++
		case op.Retain > 0:
			if op.Attributes != nil {
				if err := t.Format(txn, cursor, op.Retain, op.Attributes); err != nil {
					return fmt.Errorf("ApplyDelta op[%d] retain+format: %w", i, err)
				}
			}
			cursor += op.Retain
		case op.Delete > 0:
			if err := t.Delete(txn, cursor, op.Delete); err != nil {
				return fmt.Errorf("ApplyDelta op[%d] delete: %w", i, err)
			}
			// cursor unchanged — text shifts left under cursor
		default:
			// All-zero op (or a Retain == 0 with nil attrs) is a no-op.
		}
	}
	return nil
}

// ToDelta returns the Quill-style delta representation of the doc.
// Adjacent same-attribute string chunks are coalesced into single
// insert ops. Embeds appear as separate ops.
//
// The returned slice has no Retain / Delete ops — those exist only
// in deltas representing CHANGES, not snapshots.
func (t *Text) ToDelta() []DeltaOp {
	var out []DeltaOp
	var pendingStr string
	var pendingAttrs Attrs

	flush := func() {
		if pendingStr == "" {
			return
		}
		out = append(out, DeltaOp{
			Insert:     pendingStr,
			Attributes: pendingAttrs,
		})
		pendingStr = ""
		pendingAttrs = nil
	}

	t.Range(func(kind ChunkKind, v any, attrs Attrs) bool {
		switch kind {
		case ChunkString:
			s, _ := v.(string)
			if pendingStr != "" && !attrsEqual(pendingAttrs, attrs) {
				flush()
			}
			pendingStr += s
			if pendingAttrs == nil {
				pendingAttrs = attrs
			}
		case ChunkEmbed:
			flush()
			out = append(out, DeltaOp{
				Embed:      v,
				Attributes: copyNonEmptyAttrs(attrs),
			})
		}
		return true
	})
	flush()
	return out
}

// buildFormatMarker constructs and integrates a single KindFormat
// Item between left and right neighbours. Returns the new Item so
// the caller can chain subsequent inserts off it.
func buildFormatMarker(txn *doc.TransactionMut, left, right *block.Item, parentBranch *block.Branch, key string, value any) *block.Item {
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
	marker := &block.Item{
		ID:          block.ID{Client: clientID, Clock: clock},
		Len:         1,
		Origin:      origin,
		Left:        left,
		RightOrigin: rightOrigin,
		Right:       right,
		Content:     block.Content{Kind: block.KindFormat, FormatKey: key, Anys: []block.Any{value}},
		Parent:      block.Parent{Kind: block.ParentBranch, Branch: parentBranch},
		// FlagCountable left off — format markers do not contribute
		// to branch.BlockLen (per Content.IsCountable() returning
		// false for KindFormat).
	}
	txn.Store().PushBlock(marker)
	if dropped := marker.Integrate(txn, 0); dropped {
		txn.Delete(marker)
	}
	return marker
}

// insertStringBetween emits a KindString Item between left and
// right neighbours. Used by InsertWithAttributes after format
// markers have been placed.
func insertStringBetween(txn *doc.TransactionMut, parentBranch *block.Branch, left, right *block.Item, str string) error {
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
		Len:         utf16.Length(str),
		Origin:      origin,
		Left:        left,
		RightOrigin: rightOrigin,
		Right:       right,
		Content:     block.Content{Kind: block.KindString, Str: str},
		Parent:      block.Parent{Kind: block.ParentBranch, Branch: parentBranch},
		Flags:       block.FlagCountable,
	}
	txn.Store().PushBlock(item)
	if dropped := item.Integrate(txn, 0); dropped {
		txn.Delete(item)
	}
	return nil
}

// currentAttributesAt is the branch-aware accumulator used by
// InsertWithAttributes / Format. Walks branch.Start forward until
// it reaches the supplied right neighbour (or the end), accumulating
// format markers along the way. The last occurrence per key wins.
//
// Tombstoned items are skipped. nil-valued format markers clear
// the key (deleted from the running map).
func currentAttributesAt(branch *block.Branch, right *block.Item) Attrs {
	out := Attrs{}
	for cur := branch.Start; cur != nil; cur = cur.Right {
		if cur == right {
			break
		}
		if cur.IsDeleted() {
			continue
		}
		if cur.Content.Kind != block.KindFormat {
			continue
		}
		var value any
		if len(cur.Content.Anys) > 0 {
			value = cur.Content.Anys[0]
		}
		updateCurrentAttrs(out, cur.Content.FormatKey, value)
	}
	return out
}

// postInsertLeftOf finds the leftmost Item at or after position pos
// in the branch — essentially the same as findTextPosition's left
// return but without triggering a split. Used by InsertWithAttributes
// to position closing markers after the just-inserted string.
//
// Walks from branch.Start counting UTF-16 units in live String items.
// Returns nil if pos is at the very start (no item to anchor left of).
func postInsertLeftOf(branch *block.Branch, pos uint64) *block.Item {
	var counted uint64
	var lastSeen *block.Item
	for cur := branch.Start; cur != nil; cur = cur.Right {
		if cur.IsDeleted() || cur.Content.Kind == block.KindFormat {
			continue
		}
		counted += cur.Len
		lastSeen = cur
		if counted >= pos {
			return cur
		}
	}
	return lastSeen
}

// neighbourRightOf returns the Item immediately after item in the
// branch linked list, or nil if item is at the tail. Used to compute
// the right-neighbour for marker insertion.
func neighbourRightOf(item *block.Item) *block.Item {
	if item == nil {
		return nil
	}
	return item.Right
}

// updateCurrentAttrs applies one KindFormat marker to the running
// attrs map: a nil value clears the key, a non-nil value sets it.
func updateCurrentAttrs(attrs Attrs, key string, value any) {
	if value == nil {
		delete(attrs, key)
		return
	}
	attrs[key] = value
}

// sortedAttrKeys returns the keys of attrs in ascending order. Each
// attribute becomes its own format marker, and the order the markers
// are emitted in is part of the encoded update. yjs walks the
// attribute object in property order and closes in that same order;
// a Go map has no order of its own and ranging over one is randomised,
// which made the same edit encode to different bytes from run to run.
// Sorted keys give one order, for opening and closing markers alike.
func sortedAttrKeys(attrs Attrs) []string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// copyAttrs returns a shallow copy of attrs so callers cannot
// mutate the walker's running map.
func copyAttrs(attrs Attrs) Attrs {
	if len(attrs) == 0 {
		return nil
	}
	out := make(Attrs, len(attrs))
	for k, v := range attrs {
		out[k] = v
	}
	return out
}

// copyNonEmptyAttrs returns nil for empty attrs (cleaner serialization)
// or a deep copy otherwise.
func copyNonEmptyAttrs(attrs Attrs) Attrs {
	if len(attrs) == 0 {
		return nil
	}
	return copyAttrs(attrs)
}

// attrValuesEqual decides whether two attribute values are equal, the way yjs
// decides it: YText.js equalAttrs is `a === b || (both objects && equalFlat)`,
// and lib0 object.equalFlat is "same size, and every entry strictly equal".
//
// Each part of that is load-bearing, and each was checked against yjs 13.6.32:
//
//   - The identity check comes first, so one container reused for both values
//     is equal even when it holds a NaN, which fails an entry-by-entry check.
//   - "Strictly equal" is JS ===, so the comparison goes one level deep and a
//     nested container compares by identity: {x:{y:1}} and a separate,
//     equal-by-value {x:{y:1}} stay distinct.
//   - JS arrays and objects are both objects to equalFlat, so [1,2] equals
//     {"0":1,"1":2}, and [] equals {}.
//
// This function decides whether Format and InsertWithAttributes emit format
// markers at all, so any difference from yjs is a different item structure for
// the same operation, not just a different answer.
//
// It used to be a bare a == b, which panics at runtime when both values are
// maps or slices: reformatting a range carrying such an attribute, reading
// ToDelta across two such runs, or building an observer event for them crashed
// the process - and the event path runs on updates from peers.
//
// Scalars keep the semantics they had, including float64(1) != int(1) where JS
// has one number type; changing that would change which markers existing
// callers produce.
func attrValuesEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if sameObject(a, b) {
		return true
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if isObjectLike(va) && isObjectLike(vb) {
		return equalFlat(va, vb)
	}
	return strictEqual(a, b)
}

// isObjectLike reports whether v is what JS would call an object for the
// purposes of equalFlat: a string-keyed map, or a list.
func isObjectLike(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Map:
		return v.Type().Key().Kind() == reflect.String
	case reflect.Slice, reflect.Array:
		return true
	}
	return false
}

// equalFlat is lib0 object.equalFlat: the same number of entries, and every
// entry of a present in b and strictly equal. A list's entries are keyed by
// their decimal index, which is how a JS array exposes them.
func equalFlat(a, b reflect.Value) bool {
	if a.Len() != b.Len() {
		return false
	}
	if a.Kind() == reflect.Map {
		it := a.MapRange()
		for it.Next() {
			bv, ok := entry(b, it.Key().String())
			if !ok || !strictEqual(it.Value().Interface(), bv) {
				return false
			}
		}
		return true
	}
	for i := 0; i < a.Len(); i++ {
		bv, ok := entry(b, strconv.Itoa(i))
		if !ok || !strictEqual(a.Index(i).Interface(), bv) {
			return false
		}
	}
	return true
}

// entry looks up key in an object-like value, treating a list index as its
// canonical decimal string.
func entry(v reflect.Value, key string) (any, bool) {
	if v.Kind() == reflect.Map {
		mv := v.MapIndex(reflect.ValueOf(key).Convert(v.Type().Key()))
		if !mv.IsValid() {
			return nil, false
		}
		return mv.Interface(), true
	}
	i, err := strconv.Atoi(key)
	if err != nil || i < 0 || i >= v.Len() || strconv.Itoa(i) != key {
		return nil, false
	}
	return v.Index(i).Interface(), true
}

// sameObject is JS === between two containers: the same object, not an equal
// one. Nil containers of the same type are the same, as null is.
//
// A zero-length slice is never the same object as another. Go gives every
// empty slice the runtime's shared zerobase address, so pointer identity would
// call two separately decoded empty arrays one object - and merge runs yjs keeps
// apart ({x:[]} next to {x:[]}). A non-empty slice has a real backing array.
func sameObject(a, b any) bool {
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.Type() != vb.Type() {
		return false
	}
	switch va.Kind() {
	case reflect.Map:
		if va.IsNil() || vb.IsNil() {
			return va.IsNil() && vb.IsNil()
		}
		return va.Pointer() == vb.Pointer()
	case reflect.Slice:
		if va.IsNil() || vb.IsNil() {
			return va.IsNil() && vb.IsNil()
		}
		if va.Len() == 0 {
			return false
		}
		return va.Pointer() == vb.Pointer() && va.Len() == vb.Len()
	}
	return false
}

// strictEqual is JS === for an entry: primitives by value, containers by
// identity. It never panics, including on a comparable type whose dynamic
// contents are not, such as a struct holding a slice.
func strictEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.Type() != vb.Type() {
		return false
	}
	switch va.Kind() {
	case reflect.Map, reflect.Slice:
		return sameObject(a, b)
	}
	if !va.Comparable() {
		return false
	}
	return a == b
}

// attrsEqual reports whether two Attrs maps contain identical
// (key, value) sets. Order-independent.
func attrsEqual(a, b Attrs) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || !attrValuesEqual(va, vb) {
			return false
		}
	}
	return true
}
