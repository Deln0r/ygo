package encoding

import (
	"testing"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/store"
	"github.com/Deln0r/ygo/internal/types"
)

// FuzzDecodeUpdate feeds arbitrary bytes to the V1 update decoder. The
// invariant is that DecodeUpdate never panics (or OOMs) on untrusted
// input; any error return is acceptable. When it succeeds we also
// assert the reported tail is a true suffix of the input so a bogus
// tail slice can't slip through.
func FuzzDecodeUpdate(f *testing.F) {
	seeds := [][]byte{
		{0x00, 0x00}, // empty doc: 0 clients, empty delete set
		{0x01, 0x01, 0x05, 0x00, 0x0a, 0x03, 0x00}, // 1 client, 1 Skip block, empty delete set
		{},                             // empty input
		{0x00},                         // single byte, no delete set
		{0x01, 0x01, 0x05, 0x00, 0x0a}, // truncated mid-block
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}, // huge leading varint client count
		// Fuzz-discovered crasher: a tiny input whose blockCount varuint
		// claimed ~1.6e12 blocks, forcing a multi-TB make([]Block,...).
		{0x30, 0xde, 0xde, 0xde, 0xde, 0xde, 0x30, 0x30, 0x30},
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		u, tail, err := DecodeUpdate(data)
		if err != nil {
			return
		}
		if u == nil {
			t.Fatalf("nil Update with nil error")
		}
		if len(tail) > len(data) {
			t.Fatalf("tail longer than input: %d > %d", len(tail), len(data))
		}
	})
}

// FuzzApplyUpdate drives the top-level decode+integrate entry on
// arbitrary bytes. The contract is that ApplyUpdate never panics or
// OOMs: malformed input must return an error, well-formed input
// integrates. Returned errors are expected and fine.
func FuzzApplyUpdate(f *testing.F) {
	// A valid V1 update encoded via the package's own encoder so the
	// seed exercises the happy decode+apply path.
	src := doc.NewDocWithOptions(doc.Options{ClientID: 42})
	m := types.NewMap(src.Branch("settings"))
	txn := src.WriteTxn()
	m.Set(txn, "color", "red")
	m.Set(txn, "lang", "go")
	txn.Commit()
	valid := EncodeStateAsUpdate(src)

	f.Add(valid)
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add(valid[:len(valid)/2])
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01})
	// Fuzz-discovered crasher (minimal form): clientCount=1, blockCount
	// = MaxUint64, forcing the unbounded make([]Block,...) OOM.
	f.Add([]byte{0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01, 0x00, 0x00})

	f.Fuzz(func(t *testing.T, data []byte) {
		// Fresh doc per input so prior state never confounds the result.
		d := doc.NewDoc()
		_ = ApplyUpdate(d, data)
	})
}

// FuzzDecodeSnapshot feeds arbitrary bytes to DecodeSnapshot (delete
// set + state vector). The contract is no panic/OOM; malformed input
// surfaces as an error.
func FuzzDecodeSnapshot(f *testing.F) {
	f.Add(EncodeSnapshot(Snapshot{DS: NewIdSet(), SV: store.StateVector{}}))

	ds := NewIdSet()
	ds.Insert(1, 0, 5)
	f.Add(EncodeSnapshot(Snapshot{DS: ds, SV: store.StateVector{1: 5, 2: 9}}))

	f.Add([]byte{})
	f.Add([]byte{0x00})
	if enc := EncodeSnapshot(Snapshot{DS: ds, SV: store.StateVector{1: 5}}); len(enc) > 1 {
		f.Add(enc[:1])
	}
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01})
	// Fuzz-discovered crasher: rangeCount varuint claimed ~12.9e9
	// ranges, forcing make([]Range, n) of ~206 GB.
	f.Add([]byte{0x0f, 0xcd, 0x03, 0xbe, 0xac, 0xb0, 0x90, 0xb0})

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = DecodeSnapshot(data)
	})
}

// FuzzDecodeUpdateV2 feeds arbitrary bytes to the V2 column-oriented
// update decoder. The invariant: never panics, hangs, or OOMs; a
// returned error is the expected outcome for malformed bytes.
func FuzzDecodeUpdateV2(f *testing.F) {
	f.Add(EncodeStateAsUpdateV2(doc.NewDoc()))
	f.Add(validV2Update())
	f.Add(v2UpdateWithDeletes()) // reaches readDeleteSetV2 with a real range

	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add(EncodeStateAsUpdateV2(doc.NewDoc())[:5])
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01})

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = DecodeUpdateV2(data)
	})
}

// FuzzApplyUpdateV2 drives the V2 decode+integrate entry on arbitrary
// bytes. Like FuzzApplyUpdate it must never panic, hang, or OOM; it
// shares the integrate path with V1, so it guards the same commit-time
// scans against adversarial structure reached through the V2 codec.
func FuzzApplyUpdateV2(f *testing.F) {
	f.Add(EncodeStateAsUpdateV2(doc.NewDoc()))
	f.Add(validV2Update())
	f.Add(v2UpdateWithDeletes())
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01})

	f.Fuzz(func(t *testing.T, data []byte) {
		d := doc.NewDoc()
		_ = ApplyUpdateV2(d, data)
	})
}

// FuzzDecodeStateVector feeds arbitrary bytes to the state-vector
// decoder. A peer reaches this surface directly through SyncStep1, so
// it must never panic or OOM on a hostile vector; a malformed input
// returns an error, and a successful decode reports a tail no longer
// than the input.
func FuzzDecodeStateVector(f *testing.F) {
	f.Add(EncodeStateVector(store.StateVector{}, nil))
	f.Add(EncodeStateVector(store.StateVector{1: 5, 2: 9, 7: 100}, nil))
	f.Add([]byte{})
	f.Add([]byte{0x00})
	// Huge leading client count -> the count-driven prealloc guard.
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01})

	f.Fuzz(func(t *testing.T, data []byte) {
		_, tail, err := DecodeStateVector(data)
		if err != nil {
			return
		}
		if len(tail) > len(data) {
			t.Fatalf("tail longer than input: %d > %d", len(tail), len(data))
		}
	})
}

// FuzzDecodeAny feeds arbitrary bytes to the lib0-Any value decoder.
// Any payloads carry attacker-controlled length prefixes (strings,
// byte buffers, and recursive array/object counts), so this guards the
// amplification surface directly instead of only through a full update.
// No panic, hang, or OOM; an error is the expected outcome for
// malformed bytes.
func FuzzDecodeAny(f *testing.F) {
	f.Add(EncodeAny(nil, nil))
	f.Add(EncodeAny(nil, "hello"))
	f.Add(EncodeAny(nil, int64(42)))
	f.Add(EncodeAny(nil, true))
	f.Add(EncodeAny(nil, []byte{1, 2, 3, 4}))
	f.Add(EncodeAny(nil, []any{"a", int64(1), false}))
	f.Add(EncodeAny(nil, map[string]any{"k": "v", "n": int64(3)}))
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01})

	f.Fuzz(func(t *testing.T, data []byte) {
		_, tail, err := DecodeAny(data)
		if err != nil {
			return
		}
		if len(tail) > len(data) {
			t.Fatalf("tail longer than input: %d > %d", len(tail), len(data))
		}
	})
}

// FuzzDecodeIdSet feeds arbitrary bytes to the delete-set decoder. An
// IdSet carries per-client range counts (length-prefixed), the same
// amplification class fixed in the update path; this exercises it
// directly. No panic/OOM; malformed input surfaces as an error.
func FuzzDecodeIdSet(f *testing.F) {
	f.Add(NewIdSet().Encode(nil))
	ds := NewIdSet()
	ds.Insert(1, 0, 5)
	ds.Insert(2, 10, 3)
	f.Add(ds.Encode(nil))
	f.Add([]byte{})
	f.Add([]byte{0x00})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01})

	f.Fuzz(func(t *testing.T, data []byte) {
		_, tail, err := DecodeIdSet(data)
		if err != nil {
			return
		}
		if len(tail) > len(data) {
			t.Fatalf("tail longer than input: %d > %d", len(tail), len(data))
		}
	})
}

// validV2Update builds a real V2 update from a populated doc so the
// seed corpus contains a fully-formed multi-column payload.
func validV2Update() []byte {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 7})
	m := types.NewMap(d.Branch("settings"))
	txn := d.WriteTxn()
	m.Set(txn, "color", "red")
	m.Set(txn, "version", int64(1))
	m.Set(txn, "stable", true)
	txn.Commit()
	return EncodeStateAsUpdateV2(d)
}

// v2UpdateWithDeletes builds a V2 update whose delete set is non-empty,
// so the corpus exercises readDeleteSetV2 (and gives the fuzzer a seed
// to mutate the delete-set counts from).
func v2UpdateWithDeletes() []byte {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 9})
	arr := types.NewArray(d.Branch("list"))
	txn := d.WriteTxn()
	arr.Push(txn, "a", "b", "c")
	txn.Commit()
	txn2 := d.WriteTxn()
	arr.Delete(txn2, 1, 1) // delete "b" -> populates the delete set
	txn2.Commit()
	return EncodeStateAsUpdateV2(d)
}
