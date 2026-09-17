package encoding

import (
	"math"
	"testing"
)

// TestClassifyNumber_MatchesLib0 pins number classification to lib0 writeAny,
// case by case. The expected tags were read off lib0 0.2.117's own output for
// the same numbers (7d = varint, 7c = float32, 7b = float64): 1 7d01, -7 7d47,
// 2147483647 7dbfffffff0f, -2147483648 7ccf000000, 3000000000 7c4f32d05e,
// 1.5 7c3fc00000, 0.1 7b3fb999999999999a, 2^40 7c53800000.
//
// Note -2^31 is a float32, not a varint: lib0 tests |x| <= 0x7FFFFFFF, one short
// of the int32 range, and an equality check against int32 bounds would get it
// wrong.
func TestClassifyNumber_MatchesLib0(t *testing.T) {
	type want struct {
		kind  string // "int64", "float32", "float64"
		value float64
	}
	for _, tc := range []struct {
		in   float64
		want want
	}{
		{1, want{"int64", 1}},
		{-7, want{"int64", -7}},
		{2147483647, want{"int64", 2147483647}},
		{-2147483647, want{"int64", -2147483647}},
		{-2147483648, want{"float32", -2147483648}},
		{3000000000, want{"float32", 3000000000}},
		{1.5, want{"float32", 1.5}},
		{0.1, want{"float64", 0.1}},
		{1 << 40, want{"float32", 1 << 40}},
		{math.Inf(1), want{"float32", math.Inf(1)}},
		{math.Inf(-1), want{"float32", math.Inf(-1)}},
	} {
		got := classifyNumber(tc.in)
		var kind string
		var val float64
		switch x := got.(type) {
		case int64:
			kind, val = "int64", float64(x)
		case float32:
			kind, val = "float32", float64(x)
		case float64:
			kind, val = "float64", x
		default:
			t.Fatalf("classifyNumber(%v) returned %T", tc.in, got)
		}
		if kind != tc.want.kind || val != tc.want.value {
			t.Errorf("classifyNumber(%v) = %s %v, lib0 writes %s %v", tc.in, kind, val, tc.want.kind, tc.want.value)
		}
	}

	// NaN is a float64 in lib0 too (isFloat32 compares with ===, and NaN is
	// not equal to itself).
	if got, ok := classifyNumber(math.NaN()).(float64); !ok || !math.IsNaN(got) {
		t.Errorf("classifyNumber(NaN) = %T %v, want float64 NaN", classifyNumber(math.NaN()), classifyNumber(math.NaN()))
	}

	// Negative zero keeps its sign, as a float64. lib0 writes a signed varint
	// the Any codec cannot express; as an int64 it would silently become +0.
	if got, ok := classifyNumber(math.Copysign(0, -1)).(float64); !ok || !math.Signbit(got) {
		t.Errorf("classifyNumber(-0) = %T %v, want a float64 with its sign bit set", classifyNumber(math.Copysign(0, -1)), classifyNumber(math.Copysign(0, -1)))
	}
}

// TestClassifyNumbers_DoesNotMutate: the value being classified is the
// document's own stored content. Rewriting it in place would change the Go
// types a later reader of the document sees - an attribute stored as float64(1)
// would read back as int64(1) after merely encoding the document.
func TestClassifyNumbers_DoesNotMutate(t *testing.T) {
	inner := []any{float64(1), float64(2.5)}
	in := map[string]any{"n": float64(1), "list": inner, "nested": map[string]any{"k": float64(3)}}

	out := classifyNumbers(in).(map[string]any)

	if _, ok := in["n"].(float64); !ok {
		t.Errorf("input map value rewritten to %T", in["n"])
	}
	if _, ok := inner[0].(float64); !ok {
		t.Errorf("input slice element rewritten to %T", inner[0])
	}
	if _, ok := in["nested"].(map[string]any)["k"].(float64); !ok {
		t.Error("nested input map value rewritten")
	}
	if _, ok := out["n"].(int64); !ok {
		t.Errorf("output not classified: %T", out["n"])
	}
}

// TestJSONValueForAny_CyclicValueDegradesToNull: a caller's value that refers
// back to itself cannot be an Any tree. It used to reach encoding/json, which
// detects the cycle and yields null; walking it as a tree instead recursed until
// the stack ran out, which is fatal rather than a recoverable panic. It must
// degrade to null, as it does on the V1 path.
func TestJSONValueForAny_CyclicValueDegradesToNull(t *testing.T) {
	m := map[string]any{}
	m["self"] = m
	if got := jsonValueForAny(m); got != nil {
		t.Fatalf("cyclic map encoded as %v, want null", got)
	}

	s := []any{nil}
	s[0] = s
	if got := jsonValueForAny(s); got != nil {
		t.Fatalf("cyclic slice encoded as %v, want null", got)
	}
}

// TestClassifyNumbers_NilContainersStayNull: a nil container is JSON null, and
// null in a format attribute means "remove it". Turning it into an empty
// container would set the attribute instead.
func TestClassifyNumbers_NilContainersStayNull(t *testing.T) {
	for name, v := range map[string]any{
		"nil []any":          []any(nil),
		"nil map[string]any": map[string]any(nil),
		"nil []byte":         []byte(nil),
	} {
		if got := classifyNumbers(v); got != nil {
			t.Errorf("%s classified as %#v, want nil", name, got)
		}
	}
	// Empty but non-nil containers are values, not null.
	if got := classifyNumbers([]any{}); got == nil {
		t.Error("an empty, non-nil []any became nil")
	}
}
