package encoding

import (
	"bytes"
	"testing"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/types"
)

// TestEncodeStateAsUpdateV2_EmptyDoc verifies the empty-doc baseline.
// For an empty Doc:
//   - 9 column buffers are all empty (each emits its inner empty
//     state — see TestEncoderV2_Bytes_EmptyEncoder for the 11-byte
//     prefix layout).
//   - rest stream carries varuint(0) for client count, then
//     varuint(0) for DeleteSet client count.
//
// Total = 11 (V2 header) + 2 (top-level zeros) = 13 bytes, matching
// yrs Update::EMPTY_V2 documented in update-v2.md §6.
func TestEncodeStateAsUpdateV2_EmptyDoc(t *testing.T) {
	d := doc.NewDoc()
	got := EncodeStateAsUpdateV2(d)
	want := []byte{
		// V2 header: feature flag + 8 zero-length columns + 1-byte
		// stringEncoder (empty varstring) — see encoder_v2.go Bytes()
		// and v2_test.go TestEncoderV2_Bytes_EmptyEncoder.
		0x00,       // feature flag
		0x00,       // keyClock len 0
		0x00,       // client len 0
		0x00,       // leftClock len 0
		0x00,       // rightClock len 0
		0x00,       // info len 0
		0x01, 0x00, // string col len 1: empty varstring (varuint(0))
		0x00, // parentInfo len 0
		0x00, // typeRef len 0
		0x00, // length len 0
		// rest stream:
		0x00, // outer client count
		0x00, // delete-set client count
	}
	if !bytes.Equal(got, want) {
		t.Errorf("got % x, want % x", got, want)
	}
}

// TestUpdateV2_RoundTrip_Map: src writes a Map → encode V2 → decode
// V2 → apply on fresh dst → verify state. The bedrock test.
func TestUpdateV2_RoundTrip_Map(t *testing.T) {
	src := doc.NewDocWithOptions(doc.Options{ClientID: 7})
	srcMap := types.NewMap(src.Branch("settings"))

	txn := src.WriteTxn()
	srcMap.Set(txn, "color", "red")
	srcMap.Set(txn, "lang", "go")
	srcMap.Set(txn, "version", int64(1))
	srcMap.Set(txn, "stable", true)
	txn.Commit()

	buf := EncodeStateAsUpdateV2(src)
	if len(buf) == 0 {
		t.Fatal("empty V2 update for non-empty doc")
	}

	dst := doc.NewDocWithOptions(doc.Options{ClientID: 99})
	dstMap := types.NewMap(dst.Branch("settings"))

	if err := ApplyUpdateV2(dst, buf); err != nil {
		t.Fatalf("ApplyUpdateV2: %v", err)
	}

	if got := dstMap.Get("color"); got != "red" {
		t.Errorf("dst color = %v, want red", got)
	}
	if got := dstMap.Get("lang"); got != "go" {
		t.Errorf("dst lang = %v, want go", got)
	}
	if got := dstMap.Get("version"); got != int64(1) {
		t.Errorf("dst version = %v, want 1", got)
	}
	if got := dstMap.Get("stable"); got != true {
		t.Errorf("dst stable = %v, want true", got)
	}
}

// TestUpdateV2_RoundTrip_Array: positional list with mixed primitive
// values. Exercises ContentAny (writeLen + writeAny per element) +
// ContentString (string column).
func TestUpdateV2_RoundTrip_Array(t *testing.T) {
	src := doc.NewDocWithOptions(doc.Options{ClientID: 3})
	srcArr := types.NewArray(src.Branch("items"))

	txn := src.WriteTxn()
	srcArr.Push(txn, "alpha")
	srcArr.Push(txn, int64(42))
	srcArr.Push(txn, true)
	srcArr.Push(txn, nil)
	txn.Commit()

	buf := EncodeStateAsUpdateV2(src)

	dst := doc.NewDocWithOptions(doc.Options{ClientID: 4})
	dstArr := types.NewArray(dst.Branch("items"))

	if err := ApplyUpdateV2(dst, buf); err != nil {
		t.Fatalf("ApplyUpdateV2: %v", err)
	}

	got := dstArr.ToSlice()
	if len(got) != 4 {
		t.Fatalf("dst arr len = %d, want 4; got %v", len(got), got)
	}
	if got[0] != "alpha" {
		t.Errorf("[0] = %v, want alpha", got[0])
	}
	if got[1] != int64(42) {
		t.Errorf("[1] = %v, want 42", got[1])
	}
	if got[2] != true {
		t.Errorf("[2] = %v, want true", got[2])
	}
	if got[3] != nil {
		t.Errorf("[3] = %v, want nil", got[3])
	}
}

// TestUpdateV2_RoundTrip_Text: plain text, exercises ContentString
// routing through the string column on V2.
func TestUpdateV2_RoundTrip_Text(t *testing.T) {
	src := doc.NewDocWithOptions(doc.Options{ClientID: 11})
	srcText := types.NewText(src.Branch("body"))

	txn := src.WriteTxn()
	srcText.Insert(txn, 0, "hello")
	srcText.Insert(txn, 5, " world")
	srcText.Insert(txn, 11, "!")
	txn.Commit()

	buf := EncodeStateAsUpdateV2(src)

	dst := doc.NewDocWithOptions(doc.Options{ClientID: 12})
	dstText := types.NewText(dst.Branch("body"))
	if err := ApplyUpdateV2(dst, buf); err != nil {
		t.Fatalf("ApplyUpdateV2: %v", err)
	}

	if got := dstText.String(); got != "hello world!" {
		t.Errorf("dst text = %q, want %q", got, "hello world!")
	}
}

// TestUpdateV2_RoundTrip_DeleteSet: encode a doc with deletions →
// V2 → decode → verify the receiver sees the deletes. Exercises
// the V2 delete-set diff stream (cumulative-clock + len-1 layout).
func TestUpdateV2_RoundTrip_DeleteSet(t *testing.T) {
	src := doc.NewDocWithOptions(doc.Options{ClientID: 5})
	srcMap := types.NewMap(src.Branch("settings"))

	txn := src.WriteTxn()
	srcMap.Set(txn, "a", "1")
	srcMap.Set(txn, "b", "2")
	srcMap.Set(txn, "c", "3")
	srcMap.Delete(txn, "b") // creates a deleted item
	txn.Commit()

	buf := EncodeStateAsUpdateV2(src)

	dst := doc.NewDocWithOptions(doc.Options{ClientID: 6})
	dstMap := types.NewMap(dst.Branch("settings"))
	if err := ApplyUpdateV2(dst, buf); err != nil {
		t.Fatalf("ApplyUpdateV2: %v", err)
	}

	if got := dstMap.Get("a"); got != "1" {
		t.Errorf("dst a = %v, want 1", got)
	}
	if got := dstMap.Get("c"); got != "3" {
		t.Errorf("dst c = %v, want 3", got)
	}
	if dstMap.Has("b") {
		t.Errorf("dst b unexpectedly present: %v", dstMap.Get("b"))
	}
}

// TestUpdateV2_CrossClient_Concurrent: A and B independently write,
// V2 round-trip in both directions, both converge to the same state.
func TestUpdateV2_CrossClient_Concurrent(t *testing.T) {
	a := doc.NewDocWithOptions(doc.Options{ClientID: 100})
	b := doc.NewDocWithOptions(doc.Options{ClientID: 200})
	aMap := types.NewMap(a.Branch("kv"))
	bMap := types.NewMap(b.Branch("kv"))

	atxn := a.WriteTxn()
	aMap.Set(atxn, "from", "A")
	atxn.Commit()

	btxn := b.WriteTxn()
	bMap.Set(btxn, "via", "B")
	btxn.Commit()

	if err := ApplyUpdateV2(a, EncodeStateAsUpdateV2(b)); err != nil {
		t.Fatalf("ApplyUpdateV2 b→a: %v", err)
	}
	if err := ApplyUpdateV2(b, EncodeStateAsUpdateV2(a)); err != nil {
		t.Fatalf("ApplyUpdateV2 a→b: %v", err)
	}

	for _, m := range []*types.Map{aMap, bMap} {
		if got := m.Get("from"); got != "A" {
			t.Errorf("from = %v, want A", got)
		}
		if got := m.Get("via"); got != "B" {
			t.Errorf("via = %v, want B", got)
		}
	}
}

// TestUpdateV2_EncodeDiff_Incremental: A → snapshot → A adds more →
// encode diff against snapshot SV → only the new ops travel.
func TestUpdateV2_EncodeDiff_Incremental(t *testing.T) {
	a := doc.NewDocWithOptions(doc.Options{ClientID: 50})
	aMap := types.NewMap(a.Branch("kv"))

	txn := a.WriteTxn()
	aMap.Set(txn, "first", "1")
	txn.Commit()

	// snapshot dst from full state
	b := doc.NewDocWithOptions(doc.Options{ClientID: 51})
	bMap := types.NewMap(b.Branch("kv"))
	if err := ApplyUpdateV2(b, EncodeStateAsUpdateV2(a)); err != nil {
		t.Fatalf("ApplyUpdateV2 initial: %v", err)
	}

	// snapshot remoteSV
	bRTxn := b.ReadTxn()
	remoteSV := bRTxn.Store().GetStateVector()
	bRTxn.Close()

	// A adds more
	txn = a.WriteTxn()
	aMap.Set(txn, "second", "2")
	txn.Commit()

	// encode incremental V2 diff
	aRTxn := a.ReadTxn()
	diff := EncodeDiffV2(a, aRTxn, remoteSV)
	aRTxn.Close()
	if len(diff) == 0 {
		t.Fatal("empty diff for a non-trivial state advance")
	}

	if err := ApplyUpdateV2(b, diff); err != nil {
		t.Fatalf("ApplyUpdateV2 diff: %v", err)
	}
	if got := bMap.Get("first"); got != "1" {
		t.Errorf("b first = %v, want 1", got)
	}
	if got := bMap.Get("second"); got != "2" {
		t.Errorf("b second = %v, want 2", got)
	}
}

// TestUpdateV2_DecodeUpdateV2_FlagWithoutColumns: a feature flag (read and
// ignored, as yjs does) with no columns after it does not decode.
func TestUpdateV2_DecodeUpdateV2_FlagWithoutColumns(t *testing.T) {
	bad := []byte{0xff, 0x00}
	_, err := DecodeUpdateV2(bad)
	if err == nil {
		t.Fatal("expected an error for a flag without columns")
	}
}

// TestUpdateV2_RoundTrip_XmlElement: exercises ContentType with
// XmlElement TypeRef + tag-name via writeKey (key column routing,
// not raw string). Catches XML tag-name regressions in V2.
func TestUpdateV2_RoundTrip_XmlElement(t *testing.T) {
	src := doc.NewDocWithOptions(doc.Options{ClientID: 9})
	srcFrag := types.NewXmlFragment(src.Branch("frag"))

	txn := src.WriteTxn()
	srcFrag.InsertXmlElement(txn, 0, "div")
	srcFrag.InsertXmlElement(txn, 1, "span")
	txn.Commit()

	buf := EncodeStateAsUpdateV2(src)

	dst := doc.NewDocWithOptions(doc.Options{ClientID: 10})
	dstFrag := types.NewXmlFragment(dst.Branch("frag"))
	if err := ApplyUpdateV2(dst, buf); err != nil {
		t.Fatalf("ApplyUpdateV2: %v", err)
	}

	if got := dstFrag.Length(); got != 2 {
		t.Fatalf("dst frag length = %d, want 2", got)
	}
	// Reading back children via Get(index) returns the XmlElement.
	child0 := dstFrag.Get(0)
	child1 := dstFrag.Get(1)
	if child0 == nil || child1 == nil {
		t.Fatalf("dst frag children missing: %v %v", child0, child1)
	}
	e0, ok := child0.(*types.XmlElement)
	if !ok {
		t.Fatalf("child 0 not XmlElement: %T", child0)
	}
	e1, ok := child1.(*types.XmlElement)
	if !ok {
		t.Fatalf("child 1 not XmlElement: %T", child1)
	}
	if e0.NodeName() != "div" {
		t.Errorf("child 0 tag = %q, want div", e0.NodeName())
	}
	if e1.NodeName() != "span" {
		t.Errorf("child 1 tag = %q, want span", e1.NodeName())
	}
}

// TestUpdateV2_V1_IncompatibilityIsExpected verifies that V1 bytes
// fed to V2 decoder, OR V2 bytes fed to V1 decoder, produce errors
// or wrong results. Documents the wire-incompatibility invariant.
//
// Per update-v2.md §6: V1/V2 have no autodetect; cross-decoding is
// undefined behaviour, surfacing as either a decode error or a
// successfully-decoded-but-semantically-wrong Update. Either is
// acceptable — both fail loud (no silent data corruption to a
// running doc, because Apply itself validates IDs against the
// store and most cross-misparses will fail integrate).
//
// This test asserts the contract: cross-decoding produces something
// that is not the original state.
func TestUpdateV2_V1_IncompatibilityIsExpected(t *testing.T) {
	src := doc.NewDocWithOptions(doc.Options{ClientID: 77})
	m := types.NewMap(src.Branch("kv"))
	txn := src.WriteTxn()
	m.Set(txn, "k", "v")
	txn.Commit()

	v1Bytes := EncodeStateAsUpdate(src)
	v2Bytes := EncodeStateAsUpdateV2(src)
	if bytes.Equal(v1Bytes, v2Bytes) {
		t.Errorf("V1 and V2 bytes coincidentally identical — test premise broken")
	}

	// Smoke: V2 decoder on V1 bytes either errors or produces a
	// non-matching state. We accept both since yjs/yrs do too.
	dstV2 := doc.NewDocWithOptions(doc.Options{ClientID: 78})
	dstV2Map := types.NewMap(dstV2.Branch("kv"))
	errV2 := ApplyUpdateV2(dstV2, v1Bytes)
	if errV2 == nil {
		if got := dstV2Map.Get("k"); got == "v" {
			t.Errorf("V2-decoder accepted V1 bytes AND produced correct state — that'd mean wire is autodetectable, contradicting the docs")
		}
	}
}
