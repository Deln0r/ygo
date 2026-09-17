package encoding

import (
	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/lib0"
)

// EncoderV2 is the column-oriented update encoder matching yjs
// UpdateEncoderV2 (testdata/gen/node_modules/yjs/src/utils/
// UpdateEncoder.js:115-220) and yrs EncoderV2. Block-write methods
// route field writes to 9 per-column buffers + 1 raw rest stream;
// Bytes() concatenates them with length prefixes behind a 0x00
// feature flag byte.
//
// Wire layout — see docs/yrs-port-notes/update-v2.md §1. Column
// order is wire-load-bearing; DO NOT reorder.
//
// Top-level update headers (per-client block-count, start-clock,
// outer client-count) go to the rest stream via WriteVarUint —
// only per-block field writes get column-routed. See port note §4
// gotcha "Top-level block-run structure stays the same as V1".
//
// Pattern: construct with NewEncoderV2, call the per-field Write*
// methods + WriteVarUint for headers, then call Bytes() exactly
// once at the end to flush all columns and assemble the wire bytes.
// EncoderV2 is single-use; do not call Bytes() twice.
type EncoderV2 struct {
	rest []byte

	keyClock   *lib0.IntDiffOptRleEncoder
	client     *lib0.UintOptRleEncoder
	leftClock  *lib0.IntDiffOptRleEncoder
	rightClock *lib0.IntDiffOptRleEncoder
	info       *lib0.RleEncoder
	str        *lib0.StringEncoder
	parentInfo *lib0.RleEncoder
	typeRef    *lib0.UintOptRleEncoder
	length     *lib0.UintOptRleEncoder

	// keyClockCounter mirrors yjs's keyClock field — incremented
	// on every WriteKey because per port-note gotcha 5 the
	// encoder-side keyMap is never populated (yjs "bug" we
	// replicate bit-for-bit for wire compat).
	keyClockCounter uint32
}

// NewEncoderV2 returns a fresh EncoderV2 with all column buffers
// initialized.
func NewEncoderV2() *EncoderV2 {
	return &EncoderV2{
		keyClock:   lib0.NewIntDiffOptRleEncoder(),
		client:     lib0.NewUintOptRleEncoder(),
		leftClock:  lib0.NewIntDiffOptRleEncoder(),
		rightClock: lib0.NewIntDiffOptRleEncoder(),
		info:       lib0.NewRleEncoder(lib0.WriteUint8Func),
		str:        lib0.NewStringEncoder(),
		parentInfo: lib0.NewRleEncoder(lib0.WriteUint8Func),
		typeRef:    lib0.NewUintOptRleEncoder(),
		length:     lib0.NewUintOptRleEncoder(),
	}
}

// WriteLeftID routes the (client, clock) pair to the shared
// client column + the left-clock column. Mirrors
// UpdateEncoderV2.writeLeftID (UpdateEncoder.js:156-159).
func (e *EncoderV2) WriteLeftID(id block.ID) {
	e.client.Write(id.Client)
	e.leftClock.Write(int64(id.Clock))
}

// WriteRightID routes to the shared client column + the
// right-clock column.
func (e *EncoderV2) WriteRightID(id block.ID) {
	e.client.Write(id.Client)
	e.rightClock.Write(int64(id.Clock))
}

// WriteClient routes a standalone client ID (e.g. per-client
// block-run header) to the shared client column.
func (e *EncoderV2) WriteClient(client uint64) {
	e.client.Write(client)
}

// WriteInfo routes an info byte to the per-byte RLE column.
func (e *EncoderV2) WriteInfo(info uint8) {
	e.info.Write(uint64(info))
}

// WriteString routes a string to the string column. Used for
// parent root-type names and ContentString payloads.
func (e *EncoderV2) WriteString(s string) {
	e.str.Write(s)
}

// WriteParentInfo routes the "parent is name vs item-ID" boolean
// tag to the parent-info RLE column.
func (e *EncoderV2) WriteParentInfo(isRootName bool) {
	var v uint64
	if isRootName {
		v = 1
	}
	e.parentInfo.Write(v)
}

// WriteTypeRef routes a TypeRef tag (Array=0, Map=1, Text=2, ...)
// to the type-ref RLE column. Used inside ContentType payloads.
func (e *EncoderV2) WriteTypeRef(ref uint8) {
	e.typeRef.Write(uint64(ref))
}

// WriteLen routes a run-length (GC range, ContentDeleted, content
// element count) to the length column.
func (e *EncoderV2) WriteLen(l uint64) {
	e.length.Write(l)
}

// WriteVarUint routes a generic varuint to the raw rest stream.
// Used for top-level structure headers: outer client count,
// per-client block count, start clock.
func (e *EncoderV2) WriteVarUint(n uint64) {
	e.rest = lib0.WriteVarUint(e.rest, n)
}

// WriteRestBytes appends raw bytes directly to the rest stream.
// Used by Content encoders (ContentAny, ContentBinary, etc.)
// that produce arbitrary byte payloads.
func (e *EncoderV2) WriteRestBytes(b []byte) {
	e.rest = append(e.rest, b...)
}

// WriteAny routes a lib0 Any TLV value to the rest stream.
// Used by ContentAny / ContentEmbed when V2 hits them — note
// V2 keeps Any in rest (no column for it).
func (e *EncoderV2) WriteAny(v block.Any) {
	e.rest = EncodeAny(e.rest, v)
}

// WriteVarBuf routes a length-prefixed byte array to the rest
// stream. Used by ContentBinary.
func (e *EncoderV2) WriteVarBuf(b []byte) {
	e.rest = lib0.WriteVarUint8Array(e.rest, b)
}

// WriteKey emits a map-key reference. Mirrors yjs's "bug" per
// port-note gotcha 5: the encoder-side key table is NEVER
// populated, so every WriteKey writes a fresh string into the
// string column and increments the keyClock counter. The decoder
// side DOES build a key table — asymmetric but wire-compatible.
func (e *EncoderV2) WriteKey(key string) {
	e.keyClock.Write(int64(e.keyClockCounter))
	e.str.Write(key)
	e.keyClockCounter++
}

// WriteJSON writes a format value or an embed. In V2 that is a lib0 Any
// in the rest stream, NOT the JSON varstring V1 uses - yjs
// UpdateEncoderV2.writeJSON calls writeAny. This method used to reuse
// the V1 layout on the assumption that V2 "doesn't column-encode JSON
// payloads either", which is true and beside the point: the payload
// format differs, and the result was unreadable by yjs. See json_any.go.
func (e *EncoderV2) WriteJSON(v block.Any) {
	e.WriteAny(jsonValueForAny(v))
}

// Bytes flushes every column and assembles the final V2 wire
// bytes per port-note §1. Must be called exactly once; do not
// call any Write* method afterwards.
//
// Layout:
//
//	[0x00 feature flag]
//	[varuint len | keyClockBytes]
//	[varuint len | clientBytes]
//	[varuint len | leftClockBytes]
//	[varuint len | rightClockBytes]
//	[varuint len | infoBytes]
//	[varuint len | stringBytes]
//	[varuint len | parentInfoBytes]
//	[varuint len | typeRefBytes]
//	[varuint len | lenBytes]
//	[restBytes]                  (NO length prefix; reader takes the remainder)
func (e *EncoderV2) Bytes() []byte {
	out := []byte{0x00} // feature flag
	out = lib0.WriteVarUint8Array(out, e.keyClock.Bytes())
	out = lib0.WriteVarUint8Array(out, e.client.Bytes())
	out = lib0.WriteVarUint8Array(out, e.leftClock.Bytes())
	out = lib0.WriteVarUint8Array(out, e.rightClock.Bytes())
	out = lib0.WriteVarUint8Array(out, e.info.Bytes())
	out = lib0.WriteVarUint8Array(out, e.str.Bytes())
	out = lib0.WriteVarUint8Array(out, e.parentInfo.Bytes())
	out = lib0.WriteVarUint8Array(out, e.typeRef.Bytes())
	out = lib0.WriteVarUint8Array(out, e.length.Bytes())
	out = append(out, e.rest...)
	return out
}
