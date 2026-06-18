package gomobile

import (
	"encoding/json"
	"fmt"

	"github.com/Deln0r/ygo/internal/types"
)

// jsonToValue decodes one JSON value into the any the CRDT stores —
// string, number (float64), bool, null, or a plain array / object.
// These are exactly the values lib0 Any round-trips, so they sync
// byte-compatibly with JS Yjs.
func jsonToValue(b []byte) (any, error) {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("gomobile: parse value: %w", err)
	}
	return v, nil
}

// valueToJSON renders a stored scalar / plain value as JSON. Nested
// CRDT types (Map / Array / Text) marshal to null — read those with the
// dedicated GetMap / GetArray / GetText accessors instead.
func valueToJSON(v any) []byte {
	switch v.(type) {
	case *types.Map, *types.Array, *types.Text:
		return []byte("null")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("null")
	}
	return b
}
