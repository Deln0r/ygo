package types

import (
	"sort"
	"strings"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
)

// XmlFragment is a root container of XmlElement and XmlText
// children. Mirrors Y.XmlFragment — the top-level entry for
// collaborative XML/DOM documents (ProseMirror, Tiptap, BlockNote
// all use this shape). Children are accessed positionally.
//
// Per docs/yrs-port-notes/nested-types.md §1, the underlying
// Branch carries TypeRef = TypeRefXmlFragment; NewXmlFragment sets
// it on wrap so cross-client receivers that wrap the same root by
// name end up with consistent state.
type XmlFragment struct {
	branch *block.Branch
}

// NewXmlFragment wraps the given branch as an XmlFragment. Sets
// Branch.TypeRef = TypeRefXmlFragment.
func NewXmlFragment(branch *block.Branch) *XmlFragment {
	branch.TypeRef = block.TypeRefXmlFragment
	return &XmlFragment{branch: branch}
}

// Branch returns the underlying *block.Branch.
func (f *XmlFragment) Branch() *block.Branch { return f.branch }

// Length returns the number of live children.
func (f *XmlFragment) Length() uint64 { return f.branch.BlockLen }

// InsertXmlElement inserts a new XmlElement child with the given
// nodeName at position idx. Returns the wrapper bound to the new
// child branch.
func (f *XmlFragment) InsertXmlElement(txn *doc.TransactionMut, idx uint64, nodeName string) *XmlElement {
	inner := &block.Branch{
		TypeRef: block.TypeRefXmlElement,
		Name:    nodeName,
		Map:     map[string]*block.Item{},
	}
	insertXmlChild(txn, f.branch, idx, inner)
	return &XmlElement{branch: inner}
}

// InsertXmlText inserts a new XmlText child at position idx. Use
// the returned wrapper to populate the text content.
func (f *XmlFragment) InsertXmlText(txn *doc.TransactionMut, idx uint64) *XmlText {
	inner := &block.Branch{TypeRef: block.TypeRefXmlText}
	insertXmlChild(txn, f.branch, idx, inner)
	return &XmlText{Text: Text{branch: inner}}
}

// Get returns the child at idx as *XmlElement, *XmlText, or nil if
// idx is out of range or the underlying item is of an unsupported
// kind.
func (f *XmlFragment) Get(idx uint64) any {
	return getXmlChildAt(f.branch, idx)
}

// Delete removes length children starting at idx.
func (f *XmlFragment) Delete(txn *doc.TransactionMut, idx, length uint64) {
	asArray(f.branch).Delete(txn, idx, length)
}

// Range iterates over children in document order. fn receives the
// index and the wrapped child; return false to stop early.
func (f *XmlFragment) Range(fn func(idx uint64, child any) bool) {
	rangeXmlChildren(f.branch, fn)
}

// ToString renders the fragment as an XML-like string by
// concatenating child renderings. Attribute keys are sorted
// ascending for deterministic output (tests and snapshots compare
// stable strings).
func (f *XmlFragment) ToString() string {
	var sb strings.Builder
	f.Range(func(_ uint64, child any) bool {
		sb.WriteString(stringifyXmlChild(child))
		return true
	})
	return sb.String()
}

// XmlElement is an XML element — a tag name (NodeName), an
// attribute map (Map-like), and a list of children (Array-like) —
// all backed by the same Branch.
//
// Attributes are stored as map-keyed Items with ContentAny string
// values. Children are stored as positional Items. The two
// surfaces (map and positional) coexist on the same Branch because
// they use disjoint fields (branch.Map vs branch.Start).
type XmlElement struct {
	branch *block.Branch
}

// NewXmlElement wraps the given branch as an XmlElement. Sets
// TypeRef = TypeRefXmlElement if not already set; the caller is
// responsible for setting Branch.Name (which is the nodeName) if
// the branch is freshly constructed at the root level. For nested
// elements, use the InsertXmlElement family which sets Name
// automatically.
func NewXmlElement(branch *block.Branch) *XmlElement {
	if branch.TypeRef != block.TypeRefXmlElement {
		branch.TypeRef = block.TypeRefXmlElement
	}
	if branch.Map == nil {
		branch.Map = map[string]*block.Item{}
	}
	return &XmlElement{branch: branch}
}

// Branch returns the underlying *block.Branch.
func (e *XmlElement) Branch() *block.Branch { return e.branch }

// NodeName returns the element's tag name (the JS Yjs nodeName).
func (e *XmlElement) NodeName() string { return e.branch.Name }

// Length returns the number of live children.
func (e *XmlElement) Length() uint64 { return e.branch.BlockLen }

// GetAttribute returns the value of the named attribute. The
// second return reports whether the attribute is set.
func (e *XmlElement) GetAttribute(name string) (string, bool) {
	item, ok := e.branch.Map[name]
	if !ok || item.IsDeleted() {
		return "", false
	}
	if item.Content.Kind != block.KindAny || len(item.Content.Anys) == 0 {
		return "", false
	}
	s, ok := item.Content.Anys[0].(string)
	if !ok {
		return "", false
	}
	return s, true
}

// SetAttribute sets the named attribute to value, replacing any
// prior value. Reuses the Map.Set machinery — attributes are
// map-keyed entries on the same Branch.
func (e *XmlElement) SetAttribute(txn *doc.TransactionMut, name, value string) {
	asMap(e.branch).Set(txn, name, value)
}

// RemoveAttribute tombstones the named attribute. Calling on an
// unset attribute is a no-op.
func (e *XmlElement) RemoveAttribute(txn *doc.TransactionMut, name string) {
	asMap(e.branch).Delete(txn, name)
}

// Attributes returns a snapshot of all live attribute (name, value)
// pairs. Returned map is a copy; mutate freely.
func (e *XmlElement) Attributes() map[string]string {
	out := make(map[string]string, len(e.branch.Map))
	for k, v := range e.branch.Map {
		if v.IsDeleted() {
			continue
		}
		if v.Content.Kind != block.KindAny || len(v.Content.Anys) == 0 {
			continue
		}
		s, ok := v.Content.Anys[0].(string)
		if !ok {
			continue
		}
		out[k] = s
	}
	return out
}

// InsertXmlElement inserts a new XmlElement child with nodeName at
// position idx. Returns the wrapper.
func (e *XmlElement) InsertXmlElement(txn *doc.TransactionMut, idx uint64, nodeName string) *XmlElement {
	inner := &block.Branch{
		TypeRef: block.TypeRefXmlElement,
		Name:    nodeName,
		Map:     map[string]*block.Item{},
	}
	insertXmlChild(txn, e.branch, idx, inner)
	return &XmlElement{branch: inner}
}

// InsertXmlText inserts a new XmlText child at position idx.
func (e *XmlElement) InsertXmlText(txn *doc.TransactionMut, idx uint64) *XmlText {
	inner := &block.Branch{TypeRef: block.TypeRefXmlText}
	insertXmlChild(txn, e.branch, idx, inner)
	return &XmlText{Text: Text{branch: inner}}
}

// Get returns the child at idx, wrapped as *XmlElement or *XmlText.
func (e *XmlElement) Get(idx uint64) any {
	return getXmlChildAt(e.branch, idx)
}

// Delete removes length children starting at idx.
func (e *XmlElement) Delete(txn *doc.TransactionMut, idx, length uint64) {
	asArray(e.branch).Delete(txn, idx, length)
}

// Range iterates over children. fn returns false to stop early.
func (e *XmlElement) Range(fn func(idx uint64, child any) bool) {
	rangeXmlChildren(e.branch, fn)
}

// ToString renders the element as an XML-like string:
//
//	<nodeName attr1="value1" attr2="value2">children…</nodeName>
//
// or, for empty elements:
//
//	<nodeName attr1="value1"/>
//
// Attribute keys are sorted ascending for deterministic output.
// Attribute values are inlined without escaping — tests should
// avoid attribute values containing quotes; a real HTML encoder
// would XML-escape them.
func (e *XmlElement) ToString() string {
	var sb strings.Builder
	sb.WriteString("<")
	sb.WriteString(e.branch.Name)

	attrs := e.Attributes()
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sb.WriteString(" ")
		sb.WriteString(k)
		sb.WriteString(`="`)
		sb.WriteString(attrs[k])
		sb.WriteString(`"`)
	}

	if e.branch.BlockLen == 0 {
		sb.WriteString("/>")
		return sb.String()
	}
	sb.WriteString(">")
	e.Range(func(_ uint64, child any) bool {
		sb.WriteString(stringifyXmlChild(child))
		return true
	})
	sb.WriteString("</")
	sb.WriteString(e.branch.Name)
	sb.WriteString(">")
	return sb.String()
}

// XmlText is rich-text content within an XML tree. Embeds the
// regular Text wrapper, so every Insert / InsertWithAttributes /
// Format / InsertEmbed / Range / ToDelta method is available
// directly. The Branch's TypeRef is set to TypeRefXmlText so wire
// round-trips identify the child as text rather than a generic
// Y.Text.
type XmlText struct {
	Text
}

// NewXmlText wraps the given branch as an XmlText, setting
// TypeRef = TypeRefXmlText.
func NewXmlText(branch *block.Branch) *XmlText {
	branch.TypeRef = block.TypeRefXmlText
	return &XmlText{Text: Text{branch: branch}}
}

// ToString returns the underlying text — alias for Text.String()
// kept for parity with XmlElement/XmlFragment ToString.
func (x *XmlText) ToString() string { return x.String() }

// asArray and asMap are tiny adapters that let XML wrappers reuse
// the Array and Map machinery on their own Branch. The XML
// surfaces are subsets of those types; rather than duplicate
// findInsertPosition, SplitBlock, LWW chaining etc., the XML calls
// route through the shared implementations.
func asArray(b *block.Branch) *Array { return &Array{branch: b} }
func asMap(b *block.Branch) *Map     { return &Map{branch: b} }

// insertXmlChild builds a KindType Item whose Content.Branch is
// the supplied inner branch, integrates at position idx of parent.
func insertXmlChild(txn *doc.TransactionMut, parent *block.Branch, idx uint64, inner *block.Branch) {
	asArray(parent).insertNested(txn, idx, inner)
}

// getXmlChildAt returns the child at idx, converting Array's
// returned wrappers into the correct XML wrapper based on the
// inner Branch's TypeRef. extractValueAt already dispatches Map /
// Array / Text wrappers; for XML we re-wrap below.
func getXmlChildAt(branch *block.Branch, idx uint64) any {
	// Walk branch.Start to find the live countable item at idx,
	// then wrap based on its Content.Branch.TypeRef directly.
	// We bypass extractValueAt because the existing dispatch
	// returns *Map / *Array / *Text and we need the XML wrappers.
	var counted uint64
	for cur := branch.Start; cur != nil; cur = cur.Right {
		if cur.IsDeleted() || !cur.IsCountable() {
			continue
		}
		if counted+cur.Len > idx {
			return wrapXmlContent(cur.Content)
		}
		counted += cur.Len
	}
	return nil
}

// rangeXmlChildren walks branch.Start, emitting one (idx, wrapped)
// pair per live countable item. Stops when fn returns false.
func rangeXmlChildren(branch *block.Branch, fn func(idx uint64, child any) bool) {
	var idx uint64
	for cur := branch.Start; cur != nil; cur = cur.Right {
		if cur.IsDeleted() || !cur.IsCountable() {
			continue
		}
		// For multi-element items (rare in XML — InsertXmlElement
		// emits Len=1), expand into per-element iterations.
		for off := uint64(0); off < cur.Len; off++ {
			if !fn(idx, wrapXmlContent(cur.Content)) {
				return
			}
			idx++
		}
	}
}

// wrapXmlContent converts a Content into the appropriate XML
// wrapper based on Branch.TypeRef. Falls back to extractValueAt's
// dispatch for non-XML content kinds (e.g. a plain string embedded
// directly).
func wrapXmlContent(c block.Content) any {
	if c.Kind != block.KindType || c.Branch == nil {
		return extractValueAt(c, 0)
	}
	switch c.Branch.TypeRef {
	case block.TypeRefXmlElement:
		return &XmlElement{branch: c.Branch}
	case block.TypeRefXmlFragment:
		return &XmlFragment{branch: c.Branch}
	case block.TypeRefXmlText:
		return &XmlText{Text: Text{branch: c.Branch}}
	}
	return extractValueAt(c, 0)
}

// stringifyXmlChild formats one wrapped XML child for the ToString
// output. Unknown types render as the empty string.
func stringifyXmlChild(child any) string {
	switch c := child.(type) {
	case *XmlElement:
		return c.ToString()
	case *XmlText:
		return c.ToString()
	case *XmlFragment:
		return c.ToString()
	}
	return ""
}
