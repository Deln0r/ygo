package gomobile

import (
	"encoding/json"
	"fmt"

	"github.com/Deln0r/ygo/internal/types"
)

// ApplyDelta applies a Quill-style delta to the text in one
// transaction — the write side of the rich-text round trip, symmetric
// with ObserveChanges. A native editor (Quill / ProseMirror / a custom
// view) produces a delta on user input and hands it straight here:
//
//	[{"retain":3},{"insert":"hi","attributes":{"bold":true}},{"delete":2}]
//
// An insert value is either a string (text) or a JSON object (an embed,
// e.g. an image). Indices and retain / delete counts are UTF-16 units,
// matching ObserveChanges and JS Yjs.
func (t *Text) ApplyDelta(deltaJSON []byte) error {
	ops, err := parseQuillDelta(deltaJSON)
	if err != nil {
		return err
	}
	txn := t.d.inner.WriteTxn()
	defer txn.Commit()
	return t.inner.ApplyDelta(txn, ops)
}

// Format applies attributes (e.g. {"bold":true}) to length UTF-16 units
// starting at index — the action a toolbar fires when the user bolds a
// selection. A nil-valued attribute ({"bold":null}) clears that key.
func (t *Text) Format(index, length int, attributesJSON []byte) error {
	if index < 0 || length < 0 {
		return fmt.Errorf("gomobile: negative index/length %d/%d", index, length)
	}
	attrs, err := parseAttrs(attributesJSON)
	if err != nil {
		return err
	}
	txn := t.d.inner.WriteTxn()
	defer txn.Commit()
	return t.inner.Format(txn, uint64(index), uint64(length), attrs)
}

// InsertWithAttributes inserts s at the UTF-16 index with formatting
// already applied (attributesJSON is an object like {"italic":true}).
func (t *Text) InsertWithAttributes(index int, s string, attributesJSON []byte) error {
	if index < 0 {
		return fmt.Errorf("gomobile: negative index %d", index)
	}
	attrs, err := parseAttrs(attributesJSON)
	if err != nil {
		return err
	}
	txn := t.d.inner.WriteTxn()
	defer txn.Commit()
	return t.inner.InsertWithAttributes(txn, uint64(index), s, attrs)
}

// InsertEmbed inserts a single embedded value (an image, mention, or
// other non-text node) at the UTF-16 index. embedJSON is any JSON value,
// typically an object: {"image":"https://..."}.
func (t *Text) InsertEmbed(index int, embedJSON []byte) error {
	if index < 0 {
		return fmt.Errorf("gomobile: negative index %d", index)
	}
	var v any
	if err := json.Unmarshal(embedJSON, &v); err != nil {
		return fmt.Errorf("gomobile: parse embed: %w", err)
	}
	txn := t.d.inner.WriteTxn()
	defer txn.Commit()
	return t.inner.InsertEmbed(txn, uint64(index), v)
}

// parseAttrs unmarshals a JSON object into the format-attribute map.
// Empty input means no attributes.
func parseAttrs(b []byte) (types.Attrs, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var attrs types.Attrs
	if err := json.Unmarshal(b, &attrs); err != nil {
		return nil, fmt.Errorf("gomobile: parse attributes: %w", err)
	}
	return attrs, nil
}

// parseQuillDelta converts a Quill-style JSON delta into the internal
// op slice ApplyDelta consumes.
func parseQuillDelta(b []byte) ([]types.DeltaOp, error) {
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("gomobile: parse delta: %w", err)
	}
	ops := make([]types.DeltaOp, 0, len(raw))
	for i, m := range raw {
		var op types.DeltaOp
		switch {
		case m["insert"] != nil:
			var s string
			if json.Unmarshal(m["insert"], &s) == nil {
				op.Insert = s
			} else {
				var embed any
				if err := json.Unmarshal(m["insert"], &embed); err != nil {
					return nil, fmt.Errorf("gomobile: delta op[%d] insert: %w", i, err)
				}
				op.Embed = embed
			}
		case m["retain"] != nil:
			n, err := parseCount(m["retain"])
			if err != nil {
				return nil, fmt.Errorf("gomobile: delta op[%d] retain: %w", i, err)
			}
			op.Retain = n
		case m["delete"] != nil:
			n, err := parseCount(m["delete"])
			if err != nil {
				return nil, fmt.Errorf("gomobile: delta op[%d] delete: %w", i, err)
			}
			op.Delete = n
		}
		if a := m["attributes"]; a != nil {
			var attrs types.Attrs
			if err := json.Unmarshal(a, &attrs); err != nil {
				return nil, fmt.Errorf("gomobile: delta op[%d] attributes: %w", i, err)
			}
			op.Attributes = attrs
		}
		ops = append(ops, op)
	}
	return ops, nil
}

// parseCount reads a non-negative integer from a JSON number.
func parseCount(raw json.RawMessage) (uint64, error) {
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("negative count %d", n)
	}
	return uint64(n), nil
}
