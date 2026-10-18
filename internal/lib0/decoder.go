package lib0

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"
)

// ErrTruncated is returned when a decoder runs past the end of the input.
var ErrTruncated = errors.New("lib0: truncated input")

// ErrOverflow is returned when a varuint/varint exceeds 64 bits.
var ErrOverflow = errors.New("lib0: varint overflow")

// ErrInvalidUTF8 reports a string whose bytes are not valid UTF-8. lib0
// reads every string with TextDecoder('utf-8', {fatal: true}), so yjs
// rejects the same bytes.
var ErrInvalidUTF8 = errors.New("lib0: string is not valid UTF-8")

// ReadVarUint reads a LEB128-style varuint from buf and returns the value plus
// the number of bytes consumed.
func ReadVarUint(buf []byte) (uint64, int, error) {
	var n uint64
	var shift uint
	for i, b := range buf {
		if i == 10 && b > 1 {
			return 0, 0, ErrOverflow
		}
		n |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return n, i + 1, nil
		}
		shift += 7
		if shift >= 64 {
			return 0, 0, ErrOverflow
		}
	}
	return 0, 0, ErrTruncated
}

// ReadVarInt reads a lib0-encoded signed varint (zigzag-on-first-byte) and
// returns the value plus the number of bytes consumed.
func ReadVarInt(buf []byte) (int64, int, error) {
	if len(buf) == 0 {
		return 0, 0, ErrTruncated
	}
	first := buf[0]
	negative := first&0x40 != 0
	mag := uint64(first & 0x3f)
	if first&0x80 == 0 {
		return signedFromMagnitude(mag, negative), 1, nil
	}
	rest, consumed, err := ReadVarUint(buf[1:])
	if err != nil {
		return 0, 0, err
	}
	if consumed > 9 {
		return 0, 0, ErrOverflow
	}
	mag |= rest << 6
	return signedFromMagnitude(mag, negative), 1 + consumed, nil
}

func signedFromMagnitude(mag uint64, negative bool) int64 {
	if !negative {
		return int64(mag)
	}
	if mag == 1<<63 {
		return math.MinInt64
	}
	return -int64(mag)
}

// ReadVarString reads a varuint-prefixed UTF-8 string. Bytes that are not
// valid UTF-8 return ErrInvalidUTF8, as lib0 readVarString throws on them; a
// BOM, NUL and noncharacters are valid and kept.
func ReadVarString(buf []byte) (string, int, error) {
	b, end, err := readVarStringBytes(buf)
	if err != nil {
		return "", 0, err
	}
	if !utf8.Valid(b) {
		return "", 0, fmt.Errorf("%w (%d bytes)", ErrInvalidUTF8, len(b))
	}
	return string(b), end, nil
}

// ReadVarStringAsStored reads a varuint-prefixed string without checking
// its encoding. Updates ygo stored before v1.26 can hold strings with
// invalid UTF-8; reading them through this keeps them loading as they
// always did, byte for byte. Use it only for bytes ygo itself stored.
func ReadVarStringAsStored(buf []byte) (string, int, error) {
	b, end, err := readVarStringBytes(buf)
	if err != nil {
		return "", 0, err
	}
	return string(b), end, nil
}

func readVarStringBytes(buf []byte) ([]byte, int, error) {
	length, headerLen, err := ReadVarUint(buf)
	if err != nil {
		return nil, 0, err
	}
	end := headerLen + int(length)
	if end > len(buf) || end < headerLen {
		return nil, 0, ErrTruncated
	}
	return buf[headerLen:end], end, nil
}

// ReadVarUint8Array reads a varuint-prefixed byte slice. The returned slice
// references the input buffer; copy if you need to retain it.
func ReadVarUint8Array(buf []byte) ([]byte, int, error) {
	length, headerLen, err := ReadVarUint(buf)
	if err != nil {
		return nil, 0, err
	}
	end := headerLen + int(length)
	if end > len(buf) || end < headerLen {
		return nil, 0, ErrTruncated
	}
	return buf[headerLen:end], end, nil
}

// ReadUint8 reads a single byte.
func ReadUint8(buf []byte) (uint8, int, error) {
	if len(buf) < 1 {
		return 0, 0, ErrTruncated
	}
	return buf[0], 1, nil
}

// ReadUint16 reads a little-endian uint16.
func ReadUint16(buf []byte) (uint16, int, error) {
	if len(buf) < 2 {
		return 0, 0, ErrTruncated
	}
	return binary.LittleEndian.Uint16(buf), 2, nil
}

// ReadUint32 reads a little-endian uint32.
func ReadUint32(buf []byte) (uint32, int, error) {
	if len(buf) < 4 {
		return 0, 0, ErrTruncated
	}
	return binary.LittleEndian.Uint32(buf), 4, nil
}

// ReadFloat32 reads a big-endian IEEE-754 float32.
func ReadFloat32(buf []byte) (float32, int, error) {
	if len(buf) < 4 {
		return 0, 0, ErrTruncated
	}
	return math.Float32frombits(binary.BigEndian.Uint32(buf)), 4, nil
}

// ReadFloat64 reads a big-endian IEEE-754 float64.
func ReadFloat64(buf []byte) (float64, int, error) {
	if len(buf) < 8 {
		return 0, 0, ErrTruncated
	}
	return math.Float64frombits(binary.BigEndian.Uint64(buf)), 8, nil
}
