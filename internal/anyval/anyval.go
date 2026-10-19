// Package anyval checks and fixes the plain values ygo stores in items: the
// value kinds the Any encoder writes, and values written as JSON.
package anyval

import (
	"encoding/json"
	"sort"
	"unicode/utf8"

	"github.com/Deln0r/ygo/internal/utf16"
)

// Valid returns v with every string and object key that is not valid
// UTF-8 fixed by utf16.ValidString, for the value kinds EncodeAny writes
// (nil, bool, numbers, string, []byte, []any, map[string]any). A value that
// needs no change is returned as it is, with no allocation; otherwise only
// the containers on the way to a change are copied, and the caller's value
// is never modified. Two keys of one object that become equal once fixed
// keep the value of the key that sorts last. Below MaxTreeDepth the walk
// stops and leaves the rest as it is: such values, and cyclic ones, are
// outside what the API promises.
func Valid(v any) any {
	if !needsFix(v, 0) {
		return v
	}
	return fixAny(v, 0)
}

// ValidJSON returns v made valid for content that is written as JSON
// (embed and format values). The decision covers the whole value, as the
// encoders decide at encode (encoding.jsonValueForAny): a plain tree all
// the way down gets Valid and keeps the encoding it has in V1 and V2.
// Anything else is marshaled once, here, its bytes fixed by
// utf16.ValidString, and stored as that JSON, a json.RawMessage, which both
// encoders already take as the JSON it holds. So every encoding of the item
// writes the same bytes, whatever a marshaler would return on a later call;
// the JSON text is kept as it is, numbers and escaped surrogates included;
// and two such values with the same JSON compare equal. A value
// encoding/json cannot marshal (a cycle) becomes nil, which both formats
// already write for it.
func ValidJSON(v any) any {
	if IsTree(v, 0) {
		return Valid(v)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return json.RawMessage(utf16.ValidString(string(raw)))
}

// NeedsJSONFix reports whether ValidJSON would change v: a value that is not
// a plain tree is always marshaled.
func NeedsJSONFix(v any) bool { return !IsTree(v, 0) || needsFix(v, 0) }

// NeedsFix reports whether Valid would change v.
func NeedsFix(v any) bool { return needsFix(v, 0) }

func needsFix(v any, depth int) bool {
	if depth > MaxTreeDepth {
		return false
	}
	switch x := v.(type) {
	case string:
		return !utf8.ValidString(x)
	case []any:
		for _, el := range x {
			if needsFix(el, depth+1) {
				return true
			}
		}
	case map[string]any:
		for k, el := range x {
			if !utf8.ValidString(k) || needsFix(el, depth+1) {
				return true
			}
		}
	}
	return false
}

// fixAny rebuilds v, which needsFix reported; children that need no change
// are kept as they are, so a valid nested container keeps its identity.
func fixAny(v any, depth int) any {
	if depth > MaxTreeDepth || !needsFix(v, depth) {
		return v
	}
	switch x := v.(type) {
	case string:
		return utf16.ValidString(x)
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = fixAny(el, depth+1)
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(x))
		for _, k := range keys {
			out[utf16.ValidString(k)] = fixAny(x[k], depth+1)
		}
		return out
	}
	return v
}

// MaxTreeDepth bounds the walk over a caller's value. Nothing decoded from
// the wire is anywhere near this deep; a value that is, or that refers back to
// itself, is handed to encoding/json instead, which detects the cycle and
// degrades it to null exactly as the V1 path does. Without the bound a
// self-referencing map recursed until the stack ran out - a fatal crash, not a
// recoverable panic - where it used to encode as null.
const MaxTreeDepth = 1000

// IsTree reports whether v is a plain tree the Any encoder can take as it
// is, once its numbers are classified: nil, bool, string, []byte, int,
// int32, int64, float32, float64, and []any / map[string]any of those, no
// deeper than MaxTreeDepth.
func IsTree(v any, depth int) bool {
	if depth > MaxTreeDepth {
		return false
	}
	switch x := v.(type) {
	case nil, bool, string, []byte, int, int32, int64, float32, float64:
		return true
	case []any:
		for _, el := range x {
			if !IsTree(el, depth+1) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, el := range x {
			if !IsTree(el, depth+1) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
