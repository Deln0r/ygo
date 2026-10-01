package encoding

import (
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestAny_RoundTrip_BasicTypes(t *testing.T) {
	cases := []any{
		nil,
		true,
		false,
		"",
		"hello",
		"привет",
		int64(0),
		int64(1),
		int64(-1),
		int64(math.MaxInt32),
		int64(-math.MaxInt32),
		float64(0),
		float64(3.14),
		float64(-0.5),
		math.MaxFloat64,
	}
	for _, in := range cases {
		buf := EncodeAny(nil, in)
		out, tail, err := DecodeAny(buf)
		if err != nil {
			t.Errorf("decode %v: %v", in, err)
			continue
		}
		if len(tail) != 0 {
			t.Errorf("trailing bytes after decoding %v: % x", in, tail)
		}
		if !reflect.DeepEqual(out, in) {
			t.Errorf("round-trip %v (%T): got %v (%T)", in, in, out, out)
		}
	}
}

func TestAny_LargeIntegerPromotesToFloat64(t *testing.T) {
	// Outside lib0's BITS31 range an integer is written as a float, and
	// decodes as a float64.
	in := int64(math.MaxInt32) + 2
	buf := EncodeAny(nil, in)
	if buf[0] != AnyTagFloat64 {
		t.Errorf("expected Float64 tag (123), got %d", buf[0])
	}
	out, _, err := DecodeAny(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, ok := out.(float64); !ok || got != float64(in) {
		t.Errorf("got %v (%T), want %v as float64", out, out, float64(in))
	}
}

// TestAny_NumbersAsLib0Writes compares EncodeAny with the bytes lib0
// 0.2.118 writeAny produces for the same number (captured with node from
// testdata/gen). The one class that differs on purpose is an integral
// float64 within int31, which lib0 writes as a varint and ygo keeps a
// float64 so that it decodes back as one.
func TestAny_NumbersAsLib0Writes(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{int64(0x7FFFFFFF), "7dbfffffff0f"},
		{int64(-0x7FFFFFFF), "7dffffffff0f"},
		{int64(-0x80000000), "7ccf000000"},
		{int64(0x80000000), "7c4f000000"},
		{int64(0x80000001), "7b41e0000000200000"},
		{int64(1) << 40, "7c53800000"},
		{2.5, "7c40200000"},
		{-0.5, "7cbf000000"},
		{0.1, "7b3fb999999999999a"},
		{math.Inf(1), "7c7f800000"},
		{math.Float64frombits(0x7FF8000000000000), "7b7ff8000000000000"},
		{math.Float64frombits(0x7FF8000000000001), "7b7ff8000000000001"}, // lib0 keeps a decoded NaN's bits
		{float64(3), "7b4008000000000000"},                               // lib0: 7d03
	}
	for _, c := range cases {
		if got := hex.EncodeToString(EncodeAny(nil, c.in)); got != c.want {
			t.Errorf("EncodeAny(%v %T) = %s, want %s", c.in, c.in, got, c.want)
		}
	}
}

func TestAny_DecodeTruncated(t *testing.T) {
	if _, _, err := DecodeAny(nil); err == nil {
		t.Error("decode of empty input must error")
	}
}

func TestAny_RoundTrip_Float32(t *testing.T) {
	buf := EncodeAny(nil, float32(2.5))
	if buf[0] != AnyTagFloat32 {
		t.Errorf("expected Float32 tag (124), got %d", buf[0])
	}
	out, tail, err := DecodeAny(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(tail) != 0 {
		t.Errorf("trailing bytes: % x", tail)
	}
	// Decoder widens to float64; both 2.5 representations are exact.
	if got, ok := out.(float64); !ok || got != 2.5 {
		t.Errorf("got %v (%T), want 2.5", out, out)
	}
}

func TestAny_RoundTrip_Binary(t *testing.T) {
	in := []byte{0x00, 0x01, 0xff, 0xab, 0xcd}
	buf := EncodeAny(nil, in)
	if buf[0] != AnyTagBinary {
		t.Errorf("expected Binary tag (116), got %d", buf[0])
	}
	out, tail, err := DecodeAny(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(tail) != 0 {
		t.Errorf("trailing bytes: % x", tail)
	}
	got, ok := out.([]byte)
	if !ok {
		t.Fatalf("got %T, want []byte", out)
	}
	if !reflect.DeepEqual(got, in) {
		t.Errorf("got % x, want % x", got, in)
	}
	// Decoded slice must not alias buf.
	buf[1] = 0xaa
	if got[0] == 0xaa {
		t.Error("decoded slice aliases input buffer")
	}
}

func TestAny_RoundTrip_Array(t *testing.T) {
	in := []any{
		"hello",
		int64(42),
		true,
		nil,
		float64(3.14),
		[]any{int64(1), int64(2)},
	}
	buf := EncodeAny(nil, in)
	if buf[0] != AnyTagArray {
		t.Errorf("expected Array tag (117), got %d", buf[0])
	}
	out, tail, err := DecodeAny(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(tail) != 0 {
		t.Errorf("trailing bytes: % x", tail)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("got %v, want %v", out, in)
	}
}

func TestAny_RoundTrip_Object(t *testing.T) {
	in := map[string]any{
		"name":    "yjs",
		"version": int64(13),
		"flags":   map[string]any{"strict": true},
		"counts":  []any{int64(1), int64(2), int64(3)},
		"nullval": nil,
	}
	buf := EncodeAny(nil, in)
	if buf[0] != AnyTagObject {
		t.Errorf("expected Object tag (118), got %d", buf[0])
	}
	out, tail, err := DecodeAny(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(tail) != 0 {
		t.Errorf("trailing bytes: % x", tail)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("got %v, want %v", out, in)
	}
}

func TestAny_DecodeBigInt(t *testing.T) {
	// 8-byte BE int64. Decoder must accept; encoder side does not
	// currently emit BigInt (callers use int64/int → integer/float64).
	// JS Yjs payloads containing BigInt must still round-trip on Go.
	in := []byte{AnyTagBigInt, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x2a}
	out, tail, err := DecodeAny(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(tail) != 0 {
		t.Errorf("trailing bytes: % x", tail)
	}
	if got, ok := out.(int64); !ok || got != 42 {
		t.Errorf("got %v (%T), want int64(42)", out, out)
	}
}

func TestAny_ObjectDeterministicKeyOrder(t *testing.T) {
	// Go map iteration is randomized; sorted-keys policy makes wire
	// bytes reproducible. Run twice and assert byte-equal output.
	in := map[string]any{"c": int64(3), "a": int64(1), "b": int64(2)}
	a := EncodeAny(nil, in)
	b := EncodeAny(nil, in)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("non-deterministic encoding:\nfirst:  % x\nsecond: % x", a, b)
	}
}

func TestAny_UnknownTagStillErrors(t *testing.T) {
	// Tag 0 is not a valid Any TLV variant; decoder must error.
	_, _, err := DecodeAny([]byte{0x00})
	if !errors.Is(err, ErrUnsupportedAnyTag) {
		t.Errorf("err = %v, want ErrUnsupportedAnyTag", err)
	}
}
