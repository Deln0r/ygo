package lib0

import (
	"bytes"
	"math"
	"testing"
)

func TestWriteVarUint_KnownValues(t *testing.T) {
	cases := []struct {
		in   uint64
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{255, []byte{0xff, 0x01}},
		{16383, []byte{0xff, 0x7f}},
		{16384, []byte{0x80, 0x80, 0x01}},
		{math.MaxUint32, []byte{0xff, 0xff, 0xff, 0xff, 0x0f}},
		{math.MaxUint64, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}},
	}
	for _, tc := range cases {
		got := WriteVarUint(nil, tc.in)
		if !bytes.Equal(got, tc.want) {
			t.Errorf("WriteVarUint(%d) = % x, want % x", tc.in, got, tc.want)
		}
	}
}

func TestReadVarUint_RoundTrip(t *testing.T) {
	values := []uint64{
		0, 1, 2, 63, 64, 127, 128, 255, 256,
		16383, 16384, 1 << 20, 1 << 28, 1 << 32, 1 << 40,
		math.MaxUint32, math.MaxUint64 - 1, math.MaxUint64,
	}
	for _, v := range values {
		buf := WriteVarUint(nil, v)
		got, n, err := ReadVarUint(buf)
		if err != nil {
			t.Fatalf("ReadVarUint(%d): %v", v, err)
		}
		if n != len(buf) {
			t.Errorf("ReadVarUint(%d) consumed %d bytes, encoded %d", v, n, len(buf))
		}
		if got != v {
			t.Errorf("ReadVarUint(%d) = %d", v, got)
		}
	}
}

func TestReadVarUint_Truncated(t *testing.T) {
	if _, _, err := ReadVarUint(nil); err != ErrTruncated {
		t.Errorf("empty input: got %v, want ErrTruncated", err)
	}
	if _, _, err := ReadVarUint([]byte{0x80}); err != ErrTruncated {
		t.Errorf("hanging continuation: got %v, want ErrTruncated", err)
	}
}

func TestReadVarUint_Overflow(t *testing.T) {
	overflow := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}
	if _, _, err := ReadVarUint(overflow); err != ErrOverflow {
		t.Errorf("oversized varuint: got %v, want ErrOverflow", err)
	}
	// 2^64+1 in ten bytes: the tenth byte's bit 1 is bit 64, which wrapped
	// to 1 before.
	past := []byte{0x81, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x02}
	if v, _, err := ReadVarUint(past); err != ErrOverflow {
		t.Errorf("2^64+1: got %d, %v, want ErrOverflow", v, err)
	}
	max := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}
	if v, n, err := ReadVarUint(max); err != nil || v != math.MaxUint64 || n != 10 {
		t.Errorf("2^64-1: got %d, %d, %v", v, n, err)
	}
}

func TestReadVarInt_Overflow(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		// 6 bits, then 2^58 above them: bit 64.
		{"past 64 bits", []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x04}},
		// 2^63 (2^57 above the 6 bits), positive: past an int64.
		{"2^63", []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x02}},
		// 2^63+1, negative: past an int64.
		{"-(2^63+1)", []byte{0xc1, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x02}},
	} {
		if v, _, err := ReadVarInt(tc.in); err != ErrOverflow {
			t.Errorf("%s: got %d, %v, want ErrOverflow", tc.name, v, err)
		}
	}
	minInt := []byte{0xc0, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x02}
	if v, _, err := ReadVarInt(minInt); err != nil || v != math.MinInt64 {
		t.Errorf("-2^63: got %d, %v", v, err)
	}
}

func TestWriteVarInt_KnownValues(t *testing.T) {
	cases := []struct {
		in   int64
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{-1, []byte{0x41}},
		{63, []byte{0x3f}},
		{-63, []byte{0x7f}},
		{64, []byte{0x80, 0x01}},
		{-64, []byte{0xc0, 0x01}},
		{8191, []byte{0xbf, 0x7f}},
		{-8191, []byte{0xff, 0x7f}},
		{8192, []byte{0x80, 0x80, 0x01}},
		{-8192, []byte{0xc0, 0x80, 0x01}},
	}
	for _, tc := range cases {
		got := WriteVarInt(nil, tc.in)
		if !bytes.Equal(got, tc.want) {
			t.Errorf("WriteVarInt(%d) = % x, want % x", tc.in, got, tc.want)
		}
	}
}

func TestReadVarInt_RoundTrip(t *testing.T) {
	values := []int64{
		0, 1, -1, 63, -63, 64, -64, 127, -127, 128, -128,
		8191, -8191, 8192, -8192,
		1 << 20, -(1 << 20), 1 << 30, -(1 << 30),
		math.MaxInt32, math.MinInt32,
		math.MaxInt64, math.MinInt64,
	}
	for _, v := range values {
		buf := WriteVarInt(nil, v)
		got, n, err := ReadVarInt(buf)
		if err != nil {
			t.Fatalf("ReadVarInt(%d): %v", v, err)
		}
		if n != len(buf) {
			t.Errorf("ReadVarInt(%d) consumed %d bytes, encoded %d", v, n, len(buf))
		}
		if got != v {
			t.Errorf("ReadVarInt(%d) = %d", v, got)
		}
	}
}

func TestVarString_RoundTrip(t *testing.T) {
	cases := []string{
		"",
		"a",
		"hello",
		"привет",
		"\x00\x01\x02",
		string(make([]byte, 200)),
	}
	for _, s := range cases {
		buf := WriteVarString(nil, s)
		got, n, err := ReadVarString(buf)
		if err != nil {
			t.Fatalf("ReadVarString(%q): %v", s, err)
		}
		if n != len(buf) {
			t.Errorf("ReadVarString(%q) consumed %d bytes, encoded %d", s, n, len(buf))
		}
		if got != s {
			t.Errorf("ReadVarString(%q) = %q", s, got)
		}
	}
}

func TestVarUint8Array_RoundTrip(t *testing.T) {
	cases := [][]byte{
		nil,
		{},
		{0x01},
		{0x00, 0x01, 0x02, 0x03, 0xff},
		bytes.Repeat([]byte{0xab}, 1024),
	}
	for _, b := range cases {
		buf := WriteVarUint8Array(nil, b)
		got, n, err := ReadVarUint8Array(buf)
		if err != nil {
			t.Fatalf("ReadVarUint8Array(% x): %v", b, err)
		}
		if n != len(buf) {
			t.Errorf("consumed %d bytes, encoded %d", n, len(buf))
		}
		if !bytes.Equal(got, b) {
			t.Errorf("got % x, want % x", got, b)
		}
	}
}

func TestFloats_RoundTrip(t *testing.T) {
	for _, f := range []float32{0, 1, -1, math.MaxFloat32, math.SmallestNonzeroFloat32} {
		buf := WriteFloat32(nil, f)
		got, n, err := ReadFloat32(buf)
		if err != nil || n != 4 || got != f {
			t.Errorf("float32 %v: got %v %d %v", f, got, n, err)
		}
	}
	for _, f := range []float64{0, 1, -1, math.MaxFloat64, math.SmallestNonzeroFloat64} {
		buf := WriteFloat64(nil, f)
		got, n, err := ReadFloat64(buf)
		if err != nil || n != 8 || got != f {
			t.Errorf("float64 %v: got %v %d %v", f, got, n, err)
		}
	}
}

func TestFixedUints_RoundTrip(t *testing.T) {
	for _, n := range []uint8{0, 1, 127, 255} {
		buf := WriteUint8(nil, n)
		got, _, err := ReadUint8(buf)
		if err != nil || got != n {
			t.Errorf("uint8 %d: got %d %v", n, got, err)
		}
	}
	for _, n := range []uint16{0, 1, 256, math.MaxUint16} {
		buf := WriteUint16(nil, n)
		got, _, err := ReadUint16(buf)
		if err != nil || got != n {
			t.Errorf("uint16 %d: got %d %v", n, got, err)
		}
	}
	for _, n := range []uint32{0, 1, 1 << 16, math.MaxUint32} {
		buf := WriteUint32(nil, n)
		got, _, err := ReadUint32(buf)
		if err != nil || got != n {
			t.Errorf("uint32 %d: got %d %v", n, got, err)
		}
	}
}

func FuzzVarUintRoundTrip(f *testing.F) {
	for _, seed := range []uint64{0, 1, 127, 128, 16384, math.MaxUint32, math.MaxUint64} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, v uint64) {
		buf := WriteVarUint(nil, v)
		got, n, err := ReadVarUint(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if n != len(buf) {
			t.Fatalf("partial read: %d/%d", n, len(buf))
		}
		if got != v {
			t.Fatalf("roundtrip: %d -> %d", v, got)
		}
	})
}

func FuzzVarIntRoundTrip(f *testing.F) {
	for _, seed := range []int64{0, 1, -1, 63, -63, 64, -64, math.MaxInt32, math.MinInt32, math.MaxInt64, math.MinInt64} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, v int64) {
		buf := WriteVarInt(nil, v)
		got, n, err := ReadVarInt(buf)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if n != len(buf) {
			t.Fatalf("partial read: %d/%d", n, len(buf))
		}
		if got != v {
			t.Fatalf("roundtrip: %d -> %d", v, got)
		}
	})
}
