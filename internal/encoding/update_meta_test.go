package encoding

import (
	"bytes"
	"encoding/hex"
	"errors"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/lib0"
)

// denseEmptyArraysV2 is 36 bytes of V2 holding a million empty arrays,
// within V2StructFloor.
const denseEmptyArraysV2 = "00000441be843d050003bc843d00030700870301610101010440be843d0001c0843d0000"

// peakHeap runs f while sampling the heap and returns the most it held
// above where it started.
func peakHeap(f func()) uint64 {
	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	base, peak := ms.HeapAlloc, ms.HeapAlloc
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var s runtime.MemStats
		for {
			runtime.ReadMemStats(&s)
			peak = max(peak, s.HeapAlloc)
			select {
			case <-done:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	f()
	close(done)
	wg.Wait()
	if peak < base {
		return 0
	}
	return peak - base
}

// TestStateVectorAndMeta_DenseV2HoldNoStructs: the state vector and meta of
// an update read its structs one at a time and keep none, as yjs's lazy
// reader does. Keeping the million structs of denseEmptyArraysV2 holds
// about 500 MB.
func TestStateVectorAndMeta_DenseV2HoldNoStructs(t *testing.T) {
	in, err := hex.DecodeString(denseEmptyArraysV2)
	if err != nil {
		t.Fatal(err)
	}
	const limit = 100 << 20
	var sv []byte
	if peak := peakHeap(func() { sv, err = StateVectorFromUpdateBytes(in, true, V2StructFloor) }); peak > limit {
		t.Errorf("the state vector held %d MB", peak>>20)
	}
	if err != nil || hex.EncodeToString(sv) != "0101c0843d" {
		t.Fatalf("state vector %x, %v", sv, err)
	}
	var meta UpdateMeta
	if peak := peakHeap(func() { meta, err = ParseUpdateMetaBytes(in, true, V2StructFloor) }); peak > limit {
		t.Errorf("the meta held %d MB", peak>>20)
	}
	if err != nil || len(meta.From) != 1 || meta.From[1] != 0 || meta.To[1] != 1000000 {
		t.Fatalf("meta %v, %v", meta, err)
	}
}

// heterogeneousObjectsV1 is a V1 update of n ContentAny structs under root
// "a", each one object {z: null, a<i>: null}: n shapes of key order, which
// a decode keeps in its order table.
func heterogeneousObjectsV1(n int) []byte {
	buf := lib0.WriteVarUint(nil, 1)
	buf = lib0.WriteVarUint(buf, uint64(n))
	buf = lib0.WriteVarUint(buf, 1)
	buf = lib0.WriteVarUint(buf, 0)
	for i := 0; i < n; i++ {
		buf = append(buf, 8, 1)
		buf = lib0.WriteVarString(buf, "a")
		buf = lib0.WriteVarUint(buf, 1)
		buf = append(buf, 118)
		buf = lib0.WriteVarUint(buf, 2)
		buf = lib0.WriteVarString(buf, "z")
		buf = append(buf, 126)
		buf = lib0.WriteVarString(buf, "a"+strconv.Itoa(i))
		buf = append(buf, 126)
	}
	return append(buf, 0)
}

// TestStateVectorAndMeta_KeepNoKeyOrders: the orders of the values read are
// not kept either.
func TestStateVectorAndMeta_KeepNoKeyOrders(t *testing.T) {
	const n = 200000
	in := heterogeneousObjectsV1(n)
	var sv []byte
	var err error
	if peak := peakHeap(func() { sv, err = StateVectorFromUpdateBytes(in, false, 0) }); peak > 16<<20 {
		t.Errorf("the state vector held %d MB", peak>>20)
	}
	if want := lib0.WriteVarUint([]byte{1, 1}, n); err != nil || !bytes.Equal(sv, want) {
		t.Fatalf("state vector %x, %v; want %x", sv, err, want)
	}
	if peak := peakHeap(func() { _, err = ParseUpdateMetaBytes(in, false, 0) }); peak > 16<<20 || err != nil {
		t.Errorf("the meta held %d MB, %v", peak>>20, err)
	}
}

// TestTypeRef_RefusedOnTheWireReadAsBeforeWhenStored: a shared type ref past
// 6 is refused as yjs refuses it, but bytes ygo itself stored or encoded read
// every ref as releases before 1.53.0 did (256 as its low byte, an array): a
// store may hold an update as a client sent it.
func TestTypeRef_RefusedOnTheWireReadAsBeforeWhenStored(t *testing.T) {
	v1 := map[string]string{"7": "01010100070101740700", "255": "0101010007010174ff0100", "256": "0101010007010174800200"}
	for ref, h := range v1 {
		in, _ := hex.DecodeString(h)
		if _, _, err := DecodeUpdate(in); !errors.Is(err, ErrUnknownTypeRef) {
			t.Errorf("V1 ref %s on the wire: %v", ref, err)
		}
		if _, _, err := DecodeStoredUpdate(in); err != nil {
			t.Errorf("V1 ref %s stored: %v", ref, err)
		}
		if _, _, err := DecodeOwnUpdate(in); err != nil {
			t.Errorf("V1 ref %s own bytes: %v", ref, err)
		}
	}
	v2 := map[string]string{"7": "000001010000010703017401010101070001010000", "256": "00000101000001070301740101010280040001010000"}
	for ref, h := range v2 {
		in, _ := hex.DecodeString(h)
		if _, err := DecodeUpdateV2(in); !errors.Is(err, ErrUnknownTypeRef) {
			t.Errorf("V2 ref %s on the wire: %v", ref, err)
		}
		if _, err := DecodeOwnUpdateV2(in); err != nil {
			t.Errorf("V2 ref %s own bytes: %v", ref, err)
		}
	}
	in, _ := hex.DecodeString(v1["256"])
	u, _, _ := DecodeStoredUpdate(in)
	if c := u.Blocks[1][0].Item.Content; c.Branch == nil || c.Branch.TypeRef != block.TypeRefArray {
		t.Errorf("stored ref 256 read as %+v, want an array as before", c)
	}
}
