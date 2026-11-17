package encoding

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"unsafe"

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

// ErrAnyTooDeep is returned for an Any value whose arrays and objects nest
// deeper than anyval.MaxNesting (10000) levels: the bound encoding/json
// applies to JSON text, and past what a yjs peer reads (lib0 readAny
// overflows the JavaScript stack after about 4,700 levels in Node 26).
// Without it, an update of a few megabytes nesting arrays several hundred
// thousand levels deep overflowed the goroutine stack, a crash no caller
// can recover from.
var ErrAnyTooDeep = fmt.Errorf("encoding: Any value nested deeper than %d levels", anyval.MaxNesting)

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
// Unknown tags return ErrUnsupportedAnyTag, and arrays and objects nested
// deeper than anyval.MaxNesting levels ErrAnyTooDeep.
func DecodeAny(buf []byte) (any, []byte, error) {
	return decodeAny(buf, strictStrings)
}

// decodeAny reads one Any value, without the key order of its objects.
func decodeAny(buf []byte, strs stringMode) (any, []byte, error) {
	v, _, tail, err := decodeAnyOrdered(buf, strs, nil, 0)
	return v, tail, err
}

// decodeAnyOrdered reads one Any value at nesting depth depth and, with a
// table t, the key order of the objects inside it as a JavaScript reader
// of the same bytes holds them (lib0 readAny assigns obj[key] = value: a
// key read twice keeps its first place and its last value, and array-index
// keys enumerate first), shared through t. The order is nil where the
// fallback order is right (anyval.ObjectKeys), and always without t.
func decodeAnyOrdered(buf []byte, strs stringMode, t *orderTable, depth int) (any, *block.KeyOrder, []byte, error) {
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
		return decodeAnyArray(buf, strs, t, depth)
	case AnyTagObject:
		if t == nil {
			return decodeAnyObject(buf, strs, depth)
		}
		return decodeAnyObjectOrdered(buf, strs, t, depth)
	default:
		return nil, nil, buf, fmt.Errorf("%w: tag=%d", ErrUnsupportedAnyTag, tag)
	}
}

// The containers are read by functions of their own, so the frame that
// reads a number or a string, most of a value, stays small.

// maxAnyPrealloc caps the room an array or object makes for the count
// its bytes claim before its elements are read. The count is bounded by
// the bytes left, but each of 10000 nested containers can claim them all:
// a V1 update of 112 KB made room for 2.8 GB before it was refused. lib0
// does not make room ahead either.
const maxAnyPrealloc = 16

func preallocCount(count uint64) int {
	if count > maxAnyPrealloc {
		return maxAnyPrealloc
	}
	return int(count)
}

// decodeAnyArray reads the elements of an array, after its tag.
func decodeAnyArray(buf []byte, strs stringMode, t *orderTable, depth int) (any, *block.KeyOrder, []byte, error) {
	if depth >= anyval.MaxNesting {
		return nil, nil, buf, ErrAnyTooDeep
	}
	count, n, err := lib0.ReadVarUint(buf)
	if err != nil {
		return nil, nil, buf, err
	}
	buf = buf[n:]
	if err := checkDecodeCount(count, len(buf)); err != nil {
		return nil, nil, buf, err
	}
	arr := make([]any, 0, preallocCount(count))
	var small [8]*block.KeyOrder
	var subs []*block.KeyOrder // up to the last element with an order
	for i := uint64(0); i < count; i++ {
		el, sub, tail, err := decodeAnyOrdered(buf, strs, t, depth+1)
		if err != nil {
			return nil, nil, buf, wrapAnyErr(depth, err, "decode Any array element %d", i)
		}
		arr = append(arr, el)
		if sub != nil {
			if subs == nil {
				subs = small[:0]
			}
			for uint64(len(subs)) < i {
				subs = append(subs, nil)
			}
			subs = append(subs, sub)
		}
		buf = tail
	}
	if subs == nil {
		return arr, nil, buf, nil
	}
	return arr, t.array(subs), buf, nil
}

// decodeAnyObject reads the entries of an object, after its tag, without
// their order.
func decodeAnyObject(buf []byte, strs stringMode, depth int) (any, *block.KeyOrder, []byte, error) {
	if depth >= anyval.MaxNesting {
		return nil, nil, buf, ErrAnyTooDeep
	}
	count, n, err := lib0.ReadVarUint(buf)
	if err != nil {
		return nil, nil, buf, err
	}
	buf = buf[n:]
	if err := checkDecodeCount(count, len(buf)); err != nil {
		return nil, nil, buf, err
	}
	m := make(map[string]any, preallocCount(count))
	for i := uint64(0); i < count; i++ {
		key, kn, err := strs.readString(buf)
		if err != nil {
			return nil, nil, buf, wrapAnyErr(depth, err, "decode Any object key %d", i)
		}
		buf = buf[kn:]
		val, _, tail, err := decodeAnyOrdered(buf, strs, nil, depth+1)
		if err != nil {
			return nil, nil, buf, wrapAnyErr(depth, err, "decode Any object value for key %q", key)
		}
		m[key] = val
		buf = tail
	}
	return m, nil, buf, nil
}

// decodeAnyObjectOrdered reads the entries of an object, after its tag,
// and their order, shared through t.
func decodeAnyObjectOrdered(buf []byte, strs stringMode, t *orderTable, depth int) (any, *block.KeyOrder, []byte, error) {
	if depth >= anyval.MaxNesting {
		return nil, nil, buf, ErrAnyTooDeep
	}
	count, n, err := lib0.ReadVarUint(buf)
	if err != nil {
		return nil, nil, buf, err
	}
	buf = buf[n:]
	if err := checkDecodeCount(count, len(buf)); err != nil {
		return nil, nil, buf, err
	}
	m := make(map[string]any, preallocCount(count))
	// On the stack for an object of a few keys: arrays a struct of slices
	// into its own fields would move to the heap.
	var smallKeys [8]string
	var smallSubs [8]*block.KeyOrder
	keys := smallKeys[:0]
	var subs []*block.KeyOrder
	var at map[string]int
	// keyTrack.note, written out: this loop runs for every key read.
	unsorted, prev, havePrev := false, "", false
	for i := uint64(0); i < count; i++ {
		key, kn, err := strs.readString(buf)
		if err != nil {
			return nil, nil, buf, wrapAnyErr(depth, err, "decode Any object key %d", i)
		}
		buf = buf[kn:]
		val, sub, tail, err := decodeAnyOrdered(buf, strs, t, depth+1)
		if err != nil {
			return nil, nil, buf, wrapAnyErr(depth, err, "decode Any object value for key %q", key)
		}
		before := len(m)
		m[key] = val
		var p int
		if len(m) > before { // first time: its place is here
			p = len(keys)
			keys = append(keys, key)
			if at != nil {
				at[key] = p
			}
			if !unsorted && (key == "" || key[0] > '9' || key[0] < '0' || !anyval.IsArrayIndexKey(key)) {
				if havePrev && !keyAfter(key, prev) {
					unsorted = true
				}
				prev, havePrev = key, true
			}
		} else {
			p, at = keyIndex(keys, at, key)
		}
		if sub != nil || subs != nil {
			subs = setSub(subs, smallSubs[:0], p, sub)
		}
		buf = tail
	}
	if !unsorted && subs == nil {
		return m, nil, buf, nil // the fallback order, nothing inside
	}
	return m, objectOrder(t, keys, subs, at, unsorted, false), buf, nil
}

// An object is read as JavaScript holds it (lib0 readAny assigns
// obj[key] = value, and so does JSON.parse): a key read twice keeps its
// first place and its last value. The readers keep the keys in the order
// they first came and, at the same positions, the order of each one's
// last value (setSub), shorter than the keys where the rest have none.

// keyTrack follows whether the keys other than array indices come in
// ascending order, which is the fallback order once JavaScript puts the
// indices first.
type keyTrack struct {
	unsorted bool
	prev     string
	havePrev bool
}

// note records key, read for the first time.
func (k *keyTrack) note(key string) {
	if k.unsorted || maybeIndexKey(key) {
		return
	}
	if k.havePrev && !keyAfter(key, k.prev) {
		k.unsorted = true
	}
	k.prev, k.havePrev = key, true
}

// maybeIndexKey is anyval.IsArrayIndexKey, with the test of the first byte
// that settles most keys done without a call.
func maybeIndexKey(key string) bool {
	return key != "" && key[0] <= '9' && key[0] >= '0' && anyval.IsArrayIndexKey(key)
}

// keyAfter reports whether key sorts after prev. Most keys differ from the
// one before in their first byte, which decides without a call.
func keyAfter(key, prev string) bool {
	if key != "" && prev != "" && key[0] != prev[0] {
		return key[0] > prev[0]
	}
	return key > prev
}

// keyedOrdersInline is how many keys a repeated one is looked for among
// one by one; past it a map of positions keeps a read linear in the
// number of keys whatever repeats.
const keyedOrdersInline = 16

// keyIndex returns the position of key in keys, or -1, and the map of
// positions, made here once keys are past keyedOrdersInline; the caller
// adds the keys it appends to a map it holds.
func keyIndex(keys []string, at map[string]int, key string) (int, map[string]int) {
	if at == nil {
		if len(keys) <= keyedOrdersInline {
			for i, x := range keys {
				if x == key {
					return i, nil
				}
			}
			return -1, nil
		}
		at = make(map[string]int, 2*len(keys))
		for i, x := range keys {
			at[x] = i
		}
	}
	if i, ok := at[key]; ok {
		return i, at
	}
	return -1, at
}

// setSub sets the order at position p to sub, the order of the value just
// read there, growing subs (from buf, the caller's stack, when it has none
// yet) only when sub is set.
func setSub(subs, buf []*block.KeyOrder, p int, sub *block.KeyOrder) []*block.KeyOrder {
	if sub == nil && p >= len(subs) {
		return subs
	}
	if subs == nil {
		subs = buf
	}
	for len(subs) <= p {
		subs = append(subs, nil)
	}
	subs[p] = sub
	return subs
}

// objectOrder returns the order of an object whose keys came in the order
// keys, with subs the orders of their values at the same positions and at
// the map of positions keyIndex made, if any, normalized to JavaScript
// enumeration order and shared through t; nil when the fallback order is
// right for it and everything inside (keys not unsorted and no order
// inside). keys and subs are not kept; clone is orderTable.object's.
func objectOrder(t *orderTable, keys []string, subs []*block.KeyOrder, at map[string]int, unsorted, clone bool) *block.KeyOrder {
	hasSub := false
	for _, s := range subs {
		if s != nil {
			hasSub = true
			break
		}
	}
	if !unsorted && !hasSub {
		return nil
	}
	js := anyval.JSOrder(keys)
	if !hasSub {
		return t.object(js, nil, clone)
	}
	var small [8]*block.KeyOrder
	list := small[:0]
	if unsafe.SliceData(js) == unsafe.SliceData(keys) {
		// No array-index key: JSOrder returned the keys as they are.
		list = append(list, subs...)
		for len(list) < len(js) {
			list = append(list, nil)
		}
	} else {
		for _, key := range js {
			var s *block.KeyOrder
			var p int
			if p, at = keyIndex(keys, at, key); p >= 0 && p < len(subs) {
				s = subs[p]
			}
			list = append(list, s)
		}
	}
	return t.object(js, list, clone)
}

// anyErrContextDepth is how many of the outer levels of an Any value add
// their place to an error from inside it.
const anyErrContextDepth = 4

// wrapAnyErr adds the place of a child, format and arg, to err from
// inside it, at the outer anyErrContextDepth levels only: each level
// copies the message of the levels below, so wrapping at every level made
// a refusal 10000 levels down allocate over a gigabyte.
func wrapAnyErr(depth int, err error, format string, arg any) error {
	if depth >= anyErrContextDepth {
		return err
	}
	return fmt.Errorf(format+": %w", arg, err)
}
