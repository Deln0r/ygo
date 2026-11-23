package encoding

import (
	"bytes"
	"testing"

	"github.com/Deln0r/ygo/internal/block"
)

func TestV2_EmptyEncoder_ProducesFeatureFlagAndEmptyColumns(t *testing.T) {
	enc := NewEncoderV2()
	buf := enc.Bytes()

	// Layout for an empty V2 update:
	//   1 byte  feature flag (0x00)
	//   1 byte  keyClock column length (varuint 0)
	//   1 byte  client column length
	//   1 byte  leftClock column length
	//   1 byte  rightClock column length
	//   1 byte  info column length
	//   2 bytes string column = varuint(1) + [0x00] (empty varstring)
	//   1 byte  parentInfo column length
	//   1 byte  typeRef column length
	//   1 byte  len column length
	//   0 bytes rest
	// Total = 11 bytes. yrs's Update::EMPTY_V2 has 13 bytes —
	// the extra two are top-level varuint(0)+varuint(0) for
	// client count + delete set which our encoder doesn't write
	// unless WriteVarUint is called explicitly.
	want := []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00}
	if !bytes.Equal(buf, want) {
		t.Errorf("empty V2 update:\n got  %x\n want %x", buf, want)
	}

	// Round-trip: decoder accepts the empty encoding.
	dec, err := NewDecoderV2(buf)
	if err != nil {
		t.Fatal(err)
	}
	_ = dec
}

// TestV2_FeatureFlag_ReadAndIgnored: yjs's UpdateDecoderV2 reads the
// feature flag as a varuint and ignores it, so a nonzero or overlong flag
// decodes; a flag cut short does not.
func TestV2_FeatureFlag_ReadAndIgnored(t *testing.T) {
	empty := NewEncoderV2().Bytes()
	if empty[0] != 0 {
		t.Fatalf("the encoder's flag is %#x", empty[0])
	}
	for _, flag := range [][]byte{{0x01}, {0x80, 0x00}, {0xff, 0x7f}} {
		if _, err := NewDecoderV2(append(append([]byte(nil), flag...), empty[1:]...)); err != nil {
			t.Errorf("flag %x: %v", flag, err)
		}
	}
	if _, err := NewDecoderV2([]byte{0x80}); err == nil {
		t.Error("a flag cut short should error")
	}
}

func TestV2_ClientColumn_RoundTrip(t *testing.T) {
	enc := NewEncoderV2()
	enc.WriteClient(100)
	enc.WriteClient(100)
	enc.WriteClient(100)
	enc.WriteClient(200)
	wire := enc.Bytes()

	dec, err := NewDecoderV2(wire)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []uint64{100, 100, 100, 200} {
		got, err := dec.ReadClient()
		if err != nil {
			t.Fatalf("ReadClient %d: %v", i, err)
		}
		if got != want {
			t.Errorf("client %d = %d, want %d", i, got, want)
		}
	}
}

func TestV2_LeftRightID_SharedClientColumn(t *testing.T) {
	// WriteLeftID + WriteRightID + WriteClient ALL push to the
	// shared client column. Order matters — decoder pulls in the
	// same order.
	enc := NewEncoderV2()
	enc.WriteLeftID(block.ID{Client: 1, Clock: 10})
	enc.WriteRightID(block.ID{Client: 2, Clock: 20})
	enc.WriteClient(3)
	enc.WriteLeftID(block.ID{Client: 1, Clock: 11}) // delta from prev left clock = +1
	wire := enc.Bytes()

	dec, err := NewDecoderV2(wire)
	if err != nil {
		t.Fatal(err)
	}
	got1, _ := dec.ReadLeftID()
	if got1.Client != 1 || got1.Clock != 10 {
		t.Errorf("left[0] = %+v", got1)
	}
	got2, _ := dec.ReadRightID()
	if got2.Client != 2 || got2.Clock != 20 {
		t.Errorf("right[0] = %+v", got2)
	}
	got3, _ := dec.ReadClient()
	if got3 != 3 {
		t.Errorf("client = %d", got3)
	}
	got4, _ := dec.ReadLeftID()
	if got4.Client != 1 || got4.Clock != 11 {
		t.Errorf("left[1] = %+v", got4)
	}
}

func TestV2_InfoColumn_RleCompression(t *testing.T) {
	// 100 identical info bytes should compress dramatically via
	// the RLE column.
	enc := NewEncoderV2()
	for i := 0; i < 100; i++ {
		enc.WriteInfo(0xAB)
	}
	wire := enc.Bytes()
	// Sanity: total wire should be much less than 100 bytes for the info column.
	if len(wire) > 30 {
		t.Errorf("100 identical info bytes = %d wire bytes, expected RLE compression (~10-20)", len(wire))
	}

	dec, _ := NewDecoderV2(wire)
	for i := 0; i < 100; i++ {
		v, err := dec.ReadInfo()
		if err != nil {
			t.Fatalf("info %d: %v", i, err)
		}
		if v != 0xAB {
			t.Errorf("info %d = %x, want 0xAB", i, v)
		}
	}
}

func TestV2_StringColumn_RoundTrip(t *testing.T) {
	enc := NewEncoderV2()
	enc.WriteString("hello")
	enc.WriteString("world")
	enc.WriteString("")
	enc.WriteString("emoji 🚀 here")
	wire := enc.Bytes()

	dec, _ := NewDecoderV2(wire)
	for i, want := range []string{"hello", "world", "", "emoji 🚀 here"} {
		got, err := dec.ReadString()
		if err != nil {
			t.Fatalf("string %d: %v", i, err)
		}
		if got != want {
			t.Errorf("string %d = %q, want %q", i, got, want)
		}
	}
}

func TestV2_LeftClockDiffRle_MonotonicCompression(t *testing.T) {
	// Sequential clocks for the same client should compress
	// into a single (diff=1, count) record.
	enc := NewEncoderV2()
	for i := uint64(0); i < 50; i++ {
		enc.WriteLeftID(block.ID{Client: 1, Clock: i})
	}
	wire := enc.Bytes()

	dec, _ := NewDecoderV2(wire)
	for i := uint64(0); i < 50; i++ {
		got, err := dec.ReadLeftID()
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if got.Client != 1 || got.Clock != i {
			t.Errorf("read %d = %+v, want {1, %d}", i, got, i)
		}
	}
}

func TestV2_ParentInfo_BooleanColumn(t *testing.T) {
	enc := NewEncoderV2()
	enc.WriteParentInfo(true)
	enc.WriteParentInfo(true)
	enc.WriteParentInfo(false)
	enc.WriteParentInfo(true)
	wire := enc.Bytes()

	dec, _ := NewDecoderV2(wire)
	for i, want := range []bool{true, true, false, true} {
		got, err := dec.ReadParentInfo()
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if got != want {
			t.Errorf("read %d = %v, want %v", i, got, want)
		}
	}
}

func TestV2_TypeRef_Column(t *testing.T) {
	enc := NewEncoderV2()
	enc.WriteTypeRef(0) // Array
	enc.WriteTypeRef(1) // Map
	enc.WriteTypeRef(2) // Text
	enc.WriteTypeRef(3) // XmlElement
	wire := enc.Bytes()

	dec, _ := NewDecoderV2(wire)
	for i, want := range []uint64{0, 1, 2, 3} {
		got, err := dec.ReadTypeRef()
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if got != want {
			t.Errorf("read %d = %d, want %d", i, got, want)
		}
	}
}

func TestV2_LenColumn_RoundTrip(t *testing.T) {
	enc := NewEncoderV2()
	enc.WriteLen(1)
	enc.WriteLen(1) // run of identical
	enc.WriteLen(42)
	enc.WriteLen(1 << 20)
	wire := enc.Bytes()

	dec, _ := NewDecoderV2(wire)
	for i, want := range []uint64{1, 1, 42, 1 << 20} {
		got, err := dec.ReadLen()
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if got != want {
			t.Errorf("read %d = %d, want %d", i, got, want)
		}
	}
}

func TestV2_RestStream_VarUintAndAny(t *testing.T) {
	// Top-level headers + Any payloads go through the rest stream.
	enc := NewEncoderV2()
	enc.WriteVarUint(7)
	enc.WriteVarUint(42)
	enc.WriteAny(block.Any("hello"))
	wire := enc.Bytes()

	dec, _ := NewDecoderV2(wire)
	v1, _ := dec.ReadVarUint()
	v2, _ := dec.ReadVarUint()
	if v1 != 7 || v2 != 42 {
		t.Errorf("rest varuints = %d, %d, want 7, 42", v1, v2)
	}
	a, err := dec.ReadAny()
	if err != nil {
		t.Fatal(err)
	}
	if a.(string) != "hello" {
		t.Errorf("any = %v, want hello", a)
	}
}

func TestV2_WriteKey_PopulatesDecoderKeyTable(t *testing.T) {
	// Encoder bug (per port-note gotcha 5): keyMap never
	// populated, every WriteKey writes a fresh string +
	// increments keyClock. Decoder DOES populate keys[] —
	// asymmetric but wire-compatible. Read with a re-used
	// keyClock returns the cached string.
	enc := NewEncoderV2()
	enc.WriteKey("color") // keyClock = 0
	enc.WriteKey("size")  // keyClock = 1
	enc.WriteKey("color") // keyClock = 2 (asymmetric — encoder doesn't dedupe)
	wire := enc.Bytes()

	dec, _ := NewDecoderV2(wire)
	k1, _ := dec.ReadKey()
	k2, _ := dec.ReadKey()
	k3, _ := dec.ReadKey()
	if k1 != "color" || k2 != "size" || k3 != "color" {
		t.Errorf("keys = %q, %q, %q; want color, size, color", k1, k2, k3)
	}
}

func TestV2_FullColumnRoundTrip(t *testing.T) {
	// Exercises every column in a realistic block-write order.
	enc := NewEncoderV2()

	// Pretend we're writing one block with all fields.
	enc.WriteInfo(0x80)                               // info column
	enc.WriteLeftID(block.ID{Client: 100, Clock: 5})  // client + leftClock
	enc.WriteRightID(block.ID{Client: 100, Clock: 7}) // client + rightClock
	enc.WriteParentInfo(true)                         // parentInfo column
	enc.WriteString("settings")                       // string column
	enc.WriteKey("color")                             // keyClock + string
	enc.WriteTypeRef(1)                               // typeRef column
	enc.WriteLen(1)                                   // len column
	enc.WriteAny(block.Any("red"))                    // rest column
	enc.WriteVarUint(42)                              // rest column (header)

	wire := enc.Bytes()

	dec, err := NewDecoderV2(wire)
	if err != nil {
		t.Fatal(err)
	}

	if got, _ := dec.ReadInfo(); got != 0x80 {
		t.Errorf("info = %#x", got)
	}
	if got, _ := dec.ReadLeftID(); got.Client != 100 || got.Clock != 5 {
		t.Errorf("left = %+v", got)
	}
	if got, _ := dec.ReadRightID(); got.Client != 100 || got.Clock != 7 {
		t.Errorf("right = %+v", got)
	}
	if got, _ := dec.ReadParentInfo(); !got {
		t.Errorf("parentInfo = false")
	}
	if got, _ := dec.ReadString(); got != "settings" {
		t.Errorf("string = %q", got)
	}
	if got, _ := dec.ReadKey(); got != "color" {
		t.Errorf("key = %q", got)
	}
	if got, _ := dec.ReadTypeRef(); got != 1 {
		t.Errorf("typeRef = %d", got)
	}
	if got, _ := dec.ReadLen(); got != 1 {
		t.Errorf("len = %d", got)
	}
	got, err := dec.ReadAny()
	if err != nil {
		t.Fatal(err)
	}
	if got != "red" {
		t.Errorf("any = %v", got)
	}
	if v, _ := dec.ReadVarUint(); v != 42 {
		t.Errorf("varuint = %d", v)
	}
}

func TestV2_VarBuf_RoundTrip(t *testing.T) {
	enc := NewEncoderV2()
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	enc.WriteVarBuf(payload)
	wire := enc.Bytes()

	dec, _ := NewDecoderV2(wire)
	got, err := dec.ReadVarBuf()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("VarBuf = %x, want %x", got, payload)
	}
}

func TestV2_JSON_RoundTrip(t *testing.T) {
	enc := NewEncoderV2()
	enc.WriteJSON(map[string]any{"bold": true})
	wire := enc.Bytes()

	dec, _ := NewDecoderV2(wire)
	got, err := dec.ReadJSON()
	if err != nil {
		t.Fatal(err)
	}
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("got %T, want map[string]any", got)
	}
	if m["bold"] != true {
		t.Errorf("bold = %v, want true", m["bold"])
	}
}
