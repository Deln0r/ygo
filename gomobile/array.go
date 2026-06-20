package gomobile

import (
	"encoding/json"
	"fmt"

	"github.com/Deln0r/ygo/internal/types"
)

// Array is the bindable shared Array (ordered sequence) CRDT. Elements
// are scalar JSON values or nested Text / Map / Array types.
type Array struct {
	d         *Doc
	inner     *types.Array
	unobserve func()
}

// Array returns the shared array registered under name, creating the
// root type on first access.
func (d *Doc) Array(name string) *Array {
	return &Array{d: d, inner: types.NewArray(d.inner.Branch(name))}
}

// Length returns the number of elements.
func (a *Array) Length() int {
	rtxn := a.d.inner.ReadTxn()
	defer rtxn.Close()
	return int(a.inner.Len())
}

// PushJSON appends a scalar JSON value (string, number, bool, null, or
// a plain array / object) to the end of the array.
func (a *Array) PushJSON(valueJSON []byte) error {
	v, err := jsonToValue(valueJSON)
	if err != nil {
		return err
	}
	txn := a.d.inner.WriteTxn()
	defer txn.Commit()
	a.inner.Push(txn, v)
	return nil
}

// InsertJSON inserts a scalar JSON value at the given index.
func (a *Array) InsertJSON(index int, valueJSON []byte) error {
	if index < 0 {
		return fmt.Errorf("gomobile: negative index %d", index)
	}
	v, err := jsonToValue(valueJSON)
	if err != nil {
		return err
	}
	txn := a.d.inner.WriteTxn()
	defer txn.Commit()
	a.inner.Insert(txn, uint64(index), v)
	return nil
}

// GetJSON returns the element at index as JSON, or "null" when the
// index is out of range or holds a nested type (use GetMap / GetArray /
// GetText for those).
func (a *Array) GetJSON(index int) []byte {
	if index < 0 {
		return []byte("null")
	}
	rtxn := a.d.inner.ReadTxn()
	defer rtxn.Close()
	return valueToJSON(a.inner.Get(uint64(index)))
}

// DeleteAt removes length elements starting at index.
func (a *Array) DeleteAt(index, length int) error {
	if index < 0 || length < 0 {
		return fmt.Errorf("gomobile: negative index/length %d/%d", index, length)
	}
	txn := a.d.inner.WriteTxn()
	defer txn.Commit()
	a.inner.Delete(txn, uint64(index), uint64(length))
	return nil
}

// ToJSON renders the whole array as a JSON array. Nested types appear
// as null; use the typed accessors to descend into them.
func (a *Array) ToJSON() []byte {
	rtxn := a.d.inner.ReadTxn()
	defer rtxn.Close()
	out := make([]json.RawMessage, 0, a.inner.Len())
	a.inner.Range(func(_ uint64, v any) bool {
		out = append(out, json.RawMessage(valueToJSON(v)))
		return true
	})
	b, err := json.Marshal(out)
	if err != nil {
		return []byte("[]")
	}
	return b
}

// InsertText inserts a new nested Text at index and returns it.
func (a *Array) InsertText(index int) *Text {
	txn := a.d.inner.WriteTxn()
	defer txn.Commit()
	return &Text{d: a.d, inner: a.inner.InsertText(txn, uint64(index))}
}

// InsertMap inserts a new nested Map at index and returns it.
func (a *Array) InsertMap(index int) *Map {
	txn := a.d.inner.WriteTxn()
	defer txn.Commit()
	return &Map{d: a.d, inner: a.inner.InsertMap(txn, uint64(index))}
}

// InsertArray inserts a new nested Array at index and returns it.
func (a *Array) InsertArray(index int) *Array {
	txn := a.d.inner.WriteTxn()
	defer txn.Commit()
	return &Array{d: a.d, inner: a.inner.InsertArray(txn, uint64(index))}
}

// GetText returns the nested Text at index, or nil if the element is
// not a Text.
func (a *Array) GetText(index int) *Text {
	if index < 0 {
		return nil
	}
	rtxn := a.d.inner.ReadTxn()
	defer rtxn.Close()
	if t, ok := a.inner.Get(uint64(index)).(*types.Text); ok {
		return &Text{d: a.d, inner: t}
	}
	return nil
}

// GetMap returns the nested Map at index, or nil if not a Map.
func (a *Array) GetMap(index int) *Map {
	if index < 0 {
		return nil
	}
	rtxn := a.d.inner.ReadTxn()
	defer rtxn.Close()
	if m, ok := a.inner.Get(uint64(index)).(*types.Map); ok {
		return &Map{d: a.d, inner: m}
	}
	return nil
}

// GetArray returns the nested Array at index, or nil if not an Array.
func (a *Array) GetArray(index int) *Array {
	if index < 0 {
		return nil
	}
	rtxn := a.d.inner.ReadTxn()
	defer rtxn.Close()
	if arr, ok := a.inner.Get(uint64(index)).(*types.Array); ok {
		return &Array{d: a.d, inner: arr}
	}
	return nil
}
