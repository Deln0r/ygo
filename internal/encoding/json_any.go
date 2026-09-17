package encoding

import (
	"encoding/json"
	"math"

	"github.com/Deln0r/ygo/internal/block"
)

// Format values and embeds are "JSON" content, and the two wire formats carry
// them differently. yjs UpdateEncoderV1.writeJSON writes JSON.stringify(v) as a
// varstring; UpdateEncoderV2.writeJSON writes the value as a lib0 Any in the
// rest stream (yjs/src/utils/UpdateEncoder.js:116 and :286, and the matching
// readJSON at UpdateDecoder.js:114 and :265).
//
// ygo's V2 path used the V1 layout in both directions, so it round-tripped with
// itself and with no one else. Measured against yjs 13.6.32, a bold insert, a
// format of an existing range and an embed each made Y.applyUpdateV2 throw, and
// the same content encoded by yjs failed to decode here. Not every legacy byte
// sequence fails loudly, though: a JSON length prefix can happen to be a valid
// Any tag, so some old updates decode into a different value instead. Nothing
// caught any of it because no cross-language fixture, V1 or V2, carried a single
// attribute.

// jsonValueForAny turns a format value or embed into what lib0 writeAny would
// be handed for the same payload.
//
// Two kinds of input arrive here and they need different treatment.
//
// A value that is already an Any tree - nil, bool, string, []byte, the Go
// integer and float types, []any, map[string]any - is encoded as it stands,
// with only its numbers reclassified. That is the path for everything decoded
// from a peer, and it must not go near encoding/json: JSON has no NaN, no
// Infinity and no binary, so a yjs embed like {n: Infinity, keep: 1} relayed
// through ygo came out as null, whole, and a Uint8Array came out as a base64
// string. Measured both before this split.
//
// Anything else - a struct, a typed map or slice, a custom MarshalJSON - goes
// through encoding/json first, as the V1 path always has, because the Any
// encoder panics on those types. A value that cannot be marshalled degrades to
// null, matching writeJSON. That route is also where JSON's limits apply: a
// custom marshaler emitting a literal like 1e1000 marshals but cannot be read
// back as a float64, and also degrades to null.
func jsonValueForAny(v block.Any) block.Any {
	if isAnyTree(v, 0) {
		return classifyNumbers(v)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return classifyNumbers(out)
}

// maxAnyTreeDepth bounds the walk over a caller's value. Nothing decoded from
// the wire is anywhere near this deep; a value that is, or that refers back to
// itself, is handed to encoding/json instead, which detects the cycle and
// degrades it to null exactly as the V1 path does. Without the bound a
// self-referencing map recursed until the stack ran out - a fatal crash, not a
// recoverable panic - where it used to encode as null.
const maxAnyTreeDepth = 1000

// isAnyTree reports whether EncodeAny can take v as it is, once its numbers are
// classified.
func isAnyTree(v any, depth int) bool {
	if depth > maxAnyTreeDepth {
		return false
	}
	switch x := v.(type) {
	case nil, bool, string, []byte, int, int32, int64, float32, float64:
		return true
	case []any:
		for _, el := range x {
			if !isAnyTree(el, depth+1) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, el := range x {
			if !isAnyTree(el, depth+1) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// classifyNumbers returns a copy of v with every number classified the way lib0
// writeAny classifies a JS number, in the same order (lib0/encoding.js,
// `case 'number'`): an integer whose absolute value is at most 0x7FFFFFFF is a
// varint, a value exactly representable as a float32 is a float32, anything
// else is a float64. Every Go numeric type is first taken as a float64, which is
// what a JS number is, so an int64 beyond 2^31 is classified exactly as lib0
// would classify the same number rather than by the Any encoder's integer rule.
//
// It builds new containers instead of rewriting in place: the input is the
// document's own stored content, and encoding a document must not change the
// Go types a later reader of that document sees.
//
// One deliberate departure: negative zero stays a float64. lib0 writes it as a
// varint with the sign flag set (7d40), which the Any codec here cannot express;
// as an int64 it would become an unsigned 0 and lose its sign on the way to
// every peer. As a float64 the value survives, at the cost of byte identity for
// that one number. yjs's own V1 path does not preserve it either.
func classifyNumbers(v any) any {
	switch x := v.(type) {
	case int:
		return classifyNumber(float64(x))
	case int32:
		return classifyNumber(float64(x))
	case int64:
		return classifyNumber(float64(x))
	case float32:
		return classifyNumber(float64(x))
	case float64:
		return classifyNumber(x)
	case []any:
		if x == nil {
			return nil
		}
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = classifyNumbers(el)
		}
		return out
	case map[string]any:
		if x == nil {
			return nil
		}
		out := make(map[string]any, len(x))
		for k, el := range x {
			out[k] = classifyNumbers(el)
		}
		return out
	case []byte:
		// A nil container is JSON null, which is what V1 has always sent
		// for it - and null in a format attribute means "remove". An empty
		// container would instead SET the attribute to an empty value, so
		// the same operation would give V1 and V2 receivers different
		// documents. Measured before this case existed.
		if x == nil {
			return nil
		}
		return x
	default:
		return v
	}
}

func classifyNumber(f float64) any {
	if f == 0 && math.Signbit(f) {
		return f
	}
	if f == math.Trunc(f) && math.Abs(f) <= 0x7FFFFFFF {
		return int64(f)
	}
	if f32 := float32(f); float64(f32) == f {
		return f32
	}
	return f
}

// jsonValueFromAny maps a lib0 Any read back from V2 onto the Go types the V1
// path produces for the same content, so a caller reading a format attribute
// gets the same type whichever wire format carried it. V1 decodes JSON, where
// every number is a float64; DecodeAny returns an int64 for a varint.
//
// Values JSON cannot represent keep their Any types - []byte stays []byte, and a
// NaN or Infinity stays a float - because there is no V1 equivalent to match.
// The value is fresh from the decoder and shared with nothing, so it is
// rewritten in place.
func jsonValueFromAny(v block.Any) block.Any {
	switch x := v.(type) {
	case int64:
		return float64(x)
	case int:
		return float64(x)
	case int32:
		return float64(x)
	case []any:
		for i := range x {
			x[i] = jsonValueFromAny(x[i])
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = jsonValueFromAny(x[k])
		}
		return x
	default:
		return v
	}
}
