package gomobile

import (
	"encoding/json"

	"github.com/Deln0r/ygo/internal/types"
)

// SetJSON sets key to a scalar JSON value (string, number, bool, null,
// or a plain array / object) — the typed-value counterpart to
// SetString. Syncs byte-compatibly with JS Yjs.
func (m *Map) SetJSON(key string, valueJSON []byte) error {
	v, err := jsonToValue(valueJSON)
	if err != nil {
		return err
	}
	txn := m.d.inner.WriteTxn()
	defer txn.Commit()
	m.inner.Set(txn, key, v)
	return nil
}

// GetJSON returns the value at key as JSON, or "null" when the key is
// absent or holds a nested type (use GetMap / GetArray / GetText).
func (m *Map) GetJSON(key string) []byte {
	rtxn := m.d.inner.ReadTxn()
	defer rtxn.Close()
	return valueToJSON(m.inner.Get(key))
}

// KeysJSON returns the map's live keys as a JSON array of strings.
func (m *Map) KeysJSON() []byte {
	rtxn := m.d.inner.ReadTxn()
	defer rtxn.Close()
	keys := make([]string, 0, m.inner.Len())
	m.inner.Range(func(k string, _ any) bool {
		keys = append(keys, k)
		return true
	})
	b, err := json.Marshal(keys)
	if err != nil {
		return []byte("[]")
	}
	return b
}

// Clear removes every entry.
func (m *Map) Clear() {
	txn := m.d.inner.WriteTxn()
	defer txn.Commit()
	m.inner.Clear(txn)
}

// SetMap sets key to a new nested Map and returns it.
func (m *Map) SetMap(key string) *Map {
	txn := m.d.inner.WriteTxn()
	defer txn.Commit()
	return &Map{d: m.d, inner: m.inner.SetMap(txn, key)}
}

// SetArray sets key to a new nested Array and returns it.
func (m *Map) SetArray(key string) *Array {
	txn := m.d.inner.WriteTxn()
	defer txn.Commit()
	return &Array{d: m.d, inner: m.inner.SetArray(txn, key)}
}

// SetText sets key to a new nested Text and returns it.
func (m *Map) SetText(key string) *Text {
	txn := m.d.inner.WriteTxn()
	defer txn.Commit()
	return &Text{d: m.d, inner: m.inner.SetText(txn, key)}
}

// GetMap returns the nested Map at key, or nil if the value is not a Map.
func (m *Map) GetMap(key string) *Map {
	rtxn := m.d.inner.ReadTxn()
	defer rtxn.Close()
	if nested, ok := m.inner.Get(key).(*types.Map); ok {
		return &Map{d: m.d, inner: nested}
	}
	return nil
}

// GetArray returns the nested Array at key, or nil if not an Array.
func (m *Map) GetArray(key string) *Array {
	rtxn := m.d.inner.ReadTxn()
	defer rtxn.Close()
	if nested, ok := m.inner.Get(key).(*types.Array); ok {
		return &Array{d: m.d, inner: nested}
	}
	return nil
}

// GetText returns the nested Text at key, or nil if not a Text.
func (m *Map) GetText(key string) *Text {
	rtxn := m.d.inner.ReadTxn()
	defer rtxn.Close()
	if nested, ok := m.inner.Get(key).(*types.Text); ok {
		return &Text{d: m.d, inner: nested}
	}
	return nil
}
