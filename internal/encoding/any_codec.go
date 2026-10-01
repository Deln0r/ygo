package encoding

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"

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
//   - []byte → binary (tag 116, varuint length + bytes)
//   - []any → array (tag 117, varuint count + each element recursively)
//   - map[string]any → object (tag 118, varuint count + each (varstring key + Any value)).
//     Keys are sorted alphabetically so the wire bytes are deterministic
//     across runs — Go map iteration order is randomized, which would
//     otherwise break the fixture-determinism guarantee.
//
// Not yet supported (panics):
//   - math/big BigInt encoding (decode side returns int64 from the
//     8-byte BE payload, matching lib0 writeBigInt64; encoding from
//     Go side is deferred until an adopter actually needs it)
//
// Mirrors lib0/encoding.js writeAny.
func EncodeAny(buf []byte, v any) []byte {
	switch x := v.(type) {
	case nil:
		return append(buf, AnyTagNull)
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
		for _, el := range x {
			buf = EncodeAny(buf, el)
		}
		return buf
	case map[string]any:
		buf = append(buf, AnyTagObject)
		buf = lib0.WriteVarUint(buf, uint64(len(x)))
		// Deterministic key order — JS Object.keys preserves
		// insertion order but Go map iteration is randomized;
		// alphabetical sort gives reproducible bytes across runs.
		// Adopters who need insertion-order semantics should use
		// the wire bytes as opaque blobs, not byte-equality compare.
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			buf = lib0.WriteVarString(buf, k)
			buf = EncodeAny(buf, x[k])
		}
		return buf
	default:
		panic(fmt.Sprintf("encoding.EncodeAny: unsupported value type %T (supported: nil, bool, string, int*, float32, float64, []byte, []any, map[string]any)", v))
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

// encodeFloatAny writes f as float32 (tag 124) when float32 holds it
// exactly, as lib0's isFloat32 test does, else as float64 (tag 123).
// DecodeAny widens either back to the same float64. An integral f
// within int31 stays a float64 (see EncodeAny). A NaN keeps its bits,
// as lib0 keeps those of a NaN it decoded.
func encodeFloatAny(buf []byte, f float64) []byte {
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
//	bigint (122)    → int64 (8-byte BE; loses precision above int64 range,
//	                   matching lib0's writeBigInt64 wire format)
//	float64 (123)   → float64
//	float32 (124)   → float64 (widened; downstream callers rarely care
//	                   about the source-width distinction)
//	integer (125)   → int64
//	null (126)      → nil
//	undefined (127) → nil
//
// Unknown tags return ErrUnsupportedAnyTag.
func DecodeAny(buf []byte) (any, []byte, error) {
	if len(buf) < 1 {
		return nil, buf, lib0.ErrTruncated
	}
	tag := buf[0]
	buf = buf[1:]
	switch tag {
	case AnyTagNull, AnyTagUndefined:
		return nil, buf, nil
	case AnyTagTrue:
		return true, buf, nil
	case AnyTagFalse:
		return false, buf, nil
	case AnyTagString:
		s, n, err := lib0.ReadVarString(buf)
		if err != nil {
			return nil, buf, err
		}
		return s, buf[n:], nil
	case AnyTagInteger:
		v, n, err := lib0.ReadVarInt(buf)
		if err != nil {
			return nil, buf, err
		}
		return v, buf[n:], nil
	case AnyTagFloat64:
		v, n, err := lib0.ReadFloat64(buf)
		if err != nil {
			return nil, buf, err
		}
		return v, buf[n:], nil
	case AnyTagFloat32:
		v, n, err := lib0.ReadFloat32(buf)
		if err != nil {
			return nil, buf, err
		}
		return float64(v), buf[n:], nil
	case AnyTagBigInt:
		if len(buf) < 8 {
			return nil, buf, lib0.ErrTruncated
		}
		v := int64(binary.BigEndian.Uint64(buf[:8]))
		return v, buf[8:], nil
	case AnyTagBinary:
		b, n, err := lib0.ReadVarUint8Array(buf)
		if err != nil {
			return nil, buf, err
		}
		out := make([]byte, len(b))
		copy(out, b)
		return out, buf[n:], nil
	case AnyTagArray:
		count, n, err := lib0.ReadVarUint(buf)
		if err != nil {
			return nil, buf, err
		}
		buf = buf[n:]
		if err := checkDecodeCount(count, len(buf)); err != nil {
			return nil, buf, err
		}
		arr := make([]any, count)
		for i := uint64(0); i < count; i++ {
			el, tail, err := DecodeAny(buf)
			if err != nil {
				return nil, buf, fmt.Errorf("decode Any array element %d: %w", i, err)
			}
			arr[i] = el
			buf = tail
		}
		return arr, buf, nil
	case AnyTagObject:
		count, n, err := lib0.ReadVarUint(buf)
		if err != nil {
			return nil, buf, err
		}
		buf = buf[n:]
		if err := checkDecodeCount(count, len(buf)); err != nil {
			return nil, buf, err
		}
		m := make(map[string]any, count)
		for i := uint64(0); i < count; i++ {
			key, kn, err := lib0.ReadVarString(buf)
			if err != nil {
				return nil, buf, fmt.Errorf("decode Any object key %d: %w", i, err)
			}
			buf = buf[kn:]
			val, tail, err := DecodeAny(buf)
			if err != nil {
				return nil, buf, fmt.Errorf("decode Any object value for key %q: %w", key, err)
			}
			m[key] = val
			buf = tail
		}
		return m, buf, nil
	default:
		return nil, buf, fmt.Errorf("%w: tag=%d", ErrUnsupportedAnyTag, tag)
	}
}
