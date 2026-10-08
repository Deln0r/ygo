package block

import (
	"fmt"

	"github.com/Deln0r/ygo/internal/utf16"
)

// ContentKind discriminates the variants of Content. The numeric values
// are the Yjs wire ref numbers (BLOCK_ITEM_*_REF_NUMBER from
// yrs/src/block.rs:28-72) and must not change.
//
// KindGC (0) and KindSkip (10) are not Content variants — they are
// parallel BlockCell kinds — but the constants are reserved here so the
// nibble values stay consistent with the wire.
type ContentKind uint8

const (
	KindGC      ContentKind = 0
	KindDeleted ContentKind = 1
	KindJSON    ContentKind = 2
	KindBinary  ContentKind = 3
	KindString  ContentKind = 4
	KindEmbed   ContentKind = 5
	KindFormat  ContentKind = 6
	KindType    ContentKind = 7
	KindAny     ContentKind = 8
	KindDoc     ContentKind = 9
	KindSkip    ContentKind = 10
	KindMove    ContentKind = 11
)

// Content is the payload of an Item. A single struct (not an interface)
// so it embeds in Item without an extra allocation and reads cleanly
// in tests without a type-switch.
//
// Each ContentKind uses only a subset of fields. The unused fields on
// any given variant must be left at their zero value; tests assert this.
//
// See docs/yrs-port-notes/block.md "ItemContent tagged union" for the
// rationale and the variant-to-field mapping.
type Content struct {
	Kind ContentKind

	// Variant-specific payloads. Set on the indicated kinds; zero
	// elsewhere.

	Anys      []Any    // KindAny, KindEmbed (1 elem), KindFormat (value, 1 elem)
	JSONStrs  []string // KindJSON (legacy stringified JSON, splittable)
	Bytes     []byte   // KindBinary
	Str       string   // KindString — UTF-8 input; internally normalized to UTF-16 for wire
	FormatKey string   // KindFormat (key)

	DeletedLen uint64 // KindDeleted, KindSkip — element count only

	Branch    *Branch // KindType
	Move      *Move   // KindMove
	Doc       *Doc    // KindDoc — child doc
	ParentDoc *Doc    // KindDoc — parent doc reference, set at integrate time

	// KindDoc wire payload: a subdocument is encoded as its guid
	// (a stable string identity) followed by an options object. The
	// nested document's *content* syncs as a separate update stream
	// keyed by the guid; the parent only stores this reference.
	// Mirrors yjs ContentDoc.write (writeString(guid) + writeAny(opts)).
	DocGuid string
	DocOpts map[string]any
}

// RefNumber returns the Yjs wire ref number for the content's kind.
// This is the low nibble of the Item info byte.
func (c Content) RefNumber() uint8 {
	return uint8(c.Kind) & InfoContentMask
}

// IsCountable reports whether this content's elements contribute to
// the parent's user-facing length.
//
// yrs/src/block.rs ItemContent::is_countable: false for Deleted,
// Format, Move; true for everything else.
func (c Content) IsCountable() bool {
	switch c.Kind {
	case KindDeleted, KindFormat, KindMove, KindSkip, KindGC:
		return false
	default:
		return true
	}
}

// Len returns the number of elements (clock units) this content spans
// under the given offset semantics.
//
// For splittable variants (Any, JSON, String) this is the slice length
// in the appropriate unit; for single-element variants (Binary, Embed,
// Type, Doc) it is 1; for Deleted/Skip it is the stored length.
//
// String length is always UTF-16 code units regardless of kind, per
// yrs/src/block.rs comment on Item::new and the wire-format invariant.
// (See docs/yjs-architecture-notes.md §19 and DESIGN.md.)
//
// Return type is uint64 (yrs uses u32). We accept wire values up to
// 2^64-1; clock and length values produced by yrs and JS Yjs always
// fit in u32 in practice, so this is strictly a defensive widening.
func (c Content) Len(_ OffsetKind) uint64 {
	switch c.Kind {
	case KindAny:
		return uint64(len(c.Anys))
	case KindJSON:
		return uint64(len(c.JSONStrs))
	case KindString:
		// UTF-16 code unit count, matching JS Yjs's Item.Len for
		// String content (yrs/src/block.rs:1307 sets Item::new len
		// via content.len(OffsetKind::Utf16); the wire format
		// expects UTF-16 throughout). See
		// docs/yrs-port-notes/types-text.md gotcha 1.
		return utf16.Length(c.Str)
	case KindBinary, KindEmbed, KindType, KindDoc:
		return 1
	case KindDeleted, KindSkip:
		return c.DeletedLen
	case KindFormat, KindMove:
		return 1
	case KindGC:
		// GC is a parallel BlockCell kind; Len is meaningless on a
		// Content with KindGC and callers should not query it. Return
		// 0 to fail noisily downstream rather than silently behave.
		return 0
	}
	return 0
}

// Copy returns a value copy of the content suitable for re-insertion
// under a fresh Item ID. Used by the UndoManager's redoItem path to
// resurrect a deleted item: the restoration carries the same payload
// but a new identity. Mirrors yjs ContentX.copy().
//
// Slice-backed payloads (Anys, JSONStrs, Bytes) are cloned so the new
// item does not share mutable backing with the original. Scalar fields
// copy by value with the struct.
//
// Limitation: KindType (nested Branch), KindMove, and KindDoc carry
// pointer payloads whose deep copy is non-trivial (a nested type has
// its own item graph). The first UndoManager cut does not restore
// deletions of those kinds; Copy shallow-copies the pointer and the
// caller (redoItem) refuses to resurrect such items. Tracked in
// docs/undo-manager-design.md.
func (c Content) Copy() Content {
	out := c
	if c.Anys != nil {
		out.Anys = append([]Any(nil), c.Anys...)
	}
	if c.JSONStrs != nil {
		out.JSONStrs = append([]string(nil), c.JSONStrs...)
	}
	if c.Bytes != nil {
		out.Bytes = append([]byte(nil), c.Bytes...)
	}
	if c.DocOpts != nil {
		m := make(map[string]any, len(c.DocOpts))
		for k, v := range c.DocOpts {
			m[k] = v
		}
		out.DocOpts = m
	}
	if c.Kind == KindType && c.Branch != nil {
		// A new, empty shared type of the same kind (and tag, for an XML
		// element), as yjs ContentType.copy does with type._copy(). Its
		// content comes back item by item, restored into it.
		out.Branch = &Branch{TypeRef: c.Branch.TypeRef, Name: c.Branch.Name, Map: map[string]*Item{}}
	}
	return out
}

// CopyableForUndo reports whether Copy produces a faithful, fully
// independent restoration for this content kind. False for Move and
// Doc, which the UndoManager does not restore.
func (c Content) CopyableForUndo() bool {
	switch c.Kind {
	case KindMove, KindDoc:
		return false
	default:
		return true
	}
}

// Split cuts the content at offset and returns the right half, mutating
// the receiver to hold the left half. Returns an error if the content
// kind is not splittable or offset is out of range.
//
// Splittable kinds: String (currently byte offsets — UTF-16 awareness
// arrives with the Text shared type, see tech-debt.md), Any, JSON,
// Deleted. All other kinds are single-element or parallel cell kinds
// and reject Split.
//
// See yrs/src/block.rs ItemContent::splice.
func (c *Content) Split(offset uint64) (Content, error) {
	switch c.Kind {
	case KindString:
		return c.splitString(offset)
	case KindAny:
		return c.splitAny(offset)
	case KindJSON:
		return c.splitJSON(offset)
	case KindDeleted:
		return c.splitDeleted(offset)
	default:
		return Content{}, fmt.Errorf("block: content kind %d is not splittable", c.Kind)
	}
}

func (c *Content) splitString(offset uint64) (Content, error) {
	// offset is in UTF-16 code units (matches JS Yjs / yrs
	// SplittableString::block_offset). utf16.SplitAt does the
	// UTF-8 byte boundary translation and replaces straddled
	// surrogate pairs with U+FFFD per JS Yjs behaviour (yrs has
	// the replacement commented out at block.rs:1940-1948 — we
	// follow JS Yjs not yrs; see types-text.md gotcha 3).
	totalU16 := utf16.Length(c.Str)
	if offset == 0 || offset >= totalU16 {
		return Content{}, fmt.Errorf("block: split offset %d out of range for string utf16-length %d", offset, totalU16)
	}
	left, right := utf16.SplitAt(c.Str, offset)
	c.Str = left
	return Content{Kind: KindString, Str: right}, nil
}

func (c *Content) splitAny(offset uint64) (Content, error) {
	if offset == 0 || offset >= uint64(len(c.Anys)) {
		return Content{}, fmt.Errorf("block: split offset %d out of range for any-slice length %d", offset, len(c.Anys))
	}
	right := Content{Kind: KindAny, Anys: c.Anys[offset:]}
	c.Anys = c.Anys[:offset]
	return right, nil
}

func (c *Content) splitJSON(offset uint64) (Content, error) {
	if offset == 0 || offset >= uint64(len(c.JSONStrs)) {
		return Content{}, fmt.Errorf("block: split offset %d out of range for json-slice length %d", offset, len(c.JSONStrs))
	}
	right := Content{Kind: KindJSON, JSONStrs: c.JSONStrs[offset:]}
	c.JSONStrs = c.JSONStrs[:offset]
	return right, nil
}

func (c *Content) splitDeleted(offset uint64) (Content, error) {
	if offset == 0 || offset >= c.DeletedLen {
		return Content{}, fmt.Errorf("block: split offset %d out of range for deleted length %d", offset, c.DeletedLen)
	}
	right := Content{Kind: KindDeleted, DeletedLen: c.DeletedLen - offset}
	c.DeletedLen = offset
	return right, nil
}

// TrySquash extends the receiver to absorb other's payload, returning
// true on success. Squashable kinds: Any (slice append), Deleted
// (length sum), JSON (slice append), String (concat). Non-matching
// kinds and other variants return false without mutation.
//
// Mirrors yrs/src/block.rs:1969-1993 ItemContent::try_squash.
func (c *Content) TrySquash(other *Content) bool {
	if c.Kind != other.Kind {
		return false
	}
	switch c.Kind {
	case KindAny:
		c.Anys = append(c.Anys, other.Anys...)
		return true
	case KindDeleted:
		c.DeletedLen += other.DeletedLen
		return true
	case KindJSON:
		c.JSONStrs = append(c.JSONStrs, other.JSONStrs...)
		return true
	case KindString:
		c.Str += other.Str
		return true
	default:
		return false
	}
}
