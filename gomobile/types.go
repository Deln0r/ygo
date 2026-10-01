package gomobile

import (
	"fmt"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/internal/undo"
)

// Text is the gomobile-bindable handle for a shared text type: the
// editable surface a native note-taking / editor UI binds to. All
// indices and lengths are UTF-16 code units, matching JS Yjs (and
// NSString / Java String length semantics, which is what makes the
// mobile bridge clean).
type Text struct {
	d         *Doc
	inner     *types.Text
	unobserve func()
}

// Text returns the shared text registered under name, creating the
// root type on first access.
func (d *Doc) Text(name string) *Text {
	return &Text{d: d, inner: types.NewText(d.inner.Branch(name))}
}

// InsertAt inserts s at the given UTF-16 index.
func (t *Text) InsertAt(index int, s string) error {
	if index < 0 {
		return fmt.Errorf("gomobile: negative index %d", index)
	}
	txn := t.d.writeTxn()
	defer txn.Commit()
	return t.inner.Insert(txn, uint64(index), s)
}

// DeleteAt removes length UTF-16 units starting at index.
func (t *Text) DeleteAt(index, length int) error {
	if index < 0 || length < 0 {
		return fmt.Errorf("gomobile: negative index/length %d/%d", index, length)
	}
	txn := t.d.writeTxn()
	defer txn.Commit()
	return t.inner.Delete(txn, uint64(index), uint64(length))
}

// String returns the current text content. Safe against a concurrent
// background Client: the read runs under a read transaction.
func (t *Text) String() string {
	rtxn := t.d.inner.ReadTxn()
	defer rtxn.Close()
	return t.inner.String()
}

// Length returns the text length in UTF-16 code units.
func (t *Text) Length() int {
	rtxn := t.d.inner.ReadTxn()
	defer rtxn.Close()
	return int(t.inner.Length())
}

// EncodeCursor anchors the given index as a relative position and
// returns its binary form (byte-compatible with
// Y.encodeRelativePosition), suitable for sharing with JS peers via
// awareness. assoc >= 0 sticks to the character after the position,
// assoc < 0 to the character before it.
func (t *Text) EncodeCursor(index, assoc int) ([]byte, error) {
	if index < 0 {
		return nil, fmt.Errorf("gomobile: negative index %d", index)
	}
	rtxn := t.d.inner.ReadTxn()
	rpos, err := types.CreateRelativePositionFromTypeIndex(t.inner, uint64(index), int64(assoc))
	rtxn.Close()
	if err != nil {
		return nil, err
	}
	return types.EncodeRelativePosition(rpos)
}

// ResolveCursor resolves an encoded relative position back to a
// current index in this text. Returns -1 when the anchor is unknown
// to this replica, garbage-collected, or belongs to another type.
func (t *Text) ResolveCursor(encoded []byte) int {
	rpos, err := types.DecodeRelativePosition(encoded)
	if err != nil {
		return -1
	}
	abs, ok := types.CreateAbsolutePositionFromRelativePosition(t.d.inner, rpos)
	if !ok || abs.Branch != t.inner.Branch() {
		return -1
	}
	return int(abs.Index)
}

// Map is the gomobile-bindable handle for a shared map restricted to
// string keys and string values (the subset that crosses the bridge
// without an `any` in sight). Other value kinds written by JS peers
// read back as empty strings; use the bytes-level Doc API for those.
type Map struct {
	d         *Doc
	inner     *types.Map
	unobserve func()
}

// Map returns the shared map registered under name, creating the
// root type on first access.
func (d *Doc) Map(name string) *Map {
	return &Map{d: d, inner: types.NewMap(d.inner.Branch(name))}
}

// SetString sets key to a string value.
func (m *Map) SetString(key, value string) {
	txn := m.d.writeTxn()
	defer txn.Commit()
	m.inner.Set(txn, key, value)
}

// GetString returns the string value under key, or "" when the key
// is absent or holds a non-string value. Safe against a concurrent
// background Client: the read runs under a read transaction.
func (m *Map) GetString(key string) string {
	rtxn := m.d.inner.ReadTxn()
	defer rtxn.Close()
	s, _ := m.inner.Get(key).(string)
	return s
}

// Has reports whether key holds a live value.
func (m *Map) Has(key string) bool {
	rtxn := m.d.inner.ReadTxn()
	defer rtxn.Close()
	return m.inner.Get(key) != nil
}

// DeleteKey removes key.
func (m *Map) DeleteKey(key string) {
	txn := m.d.writeTxn()
	defer txn.Commit()
	m.inner.Delete(txn, key)
}

// Len returns the number of live entries.
func (m *Map) Len() int {
	rtxn := m.d.inner.ReadTxn()
	defer rtxn.Close()
	return m.inner.Len()
}

// UndoManager is the gomobile-bindable scoped undo/redo handle.
// Construct via Doc.NewTextUndoManager / Doc.NewMapUndoManager.
type UndoManager struct {
	inner *undo.UndoManager
}

// NewTextUndoManager returns an UndoManager watching the shared text
// registered under name. It captures the transactions that carry this
// handle's origin (see WithOrigin); on the plain handle, those with no
// origin, which includes updates applied with plain ApplyUpdate.
func (d *Doc) NewTextUndoManager(name string) *UndoManager {
	t := types.NewText(d.inner.Branch(name))
	return &UndoManager{inner: undo.NewUndoManager(d.inner, []*block.Branch{t.Branch()}, d.undoOptions())}
}

// NewMapUndoManager returns an UndoManager watching the shared map
// registered under name, tracking this handle's origin as
// NewTextUndoManager does.
func (d *Doc) NewMapUndoManager(name string) *UndoManager {
	m := types.NewMap(d.inner.Branch(name))
	return &UndoManager{inner: undo.NewUndoManager(d.inner, []*block.Branch{m.Branch()}, d.undoOptions())}
}

// undoOptions tracks the handle's origin; the zero Options track nil.
func (d *Doc) undoOptions() undo.Options {
	if d.origin == "" {
		return undo.Options{}
	}
	return undo.Options{TrackedOrigins: map[any]struct{}{d.origin: {}}}
}

// AddTrackedOrigin makes the manager also capture transactions with this
// origin: edits through a WithOrigin handle, or updates applied with
// ApplyUpdateWithOrigin. "" stands for no origin.
func (u *UndoManager) AddTrackedOrigin(origin string) {
	u.inner.AddTrackedOrigin(originValue(origin))
}

// RemoveTrackedOrigin stops capturing transactions with this origin. ""
// stands for no origin: removing it leaves plain edits and plain
// ApplyUpdate calls out of the history.
func (u *UndoManager) RemoveTrackedOrigin(origin string) {
	u.inner.RemoveTrackedOrigin(originValue(origin))
}

// Undo reverts the most recent captured local change. Returns false
// when there is nothing to undo.
func (u *UndoManager) Undo() bool { return u.inner.Undo() }

// Redo re-applies the most recently undone change. Returns false when
// there is nothing to redo.
func (u *UndoManager) Redo() bool { return u.inner.Redo() }

// CanUndo reports whether the undo stack is non-empty.
func (u *UndoManager) CanUndo() bool { return u.inner.CanUndo() }

// CanRedo reports whether the redo stack is non-empty.
func (u *UndoManager) CanRedo() bool { return u.inner.CanRedo() }

// StopCapturing forces the next edit to start a new undo group.
func (u *UndoManager) StopCapturing() { u.inner.StopCapturing() }

// Close detaches the UndoManager from the document.
func (u *UndoManager) Close() { u.inner.Close() }
