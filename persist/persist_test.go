package persist_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/persist"
	"github.com/Deln0r/ygo/persist/sqlite"
)

// newTestStore returns a fresh in-memory sqlite store with cleanup
// registered on the test. Use this for every persist-layer test so
// no test leaves shared state behind.
func newTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

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

func TestStoreUpdate_RejectsEmpty(t *testing.T) {
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

func TestStoreUpdate_GetUpdates_RoundTrip(t *testing.T) {
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

func TestGetUpdates_UnknownDoc_ReturnsEmpty(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetUpdates(context.Background(), "never-seen")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("len=%d, want 0", len(got))
	}
}

func TestDocumentExists(t *testing.T) {
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

func TestListDocuments(t *testing.T) {
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

func TestClearDocument(t *testing.T) {
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

func TestLoadDoc_ReplayProducesEquivalentState(t *testing.T) {
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

func TestLoadDoc_UnknownDoc_ReturnsEmpty(t *testing.T) {
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

func TestLoadDoc_MultipleSeparateUpdates(t *testing.T) {
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

func TestFlush_CompactsToSingleUpdate(t *testing.T) {
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

func TestFlush_NoOpOnZeroOrOneUpdate(t *testing.T) {
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

func TestGetStateVector(t *testing.T) {
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

func TestGetDiff_AgainstEmptySV_EmitsAll(t *testing.T) {
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

func TestGetDiff_AgainstFullSV_EmitsNothingMeaningful(t *testing.T) {
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

func TestGetDiff_UnknownDoc_ReturnsNil(t *testing.T) {
	s := newTestStore(t)
	diff, err := persist.GetDiff(context.Background(), s, "missing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if diff != nil {
		t.Errorf("got %v, want nil", diff)
	}
}

func TestMergeUpdates_PreservesState(t *testing.T) {
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

func TestMergeUpdates_EmptyInput(t *testing.T) {
	got, err := persist.MergeUpdates(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestCrossDocIsolation(t *testing.T) {
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

func TestStoreUpdate_ConcurrentAppends(t *testing.T) {
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

func TestClose_Idempotent(t *testing.T) {
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
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
func TestLoadDoc_FixedClientIDInTheLogIsReplaced(t *testing.T) {
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
