package lib0

import "errors"

// Run-length-encoded (RLE) and diff-RLE primitives matching the
// stateful encoders in lib0's encoding.js (RleEncoder,
// UintOptRleEncoder, IntDiffOptRleEncoder, StringEncoder). These
// are the byte-level building blocks the V2 update wire format
// uses for column compression — see
// docs/yrs-port-notes/update-v2.md.
//
// Each encoder is a struct that accumulates state across Write
// calls; the run is flushed lazily on the next non-matching
// Write OR on the terminal Bytes() call. Forgetting Bytes() ==
// silently dropping the last run — call it exactly once at the
// end of column construction.
//
// The decoders are also stateful: they pre-read the leading
// run-header on first Read and replay the cached value until the
// count exhausts. They are tied to the byte buffer they were
// constructed with; do not concurrently use a decoder from
// multiple goroutines.

// RleEncoder pairs `(value, count-1)`. On Write(v): if v matches
// the current-run value, increment count; else flush previous run
// as a varuint(count-1) followed by the new value (written via
// the user-supplied writer func — typically a single byte for
// V2's info / parentInfo columns).
//
// Mirrors testdata/gen/node_modules/lib0/encoding.js
// RleEncoder (lines 615-650). Tail-flush happens on Bytes().
type RleEncoder struct {
	buf   []byte
	w     func(buf []byte, v uint64) []byte
	s     uint64
	count uint64
}

// NewRleEncoder returns an RLE encoder that writes each run-value
// via the supplied writer. For V2's info/parentInfo columns pass
// WriteUint8Func — a thin wrapper around WriteUint8.
func NewRleEncoder(writer func(buf []byte, v uint64) []byte) *RleEncoder {
	return &RleEncoder{w: writer}
}

// Write records v into the current run. If v matches the previous
// value the count grows; otherwise the previous run flushes and a
// new one starts.
func (e *RleEncoder) Write(v uint64) {
	if e.count > 0 && e.s == v {
		e.count++
		return
	}
	if e.count > 0 {
		// Flush prior run: count-1 as a varuint precedes the value.
		e.buf = WriteVarUint(e.buf, e.count-1)
	}
	e.count = 1
	e.buf = e.w(e.buf, v)
	e.s = v
}

// Bytes flushes any pending run and returns the accumulated byte
// buffer. Must be called exactly once at the end of column
// construction; subsequent Writes after Bytes are undefined.
func (e *RleEncoder) Bytes() []byte {
	// RleEncoder has no terminal flush — the JS impl also doesn't
	// write a final count varuint. The decoder's "count = -1
	// (forever)" branch handles the unbounded tail.
	return e.buf
}

// WriteUint8Func wraps WriteUint8 to fit the RleEncoder writer
// signature. The value MUST fit in a uint8; higher bits are
// discarded by the underlying WriteUint8 cast.
func WriteUint8Func(buf []byte, v uint64) []byte {
	return WriteUint8(buf, uint8(v))
}

// RleDecoder is the read counterpart to RleEncoder.
type RleDecoder struct {
	buf   []byte
	pos   int
	r     func(buf []byte) (uint64, int, error)
	s     uint64
	count int64 // -1 means "forever" (unbounded tail per JS hasContent branch)
}

// NewRleDecoder returns a decoder over buf using the supplied
// value-reader (matches the encoder's writer).
func NewRleDecoder(buf []byte, reader func(buf []byte) (uint64, int, error)) *RleDecoder {
	return &RleDecoder{buf: buf, r: reader}
}

// ReadUint8Func wraps ReadUint8 for the RleDecoder reader signature.
func ReadUint8Func(buf []byte) (uint64, int, error) {
	v, n, err := ReadUint8(buf)
	return uint64(v), n, err
}

// Read returns the next value in the run. Re-uses the cached run
// value until the count exhausts, then reads a fresh value +
// count pair from the underlying buffer.
//
// Returns an error if the underlying buffer is malformed mid-run.
func (d *RleDecoder) Read() (uint64, error) {
	if d.count == 0 {
		v, n, err := d.r(d.buf[d.pos:])
		if err != nil {
			return 0, err
		}
		d.s = v
		d.pos += n
		if d.pos >= len(d.buf) {
			d.count = -1 // forever
		} else {
			count, n, err := ReadVarUint(d.buf[d.pos:])
			if err != nil {
				return 0, err
			}
			d.pos += n
			d.count = int64(count) + 1
		}
	}
	if d.count > 0 {
		d.count--
	}
	return d.s, nil
}

// UintOptRleEncoder is the optimized RLE for absolute uints. A
// single occurrence emits one varint with positive sign; a run of
// N (N>=2) emits varint(-value) + varuint(count-2). Mirrors
// lib0 encoding.js UintOptRleEncoder (lines 742-773).
//
// 31-bit value limit: the value-sign bit lives in the leading
// varint; values larger than 2^31-1 would collide with the sign
// flag.
type UintOptRleEncoder struct {
	buf   []byte
	s     uint64
	count uint64
}

// NewUintOptRleEncoder returns an empty UintOptRle encoder.
func NewUintOptRleEncoder() *UintOptRleEncoder {
	return &UintOptRleEncoder{}
}

// Write records v into the current run.
func (e *UintOptRleEncoder) Write(v uint64) {
	if e.count > 0 && e.s == v {
		e.count++
		return
	}
	e.buf = flushUintOptRle(e.buf, e.s, e.count)
	e.count = 1
	e.s = v
}

// Bytes flushes the pending run and returns the buffer. Call once.
func (e *UintOptRleEncoder) Bytes() []byte {
	e.buf = flushUintOptRle(e.buf, e.s, e.count)
	e.count = 0
	return e.buf
}

// flushUintOptRle writes one (value, count) run.
//
// count == 0: no-op (nothing accumulated yet, e.g. before first Write).
// count == 1: writeVarInt(+s)
// count >= 2: writeVarInt(-s) + writeVarUint(count-2)
//
// The sign of the leading varint tells the decoder whether to
// read a count. Value 0 with count >= 2 needs an explicit
// "negative zero" — we encode it as varint with the negative-sign
// bit set even though magnitude is 0.
func flushUintOptRle(buf []byte, s, count uint64) []byte {
	if count == 0 {
		return buf
	}
	if count == 1 {
		return writeVarIntSigned(buf, s, false)
	}
	buf = writeVarIntSigned(buf, s, true) // negative sign signals "count follows"
	return WriteVarUint(buf, count-2)
}

// writeVarIntSigned writes a lib0 varint with an explicit sign
// bit. Unlike WriteVarInt this allows encoding "negative zero"
// (magnitude 0 with the sign bit set) which UintOptRle and
// IntDiffOptRle rely on for the count-follows signal.
func writeVarIntSigned(buf []byte, mag uint64, negative bool) []byte {
	var sign byte
	if negative {
		sign = 0x40
	}
	// First byte: 6 bits of magnitude + sign bit + continuation if more.
	first := byte(mag&0x3f) | sign
	mag >>= 6
	if mag == 0 {
		return append(buf, first)
	}
	buf = append(buf, first|0x80)
	for mag > 0x7f {
		buf = append(buf, byte(mag&0x7f)|0x80)
		mag >>= 7
	}
	return append(buf, byte(mag&0x7f))
}

// readVarIntSigned mirrors writeVarIntSigned — returns magnitude
// + sign separately so callers can distinguish negative zero
// (mag=0, negative=true) from positive zero (mag=0, negative=false).
func readVarIntSigned(buf []byte) (mag uint64, negative bool, n int, err error) {
	if len(buf) == 0 {
		return 0, false, 0, ErrTruncated
	}
	first := buf[0]
	negative = first&0x40 != 0
	mag = uint64(first & 0x3f)
	if first&0x80 == 0 {
		return mag, negative, 1, nil
	}
	rest, consumed, err := ReadVarUint(buf[1:])
	if err != nil {
		return 0, false, 0, err
	}
	mag |= rest << 6
	return mag, negative, 1 + consumed, nil
}

// UintOptRleDecoder is the read counterpart to UintOptRleEncoder.
type UintOptRleDecoder struct {
	buf   []byte
	pos   int
	s     uint64
	count int64
}

// NewUintOptRleDecoder returns a decoder over buf.
func NewUintOptRleDecoder(buf []byte) *UintOptRleDecoder {
	return &UintOptRleDecoder{buf: buf}
}

// Read returns the next value.
func (d *UintOptRleDecoder) Read() (uint64, error) {
	if d.count == 0 {
		mag, negative, n, err := readVarIntSigned(d.buf[d.pos:])
		if err != nil {
			return 0, err
		}
		d.pos += n
		d.s = mag
		d.count = 1
		if negative {
			count, n, err := ReadVarUint(d.buf[d.pos:])
			if err != nil {
				return 0, err
			}
			d.pos += n
			d.count = int64(count) + 2
		}
	}
	d.count--
	return d.s, nil
}

// IntDiffOptRleEncoder packs `(diff << 1) | hasCount` into the
// first varint of each run. Mirrors lib0 encoding.js (lines
// 853-887). 31-bit diff limit due to the shift.
//
// Receiver maintains its own accumulator and adds each diff
// repeatedly. Resetting state mid-stream requires writing a
// record that breaks the current run — the encoder/decoder will
// then naturally re-sync on a fresh diff.
type IntDiffOptRleEncoder struct {
	buf   []byte
	s     int64
	count uint64
	diff  int64
}

// NewIntDiffOptRleEncoder returns an empty encoder.
func NewIntDiffOptRleEncoder() *IntDiffOptRleEncoder {
	return &IntDiffOptRleEncoder{}
}

// Write records v into the current run. If the delta from the
// running accumulator matches the current run's delta the count
// grows; otherwise the run flushes and a new one starts.
func (e *IntDiffOptRleEncoder) Write(v int64) {
	if e.count > 0 && v-e.s == e.diff {
		e.s = v
		e.count++
		return
	}
	e.buf = flushIntDiffOptRle(e.buf, e.diff, e.count)
	e.count = 1
	e.diff = v - e.s
	e.s = v
}

// Bytes flushes and returns.
func (e *IntDiffOptRleEncoder) Bytes() []byte {
	e.buf = flushIntDiffOptRle(e.buf, e.diff, e.count)
	e.count = 0
	return e.buf
}

// flushIntDiffOptRle writes one (diff, count) run.
//
// Packs (|diff|, sign, hasCount) into the leading varint:
//
//	encodedDiff = diff * 2 + (count == 1 ? 0 : 1)
//
// The JS encoder uses signed integer math because diff can be
// negative; the doubling preserves the sign in the varint
// representation. Go-side: we split into magnitude + sign and
// route through writeVarIntSigned so the negative-zero edge case
// (diff=0 with count>=2) encodes correctly.
func flushIntDiffOptRle(buf []byte, diff int64, count uint64) []byte {
	if count == 0 {
		return buf
	}
	hasCount := count >= 2
	// Compute encoded = diff * 2 + (hasCount ? 1 : 0) in signed
	// arithmetic — the +1 partially cancels the sign for negative
	// diffs (e.g. -10 * 2 + 1 = -19, magnitude 19 not 21). Then
	// split into magnitude + sign for writeVarIntSigned.
	encoded := diff * 2
	if hasCount {
		encoded++
	}
	mag := uint64(encoded)
	negative := false
	if encoded < 0 {
		mag = uint64(-encoded)
		negative = true
	}
	buf = writeVarIntSigned(buf, mag, negative)
	if hasCount {
		buf = WriteVarUint(buf, count-2)
	}
	return buf
}

// IntDiffOptRleDecoder is the read counterpart.
type IntDiffOptRleDecoder struct {
	buf   []byte
	pos   int
	s     int64
	count int64
	diff  int64
}

// NewIntDiffOptRleDecoder returns a decoder over buf.
func NewIntDiffOptRleDecoder(buf []byte) *IntDiffOptRleDecoder {
	return &IntDiffOptRleDecoder{}
}

// Read returns the next reconstructed value.
func (d *IntDiffOptRleDecoder) Read() (int64, error) {
	if d.count == 0 {
		// Refill from a fresh run. Reconstruct the SIGNED encoded
		// value first, then extract hasCount (LSB) and diff (>>1).
		// Doing the &1 / >>1 on the magnitude before applying the
		// sign produces off-by-one for negative diffs because the
		// +1 (count-follows bit) partially cancels the negation.
		mag, negative, n, err := readVarIntSigned(d.buf[d.pos:])
		if err != nil {
			return 0, err
		}
		d.pos += n
		encoded := int64(mag)
		if negative {
			encoded = -encoded
		}
		hasCount := (encoded & 1) != 0
		// Go arithmetic right-shift preserves sign for int64 — for
		// negative numbers this matches JS Math.floor(encoded/2),
		// the exact inverse of the encoder's `* 2 + hasCount`.
		d.diff = encoded >> 1
		d.count = 1
		if hasCount {
			count, n, err := ReadVarUint(d.buf[d.pos:])
			if err != nil {
				return 0, err
			}
			d.pos += n
			d.count = int64(count) + 2
		}
	}
	d.s += d.diff
	d.count--
	return d.s, nil
}

// Reset re-points the decoder at buf (e.g. after column-buffer
// slicing in V2's parallel reader). Internal accumulator state
// (s) is preserved per lib0 convention — IntDiffOpt decoders are
// expected to start at s=0 and accumulate from there.
func (d *IntDiffOptRleDecoder) Reset(buf []byte) {
	d.buf = buf
	d.pos = 0
	d.count = 0
	d.diff = 0
	d.s = 0
}

// NewIntDiffOptRleDecoderFor returns a decoder over buf — same
// as NewIntDiffOptRleDecoder but also takes the buffer in the
// constructor (the original ctor takes no args, matching the JS
// API; this helper avoids the Reset dance for callers that have
// the buffer up front).
func NewIntDiffOptRleDecoderFor(buf []byte) *IntDiffOptRleDecoder {
	return &IntDiffOptRleDecoder{buf: buf}
}

// StringEncoder concatenates strings into one big buffer and
// emits them with a parallel UintOptRle length stream. Output
// layout: `varstring(concatenated) + uintOptRleLengthBytes` —
// two segments back-to-back, no inner length prefix on the
// length stream (outer V2 column wrapper bounds the whole
// thing). Mirrors lib0 encoding.js StringEncoder (lines 900-922).
type StringEncoder struct {
	// sarr accumulates string chunks; flushed to s when s grows
	// past 19 chars (matches JS staging heuristic).
	sarr []string
	s    string
	lens *UintOptRleEncoder
}

// NewStringEncoder returns an empty StringEncoder.
func NewStringEncoder() *StringEncoder {
	return &StringEncoder{lens: NewUintOptRleEncoder()}
}

// Write appends one string to the column.
func (e *StringEncoder) Write(s string) {
	e.s += s
	if len(e.s) > 19 {
		e.sarr = append(e.sarr, e.s)
		e.s = ""
	}
	// Length is in UTF-16 code units to match JS string.length —
	// not Go's byte len. Use a helper from internal/utf16 if
	// needed; for now Go's len(s) returns bytes which differs
	// for non-ASCII content. yjs writes string.length (UTF-16
	// units); to match, convert via internal/utf16.Length when
	// non-ASCII strings flow through. For ASCII-only columns
	// (the common case for nodeName / parentName / key) byte
	// length and UTF-16 length agree.
	e.lens.Write(stringUTF16Length(s))
}

// Bytes flushes the staging buffer and emits the wire bytes:
// varstring(concatenated) followed by the UintOptRle length
// stream (no length prefix on the length stream — outer wrapper
// bounds the whole thing).
func (e *StringEncoder) Bytes() []byte {
	e.sarr = append(e.sarr, e.s)
	e.s = ""
	var concat string
	for _, chunk := range e.sarr {
		concat += chunk
	}
	buf := WriteVarString(nil, concat)
	return append(buf, e.lens.Bytes()...)
}

// stringUTF16Length returns the UTF-16 code-unit count of s.
// Pure ASCII: equals len(s). Non-BMP characters (emoji etc.)
// contribute 2. Matches JS string.length semantics.
func stringUTF16Length(s string) uint64 {
	var n uint64
	for _, r := range s {
		if r <= 0xFFFF {
			n++
		} else {
			n += 2
		}
	}
	return n
}

// StringDecoder reads back what StringEncoder wrote: a varstring
// of concatenated content followed by a UintOptRle length stream.
type StringDecoder struct {
	str  string
	pos  int // byte position in str
	lens *UintOptRleDecoder
}

// ErrSplitSurrogate reports a V2 string column whose lengths cut a
// surrogate pair in two. lib0 concatenates a column's strings before it
// encodes them and slices them back by UTF-16 length, so a JS string that
// ends in a lone high surrogate, followed by one that starts with a lone
// low surrogate, travels as valid UTF-8 and comes back as the two lone
// halves. A Go string cannot hold a lone surrogate, and replacing the
// halves would change keys and names that yjs keeps apart, so the update
// is refused. In V1 each string is encoded on its own, and JS TextEncoder
// writes a lone surrogate as U+FFFD.
var ErrSplitSurrogate = errors.New("lib0: V2 string column splits a surrogate pair (a string with a lone surrogate)")

// NewStringDecoder constructs a decoder over the StringEncoder
// column bytes. A column that is not valid UTF-8 returns ErrInvalidUTF8,
// as lib0 reads the column with readVarString.
func NewStringDecoder(buf []byte) (*StringDecoder, error) {
	s, n, err := ReadVarString(buf)
	if err != nil {
		return nil, err
	}
	lens := NewUintOptRleDecoder(buf[n:])
	return &StringDecoder{str: s, lens: lens}, nil
}

// Read returns the next string slice. Lengths count UTF-16 units, as JS
// string slicing does: a character outside the BMP counts two. A length
// that ends between the two units of such a character returns
// ErrSplitSurrogate.
func (d *StringDecoder) Read() (string, error) {
	length, err := d.lens.Read()
	if err != nil {
		return "", err
	}
	rest := d.str[d.pos:]
	end := len(rest)
	var consumed uint64
	for i, r := range rest {
		if consumed == length {
			end = i
			break
		}
		consumed++
		if r > 0xFFFF {
			if consumed == length {
				return "", ErrSplitSurrogate
			}
			consumed++
		}
	}
	d.pos += end
	return rest[:end], nil
}
