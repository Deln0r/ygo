package encoding

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/Deln0r/ygo/internal/anyval"
	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/lib0"
)

// lib0 Any TLV type tags. Tag bytes are written as a single uint8
// followed by the variant-specific payload. Constants reproduced
// verbatim from lib0/encoding.js writeAny (and confirmed against
// docs/yrs-port-notes/update-v1.md content table).
const (
	AnyTagBinary    uint8 = 116
	AnyTagArray     uint8 = 117
	AnyTagObject    uint8 = 118
	AnyTagString    uint8 = 119
	AnyTagTrue      uint8 = 120
	AnyTagFalse     uint8 = 121
	AnyTagBigInt    uint8 = 122
	AnyTagFloat64   uint8 = 123
	AnyTagFloat32   uint8 = 124
	AnyTagInteger   uint8 = 125
	AnyTagNull      uint8 = 126
	AnyTagUndefined uint8 = 127
)

// ErrUnsupportedAnyTag is returned when DecodeAny encounters a type
// tag that cannot be mapped to a Go value.
var ErrUnsupportedAnyTag = errors.New("encoding: unsupported Any tag")

// EncodeAny appends the lib0 Any TLV encoding of v to buf.
//
// Supported variants:
//   - nil → null (tag 126)
//   - bool → true/false (tags 120/121)
//   - string → string (tag 119, varstring payload)
//   - int / int32 / int64 with |v| <= 2^31-1 → integer (tag 125, varint
//     payload), lib0's BITS31 test
//   - any other integer → float32 (tag 124) when float32 holds it
//     exactly, else float64 (tag 123), as lib0 classifies the same number
//   - float32 → float32 (tag 124, 4-byte BE payload)
//   - float64 → float32 (tag 124) when it is not an integer and float32
//     holds it exactly (0.5, 2.5, ±Inf), else float64 (tag 123, 8-byte
//     BE payload). lib0 also writes an integral double such as 3.0 as a
//     varint; ygo keeps it a float64 so it decodes back as a float64.
//     That integral case is the one number class whose bytes differ.
//   - negative zero (float64 or float32) → 7d40, the varint with the sign
//     bit and no magnitude that lib0 writes for -0
//   - block.Undefined → undefined (tag 127)
//   - block.BigInt → bigint (tag 122, 8 bytes big-endian signed, lib0's
//     writeBigInt64)
//   - []byte → binary (tag 116, varuint length + bytes)
//   - []any → array (tag 117, varuint count + each element recursively)
//   - map[string]any → object (tag 118, varuint count + each (varstring key + Any value)).
//     A Go map has no order, so the keys go out as JavaScript would
//     enumerate them after decoding: array-index keys first in ascending
//     order, then the others sorted (anyval.SortedJSOrder). Deterministic
//     across runs.
//
// Mirrors lib0/encoding.js writeAny.
func EncodeAny(buf []byte, v any) []byte { return encodeAnyOrdered(buf, v, nil) }

// encodeAnyOrdered is EncodeAny writing the objects inside v in the key
// order o keeps for them (decodeAnyOrdered), the fallback order elsewhere.
func encodeAnyOrdered(buf []byte, v any, o *block.KeyOrder) []byte {
	switch x := v.(type) {
	case nil:
		return append(buf, AnyTagNull)
	case block.Undefined:
		return append(buf, AnyTagUndefined)
	case block.BigInt:
		buf = append(buf, AnyTagBigInt)
		return binary.BigEndian.AppendUint64(buf, uint64(x))
	case bool:
		if x {
			return append(buf, AnyTagTrue)
		}
		return append(buf, AnyTagFalse)
	case string:
		buf = append(buf, AnyTagString)
		return lib0.WriteVarString(buf, x)
	case int:
		return encodeIntAny(buf, int64(x))
	case int32:
		return encodeIntAny(buf, int64(x))
	case int64:
		return encodeIntAny(buf, x)
	case float32:
		if x == 0 && math.Signbit(float64(x)) {
			return append(buf, AnyTagInteger, negativeZeroVarInt)
		}
		buf = append(buf, AnyTagFloat32)
		return lib0.WriteFloat32(buf, x)
	case float64:
		return encodeFloatAny(buf, x)
	case []byte:
		buf = append(buf, AnyTagBinary)
		return lib0.WriteVarUint8Array(buf, x)
	case []any:
		buf = append(buf, AnyTagArray)
		buf = lib0.WriteVarUint(buf, uint64(len(x)))
		for i, el := range x {
			buf = encodeAnyOrdered(buf, el, anyval.SubOrder(o, i))
		}
		return buf
	case map[string]any:
		keys, subs := anyval.ObjectKeys(x, o)
		buf = append(buf, AnyTagObject)
		buf = lib0.WriteVarUint(buf, uint64(len(keys)))
		for i, k := range keys {
			buf = lib0.WriteVarString(buf, k)
			var sub *block.KeyOrder
			if i < len(subs) {
				sub = subs[i]
			}
			buf = encodeAnyOrdered(buf, x[k], sub)
		}
		return buf
	case *anyval.Object:
		// JSON text parsed for writing (a json.RawMessage embed or
		// format value in V2): its own key order.
		buf = append(buf, AnyTagObject)
		buf = lib0.WriteVarUint(buf, uint64(len(x.Keys)))
		for _, k := range x.Keys {
			buf = lib0.WriteVarString(buf, anyval.SanitizeJSString(k))
			buf = EncodeAny(buf, x.Vals[k])
		}
		return buf
	default:
		panic(fmt.Sprintf("encoding.EncodeAny: unsupported value type %T (supported: nil, Undefined, BigInt, bool, string, int*, float32, float64, []byte, []any, map[string]any)", v))
	}
}

// EncodeDocOpts appends a subdocument's options as the lib0 Any object
// yjs writes for them. yjs builds that object in a fixed order, gc,
// autoLoad, meta (ContentDoc's constructor), where EncodeAny sorts keys,
// so with two or more options the bytes would differ. Keys yjs never
// writes follow, sorted.
func EncodeDocOpts(buf []byte, opts map[string]any) []byte {
	keys := make([]string, 0, len(opts))
	for _, k := range []string{"gc", "autoLoad", "meta"} {
		if _, ok := opts[k]; ok {
			keys = append(keys, k)
		}
	}
	var rest []string
	for k := range opts {
		if k != "gc" && k != "autoLoad" && k != "meta" {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	keys = append(keys, rest...)
	buf = append(buf, AnyTagObject)
	buf = lib0.WriteVarUint(buf, uint64(len(keys)))
	for _, k := range keys {
		buf = lib0.WriteVarString(buf, k)
		buf = EncodeAny(buf, opts[k])
	}
	return buf
}

// encodeIntAny writes an integer the way lib0 writeAny writes the same
// JS number: a varint when |x| <= 2^31-1 (lib0's BITS31 test, so -2^31
// is not one), otherwise a float through encodeFloatAny.
func encodeIntAny(buf []byte, x int64) []byte {
	if x >= -0x7FFFFFFF && x <= 0x7FFFFFFF {
		buf = append(buf, AnyTagInteger)
		return lib0.WriteVarInt(buf, x)
	}
	return encodeFloatAny(buf, float64(x))
}

// negativeZeroVarInt is lib0 writeVarInt(-0): the sign bit (0x40) with no
// magnitude and no continuation. lib0 writes -0 as an integer, since
// Number.isInteger(-0) is true.
const negativeZeroVarInt = 0x40

// encodeFloatAny writes f as float32 (tag 124) when float32 holds it
// exactly, as lib0's isFloat32 test does, else as float64 (tag 123).
// DecodeAny widens either back to the same float64. An integral f
// within int31 stays a float64 (see EncodeAny). A NaN keeps its bits,
// as lib0 keeps those of a NaN it decoded.
func encodeFloatAny(buf []byte, f float64) []byte {
	if f == 0 && math.Signbit(f) {
		return append(buf, AnyTagInteger, negativeZeroVarInt)
	}
	integral := f == math.Trunc(f) && math.Abs(f) <= 0x7FFFFFFF
	if f32 := float32(f); !integral && float64(f32) == f {
		buf = append(buf, AnyTagFloat32)
		return lib0.WriteFloat32(buf, f32)
	}
	buf = append(buf, AnyTagFloat64)
	return lib0.WriteFloat64(buf, f)
}

// DecodeAny reads one lib0-Any-encoded value from buf and returns the
// value plus the unconsumed tail.
//
// Tag → Go type mapping:
//
//	binary (116)    → []byte (slice is copied; never aliases buf)
//	array (117)     → []any (recursive)
//	object (118)    → map[string]any (recursive)
//	string (119)    → string
//	true (120)      → true
//	false (121)     → false
//	bigint (122)    → block.BigInt (8 bytes BE signed, lib0's writeBigInt64)
//	float64 (123)   → float64
//	float32 (124)   → float64 (widened; downstream callers rarely care
//	                   about the source-width distinction)
//	integer (125)   → int64, or float64 negative zero for a zero with the
//	                   sign bit, which lib0 reads as -0
//	null (126)      → nil
//	undefined (127) → block.Undefined
//
// Unknown tags return ErrUnsupportedAnyTag.
func DecodeAny(buf []byte) (any, []byte, error) {
	return decodeAny(buf, strictStrings)
}

// decodeAny reads one Any value, without the key order of its objects.
func decodeAny(buf []byte, strs stringMode) (any, []byte, error) {
	v, _, tail, err := decodeAnyOrdered(buf, strs, false)
	return v, tail, err
}

// decodeAnyOrdered reads one Any value and, with wantOrder, the key order
// of the objects inside it as a JavaScript reader of the same bytes holds
// them (lib0 readAny assigns obj[key] = value: a key read twice keeps its
// first place and its last value, and array-index keys enumerate first).
// The order is nil where the fallback order is right (anyval.KeyOrder).
func decodeAnyOrdered(buf []byte, strs stringMode, wantOrder bool) (any, *block.KeyOrder, []byte, error) {
	if len(buf) < 1 {
		return nil, nil, buf, lib0.ErrTruncated
	}
	tag := buf[0]
	buf = buf[1:]
	switch tag {
	case AnyTagNull:
		return nil, nil, buf, nil
	case AnyTagUndefined:
		return block.Undefined{}, nil, buf, nil
	case AnyTagTrue:
		return true, nil, buf, nil
	case AnyTagFalse:
		return false, nil, buf, nil
	case AnyTagString:
		s, n, err := strs.readString(buf)
		if err != nil {
			return nil, nil, buf, err
		}
		return s, nil, buf[n:], nil
	case AnyTagInteger:
		v, n, err := lib0.ReadVarInt(buf)
		if err != nil {
			return nil, nil, buf, err
		}
		// The sign bit is in the first byte whatever the length, so this
		// also catches a non-canonical zero, which lib0 reads as -0 too.
		if v == 0 && buf[0]&negativeZeroVarInt != 0 {
			return math.Copysign(0, -1), nil, buf[n:], nil
		}
		return v, nil, buf[n:], nil
	case AnyTagFloat64:
		v, n, err := lib0.ReadFloat64(buf)
		if err != nil {
			return nil, nil, buf, err
		}
		return v, nil, buf[n:], nil
	case AnyTagFloat32:
		v, n, err := lib0.ReadFloat32(buf)
		if err != nil {
			return nil, nil, buf, err
		}
		return float64(v), nil, buf[n:], nil
	case AnyTagBigInt:
		if len(buf) < 8 {
			return nil, nil, buf, lib0.ErrTruncated
		}
		v := block.BigInt(int64(binary.BigEndian.Uint64(buf[:8])))
		return v, nil, buf[8:], nil
	case AnyTagBinary:
		b, n, err := lib0.ReadVarUint8Array(buf)
		if err != nil {
			return nil, nil, buf, err
		}
		out := make([]byte, len(b))
		copy(out, b)
		return out, nil, buf[n:], nil
	case AnyTagArray:
		count, n, err := lib0.ReadVarUint(buf)
		if err != nil {
			return nil, nil, buf, err
		}
		buf = buf[n:]
		if err := checkDecodeCount(count, len(buf)); err != nil {
			return nil, nil, buf, err
		}
		arr := make([]any, count)
		var subs []*block.KeyOrder
		for i := uint64(0); i < count; i++ {
			el, sub, tail, err := decodeAnyOrdered(buf, strs, wantOrder)
			if err != nil {
				return nil, nil, buf, fmt.Errorf("decode Any array element %d: %w", i, err)
			}
			arr[i] = el
			if sub != nil {
				if subs == nil {
					subs = make([]*block.KeyOrder, count)
				}
				subs[i] = sub
			}
			buf = tail
		}
		if subs == nil {
			return arr, nil, buf, nil
		}
		return arr, &block.KeyOrder{Sub: subs}, buf, nil
	case AnyTagObject:
		count, n, err := lib0.ReadVarUint(buf)
		if err != nil {
			return nil, nil, buf, err
		}
		buf = buf[n:]
		if err := checkDecodeCount(count, len(buf)); err != nil {
			return nil, nil, buf, err
		}
		m := make(map[string]any, count)
		if !wantOrder {
			for i := uint64(0); i < count; i++ {
				key, kn, err := strs.readString(buf)
				if err != nil {
					return nil, nil, buf, fmt.Errorf("decode Any object key %d: %w", i, err)
				}
				buf = buf[kn:]
				val, _, tail, err := decodeAnyOrdered(buf, strs, false)
				if err != nil {
					return nil, nil, buf, fmt.Errorf("decode Any object value for key %q: %w", key, err)
				}
				m[key] = val
				buf = tail
			}
			return m, nil, buf, nil
		}
		// The keys in the order they first came, and the orders of their
		// values; most objects need neither past this function. sorted
		// stays true while the keys other than array indices come in
		// ascending order, which is the fallback order once JavaScript
		// puts the indices first.
		var small [8]string
		keys := small[:0]
		var subs keyedOrders
		sorted, prev, havePrev := true, "", false
		for i := uint64(0); i < count; i++ {
			key, kn, err := strs.readString(buf)
			if err != nil {
				return nil, nil, buf, fmt.Errorf("decode Any object key %d: %w", i, err)
			}
			buf = buf[kn:]
			val, sub, tail, err := decodeAnyOrdered(buf, strs, true)
			if err != nil {
				return nil, nil, buf, fmt.Errorf("decode Any object value for key %q: %w", key, err)
			}
			before := len(m)
			m[key] = val
			if len(m) > before { // first time: its place is here
				keys = append(keys, key)
				if sorted && !anyval.IsArrayIndexKey(key) {
					if havePrev && key <= prev {
						sorted = false
					}
					prev, havePrev = key, true
				}
			}
			subs.set(key, sub) // the last value's order, or none
			buf = tail
		}
		if sorted && subs.empty() {
			return m, nil, buf, nil
		}
		return m, objectOrder(keys, &subs), buf, nil
	default:
		return nil, nil, buf, fmt.Errorf("%w: tag=%d", ErrUnsupportedAnyTag, tag)
	}
}

// objectOrder returns the order of an object whose keys came in the order
// keys, with subs the orders of their values, normalized to JavaScript
// enumeration order; nil when the fallback order is right for it and
// everything inside. keys is not kept.
func objectOrder(keys []string, subs *keyedOrders) *block.KeyOrder {
	if subs.empty() && anyval.IsFallbackOrder(anyval.JSOrder(keys)) {
		return nil
	}
	js := anyval.JSOrder(keys)
	var list []*block.KeyOrder
	if !subs.empty() {
		var small [8]*block.KeyOrder
		list = small[:0]
		for _, k := range js {
			list = append(list, subs.get(k))
		}
	}
	return internOrder(js, list)
}

// keyedOrders holds the orders of an object's values while it is decoded:
// a few pairs inline, a map past keyedOrdersInline of them, so a decode
// stays linear in the number of keys whatever repeats.
type keyedOrders struct {
	pairs []keyedOrder
	m     map[string]*block.KeyOrder
}

type keyedOrder struct {
	key   string
	order *block.KeyOrder
}

const keyedOrdersInline = 16

func (s *keyedOrders) empty() bool { return len(s.pairs) == 0 && len(s.m) == 0 }

// set records o as the order of key's value, replacing an earlier one; a
// nil o removes it.
func (s *keyedOrders) set(key string, o *block.KeyOrder) {
	if s.m != nil {
		if o == nil {
			delete(s.m, key)
		} else {
			s.m[key] = o
		}
		return
	}
	for i := range s.pairs {
		if s.pairs[i].key == key {
			if o == nil {
				s.pairs = append(s.pairs[:i], s.pairs[i+1:]...)
			} else {
				s.pairs[i].order = o
			}
			return
		}
	}
	if o == nil {
		return
	}
	if len(s.pairs) < keyedOrdersInline {
		s.pairs = append(s.pairs, keyedOrder{key, o})
		return
	}
	s.m = make(map[string]*block.KeyOrder, 2*keyedOrdersInline)
	for _, p := range s.pairs {
		s.m[p.key] = p.order
	}
	s.pairs = nil
	s.m[key] = o
}

func (s *keyedOrders) get(key string) *block.KeyOrder {
	if s.m != nil {
		return s.m[key]
	}
	for _, p := range s.pairs {
		if p.key == key {
			return p.order
		}
	}
	return nil
}
