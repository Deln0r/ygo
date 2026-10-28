package encoding_test

import (
	"testing"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/types"
)

// fullEncode captures the current full V1 state of d. Convenience.
func fullEncode(t *testing.T, d *doc.Doc) []byte {
	t.Helper()
	return encoding.EncodeStateAsUpdate(d)
}

// applyExpectNoErr panics on any decode/apply failure.
func applyExpectNoErr(t *testing.T, target *doc.Doc, raw []byte) {
	t.Helper()
	if err := encoding.ApplyUpdate(target, raw); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}
}

// TestPending_TwoClientsOutOfOrder_RetryDrains is the canonical
// scenario: client A inserts an Item, client B (which has already
// observed A) inserts a second Item whose Origin references A's
// tail. We encode B's full state and apply it to a target that has
// never seen A. B's Item should land in pending; once A's update
// arrives, the pending drains and both Items are visible.
func TestPending_TwoClientsOutOfOrder_RetryDrains(t *testing.T) {
	// Source A.
	a := doc.NewDocWithOptions(doc.Options{ClientID: 1001})
	arrA := types.NewArray(a.Branch("items"))
	t1 := a.WriteTxn()
	arrA.Push(t1, "from-a")
	t1.Commit()

	// Source B: import A then add its own item (Origin will reference A).
	b := doc.NewDocWithOptions(doc.Options{ClientID: 1002})
	applyExpectNoErr(t, b, fullEncode(t, a))
	arrB := types.NewArray(b.Branch("items"))
	t2 := b.WriteTxn()
	arrB.Push(t2, "from-b")
	t2.Commit()

	// Target receives B's state WITHOUT seeing A first.
	target := doc.NewDoc()
	applyExpectNoErr(t, target, fullEncode(t, b))

	// At this point B sent its client-list and A's client-list both
	// (EncodeStateAsUpdate sends everything). So B's items can
	// integrate immediately. To force pending, encode JUST B's client.
	//
	// Recipe: capture B's full state, manually re-encode emitting
	// only client B's blocks (skip A). We construct that via
	// EncodeDiff with a remoteSV that already covers A.
	rtxn := b.ReadTxn()
	knowAFully := rtxn.Store().GetStateVector()
	rtxn.Close()

	// Inject a fake "remote already has A" SV by setting A's entry
	// to its current value. Then EncodeDiff returns only B's blocks.
	remoteAlreadyHasA := map[uint64]uint64{1001: knowAFully[1001]}
	rtxn = b.ReadTxn()
	bOnly := encoding.EncodeDiff(b, rtxn, remoteAlreadyHasA)
	rtxn.Close()

	target2 := doc.NewDoc()
	applyExpectNoErr(t, target2, bOnly)

	// B's item references A — without A in the store, it queues.
	rtxn = target2.ReadTxn()
	if !encoding.HasPending(rtxn) {
		t.Fatalf("expected pending after applying B-only; blocks queued=%d",
			encoding.GetPending(rtxn).BlockCount())
	}
	missing := encoding.MissingSV(rtxn)
	rtxn.Close()
	// The request names A at the clock the target has for it, 0, so a
	// diff against it starts at A's first block.
	if got, ok := missing[1001]; !ok || got != 0 {
		t.Errorf("MissingSV = %v, want 1001 at 0", missing)
	}

	// Array should not yet show B's item (it's queued).
	larr := types.NewArray(target2.Branch("items"))
	if larr.Len() != 0 {
		t.Errorf("pre-drain Len = %d, want 0", larr.Len())
	}

	// Now apply A's full state — pending drains.
	applyExpectNoErr(t, target2, fullEncode(t, a))

	rtxn = target2.ReadTxn()
	defer rtxn.Close()
	if encoding.HasPending(rtxn) {
		t.Errorf("pending still has %d blocks after fill-in apply",
			encoding.GetPending(rtxn).BlockCount())
	}
	if larr.Len() != 2 {
		t.Fatalf("post-drain Len = %d, want 2", larr.Len())
	}
	got := larr.ToSlice()
	if got[0] != "from-a" || got[1] != "from-b" {
		t.Errorf("post-drain items = %v, want [from-a from-b]", got)
	}
}

// TestPending_DeleteSetBeforeItem queues a delete-set range whose
// target ID is not yet present, then verifies the tombstone
// applies once the item arrives. The classic scenario for this:
// client A creates an item; client B (observing A) deletes it;
// target receives B's update first.
func TestPending_DeleteSetBeforeItem(t *testing.T) {
	a := doc.NewDocWithOptions(doc.Options{ClientID: 4001})
	arrA := types.NewArray(a.Branch("items"))
	t1 := a.WriteTxn()
	arrA.Push(t1, "doomed")
	t1.Commit()

	b := doc.NewDocWithOptions(doc.Options{ClientID: 4002})
	applyExpectNoErr(t, b, fullEncode(t, a))
	arrB := types.NewArray(b.Branch("items"))
	t2 := b.WriteTxn()
	arrB.Delete(t2, 0, 1)
	t2.Commit()

	// Encode JUST B's state (client B has no items but does have
	// a delete-set referencing A's item).
	rtxn := b.ReadTxn()
	bSV := rtxn.Store().GetStateVector()
	rtxn.Close()
	remoteAlreadyHasA := map[uint64]uint64{4001: bSV[4001]}
	rtxn = b.ReadTxn()
	bOnly := encoding.EncodeDiff(b, rtxn, remoteAlreadyHasA)
	rtxn.Close()

	target := doc.NewDoc()
	applyExpectNoErr(t, target, bOnly)

	rtxn = target.ReadTxn()
	hasPending := encoding.HasPending(rtxn)
	rtxn.Close()
	if !hasPending {
		t.Fatal("expected pending DS for unseen target")
	}

	// Apply A — both A's item integrates and the pending DS finds
	// its target on the same Drain pass.
	applyExpectNoErr(t, target, fullEncode(t, a))

	larr := types.NewArray(target.Branch("items"))
	if larr.Len() != 0 {
		t.Errorf("Len = %d, want 0 (item tombstoned by retried DS)", larr.Len())
	}
	rtxn = target.ReadTxn()
	defer rtxn.Close()
	if encoding.HasPending(rtxn) {
		t.Errorf("pending non-empty after both applied: %d", encoding.GetPending(rtxn).BlockCount())
	}
}

// TestPending_StuckBlocksMissingSV verifies MissingSV exposes the
// gap a re-fetch should target.
func TestPending_StuckBlocksMissingSV(t *testing.T) {
	a := doc.NewDocWithOptions(doc.Options{ClientID: 3001})
	arrA := types.NewArray(a.Branch("items"))
	t1 := a.WriteTxn()
	arrA.Push(t1, "first")
	t1.Commit()

	b := doc.NewDocWithOptions(doc.Options{ClientID: 3002})
	applyExpectNoErr(t, b, fullEncode(t, a))
	arrB := types.NewArray(b.Branch("items"))
	t2 := b.WriteTxn()
	arrB.Push(t2, "second")
	t2.Commit()

	rtxn := b.ReadTxn()
	bSV := rtxn.Store().GetStateVector()
	rtxn.Close()
	remoteAlreadyHasA := map[uint64]uint64{3001: bSV[3001]}
	rtxn = b.ReadTxn()
	bOnly := encoding.EncodeDiff(b, rtxn, remoteAlreadyHasA)
	rtxn.Close()

	target := doc.NewDoc()
	applyExpectNoErr(t, target, bOnly)

	rtxn = target.ReadTxn()
	if !encoding.HasPending(rtxn) {
		rtxn.Close()
		t.Fatal("expected stuck pending")
	}
	missing := encoding.MissingSV(rtxn)
	rtxn.Close()
	if got, ok := missing[3001]; !ok || got != 0 {
		t.Fatalf("MissingSV = %v, want 3001 at 0, the clock the target has", missing)
	}
	// What the vector names is what A sends: a diff against it drains
	// the queue. Naming 3001 at 1, past A's only block, left it stuck.
	rtxn = a.ReadTxn()
	fill := encoding.EncodeDiff(a, rtxn, missing)
	rtxn.Close()
	applyExpectNoErr(t, target, fill)
	rtxn = target.ReadTxn()
	defer rtxn.Close()
	if encoding.HasPending(rtxn) {
		t.Fatal("the diff MissingSV asked for did not drain the queue")
	}
}

// TestPending_ApplyIdempotent ensures applying the same update
// twice does not double-queue and does not produce error.
func TestPending_ApplyIdempotent(t *testing.T) {
	a := doc.NewDocWithOptions(doc.Options{ClientID: 5001})
	arrA := types.NewArray(a.Branch("items"))
	t1 := a.WriteTxn()
	arrA.Push(t1, "alpha")
	arrA.Push(t1, "beta")
	t1.Commit()
	raw := fullEncode(t, a)

	target := doc.NewDoc()
	applyExpectNoErr(t, target, raw)
	applyExpectNoErr(t, target, raw) // idempotent

	larr := types.NewArray(target.Branch("items"))
	if larr.Len() != 2 {
		t.Errorf("Len = %d, want 2", larr.Len())
	}
	rtxn := target.ReadTxn()
	defer rtxn.Close()
	if encoding.HasPending(rtxn) {
		t.Errorf("pending non-empty after idempotent applies: %d",
			encoding.GetPending(rtxn).BlockCount())
	}
}

// TestPending_NoMissingDepsCleanApply confirms an in-order apply
// of a self-contained update leaves pending empty.
func TestPending_NoMissingDepsCleanApply(t *testing.T) {
	src := doc.NewDocWithOptions(doc.Options{ClientID: 7001})
	arrSrc := types.NewArray(src.Branch("items"))
	t1 := src.WriteTxn()
	arrSrc.Push(t1, "x")
	t1.Commit()

	target := doc.NewDoc()
	applyExpectNoErr(t, target, fullEncode(t, src))

	rtxn := target.ReadTxn()
	defer rtxn.Close()
	if encoding.HasPending(rtxn) {
		t.Errorf("pending non-empty after clean apply: %d",
			encoding.GetPending(rtxn).BlockCount())
	}
}

// TestPending_EmptyDocReportsEmptyState confirms the helpers
// behave on a brand-new doc (no pending state installed at all).
func TestPending_EmptyDocReportsEmptyState(t *testing.T) {
	d := doc.NewDoc()
	rtxn := d.ReadTxn()
	defer rtxn.Close()

	if encoding.HasPending(rtxn) {
		t.Error("fresh doc reports HasPending")
	}
	if got := encoding.GetPending(rtxn); got != nil {
		t.Errorf("fresh doc GetPending = %v, want nil", got)
	}
	if sv := encoding.MissingSV(rtxn); len(sv) != 0 {
		t.Errorf("fresh doc MissingSV = %v, want empty", sv)
	}
}

// TestPending_DrainConvergesInOneApply: a single Apply call with a
// self-contained update that references everything internally should
// drain in one go (no pending state installed at the end).
func TestPending_DrainConvergesInOneApply(t *testing.T) {
	src := doc.NewDocWithOptions(doc.Options{ClientID: 8001})
	arrSrc := types.NewArray(src.Branch("items"))
	for i := 0; i < 5; i++ {
		txn := src.WriteTxn()
		arrSrc.Push(txn, "v")
		txn.Commit()
	}

	target := doc.NewDoc()
	applyExpectNoErr(t, target, fullEncode(t, src))

	larr := types.NewArray(target.Branch("items"))
	rtxn := target.ReadTxn()
	defer rtxn.Close()
	if encoding.HasPending(rtxn) {
		t.Errorf("self-contained update left pending: %d blocks",
			encoding.GetPending(rtxn).BlockCount())
	}
	if larr.Len() != 5 {
		t.Errorf("Len = %d, want 5", larr.Len())
	}
}
