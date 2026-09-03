package ygo_test

import (
	"bytes"
	"testing"

	"github.com/Deln0r/ygo"
)

// These properties are what makes an unreliable, unordered transport (a
// Matrix room, a gossip bus, an at-least-once queue) a legal carrier for ygo
// updates: redelivering an update must be a no-op, and delivery order must
// not affect the result. YATA gives them by construction - items are
// addressed by client+clock and integrate skips what the store already has -
// but "by construction" is a claim, and integration/matrix now depends on it,
// so it is pinned here. The whole fixture suite never exercised redelivery:
// every scenario applies each update exactly once.

func textOf(t *testing.T, d *ygo.Doc, name string) string {
	t.Helper()
	txt := ygo.NewText(d, name)
	rt := d.ReadTxn()
	defer rt.Close()
	return txt.String()
}

func helloUpdate(t *testing.T, clientID uint64, s string) []byte {
	t.Helper()
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: clientID})
	txt := ygo.NewText(d, "t")
	txn := d.WriteTxn()
	if err := txt.Insert(txn, 0, s); err != nil {
		t.Fatalf("insert: %v", err)
	}
	txn.Commit()
	return ygo.EncodeStateAsUpdate(d)
}

// TestApplyUpdate_IsIdempotent: redelivering the same update leaves the
// document unchanged, in V1 and V2, whether the duplicate arrives as a second
// ApplyUpdate call or concatenated into one blob.
func TestApplyUpdate_IsIdempotent(t *testing.T) {
	u := helloUpdate(t, 1, "hello")

	d := ygo.NewDoc()
	if err := ygo.ApplyUpdate(d, u); err != nil {
		t.Fatal(err)
	}
	once := textOf(t, d, "t")
	if once != "hello" {
		t.Fatalf("first apply = %q, want %q", once, "hello")
	}
	if err := ygo.ApplyUpdate(d, u); err != nil {
		t.Fatal(err)
	}
	if got := textOf(t, d, "t"); got != once {
		t.Fatalf("redelivery changed the document: %q -> %q", once, got)
	}

	// Concatenation is NOT a way to batch updates, and asserting on u||u
	// proves nothing about idempotence: ApplyUpdate decodes one update and
	// ignores the rest of the buffer, so u||u and u||anything-else give the
	// same answer. Measured on yjs 13.6.32: identical behaviour, so this is
	// the port being faithful rather than a defect. What matters is that the
	// loss is detectable before it happens - ValidateUpdate is the check a
	// network-facing caller runs, and it must refuse the trailing bytes.
	appended := append(append([]byte(nil), u...), helloUpdate(t, 3, "world")...)
	catDoc := ygo.NewDoc()
	if err := ygo.ApplyUpdate(catDoc, appended); err != nil {
		t.Fatal(err)
	}
	if got := textOf(t, catDoc, "t"); got != once {
		t.Fatalf("appended updates = %q, want %q (only the first is read)", got, once)
	}
	if err := ygo.ValidateUpdate(appended); err == nil {
		t.Fatal("ValidateUpdate accepted concatenated updates; the silent half-loss above would reach a document undetected")
	}
	if err := ygo.ValidateUpdate(u); err != nil {
		t.Fatalf("ValidateUpdate rejected a single well-formed update: %v", err)
	}
	// MergeUpdates is the supported way to combine, and it keeps both halves.
	merged, err := ygo.MergeUpdates([][]byte{u, helloUpdate(t, 3, "world")})
	if err != nil {
		t.Fatal(err)
	}
	mDoc := ygo.NewDoc()
	if err := ygo.ApplyUpdate(mDoc, merged); err != nil {
		t.Fatal(err)
	}
	if got := textOf(t, mDoc, "t"); len(got) != len("hello")+len("world") {
		t.Fatalf("merged updates = %q, want both halves present", got)
	}

	// Positive control: two DIFFERENT clients inserting the same string is
	// genuinely new content, not a redelivery. If this assertion ever reads
	// "hello", the checks above are vacuous - they would be passing because
	// the comparison cannot see duplication at all.
	other := helloUpdate(t, 2, "hello")
	dc := ygo.NewDoc()
	if err := ygo.ApplyUpdate(dc, u); err != nil {
		t.Fatal(err)
	}
	if err := ygo.ApplyUpdate(dc, other); err != nil {
		t.Fatal(err)
	}
	if got := textOf(t, dc, "t"); got != "hellohello" {
		t.Fatalf("distinct-client duplicate content = %q, want %q (control: the checks above must be able to see duplication)", got, "hellohello")
	}

	v2 := func() []byte {
		s := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
		txt := ygo.NewText(s, "t")
		txn := s.WriteTxn()
		_ = txt.Insert(txn, 0, "hello")
		txn.Commit()
		return ygo.EncodeStateAsUpdateV2(s)
	}()
	dv := ygo.NewDoc()
	if err := ygo.ApplyUpdateV2(dv, v2); err != nil {
		t.Fatal(err)
	}
	v2once := textOf(t, dv, "t")
	if err := ygo.ApplyUpdateV2(dv, v2); err != nil {
		t.Fatal(err)
	}
	if got := textOf(t, dv, "t"); got != v2once {
		t.Fatalf("V2 redelivery changed the document: %q -> %q", v2once, got)
	}
}

// TestMergeUpdates_IsIdempotent: merging an update with itself yields the
// same state as merging it alone - the shape a log compaction sees when a
// transport delivered a duplicate.
func TestMergeUpdates_IsIdempotent(t *testing.T) {
	u := helloUpdate(t, 1, "hello")

	one, err := ygo.MergeUpdates([][]byte{u})
	if err != nil {
		t.Fatalf("merge one: %v", err)
	}
	twice, err := ygo.MergeUpdates([][]byte{u, u})
	if err != nil {
		t.Fatalf("merge doubled: %v", err)
	}
	if !bytes.Equal(one, twice) {
		t.Errorf("merge([u,u]) differs byte-wise from merge([u]): %d vs %d bytes", len(one), len(twice))
	}
	d1, d2 := ygo.NewDoc(), ygo.NewDoc()
	if err := ygo.ApplyUpdate(d1, one); err != nil {
		t.Fatal(err)
	}
	if err := ygo.ApplyUpdate(d2, twice); err != nil {
		t.Fatal(err)
	}
	if got, want := textOf(t, d2, "t"), textOf(t, d1, "t"); got != want {
		t.Fatalf("doubled merge = %q, single merge = %q", got, want)
	}
}

// TestApplyUpdate_OrderIndependent: concurrent edits converge regardless of
// arrival order, and survive a shuffled stream with duplicates - the delivery
// guarantee a Matrix room actually offers (none).
func TestApplyUpdate_OrderIndependent(t *testing.T) {
	uA := helloUpdate(t, 7, "alpha")
	uB := helloUpdate(t, 9, "beta")

	ab, ba := ygo.NewDoc(), ygo.NewDoc()
	for _, u := range [][]byte{uA, uB} {
		if err := ygo.ApplyUpdate(ab, u); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range [][]byte{uB, uA} {
		if err := ygo.ApplyUpdate(ba, u); err != nil {
			t.Fatal(err)
		}
	}
	want := textOf(t, ab, "t")
	if got := textOf(t, ba, "t"); got != want {
		t.Fatalf("B-then-A = %q, A-then-B = %q", got, want)
	}
	// Convergence on an empty document is also convergence and proves
	// nothing: assert both edits survived.
	if len(want) != len("alphabeta") {
		t.Fatalf("converged text %q lost content; want both edits present", want)
	}

	shuffled := ygo.NewDoc()
	for _, u := range [][]byte{uB, uA, uA, uB, uA} {
		if err := ygo.ApplyUpdate(shuffled, u); err != nil {
			t.Fatal(err)
		}
	}
	if got := textOf(t, shuffled, "t"); got != want {
		t.Fatalf("shuffled duplicated stream = %q, want %q", got, want)
	}
}

// TestApplyUpdate_HoldsClockGapPending pins the property the Matrix transport
// leans on hardest, and the one this port got wrong until 2026-09-03: an
// update that starts above the clock we have for its own client must WAIT,
// not integrate.
//
// The scenario is ordinary, not adversarial. One client edits root A, records
// a state vector, edits root B, and publishes only the second delta - exactly
// what ygo.EncodeDiff is for. A Matrix reader pages history newest-first, so
// it meets that delta before the earlier one. Integrating it immediately
// pushes the store clock past the hole; the earlier update then reads as
// already-known and is dropped, and root A is gone for good. Before the fix
// this test read A="" both times. yjs 13.6.32 holds the delta pending and
// ends with both roots populated; so must we.
func TestApplyUpdate_HoldsClockGapPending(t *testing.T) {
	c := ygo.NewDocWithOptions(ygo.Options{ClientID: 42})
	a := ygo.NewText(c, "A")
	txn := c.WriteTxn()
	if err := a.Insert(txn, 0, "first"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	early := ygo.EncodeStateAsUpdate(c)
	sv := ygo.EncodeStateVector(c)

	b := ygo.NewText(c, "B")
	txn = c.WriteTxn()
	if err := b.Insert(txn, 0, "second"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	late, err := ygo.EncodeDiff(c, sv)
	if err != nil {
		t.Fatal(err)
	}

	r := ygo.NewDoc()
	if err := ygo.ApplyUpdate(r, late); err != nil {
		t.Fatal(err)
	}
	if got := textOf(t, r, "B"); got != "" {
		t.Fatalf("B = %q after a delta with a clock gap; it must queue, not integrate", got)
	}
	if !ygo.HasPending(r) {
		t.Fatal("no pending state after a delta with a clock gap: the gap was integrated over, which is what loses the earlier update below")
	}

	if err := ygo.ApplyUpdate(r, early); err != nil {
		t.Fatal(err)
	}
	if got := textOf(t, r, "A"); got != "first" {
		t.Fatalf("A = %q after the gap was filled, want %q - the earlier update was dropped as already-known", got, "first")
	}
	if got := textOf(t, r, "B"); got != "second" {
		t.Fatalf("B = %q after the gap was filled, want %q - the queued delta never drained", got, "second")
	}

	// Positive control: the same two updates in causal order must also
	// converge, so a failure above is about the gap and not about EncodeDiff
	// producing something unusable in the first place.
	f := ygo.NewDoc()
	if err := ygo.ApplyUpdate(f, early); err != nil {
		t.Fatal(err)
	}
	if err := ygo.ApplyUpdate(f, late); err != nil {
		t.Fatal(err)
	}
	if textOf(t, f, "A") != "first" || textOf(t, f, "B") != "second" {
		t.Fatalf("control: in-order delivery gave A=%q B=%q", textOf(t, f, "A"), textOf(t, f, "B"))
	}
}
