package encoding

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/lib0"
)

// formatUpdate writes a V2 update of n format markers in root "t", all
// keyed by key. With cached set, the key is written once and read again
// through the key table (yjs's decoder reads such updates; its encoder
// does not write them).
func formatUpdate(key string, n int, cached bool) []byte {
	enc := NewEncoderV2()
	enc.WriteVarUint(1) // clients
	enc.WriteVarUint(uint64(n))
	enc.WriteClient(1)
	enc.WriteVarUint(0) // start clock
	for i := 0; i < n; i++ {
		enc.WriteInfo(uint8(block.KindFormat))
		enc.WriteParentInfo(true)
		enc.WriteString("t")
		if cached && i > 0 {
			enc.keyClock.Write(0)
		} else {
			enc.WriteKey(key)
		}
		enc.WriteJSON(true)
	}
	enc.WriteVarUint(0) // no deletes
	return enc.Bytes()
}

// TestDecodeUpdateV2_CachedKeyBudget: a cached key read again costs its
// length against the same budget as structs (max(bytes, V2StructFloor)),
// since a writer of the decoded update writes the key each time: 64 KiB
// read 8 times passes, read 32 times (2 MiB) does not.
func TestDecodeUpdateV2_CachedKeyBudget(t *testing.T) {
	key := strings.Repeat("k", 64<<10)
	u, err := DecodeUpdateV2(formatUpdate(key, 8, true))
	if err != nil {
		t.Fatalf("8 reads of a cached key: %v", err)
	}
	if got := u.Blocks[1][7].Item.Content.FormatKey; got != key {
		t.Fatalf("eighth marker's key has %d bytes, want %d", len(got), len(key))
	}
	if _, err := DecodeUpdateV2(formatUpdate(key, 32, true)); !errors.Is(err, ErrUpdateTooDense) {
		t.Fatalf("32 reads of a cached 64 KiB key: %v, want ErrUpdateTooDense", err)
	}
	// Keys written each time (what yjs and ygo write) cost their bytes.
	if _, err := DecodeUpdateV2(formatUpdate(key, 32, false)); err != nil {
		t.Fatalf("32 keys written each time: %v", err)
	}
}

// TestDecodeUpdateV2_ClockOverflow: block lengths and delete ranges that
// run past the clock space are refused before the clocks wrap, as V1
// refuses them.
func TestDecodeUpdateV2_ClockOverflow(t *testing.T) {
	enc := NewEncoderV2()
	enc.WriteVarUint(1)
	enc.WriteVarUint(2)
	enc.WriteClient(1)
	enc.WriteVarUint(0)
	enc.WriteInfo(0) // GC
	enc.WriteLen(MaxClock)
	enc.WriteInfo(0)
	enc.WriteLen(5)
	enc.WriteVarUint(0)
	if _, err := DecodeUpdateV2(enc.Bytes()); !errors.Is(err, lib0.ErrTruncated) {
		t.Fatalf("GC runs past the clock space: %v", err)
	}

	ds := func(ranges ...[2]uint64) []byte {
		e := NewEncoderV2()
		e.WriteVarUint(0) // no structs
		e.WriteVarUint(1)
		e.WriteVarUint(1)
		e.WriteVarUint(uint64(len(ranges)))
		for _, r := range ranges {
			e.WriteVarUint(r[0]) // clock diff
			e.WriteVarUint(r[1]) // length - 1
		}
		return e.Bytes()
	}
	for name, b := range map[string][]byte{
		"a clock diff past the space": ds([2]uint64{MaxClock, 0}, [2]uint64{5, 0}),
		"a clock diff that wraps":     ds([2]uint64{0, 0}, [2]uint64{^uint64(0), 0}),
		"a length past the space":     ds([2]uint64{3, MaxClock}),
		"a length that wraps":         ds([2]uint64{0, ^uint64(0)}),
	} {
		if _, err := DecodeUpdateV2(b); !errors.Is(err, lib0.ErrTruncated) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestDeleteSet_RepeatedSectionsSortOnce: a client written in many delete
// set sections, each range below the one before, decodes to the same
// sorted set as one section would, in V1 and V2. Inserting each range on
// its own was quadratic: 200,000 such sections took minutes; a million
// take a fraction of a second once sorted.
func TestDeleteSet_RepeatedSectionsSortOnce(t *testing.T) {
	const n = 1000000
	check := func(name string, s *IdSet) {
		t.Helper()
		got := s.clients[1]
		if len(got) != n {
			t.Fatalf("%s: %d ranges, want %d", name, len(got), n)
		}
		for i, r := range got {
			if r.Start != uint64(i)*2 || r.Length != 1 {
				t.Fatalf("%s: range %d = %+v, want {%d 1}", name, i, r, i*2)
			}
		}
	}
	v1 := lib0.WriteVarUint(nil, n)
	for i := n - 1; i >= 0; i-- {
		v1 = lib0.WriteVarUint(v1, 1)
		v1 = lib0.WriteVarUint(v1, 1)
		v1 = lib0.WriteVarUint(v1, uint64(i)*2)
		v1 = lib0.WriteVarUint(v1, 1)
	}
	s, _, err := DecodeIdSet(v1)
	if err != nil {
		t.Fatal(err)
	}
	check("V1", s)

	enc := NewEncoderV2()
	enc.WriteVarUint(0)
	enc.WriteVarUint(n)
	for i := n - 1; i >= 0; i-- {
		enc.WriteVarUint(1)
		enc.WriteVarUint(1)
		enc.WriteVarUint(uint64(i) * 2)
		enc.WriteVarUint(0)
	}
	u, err := DecodeUpdateV2(enc.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	check("V2", u.DeleteSet)
}

// TestLooksLikeUpdateV2_IsCheap: diagnosing bytes as V2 costs no more than
// their length: a 42-byte state holding a million structs is not expanded
// (ValidateUpdate asks this of bytes it refused, on a network path).
func TestLooksLikeUpdateV2_IsCheap(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/v2-dense-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Scenarios []struct {
			Structs int    `json:"structs"`
			State   string `json:"state"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	for _, s := range f.Scenarios {
		if s.Structs < 1000000 {
			continue
		}
		b, _ := hex.DecodeString(s.State)
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		looks := LooksLikeUpdateV2(b)
		runtime.ReadMemStats(&m1)
		if mb := (m1.TotalAlloc - m0.TotalAlloc) >> 20; mb > 4 {
			t.Fatalf("diagnosing %d structs in %d bytes allocated %d MB", s.Structs, len(b), mb)
		}
		if looks {
			t.Fatalf("%d structs in %d bytes look like an update within its length", s.Structs, len(b))
		}
	}
	if !LooksLikeUpdateV2(formatUpdate("bold", 3, false)) {
		t.Fatal("an ordinary V2 update does not look like one")
	}
}

// TestDeleteRange_Boundary: a delete range may end at MaxClock+1 (it
// deletes the last clock, which yjs reads and writes back), not past it;
// a GC run may end at MaxClock (its end is a state-vector clock).
func TestDeleteRange_Boundary(t *testing.T) {
	ds := func(diff, lenMinus1 uint64) []byte {
		e := NewEncoderV2()
		e.WriteVarUint(0)
		e.WriteVarUint(1)
		e.WriteVarUint(1)
		e.WriteVarUint(1)
		e.WriteVarUint(diff)
		e.WriteVarUint(lenMinus1)
		return e.Bytes()
	}
	for name, b := range map[string][]byte{
		"the last clock":     ds(MaxClock, 0),
		"every clock from 0": ds(0, MaxClock),
		"V1, the last clock": lib0.WriteVarUint(lib0.WriteVarUint(lib0.WriteVarUint(lib0.WriteVarUint(lib0.WriteVarUint(lib0.WriteVarUint(nil, 0), 1), 1), 1), MaxClock), 1),
	} {
		var err error
		if strings.HasPrefix(name, "V1") {
			_, _, err = DecodeUpdate(b)
		} else {
			_, err = DecodeUpdateV2(b)
		}
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := DecodeUpdateV2(ds(1, MaxClock)); !errors.Is(err, lib0.ErrTruncated) {
		t.Errorf("a range one past the space: %v", err)
	}
	enc := NewEncoderV2()
	enc.WriteVarUint(1)
	enc.WriteVarUint(1)
	enc.WriteClient(1)
	enc.WriteVarUint(0)
	enc.WriteInfo(0)
	enc.WriteLen(MaxClock)
	enc.WriteVarUint(0)
	if _, err := DecodeUpdateV2(enc.Bytes()); err != nil {
		t.Errorf("a GC run ending at MaxClock: %v", err)
	}
}

// TestDecodeUpdate_ItemClockWrap: a V1 item whose length runs past the
// clock space is refused, as GC and Skip runs are: the next block's clock
// wrapped to zero.
func TestDecodeUpdate_ItemClockWrap(t *testing.T) {
	b := lib0.WriteVarUint(nil, 1)
	b = lib0.WriteVarUint(b, 1)
	b = lib0.WriteVarUint(b, 1)
	b = lib0.WriteVarUint(b, 1)          // start clock
	b = append(b, 0x01)                  // ContentDeleted, no origins
	b = append(b, 0x01, 0x01, 't')       // parent named "t"
	b = lib0.WriteVarUint(b, ^uint64(0)) // length
	b = lib0.WriteVarUint(b, 0)
	if _, _, err := DecodeUpdate(b); !errors.Is(err, lib0.ErrTruncated) {
		t.Fatalf("an item running past the clock space: %v", err)
	}
}

// TestInsertAll_MatchesInsert: insertAll gives what Insert gives range by
// range, on random ranges that overlap, touch, repeat and are empty.
func TestInsertAll_MatchesInsert(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 9))
	rnd := func(n int) []Range {
		var out []Range
		for range n {
			out = append(out, Range{Start: uint64(rng.IntN(60)), Length: uint64(rng.IntN(5))})
		}
		return out
	}
	for run := range 20000 {
		existing, added := rnd(rng.IntN(6)), rnd(1+rng.IntN(8))
		a, b := NewIdSet(), NewIdSet()
		for _, r := range existing {
			a.Insert(1, r.Start, r.Length)
			b.Insert(1, r.Start, r.Length)
		}
		for _, r := range added {
			a.Insert(1, r.Start, r.Length)
		}
		b.insertAll(1, added)
		if !slices.Equal(a.clients[1], b.clients[1]) || slices.Equal(a.order, []uint64{1}) != slices.Equal(b.order, []uint64{1}) {
			t.Fatalf("run %d: existing %v, added %v:\n Insert    %v\n insertAll %v", run, existing, added, a.clients[1], b.clients[1])
		}
	}
}

// TestApplyDeleteSet_OneEntryDescending: a V1 delete set keeps a client's
// first entry as written, and a peer may write it descending. Queuing a
// million such ranges, for items nobody has, is not quadratic (they were
// inserted one by one), nor is queuing a million more below them.
func TestApplyDeleteSet_OneEntryDescending(t *testing.T) {
	const n = 1000000
	descending := func(from uint64) []byte {
		b := lib0.WriteVarUint(nil, 0) // no structs
		b = lib0.WriteVarUint(b, 1)
		b = lib0.WriteVarUint(b, 7)
		b = lib0.WriteVarUint(b, n)
		for i := n - 1; i >= 0; i-- {
			b = lib0.WriteVarUint(b, from+uint64(i)*2)
			b = lib0.WriteVarUint(b, 1)
		}
		return b
	}
	d := doc.NewDoc()
	for _, from := range []uint64{2 * n, 0} {
		if err := ApplyUpdate(d, descending(from)); err != nil {
			t.Fatal(err)
		}
	}
	txn := d.ReadTxn()
	defer txn.Close()
	got := GetPending(txn).DeleteSet.clients[7]
	if len(got) != 2*n || got[0].Start != 0 || got[2*n-1].Start != uint64(2*n-1)*2 {
		t.Fatalf("queued %d ranges, want %d sorted", len(got), 2*n)
	}
}

// TestDecodeIdSet_EmptyRepeatKeepsEntry: V1 keeps a client's first entry
// as written; an entry for it again holding only an empty range changes
// nothing (Insert of an empty range does nothing), so the first entry is
// not sorted or merged.
func TestDecodeIdSet_EmptyRepeatKeepsEntry(t *testing.T) {
	b := []byte{0x02, 0x01, 0x02, 0x04, 0x01, 0x00, 0x01, 0x01, 0x01, 0x00, 0x00}
	s, rest, err := DecodeIdSet(b)
	if err != nil || len(rest) != 0 {
		t.Fatalf("decode: %v, %d left", err, len(rest))
	}
	if got, want := s.ClientRanges(1), []Range{{Start: 4, Length: 1}, {Start: 0, Length: 1}}; !slices.Equal(got, want) {
		t.Fatalf("client 1: %v, want %v as written", got, want)
	}
}
