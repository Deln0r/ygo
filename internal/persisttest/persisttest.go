// Package persisttest is the conformance suite for persist.VersionedStore
// implementations: what a store does with updates (append, read back,
// compact with Flush, refuse to compact an incomplete log), documents
// (exist, list, clear) and versions (save, list, read, delete, restore,
// prune), and the persist helpers on top of it. Run calls every test
// against stores opened by the caller, so the core's in-memory store and
// persist/sqlite are held to one behaviour.
package persisttest

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"

	ygo "github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/persist"
)

// openStore opens the store under test (set by Run); newTestStore also
// closes it when the test ends.
var openStore func(t *testing.T) persist.VersionedStore

func newTestStore(t *testing.T) persist.VersionedStore {
	t.Helper()
	s := openStore(t)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Run runs the suite, each test against a store of its own from open.
func Run(t *testing.T, open func(t *testing.T) persist.VersionedStore) {
	openStore = open
	defer func() { openStore = nil }()
	for _, tc := range tests {
		t.Run(tc.name, tc.fn)
	}
}

var tests = []struct {
	name string
	fn   func(*testing.T)
}{
	{"TestCanceledContext_ChangesNothing", testCanceledContext_ChangesNothing},
	{"TestStoreUpdate_RejectsEmpty", testStoreUpdate_RejectsEmpty},
	{"TestStoreUpdate_GetUpdates_RoundTrip", testStoreUpdate_GetUpdates_RoundTrip},
	{"TestGetUpdates_UnknownDoc_ReturnsEmpty", testGetUpdates_UnknownDoc_ReturnsEmpty},
	{"TestDocumentExists", testDocumentExists},
	{"TestListDocuments", testListDocuments},
	{"TestClearDocument", testClearDocument},
	{"TestLoadDoc_ReplayProducesEquivalentState", testLoadDoc_ReplayProducesEquivalentState},
	{"TestLoadDoc_UnknownDoc_ReturnsEmpty", testLoadDoc_UnknownDoc_ReturnsEmpty},
	{"TestLoadDoc_MultipleSeparateUpdates", testLoadDoc_MultipleSeparateUpdates},
	{"TestFlush_CompactsToSingleUpdate", testFlush_CompactsToSingleUpdate},
	{"TestFlush_NoOpOnZeroOrOneUpdate", testFlush_NoOpOnZeroOrOneUpdate},
	{"TestGetStateVector", testGetStateVector},
	{"TestGetDiff_AgainstEmptySV_EmitsAll", testGetDiff_AgainstEmptySV_EmitsAll},
	{"TestGetDiff_CarriesUpdatesThatWait", testGetDiff_CarriesUpdatesThatWait},
	{"TestGetDiff_AgainstFullSV_EmitsNothingMeaningful", testGetDiff_AgainstFullSV_EmitsNothingMeaningful},
	{"TestGetDiff_UnknownDoc_ReturnsNil", testGetDiff_UnknownDoc_ReturnsNil},
	{"TestMergeUpdates_PreservesState", testMergeUpdates_PreservesState},
	{"TestMergeUpdates_EmptyInput", testMergeUpdates_EmptyInput},
	{"TestCrossDocIsolation", testCrossDocIsolation},
	{"TestStoreUpdate_ConcurrentAppends", testStoreUpdate_ConcurrentAppends},
	{"TestClose_Idempotent", testClose_Idempotent},
	{"TestLoadDoc_FixedClientIDInTheLogIsReplaced", testLoadDoc_FixedClientIDInTheLogIsReplaced},
	{"TestSaveVersion_RoundTrip", testSaveVersion_RoundTrip},
	{"TestSaveVersion_UnknownDocument", testSaveVersion_UnknownDocument},
	{"TestVersions_SurviveFlushAndClear", testVersions_SurviveFlushAndClear},
	{"TestRestoreVersion_RewindsLiveLog", testRestoreVersion_RewindsLiveLog},
	{"TestRestoreVersion_NotFound", testRestoreVersion_NotFound},
	{"TestGetVersionState_NotFound", testGetVersionState_NotFound},
	{"TestDeleteVersion_Idempotent", testDeleteVersion_Idempotent},
	{"TestPruneVersions_KeepsNewest", testPruneVersions_KeepsNewest},
	{"TestSaveVersion_EmptyStateRejected", testSaveVersion_EmptyStateRejected},
	{"TestLoadVersion_FixedClientIDInTheStateIsReplaced", testLoadVersion_FixedClientIDInTheStateIsReplaced},
	{"TestMergeUpdates_RefusesIncompleteLog", testMergeUpdates_RefusesIncompleteLog},
	{"TestFlush_IncompleteLogSurvives", testFlush_IncompleteLogSurvives},
}

// From persist/persist_test.go.

// applyMapSet builds a doc, performs one Map.Set, and returns the
// resulting V1 update bytes (the diff from empty SV). Convenience
// helper used across tests.
func applyMapSet(t *testing.T, clientID uint64, key string, value any) []byte {
	t.Helper()
	d := doc.NewDocWithOptions(doc.Options{ClientID: clientID})
	m := types.NewMap(d.Branch("settings"))
	txn := d.WriteTxn()
	m.Set(txn, key, value)
	txn.Commit()
	return encoding.EncodeStateAsUpdate(d)
}

func testStoreUpdate_RejectsEmpty(t *testing.T) {
	s := newTestStore(t)
	err := s.StoreUpdate(context.Background(), "doc", nil)
	if !errors.Is(err, persist.ErrEmptyUpdate) {
		t.Fatalf("nil update: want ErrEmptyUpdate, got %v", err)
	}
	err = s.StoreUpdate(context.Background(), "doc", []byte{})
	if !errors.Is(err, persist.ErrEmptyUpdate) {
		t.Fatalf("empty update: want ErrEmptyUpdate, got %v", err)
	}
}

func testStoreUpdate_GetUpdates_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	u1 := applyMapSet(t, 101, "color", "red")
	u2 := applyMapSet(t, 102, "size", "large")

	if err := s.StoreUpdate(ctx, "doc-a", u1); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreUpdate(ctx, "doc-a", u2); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetUpdates(ctx, "doc-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2", len(got))
	}
	if !bytesEqual(got[0], u1) {
		t.Errorf("update[0] mismatch")
	}
	if !bytesEqual(got[1], u2) {
		t.Errorf("update[1] mismatch")
	}
}

func testGetUpdates_UnknownDoc_ReturnsEmpty(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetUpdates(context.Background(), "never-seen")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("len=%d, want 0", len(got))
	}
}

func testDocumentExists(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	exists, err := s.DocumentExists(ctx, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("missing doc reports exists")
	}

	if err := s.StoreUpdate(ctx, "real", applyMapSet(t, 200, "k", "v")); err != nil {
		t.Fatal(err)
	}
	exists, err = s.DocumentExists(ctx, "real")
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("known doc reports missing")
	}
}

func testListDocuments(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := s.StoreUpdate(ctx, name, applyMapSet(t, 300, "k", name)); err != nil {
			t.Fatal(err)
		}
		// Two updates per doc to confirm DISTINCT works.
		if err := s.StoreUpdate(ctx, name, applyMapSet(t, 301, "k2", name)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.ListDocuments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "alpha" || got[1] != "beta" || got[2] != "gamma" {
		t.Errorf("got %v, want [alpha beta gamma]", got)
	}
}

func testClearDocument(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.StoreUpdate(ctx, "doc", applyMapSet(t, 400, "k", "v")); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearDocument(ctx, "doc"); err != nil {
		t.Fatal(err)
	}
	exists, _ := s.DocumentExists(ctx, "doc")
	if exists {
		t.Error("doc still exists after Clear")
	}
	// Idempotent: clearing a missing doc is a no-op.
	if err := s.ClearDocument(ctx, "doc"); err != nil {
		t.Errorf("second clear: %v", err)
	}
	if err := s.ClearDocument(ctx, "never-existed"); err != nil {
		t.Errorf("clear unknown: %v", err)
	}
}

func testLoadDoc_ReplayProducesEquivalentState(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Source doc: three map keys set across two transactions.
	src := doc.NewDocWithOptions(doc.Options{ClientID: 500})
	m := types.NewMap(src.Branch("settings"))

	t1 := src.WriteTxn()
	m.Set(t1, "color", "red")
	m.Set(t1, "size", "large")
	t1.Commit()

	t2 := src.WriteTxn()
	m.Set(t2, "color", "blue") // LWW overwrite
	m.Set(t2, "shape", "circle")
	t2.Commit()

	if err := s.StoreUpdate(ctx, "doc", encoding.EncodeStateAsUpdate(src)); err != nil {
		t.Fatal(err)
	}

	loaded, err := persist.LoadDoc(ctx, s, "doc", doc.Options{})
	if err != nil {
		t.Fatal(err)
	}
	lm := types.NewMap(loaded.Branch("settings"))

	rtxn := loaded.ReadTxn()
	defer rtxn.Close()

	if got := lm.Get("color"); got != "blue" {
		t.Errorf("color = %v, want blue", got)
	}
	if got := lm.Get("size"); got != "large" {
		t.Errorf("size = %v, want large", got)
	}
	if got := lm.Get("shape"); got != "circle" {
		t.Errorf("shape = %v, want circle", got)
	}
}

func testLoadDoc_UnknownDoc_ReturnsEmpty(t *testing.T) {
	s := newTestStore(t)
	d, err := persist.LoadDoc(context.Background(), s, "never-seen", doc.Options{ClientID: 600})
	if err != nil {
		t.Fatal(err)
	}
	if d == nil {
		t.Fatal("got nil doc")
	}
	if d.ClientID() != 600 {
		t.Errorf("ClientID = %d, want 600 (the provided override)", d.ClientID())
	}
}

func testLoadDoc_MultipleSeparateUpdates(t *testing.T) {
	// Each update is encoded as its own diff and stored separately.
	// LoadDoc must replay them in order and converge to the same
	// state a single combined update would produce.
	s := newTestStore(t)
	ctx := context.Background()

	src := doc.NewDocWithOptions(doc.Options{ClientID: 700})
	m := types.NewMap(src.Branch("settings"))

	prevSV := make(map[uint64]uint64)
	for i, kv := range []struct{ k, v string }{
		{"a", "1"}, {"b", "2"}, {"c", "3"}, {"d", "4"},
	} {
		txn := src.WriteTxn()
		m.Set(txn, kv.k, kv.v)
		txn.Commit()

		rtxn := src.ReadTxn()
		diff := encoding.EncodeDiff(src, rtxn, prevSV)
		// Snapshot the SV BEFORE releasing the read lock so the
		// next iteration's diff is against the same boundary.
		newSV := rtxn.Store().GetStateVector()
		rtxn.Close()

		if err := s.StoreUpdate(ctx, "doc", diff); err != nil {
			t.Fatalf("StoreUpdate[%d]: %v", i, err)
		}
		prevSV = newSV
	}

	got, _ := s.GetUpdates(ctx, "doc")
	if len(got) != 4 {
		t.Fatalf("got %d updates, want 4", len(got))
	}

	loaded, err := persist.LoadDoc(ctx, s, "doc", doc.Options{})
	if err != nil {
		t.Fatal(err)
	}
	lm := types.NewMap(loaded.Branch("settings"))
	rtxn := loaded.ReadTxn()
	defer rtxn.Close()
	for _, kv := range []struct{ k, v string }{
		{"a", "1"}, {"b", "2"}, {"c", "3"}, {"d", "4"},
	} {
		if got := lm.Get(kv.k); got != kv.v {
			t.Errorf("Get(%q) = %v, want %v", kv.k, got, kv.v)
		}
	}
}

func testFlush_CompactsToSingleUpdate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Store 5 separate updates. Use string values so equality
	// comparison is type-stable across the Any encode/decode
	// round-trip (numeric types may widen — see tech-debt.md).
	for i := 0; i < 5; i++ {
		raw := applyMapSet(t, uint64(800+i), fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i))
		if err := s.StoreUpdate(ctx, "doc", raw); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := s.GetUpdates(ctx, "doc")
	if len(before) != 5 {
		t.Fatalf("pre-flush len=%d, want 5", len(before))
	}

	if err := s.Flush(ctx, "doc"); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	after, _ := s.GetUpdates(ctx, "doc")
	if len(after) != 1 {
		t.Fatalf("post-flush len=%d, want 1", len(after))
	}

	// Loading from the flushed snapshot must reproduce all keys.
	loaded, err := persist.LoadDoc(ctx, s, "doc", doc.Options{})
	if err != nil {
		t.Fatal(err)
	}
	lm := types.NewMap(loaded.Branch("settings"))
	rtxn := loaded.ReadTxn()
	defer rtxn.Close()
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("k%d", i)
		want := fmt.Sprintf("v%d", i)
		if got := lm.Get(key); got != want {
			t.Errorf("post-flush Get(%q) = %v, want %v", key, got, want)
		}
	}
}

func testFlush_NoOpOnZeroOrOneUpdate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Zero updates — no-op, no error, no row created.
	if err := s.Flush(ctx, "empty"); err != nil {
		t.Errorf("flush empty: %v", err)
	}
	if exists, _ := s.DocumentExists(ctx, "empty"); exists {
		t.Error("flush on empty created a row")
	}

	// One update — already optimal, flush is no-op.
	raw := applyMapSet(t, 900, "k", "v")
	if err := s.StoreUpdate(ctx, "single", raw); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx, "single"); err != nil {
		t.Errorf("flush single: %v", err)
	}
	got, _ := s.GetUpdates(ctx, "single")
	if len(got) != 1 {
		t.Errorf("post-flush single: len=%d, want 1", len(got))
	}
	if !bytesEqual(got[0], raw) {
		t.Errorf("single-update flush altered the blob")
	}
}

func testGetStateVector(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Unknown doc -> nil.
	sv, err := persist.GetStateVector(ctx, s, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if sv != nil {
		t.Errorf("unknown doc: SV = %v, want nil", sv)
	}

	// Known doc -> matches direct encode.
	src := doc.NewDocWithOptions(doc.Options{ClientID: 1000})
	m := types.NewMap(src.Branch("settings"))
	txn := src.WriteTxn()
	m.Set(txn, "k", "v")
	txn.Commit()
	if err := s.StoreUpdate(ctx, "doc", encoding.EncodeStateAsUpdate(src)); err != nil {
		t.Fatal(err)
	}

	gotSV, err := persist.GetStateVector(ctx, s, "doc")
	if err != nil {
		t.Fatal(err)
	}
	rtxn := src.ReadTxn()
	wantSV := encoding.EncodeStateVector(rtxn.Store().GetStateVector(), nil)
	rtxn.Close()

	if !bytesEqual(gotSV, wantSV) {
		t.Errorf("SV mismatch:\n got  %x\n want %x", gotSV, wantSV)
	}
}

func testGetDiff_AgainstEmptySV_EmitsAll(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	src := doc.NewDocWithOptions(doc.Options{ClientID: 1100})
	m := types.NewMap(src.Branch("settings"))
	txn := src.WriteTxn()
	m.Set(txn, "k", "v")
	txn.Commit()
	if err := s.StoreUpdate(ctx, "doc", encoding.EncodeStateAsUpdate(src)); err != nil {
		t.Fatal(err)
	}

	diff, err := persist.GetDiff(ctx, s, "doc", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Apply the diff to a fresh doc and confirm it carries the value.
	target := doc.NewDoc()
	upd, _, err := encoding.DecodeUpdate(diff)
	if err != nil {
		t.Fatal(err)
	}
	wtxn := target.WriteTxn()
	if err := upd.Apply(wtxn); err != nil {
		t.Fatal(err)
	}
	wtxn.Commit()

	lm := types.NewMap(target.Branch("settings"))
	rtxn := target.ReadTxn()
	defer rtxn.Close()
	if got := lm.Get("k"); got != "v" {
		t.Errorf("applied diff: Get(k)=%v, want v", got)
	}
}

// testGetDiff_CarriesUpdatesThatWait: a log holding an update whose
// dependency was never stored gives it to the peer, as yjs's state of the
// same log does; GetDiff left it out.
func testGetDiff_CarriesUpdatesThatWait(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	src := doc.NewDocWithOptions(doc.Options{ClientID: 1200})
	x := types.NewText(src.Branch("t"))
	txn := src.WriteTxn()
	if err := x.Insert(txn, 0, "ab"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	rtxn := src.ReadTxn()
	sv := rtxn.Store().GetStateVector()
	rtxn.Close()
	txn = src.WriteTxn()
	if err := x.Insert(txn, 2, "cd"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	rtxn = src.ReadTxn()
	late := encoding.EncodeDiff(src, rtxn, sv)
	rtxn.Close()
	if err := s.StoreUpdate(ctx, "doc", late); err != nil {
		t.Fatal(err)
	}

	diff, err := persist.GetDiff(ctx, s, "doc", nil)
	if err != nil {
		t.Fatal(err)
	}
	upd, _, err := encoding.DecodeUpdate(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(upd.Blocks[1200]) == 0 {
		t.Fatalf("GetDiff %x carries none of the waiting update %x", diff, late)
	}
}

func testGetDiff_AgainstFullSV_EmitsNothingMeaningful(t *testing.T) {
	// A diff against a remote SV that already covers everything
	// should be applyable to a fresh doc without ill effect; in the
	// trivial case it is the empty-update wire form (one client-count
	// of 0 + empty delete set).
	s := newTestStore(t)
	ctx := context.Background()

	src := doc.NewDocWithOptions(doc.Options{ClientID: 1200})
	m := types.NewMap(src.Branch("settings"))
	txn := src.WriteTxn()
	m.Set(txn, "k", "v")
	txn.Commit()
	if err := s.StoreUpdate(ctx, "doc", encoding.EncodeStateAsUpdate(src)); err != nil {
		t.Fatal(err)
	}

	rtxn := src.ReadTxn()
	fullSV := encoding.EncodeStateVector(rtxn.Store().GetStateVector(), nil)
	rtxn.Close()

	diff, err := persist.GetDiff(ctx, s, "doc", fullSV)
	if err != nil {
		t.Fatal(err)
	}

	// Diff against full SV: empty client list + empty delete set.
	// Should decode to an Update with no blocks.
	upd, _, err := encoding.DecodeUpdate(diff)
	if err != nil {
		t.Fatalf("decode diff: %v", err)
	}
	if len(upd.Blocks) != 0 {
		t.Errorf("diff against full SV emitted %d clients, want 0", len(upd.Blocks))
	}
}

func testGetDiff_UnknownDoc_ReturnsNil(t *testing.T) {
	s := newTestStore(t)
	diff, err := persist.GetDiff(context.Background(), s, "missing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if diff != nil {
		t.Errorf("got %v, want nil", diff)
	}
}

func testMergeUpdates_PreservesState(t *testing.T) {
	// MergeUpdates standalone (no storage) must produce the same
	// final state as applying its inputs sequentially.
	src := doc.NewDocWithOptions(doc.Options{ClientID: 1300})
	m := types.NewMap(src.Branch("settings"))

	var raws [][]byte
	for _, kv := range []struct{ k, v string }{
		{"a", "1"}, {"b", "2"}, {"a", "1-updated"},
	} {
		prevSV := func() map[uint64]uint64 {
			r := src.ReadTxn()
			defer r.Close()
			return r.Store().GetStateVector()
		}()

		txn := src.WriteTxn()
		m.Set(txn, kv.k, kv.v)
		txn.Commit()

		r := src.ReadTxn()
		diff := encoding.EncodeDiff(src, r, prevSV)
		r.Close()

		raws = append(raws, diff)
	}

	merged, err := persist.MergeUpdates(raws)
	if err != nil {
		t.Fatal(err)
	}

	target := doc.NewDoc()
	upd, _, err := encoding.DecodeUpdate(merged)
	if err != nil {
		t.Fatal(err)
	}
	wtxn := target.WriteTxn()
	if err := upd.Apply(wtxn); err != nil {
		t.Fatal(err)
	}
	wtxn.Commit()

	lm := types.NewMap(target.Branch("settings"))
	rtxn := target.ReadTxn()
	defer rtxn.Close()
	if got := lm.Get("a"); got != "1-updated" {
		t.Errorf("a = %v, want 1-updated (LWW)", got)
	}
	if got := lm.Get("b"); got != "2" {
		t.Errorf("b = %v, want 2", got)
	}
}

func testMergeUpdates_EmptyInput(t *testing.T) {
	got, err := persist.MergeUpdates(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func testCrossDocIsolation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.StoreUpdate(ctx, "doc-a", applyMapSet(t, 1400, "k", "from-a")); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreUpdate(ctx, "doc-b", applyMapSet(t, 1401, "k", "from-b")); err != nil {
		t.Fatal(err)
	}

	la, _ := persist.LoadDoc(ctx, s, "doc-a", doc.Options{})
	lb, _ := persist.LoadDoc(ctx, s, "doc-b", doc.Options{})

	ma := types.NewMap(la.Branch("settings"))
	mb := types.NewMap(lb.Branch("settings"))

	ra := la.ReadTxn()
	rb := lb.ReadTxn()
	defer ra.Close()
	defer rb.Close()

	if got := ma.Get("k"); got != "from-a" {
		t.Errorf("doc-a: k = %v, want from-a", got)
	}
	if got := mb.Get("k"); got != "from-b" {
		t.Errorf("doc-b: k = %v, want from-b", got)
	}
}

func testStoreUpdate_ConcurrentAppends(t *testing.T) {
	// SQLite serializes writes at the file lock. We verify the
	// persist contract under contention: every StoreUpdate must
	// either succeed or return error, and the final stored count
	// matches the total attempted.
	const goroutines = 8
	const perGoroutine = 25

	s := newTestStore(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				raw := applyMapSet(t, uint64(g*1000+i+1), fmt.Sprintf("g%d-k%d", g, i), i)
				if err := s.StoreUpdate(ctx, "doc", raw); err != nil {
					t.Errorf("g=%d i=%d: %v", g, i, err)
					return
				}
			}
		}()
	}
	wg.Wait()

	got, err := s.GetUpdates(ctx, "doc")
	if err != nil {
		t.Fatal(err)
	}
	if want := goroutines * perGoroutine; len(got) != want {
		t.Errorf("stored count = %d, want %d", len(got), want)
	}
}

func testClose_Idempotent(t *testing.T) {
	s := openStore(t)
	if err := s.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A doc loaded with a fixed ClientID that the log wrote under takes a new
// random ID, as yjs does when it loads its own history, so its next edits
// cannot collide with the stored ones; with another ID it keeps it.
func testLoadDoc_FixedClientIDInTheLogIsReplaced(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.StoreUpdate(ctx, "doc", applyMapSet(t, 7, "k", "v")); err != nil {
		t.Fatal(err)
	}
	d, err := persist.LoadDoc(ctx, s, "doc", doc.Options{ClientID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if d.ClientID() == 7 {
		t.Fatal("the loaded doc kept client ID 7, which the log wrote under")
	}
	d, err = persist.LoadDoc(ctx, s, "doc", doc.Options{ClientID: 8})
	if err != nil {
		t.Fatal(err)
	}
	if d.ClientID() != 8 {
		t.Fatalf("a doc loaded with an ID the log does not use got %d", d.ClientID())
	}
}

// From persist/versions_test.go.

// readKey loads the "settings" root map value for key from d.
func readKey(d *doc.Doc, key string) any {
	return types.NewMap(d.Branch("settings")).Get(key)
}

func testSaveVersion_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.StoreUpdate(ctx, "doc", applyMapSet(t, 100, "a", "one")); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreUpdate(ctx, "doc", applyMapSet(t, 101, "b", "two")); err != nil {
		t.Fatal(err)
	}

	id, err := persist.SaveVersion(ctx, s, "doc", "checkpoint")
	if err != nil {
		t.Fatalf("SaveVersion: %v", err)
	}

	infos, err := s.ListVersions(ctx, "doc")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("got %d versions, want 1", len(infos))
	}
	if infos[0].ID != id || infos[0].Label != "checkpoint" || infos[0].Size == 0 {
		t.Errorf("info = %+v, want ID %d label %q non-zero size", infos[0], id, "checkpoint")
	}
	if infos[0].CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}

	v, err := persist.LoadVersion(ctx, s, "doc", id, doc.Options{})
	if err != nil {
		t.Fatalf("LoadVersion: %v", err)
	}
	if got := readKey(v, "a"); got != "one" {
		t.Errorf("a = %v, want one", got)
	}
	if got := readKey(v, "b"); got != "two" {
		t.Errorf("b = %v, want two", got)
	}
}

func testSaveVersion_UnknownDocument(t *testing.T) {
	s := newTestStore(t)
	_, err := persist.SaveVersion(context.Background(), s, "ghost", "")
	if !errors.Is(err, persist.ErrUnknownDocument) {
		t.Errorf("err = %v, want ErrUnknownDocument", err)
	}
}

// testVersions_SurviveFlushAndClear locks the independence contract:
// the live log can be compacted or cleared without touching history.
func testVersions_SurviveFlushAndClear(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.StoreUpdate(ctx, "doc", applyMapSet(t, 110, "k", "v")); err != nil {
		t.Fatal(err)
	}
	id, err := persist.SaveVersion(ctx, s, "doc", "kept")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Flush(ctx, "doc"); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := s.ClearDocument(ctx, "doc"); err != nil {
		t.Fatalf("ClearDocument: %v", err)
	}

	v, err := persist.LoadVersion(ctx, s, "doc", id, doc.Options{})
	if err != nil {
		t.Fatalf("LoadVersion after clear: %v", err)
	}
	if got := readKey(v, "k"); got != "v" {
		t.Errorf("k = %v, want v", got)
	}
}

func testRestoreVersion_RewindsLiveLog(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.StoreUpdate(ctx, "doc", applyMapSet(t, 120, "k", "old")); err != nil {
		t.Fatal(err)
	}
	id, err := persist.SaveVersion(ctx, s, "doc", "before")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StoreUpdate(ctx, "doc", applyMapSet(t, 121, "k", "new")); err != nil {
		t.Fatal(err)
	}

	if err := s.RestoreVersion(ctx, "doc", id); err != nil {
		t.Fatalf("RestoreVersion: %v", err)
	}

	d, err := persist.LoadDoc(ctx, s, "doc", doc.Options{})
	if err != nil {
		t.Fatalf("LoadDoc: %v", err)
	}
	if got := readKey(d, "k"); got != "old" {
		t.Errorf("k = %v, want old (post-version update rewound)", got)
	}
	updates, err := s.GetUpdates(ctx, "doc")
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 {
		t.Errorf("log has %d blobs after restore, want 1", len(updates))
	}
}

func testRestoreVersion_NotFound(t *testing.T) {
	s := newTestStore(t)
	err := s.RestoreVersion(context.Background(), "doc", 999)
	if !errors.Is(err, persist.ErrVersionNotFound) {
		t.Errorf("err = %v, want ErrVersionNotFound", err)
	}
}

func testGetVersionState_NotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetVersionState(context.Background(), "doc", 999)
	if !errors.Is(err, persist.ErrVersionNotFound) {
		t.Errorf("err = %v, want ErrVersionNotFound", err)
	}
}

func testDeleteVersion_Idempotent(t *testing.T) {
	s := newTestStore(t)
	if err := s.DeleteVersion(context.Background(), "doc", 999); err != nil {
		t.Errorf("deleting unknown version: %v", err)
	}
}

func testPruneVersions_KeepsNewest(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.StoreUpdate(ctx, "doc", applyMapSet(t, 130, "k", "v")); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for i := 0; i < 5; i++ {
		id, err := persist.SaveVersion(ctx, s, "doc", "")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	deleted, err := persist.PruneVersions(ctx, s, "doc", 2)
	if err != nil {
		t.Fatalf("PruneVersions: %v", err)
	}
	if deleted != 3 {
		t.Errorf("deleted = %d, want 3", deleted)
	}
	infos, err := s.ListVersions(ctx, "doc")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].ID != ids[3] || infos[1].ID != ids[4] {
		t.Errorf("survivors = %+v, want IDs %d, %d", infos, ids[3], ids[4])
	}

	// Idempotent: nothing more to prune.
	deleted, err = persist.PruneVersions(ctx, s, "doc", 2)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Errorf("second prune deleted %d, want 0", deleted)
	}
}

func testSaveVersion_EmptyStateRejected(t *testing.T) {
	s := newTestStore(t)
	_, err := s.SaveVersion(context.Background(), "doc", "x", nil)
	if !errors.Is(err, persist.ErrEmptyUpdate) {
		t.Errorf("err = %v, want ErrEmptyUpdate", err)
	}
}

// LoadVersion with a fixed ClientID that the version's state wrote under
// gives the loaded doc a new random ID, as LoadDoc does.
func testLoadVersion_FixedClientIDInTheStateIsReplaced(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.StoreUpdate(ctx, "doc", applyMapSet(t, 7, "k", "v")); err != nil {
		t.Fatal(err)
	}
	id, err := persist.SaveVersion(ctx, s, "doc", "")
	if err != nil {
		t.Fatal(err)
	}
	d, err := persist.LoadVersion(ctx, s, "doc", id, doc.Options{ClientID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if d.ClientID() == 7 {
		t.Fatal("the loaded version kept client ID 7, which its state wrote under")
	}
}

// From persist/incomplete_log_test.go.

// Two consecutive transaction updates captured from real yjs@13.6.32
// (doc.on("update") on a Y.Doc with clientID=42: push "A", then push "B").
// uB carries ONLY B's block, whose origin references A's - the exact shape a
// y-websocket client broadcasts. A log holding uB without uA is causally
// incomplete: reachable in the server because an update is applied and
// broadcast BEFORE it is persisted, so a fast peer's dependent edit can
// reach the Store first.
const (
	danglingUA = "01012a000801056974656d730177014100"
	danglingUB = "01012a01882a000177014200"
)

func mustHex(t *testing.T, h string) []byte {
	t.Helper()
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func itemsLen(t *testing.T, blobs ...[]byte) int {
	t.Helper()
	d := ygo.NewDoc()
	for _, b := range blobs {
		if len(b) == 0 {
			continue
		}
		if err := ygo.ApplyUpdate(d, b); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
	arr := ygo.NewArray(d, "items")
	rt := d.ReadTxn()
	defer rt.Close()
	return int(arr.Len())
}

// testMergeUpdates_RefusesIncompleteLog is the data-loss regression: before
// the fix, MergeUpdates of a log holding only the dependent update returned
// an EMPTY snapshot (the pending block was silently dropped), and the
// destructive replace in Flush then erased the original bytes forever. Now
// it refuses with ErrIncompleteLog, and the complete log still compacts.
func testMergeUpdates_RefusesIncompleteLog(t *testing.T) {
	uA, uB := mustHex(t, danglingUA), mustHex(t, danglingUB)
	if got := itemsLen(t, uB, uA); got != 2 {
		t.Fatalf("sanity: out-of-order replay len=%d, want 2", got)
	}

	if _, err := persist.MergeUpdates([][]byte{uB}); !errors.Is(err, persist.ErrIncompleteLog) {
		t.Fatalf("MergeUpdates(dangling) err=%v, want ErrIncompleteLog", err)
	}

	snap, err := persist.MergeUpdates([][]byte{uB, uA})
	if err != nil {
		t.Fatalf("MergeUpdates(complete) err=%v", err)
	}
	if got := itemsLen(t, snap); got != 2 {
		t.Fatalf("compacted complete log replays len=%d, want 2", got)
	}
}

// testFlush_IncompleteLogSurvives drives the same shape through the
// store: a Flush over the causally incomplete log must refuse, leave
// the original bytes in place, and succeed once the ancestor lands.
func testFlush_IncompleteLogSurvives(t *testing.T) {
	uA, uB := mustHex(t, danglingUA), mustHex(t, danglingUB)
	s := newTestStore(t)
	ctx := context.Background()

	// An unrelated, self-contained update from another client: the log needs
	// two entries because Flush short-circuits a single-update log without
	// merging (already optimal - and, usefully, incapable of losing anything).
	other := ygo.NewDocWithOptions(ygo.Options{ClientID: 7})
	om := ygo.NewMap(other, "other")
	otxn := other.WriteTxn()
	om.Set(otxn, "k", "v")
	otxn.Commit()
	uC := ygo.EncodeStateAsUpdate(other)

	if err := s.StoreUpdate(ctx, "d", uB); err != nil { // dependent, ancestor absent
		t.Fatal(err)
	}
	if err := s.StoreUpdate(ctx, "d", uC); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx, "d"); !errors.Is(err, persist.ErrIncompleteLog) {
		t.Fatalf("Flush(incomplete) err=%v, want ErrIncompleteLog", err)
	}
	got, err := s.GetUpdates(ctx, "d")
	if err != nil || len(got) != 2 || itemsLen(t, got[0], got[1], uA) != 2 {
		t.Fatalf("refused flush must leave the log intact: n=%d err=%v", len(got), err)
	}

	if err := s.StoreUpdate(ctx, "d", uA); err != nil { // ancestor lands
		t.Fatal(err)
	}
	if err := s.Flush(ctx, "d"); err != nil {
		t.Fatalf("Flush(complete) err=%v", err)
	}
	got, err = s.GetUpdates(ctx, "d")
	if err != nil {
		t.Fatal(err)
	}
	if got2 := itemsLen(t, got...); got2 != 2 {
		t.Fatalf("post-flush replay len=%d, want 2 (compaction lost a dependent update)", got2)
	}
	if len(got) != 1 {
		t.Fatalf("complete log compacted into %d blobs, want 1", len(got))
	}
}

// testCanceledContext_ChangesNothing: a store refuses work under a
// canceled context and changes nothing, as SQLite's database/sql does; a
// client's shutdown persists with a fresh context for that reason.
func testCanceledContext_ChangesNothing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	u1 := applyMapSet(t, 801, "a", "1")
	u2 := applyMapSet(t, 802, "b", "2")
	for _, u := range [][]byte{u1, u2} {
		if err := s.StoreUpdate(ctx, "d", u); err != nil {
			t.Fatal(err)
		}
	}
	id, err := s.SaveVersion(ctx, "d", "v", u1)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	for name, op := range map[string]func() error{
		"StoreUpdate":    func() error { return s.StoreUpdate(canceled, "d", u1) },
		"Flush":          func() error { return s.Flush(canceled, "d") },
		"ClearDocument":  func() error { return s.ClearDocument(canceled, "d") },
		"RestoreVersion": func() error { return s.RestoreVersion(canceled, "d", id) },
		"DeleteVersion":  func() error { return s.DeleteVersion(canceled, "d", id) },
		"SaveVersion":    func() error { _, err := s.SaveVersion(canceled, "d", "w", u2); return err },
	} {
		if err := op(); err == nil {
			t.Errorf("%s under a canceled context: no error", name)
		}
	}
	got, err := s.GetUpdates(ctx, "d")
	if err != nil || len(got) != 2 || !bytesEqual(got[0], u1) || !bytesEqual(got[1], u2) {
		t.Fatalf("the log changed under a canceled context: %d updates, %v", len(got), err)
	}
	versions, err := s.ListVersions(ctx, "d")
	if err != nil || len(versions) != 1 || versions[0].ID != id {
		t.Fatalf("the versions changed under a canceled context: %v, %v", versions, err)
	}
}
