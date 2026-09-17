package encoding

import (
	"errors"
	"fmt"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/lib0"
)

// DecoderV2 is the column-oriented update decoder matching yjs
// UpdateDecoderV2 (testdata/gen/node_modules/yjs/src/utils/
// UpdateDecoder.js:96-180). The constructor reads all 9 column
// decoders upfront from the wire (each as a length-prefixed
// varbuffer); per-field Read* methods then pull one value from
// the appropriate column per block iteration.
//
// Top-level update headers (block-count, start-clock, client-count)
// come from the raw rest stream via ReadVarUint — only per-block
// field reads pull from columns.
//
// Pattern: construct with NewDecoderV2, call Read* in the SAME
// order the encoder called Write*. Wire format has no length
// self-check; any iteration-order mismatch silently corrupts.
type DecoderV2 struct {
	rest    []byte
	restPos int

	keyClock   *lib0.IntDiffOptRleDecoder
	client     *lib0.UintOptRleDecoder
	leftClock  *lib0.IntDiffOptRleDecoder
	rightClock *lib0.IntDiffOptRleDecoder
	info       *lib0.RleDecoder
	str        *lib0.StringDecoder
	parentInfo *lib0.RleDecoder
	typeRef    *lib0.UintOptRleDecoder
	length     *lib0.UintOptRleDecoder

	// keys is the receiver's key-table — populated as new keys
	// are encountered (asymmetric with the encoder side; see
	// port-note gotcha 5). ReadKey indexes by keyClock value.
	keys []string
}

// ErrV2BadFeatureFlag is returned when the leading byte isn't
// the expected 0x00. Indicates wire-format drift (a future V3?)
// or that the bytes aren't actually V2 (could be V1 or junk).
var ErrV2BadFeatureFlag = errors.New("encoding: V2 update has non-zero feature flag")

// NewDecoderV2 wraps buf as a V2 decoder. Reads the feature flag
// + 9 column lengths upfront so subsequent Read* calls are O(1)
// per value.
func NewDecoderV2(buf []byte) (*DecoderV2, error) {
	if len(buf) < 1 {
		return nil, fmt.Errorf("V2 update: empty buffer")
	}
	if buf[0] != 0x00 {
		return nil, fmt.Errorf("%w: got %#x", ErrV2BadFeatureFlag, buf[0])
	}
	buf = buf[1:]

	keyClockBytes, n, err := lib0.ReadVarUint8Array(buf)
	if err != nil {
		return nil, fmt.Errorf("V2 decode keyClock column: %w", err)
	}
	buf = buf[n:]

	clientBytes, n, err := lib0.ReadVarUint8Array(buf)
	if err != nil {
		return nil, fmt.Errorf("V2 decode client column: %w", err)
	}
	buf = buf[n:]

	leftClockBytes, n, err := lib0.ReadVarUint8Array(buf)
	if err != nil {
		return nil, fmt.Errorf("V2 decode leftClock column: %w", err)
	}
	buf = buf[n:]

	rightClockBytes, n, err := lib0.ReadVarUint8Array(buf)
	if err != nil {
		return nil, fmt.Errorf("V2 decode rightClock column: %w", err)
	}
	buf = buf[n:]

	infoBytes, n, err := lib0.ReadVarUint8Array(buf)
	if err != nil {
		return nil, fmt.Errorf("V2 decode info column: %w", err)
	}
	buf = buf[n:]

	stringBytes, n, err := lib0.ReadVarUint8Array(buf)
	if err != nil {
		return nil, fmt.Errorf("V2 decode string column: %w", err)
	}
	buf = buf[n:]

	parentInfoBytes, n, err := lib0.ReadVarUint8Array(buf)
	if err != nil {
		return nil, fmt.Errorf("V2 decode parentInfo column: %w", err)
	}
	buf = buf[n:]

	typeRefBytes, n, err := lib0.ReadVarUint8Array(buf)
	if err != nil {
		return nil, fmt.Errorf("V2 decode typeRef column: %w", err)
	}
	buf = buf[n:]

	lenBytes, n, err := lib0.ReadVarUint8Array(buf)
	if err != nil {
		return nil, fmt.Errorf("V2 decode len column: %w", err)
	}
	buf = buf[n:]

	strDec, err := lib0.NewStringDecoder(stringBytes)
	if err != nil {
		return nil, fmt.Errorf("V2 decode string-column inner: %w", err)
	}

	return &DecoderV2{
		rest:       buf,
		keyClock:   lib0.NewIntDiffOptRleDecoderFor(keyClockBytes),
		client:     lib0.NewUintOptRleDecoder(clientBytes),
		leftClock:  lib0.NewIntDiffOptRleDecoderFor(leftClockBytes),
		rightClock: lib0.NewIntDiffOptRleDecoderFor(rightClockBytes),
		info:       lib0.NewRleDecoder(infoBytes, lib0.ReadUint8Func),
		str:        strDec,
		parentInfo: lib0.NewRleDecoder(parentInfoBytes, lib0.ReadUint8Func),
		typeRef:    lib0.NewUintOptRleDecoder(typeRefBytes),
		length:     lib0.NewUintOptRleDecoder(lenBytes),
	}, nil
}

// ReadLeftID pulls one (client, clock) pair from the shared
// client column + left-clock column.
func (d *DecoderV2) ReadLeftID() (block.ID, error) {
	c, err := d.client.Read()
	if err != nil {
		return block.ID{}, fmt.Errorf("ReadLeftID client: %w", err)
	}
	clock, err := d.leftClock.Read()
	if err != nil {
		return block.ID{}, fmt.Errorf("ReadLeftID clock: %w", err)
	}
	return block.ID{Client: c, Clock: uint64(clock)}, nil
}

// ReadRightID pulls from the shared client + right-clock columns.
func (d *DecoderV2) ReadRightID() (block.ID, error) {
	c, err := d.client.Read()
	if err != nil {
		return block.ID{}, fmt.Errorf("ReadRightID client: %w", err)
	}
	clock, err := d.rightClock.Read()
	if err != nil {
		return block.ID{}, fmt.Errorf("ReadRightID clock: %w", err)
	}
	return block.ID{Client: c, Clock: uint64(clock)}, nil
}

// ReadClient returns the next client ID from the shared client column.
func (d *DecoderV2) ReadClient() (uint64, error) {
	return d.client.Read()
}

// ReadInfo returns the next info byte from the info RLE column.
func (d *DecoderV2) ReadInfo() (uint8, error) {
	v, err := d.info.Read()
	return uint8(v), err
}

// ReadString returns the next string from the string column.
func (d *DecoderV2) ReadString() (string, error) {
	return d.str.Read()
}

// ReadParentInfo returns true if the parent is referenced by
// root-name, false if by item ID. Maps the 0/1 byte from the
// parent-info RLE column.
func (d *DecoderV2) ReadParentInfo() (bool, error) {
	v, err := d.parentInfo.Read()
	return v == 1, err
}

// ReadTypeRef returns the next TypeRef byte from the type-ref
// RLE column.
func (d *DecoderV2) ReadTypeRef() (uint8, error) {
	v, err := d.typeRef.Read()
	return uint8(v), err
}

// ReadLen returns the next length value from the length column.
func (d *DecoderV2) ReadLen() (uint64, error) {
	return d.length.Read()
}

// ReadVarUint returns the next varuint from the raw rest stream.
// Used for top-level structure headers.
func (d *DecoderV2) ReadVarUint() (uint64, error) {
	v, n, err := lib0.ReadVarUint(d.rest[d.restPos:])
	if err != nil {
		return 0, err
	}
	d.restPos += n
	return v, nil
}

// ReadRestBytes returns the unconsumed rest stream — used by
// content decoders that need raw access.
func (d *DecoderV2) ReadRestBytes() []byte {
	return d.rest[d.restPos:]
}

// AdvanceRest moves the rest cursor forward by n bytes. Content
// decoders call this after consuming bytes via ReadRestBytes.
func (d *DecoderV2) AdvanceRest(n int) {
	d.restPos += n
}

// ReadAny consumes the next lib0 Any TLV from the rest stream.
func (d *DecoderV2) ReadAny() (block.Any, error) {
	v, tail, err := DecodeAny(d.rest[d.restPos:])
	if err != nil {
		return nil, err
	}
	d.restPos = len(d.rest) - len(tail)
	return v, nil
}

// ReadVarBuf consumes the next varbuffer from the rest stream.
func (d *DecoderV2) ReadVarBuf() ([]byte, error) {
	b, n, err := lib0.ReadVarUint8Array(d.rest[d.restPos:])
	if err != nil {
		return nil, err
	}
	d.restPos += n
	// Copy — the underlying rest slice may be reused.
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

// ReadKey resolves a map-key reference. Unlike the encoder side,
// the decoder DOES populate a key table — see port-note gotcha 5
// for the asymmetry. First read of a given keyClock value reads
// a string from the string column AND caches it; subsequent reads
// of the same keyClock return the cached string.
func (d *DecoderV2) ReadKey() (string, error) {
	clock, err := d.keyClock.Read()
	if err != nil {
		return "", fmt.Errorf("ReadKey keyClock: %w", err)
	}
	idx := int(clock)
	if idx >= 0 && idx < len(d.keys) {
		return d.keys[idx], nil
	}
	// Index out of range or larger than current keys — read a
	// new string and grow the table.
	s, err := d.str.Read()
	if err != nil {
		return "", fmt.Errorf("ReadKey string: %w", err)
	}
	d.keys = append(d.keys, s)
	return s, nil
}

// ReadJSON reads a format value or an embed: a lib0 Any in the rest
// stream, as yjs UpdateDecoderV2.readJSON reads it, with numbers mapped
// to the types the V1 path yields so callers see the same Go types
// whichever wire format delivered the document. See json_any.go.
func (d *DecoderV2) ReadJSON() (block.Any, error) {
	v, err := d.ReadAny()
	if err != nil {
		return nil, err
	}
	return jsonValueFromAny(v), nil
}
