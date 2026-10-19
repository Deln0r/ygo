package types

import (
	"errors"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/lib0"
	"github.com/Deln0r/ygo/internal/store"
	"github.com/Deln0r/ygo/internal/utf16"
)

// RelativePosition is a position anchored to the Yjs document model
// rather than to a numeric index, so it stays attached to the same
// logical character as concurrent edits land before or after it. The
// canonical use is collaborative cursors and selections.
//
// Exactly one of Item, TName, Type identifies the anchor:
//   - Item: the position sits on a concrete character, identified by
//     the ID of the item (plus offset baked into the clock).
//   - TName: the position sits at the end of a ROOT type with this name.
//   - Type: the position sits at the end of a NESTED type, identified
//     by the ID of the item whose content holds the type.
//
// Assoc is the side the position sticks to: >= 0 associates with the
// character after the position (default), < 0 with the character
// before it.
//
// Mirrors yjs RelativePosition (utils/RelativePosition.js); the binary
// form produced by Encode is byte-compatible with
// Y.encodeRelativePosition.
//
// TName is a pointer because the empty string is a legal root-type
// name in yjs (doc.get(”)); nil means "not a root anchor".
type RelativePosition struct {
	Type  *block.ID
	TName *string
	Item  *block.ID
	Assoc int64
}

// AbsolutePosition is a resolved RelativePosition: the branch of the
// shared type the position lives in and the current numeric index
// within it (UTF-16 code units for Text, elements for Array).
type AbsolutePosition struct {
	Branch *block.Branch
	Index  uint64
	Assoc  int64
}

// BranchHolder is satisfied by every shared-type wrapper (Map, Array,
// Text, XmlFragment, XmlElement); it exposes the underlying Branch.
type BranchHolder interface {
	Branch() *block.Branch
}

// ErrDetachedType reports a wrapper whose branch is neither a root
// type nor attached to a live item, so no stable anchor exists for it.
var ErrDetachedType = errors.New("ygo: relative position: type is not anchored in a document")

// CreateRelativePositionFromTypeIndex anchors the numeric position
// index within the shared type t as a RelativePosition. index counts
// UTF-16 code units for Text and elements for Array. assoc chooses the
// side the position sticks to (>= 0 right, < 0 left).
//
// Mirrors yjs createRelativePositionFromTypeIndex. The caller must not
// hold an open write transaction mutating t concurrently.
func CreateRelativePositionFromTypeIndex(t BranchHolder, index uint64, assoc int64) (RelativePosition, error) {
	br := t.Branch()
	if br == nil {
		return RelativePosition{}, ErrDetachedType
	}
	rpos, err := anchorOnly(br, assoc)
	if err != nil {
		return RelativePosition{}, err
	}

	it := br.Start
	if assoc < 0 {
		// Associated to the left character (or the type start): the
		// anchor is the character BEFORE the index.
		if index == 0 {
			return rpos, nil
		}
		index--
	}
	for it != nil {
		if !it.IsDeleted() && it.IsCountable() {
			if it.Len > index {
				// The position lands inside this item; bake the offset
				// into the clock.
				rpos.Item = &block.ID{Client: it.ID.Client, Clock: it.ID.Clock + index}
				return rpos, nil
			}
			index -= it.Len
		}
		if it.Right == nil && assoc < 0 {
			// Left-associated position past the end: anchor on the last
			// available character.
			last := it.LastID()
			rpos.Item = &last
			return rpos, nil
		}
		it = it.Right
	}
	return rpos, nil
}

// anchorOnly builds the end-of-type RelativePosition for a branch (no
// Item): TName for root types, the holding item's ID for nested types.
func anchorOnly(br *block.Branch, assoc int64) (RelativePosition, error) {
	switch {
	case br.Item != nil:
		id := br.Item.ID
		return RelativePosition{Type: &id, Assoc: assoc}, nil
	case br.Name != "":
		// Branch.Name's zero value doubles as the "not a root" sentinel,
		// so a root type actually NAMED "" cannot be anchored from the
		// creation side (decode/resolve of such anchors works fine).
		name := br.Name
		return RelativePosition{TName: &name, Assoc: assoc}, nil
	default:
		return RelativePosition{}, ErrDetachedType
	}
}

// ErrEmptyAnchor reports a RelativePosition with no anchor set (none
// of Item, TName, Type). Such a value cannot come from Create or
// Decode; it indicates a caller-constructed zero value.
var ErrEmptyAnchor = errors.New("ygo: relative position: empty anchor")

// EncodeRelativePosition serialises rpos to the binary form of
// Y.encodeRelativePosition: a varuint tag (0 item / 1 tname / 2 type),
// the payload, then assoc as a signed varint. Returns ErrEmptyAnchor
// for a zero-value rpos (yjs throws unexpectedCase here).
func EncodeRelativePosition(rpos RelativePosition) ([]byte, error) {
	buf := make([]byte, 0, 16)
	switch {
	case rpos.Item != nil:
		buf = lib0.WriteVarUint(buf, 0)
		buf = lib0.WriteVarUint(buf, rpos.Item.Client)
		buf = lib0.WriteVarUint(buf, rpos.Item.Clock)
	case rpos.TName != nil:
		buf = lib0.WriteVarUint(buf, 1)
		// A root name ygo creates is valid UTF-8 (the typed constructors fix
		// it); one a caller set by hand is fixed the same way, since lib0
		// rejects anything else.
		buf = lib0.WriteVarString(buf, utf16.ValidString(*rpos.TName))
	case rpos.Type != nil:
		buf = lib0.WriteVarUint(buf, 2)
		buf = lib0.WriteVarUint(buf, rpos.Type.Client)
		buf = lib0.WriteVarUint(buf, rpos.Type.Clock)
	default:
		return nil, ErrEmptyAnchor
	}
	buf = lib0.WriteVarInt(buf, rpos.Assoc)
	return buf, nil
}

// DecodeRelativePosition parses the binary form produced by
// EncodeRelativePosition / Y.encodeRelativePosition. A missing assoc
// (older producers) decodes as 0, matching yjs.
func DecodeRelativePosition(buf []byte) (RelativePosition, error) {
	tag, n, err := lib0.ReadVarUint(buf)
	if err != nil {
		return RelativePosition{}, err
	}
	buf = buf[n:]
	var rpos RelativePosition
	switch tag {
	case 0:
		client, n, err := lib0.ReadVarUint(buf)
		if err != nil {
			return RelativePosition{}, err
		}
		buf = buf[n:]
		clock, n, err := lib0.ReadVarUint(buf)
		if err != nil {
			return RelativePosition{}, err
		}
		buf = buf[n:]
		rpos.Item = &block.ID{Client: client, Clock: clock}
	case 1:
		s, n, err := lib0.ReadVarString(buf)
		if err != nil {
			return RelativePosition{}, err
		}
		buf = buf[n:]
		rpos.TName = &s
	case 2:
		client, n, err := lib0.ReadVarUint(buf)
		if err != nil {
			return RelativePosition{}, err
		}
		buf = buf[n:]
		clock, n, err := lib0.ReadVarUint(buf)
		if err != nil {
			return RelativePosition{}, err
		}
		buf = buf[n:]
		rpos.Type = &block.ID{Client: client, Clock: clock}
	default:
		// Deliberate divergence from yjs: readRelativePosition has no
		// default case and yields an all-null rpos on an unknown tag; we
		// surface the malformed input as an error instead.
		return RelativePosition{}, errors.New("ygo: relative position: unknown anchor tag")
	}
	if len(buf) > 0 {
		assoc, _, err := lib0.ReadVarInt(buf)
		if err != nil {
			return RelativePosition{}, err
		}
		rpos.Assoc = assoc
	}
	return rpos, nil
}

// CreateAbsolutePositionFromRelativePosition resolves rpos against the
// current state of d. Returns ok=false when the anchor refers to state
// this replica has not seen, or to a garbage-collected range.
//
// Follows undone deletions (the yjs default): an anchor on a character
// that was deleted and restored by undo resolves to the restored copy.
//
// Mirrors yjs createAbsolutePositionFromRelativePosition.
func CreateAbsolutePositionFromRelativePosition(d *doc.Doc, rpos RelativePosition) (AbsolutePosition, bool) {
	// Root lookup may lazily create the branch (yjs doc.get semantics),
	// which takes the write lock; do it before opening the read txn.
	var rootBr *block.Branch
	if rpos.Item == nil && rpos.TName != nil {
		rootBr = d.Branch(*rpos.TName)
	}

	txn := d.ReadTxn()
	defer txn.Close()
	bs := txn.Store()

	if rpos.Item != nil {
		if bs.GetClock(rpos.Item.Client) <= rpos.Item.Clock {
			return AbsolutePosition{}, false // anchor not seen yet
		}
		right, diff := followRedoneRead(bs, *rpos.Item)
		if right == nil {
			return AbsolutePosition{}, false // garbage collected
		}
		if right.Parent.Kind != block.ParentBranch || right.Parent.Branch == nil {
			return AbsolutePosition{}, false
		}
		br := right.Parent.Branch
		var index uint64
		if br.Item == nil || !br.Item.IsDeleted() {
			if !right.IsDeleted() && right.IsCountable() {
				index = diff
				if rpos.Assoc < 0 {
					index++
				}
			}
			for n := right.Left; n != nil; n = n.Left {
				if !n.IsDeleted() && n.IsCountable() {
					index += n.Len
				}
			}
		}
		return AbsolutePosition{Branch: br, Index: index, Assoc: rpos.Assoc}, true
	}

	var br *block.Branch
	switch {
	case rpos.TName != nil:
		br = rootBr
	case rpos.Type != nil:
		if bs.GetClock(rpos.Type.Client) <= rpos.Type.Clock {
			return AbsolutePosition{}, false
		}
		holder, _ := followRedoneRead(bs, *rpos.Type)
		if holder == nil || holder.Content.Kind != block.KindType || holder.Content.Branch == nil {
			return AbsolutePosition{}, false
		}
		br = holder.Content.Branch
	default:
		return AbsolutePosition{}, false
	}
	var index uint64
	if rpos.Assoc >= 0 {
		// End-of-type anchor tracks the end as the type grows.
		index = br.BlockLen
	}
	return AbsolutePosition{Branch: br, Index: index, Assoc: rpos.Assoc}, true
}

// followRedoneRead walks the Redone chain from id to its latest live
// representative without mutating the store, carrying the in-item
// offset (diff) across hops. Returns (nil, 0) when the chain dead-ends
// in a garbage-collected range.
//
// Read-only mirror of yjs followRedone (utils/StructStore.js); the
// undo package has a write-path variant that materialises splits, which
// resolution must not do.
func followRedoneRead(bs *store.BlockStore, id block.ID) (*block.Item, uint64) {
	next := id
	for {
		it := bs.GetItem(next)
		if it == nil {
			return nil, 0
		}
		diff := next.Clock - it.ID.Clock
		if it.Redone == nil {
			return it, diff
		}
		next = block.ID{Client: it.Redone.Client, Clock: it.Redone.Clock + diff}
	}
}
