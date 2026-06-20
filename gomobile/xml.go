package gomobile

import (
	"encoding/json"

	"github.com/Deln0r/ygo/internal/types"
)

// XmlFragment is the bindable root of an XML document tree — the shape
// a ProseMirror / Tiptap model maps onto. It holds an ordered list of
// XmlElement and XmlText children.
type XmlFragment struct {
	d     *Doc
	inner *types.XmlFragment
}

// XmlElement is a named node with string attributes and ordered
// children (elements or text).
type XmlElement struct {
	d     *Doc
	inner *types.XmlElement
}

// XmlText is a rich-text leaf in an XML tree. Use Text() for the full
// formatting surface (ApplyDelta / Format / InsertAt).
type XmlText struct {
	d     *Doc
	inner *types.XmlText
}

// XmlFragment returns the shared XML fragment registered under name,
// creating the root type on first access.
func (d *Doc) XmlFragment(name string) *XmlFragment {
	return &XmlFragment{d: d, inner: types.NewXmlFragment(d.inner.Branch(name))}
}

// Length returns the number of children.
func (f *XmlFragment) Length() int {
	rtxn := f.d.inner.ReadTxn()
	defer rtxn.Close()
	return int(f.inner.Length())
}

// InsertElement inserts a new <nodeName> element at index and returns it.
func (f *XmlFragment) InsertElement(index int, nodeName string) *XmlElement {
	txn := f.d.inner.WriteTxn()
	defer txn.Commit()
	return &XmlElement{d: f.d, inner: f.inner.InsertXmlElement(txn, uint64(index), nodeName)}
}

// InsertText inserts a new text node at index and returns it.
func (f *XmlFragment) InsertText(index int) *XmlText {
	txn := f.d.inner.WriteTxn()
	defer txn.Commit()
	return &XmlText{d: f.d, inner: f.inner.InsertXmlText(txn, uint64(index))}
}

// GetElement returns the child element at index, or nil if it is not
// an element.
func (f *XmlFragment) GetElement(index int) *XmlElement {
	if index < 0 {
		return nil
	}
	rtxn := f.d.inner.ReadTxn()
	defer rtxn.Close()
	if e, ok := f.inner.Get(uint64(index)).(*types.XmlElement); ok {
		return &XmlElement{d: f.d, inner: e}
	}
	return nil
}

// GetText returns the child text node at index, or nil if not text.
func (f *XmlFragment) GetText(index int) *XmlText {
	if index < 0 {
		return nil
	}
	rtxn := f.d.inner.ReadTxn()
	defer rtxn.Close()
	if x, ok := f.inner.Get(uint64(index)).(*types.XmlText); ok {
		return &XmlText{d: f.d, inner: x}
	}
	return nil
}

// DeleteAt removes length children starting at index.
func (f *XmlFragment) DeleteAt(index, length int) {
	if index < 0 || length < 0 {
		return
	}
	txn := f.d.inner.WriteTxn()
	defer txn.Commit()
	f.inner.Delete(txn, uint64(index), uint64(length))
}

// ToString renders the fragment and its subtree as HTML-like markup.
func (f *XmlFragment) ToString() string {
	rtxn := f.d.inner.ReadTxn()
	defer rtxn.Close()
	return f.inner.ToString()
}

// NodeName returns the element's tag name.
func (e *XmlElement) NodeName() string { return e.inner.NodeName() }

// SetAttribute sets a string attribute.
func (e *XmlElement) SetAttribute(name, value string) {
	txn := e.d.inner.WriteTxn()
	defer txn.Commit()
	e.inner.SetAttribute(txn, name, value)
}

// GetAttribute returns the attribute value, or "" when absent (use
// HasAttribute to distinguish an empty value from an absent one).
func (e *XmlElement) GetAttribute(name string) string {
	rtxn := e.d.inner.ReadTxn()
	defer rtxn.Close()
	v, _ := e.inner.GetAttribute(name)
	return v
}

// HasAttribute reports whether name is set.
func (e *XmlElement) HasAttribute(name string) bool {
	rtxn := e.d.inner.ReadTxn()
	defer rtxn.Close()
	_, ok := e.inner.GetAttribute(name)
	return ok
}

// RemoveAttribute deletes the attribute.
func (e *XmlElement) RemoveAttribute(name string) {
	txn := e.d.inner.WriteTxn()
	defer txn.Commit()
	e.inner.RemoveAttribute(txn, name)
}

// AttributesJSON returns all attributes as a JSON object of strings.
func (e *XmlElement) AttributesJSON() []byte {
	rtxn := e.d.inner.ReadTxn()
	defer rtxn.Close()
	b, err := json.Marshal(e.inner.Attributes())
	if err != nil {
		return []byte("{}")
	}
	return b
}

// Length returns the number of children.
func (e *XmlElement) Length() int {
	rtxn := e.d.inner.ReadTxn()
	defer rtxn.Close()
	return int(e.inner.Length())
}

// InsertElement inserts a child <nodeName> element at index.
func (e *XmlElement) InsertElement(index int, nodeName string) *XmlElement {
	txn := e.d.inner.WriteTxn()
	defer txn.Commit()
	return &XmlElement{d: e.d, inner: e.inner.InsertXmlElement(txn, uint64(index), nodeName)}
}

// InsertText inserts a child text node at index.
func (e *XmlElement) InsertText(index int) *XmlText {
	txn := e.d.inner.WriteTxn()
	defer txn.Commit()
	return &XmlText{d: e.d, inner: e.inner.InsertXmlText(txn, uint64(index))}
}

// GetElement returns the child element at index, or nil if not an element.
func (e *XmlElement) GetElement(index int) *XmlElement {
	if index < 0 {
		return nil
	}
	rtxn := e.d.inner.ReadTxn()
	defer rtxn.Close()
	if c, ok := e.inner.Get(uint64(index)).(*types.XmlElement); ok {
		return &XmlElement{d: e.d, inner: c}
	}
	return nil
}

// GetText returns the child text node at index, or nil if not text.
func (e *XmlElement) GetText(index int) *XmlText {
	if index < 0 {
		return nil
	}
	rtxn := e.d.inner.ReadTxn()
	defer rtxn.Close()
	if x, ok := e.inner.Get(uint64(index)).(*types.XmlText); ok {
		return &XmlText{d: e.d, inner: x}
	}
	return nil
}

// DeleteAt removes length children starting at index.
func (e *XmlElement) DeleteAt(index, length int) {
	if index < 0 || length < 0 {
		return
	}
	txn := e.d.inner.WriteTxn()
	defer txn.Commit()
	e.inner.Delete(txn, uint64(index), uint64(length))
}

// ToString renders the element and its subtree as HTML-like markup.
func (e *XmlElement) ToString() string {
	rtxn := e.d.inner.ReadTxn()
	defer rtxn.Close()
	return e.inner.ToString()
}

// String returns the text content.
func (x *XmlText) String() string {
	rtxn := x.d.inner.ReadTxn()
	defer rtxn.Close()
	return x.inner.ToString()
}

// Text returns the text node as a Text handle, exposing the full
// rich-text surface (ApplyDelta / Format / InsertAt / ObserveChanges).
func (x *XmlText) Text() *Text {
	return &Text{d: x.d, inner: &x.inner.Text}
}
