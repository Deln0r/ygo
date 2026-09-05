package ygo_test

import (
	"bytes"
	"testing"

	"github.com/Deln0r/ygo"
)

// gapUpdates builds one client's edits to three separate roots and returns the
// pieces a merge can be asked about: the first root's update, a delta carrying
// only the third root (so it starts above a hole), and a filler that closes
// the hole.
//
// Separate roots matter. A single Text run squashes into one cell at commit,
// and EncodeDiff then emits the whole run, so a same-root scenario produces no
// gap at all and would test nothing.
func gapUpdates(t *testing.T) (first, late, filler, full []byte) {
	t.Helper()
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 42})

	a := ygo.NewText(d, "A")
	txn := d.WriteTxn()
	if err := a.Insert(txn, 0, "aaaaa"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	first = ygo.EncodeStateAsUpdate(d)
	svAfterA := ygo.EncodeStateVector(d)

	b := ygo.NewText(d, "B")
	txn = d.WriteTxn()
	if err := b.Insert(txn, 0, "bbbbb"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	svAfterB := ygo.EncodeStateVector(d)

	c := ygo.NewText(d, "C")
	txn = d.WriteTxn()
	if err := c.Insert(txn, 0, "ccccc"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()

	var err error
	if late, err = ygo.EncodeDiff(d, svAfterB); err != nil {
		t.Fatal(err)
	}
	if filler, err = ygo.EncodeDiff(d, svAfterA); err != nil {
		t.Fatal(err)
	}
	return first, late, filler, ygo.EncodeStateAsUpdate(d)
}

func rootText(t *testing.T, d *ygo.Doc, name string) string {
	t.Helper()
	txt := ygo.NewText(d, name)
	rt := d.ReadTxn()
	defer rt.Close()
	return txt.String()
}

// TestMergeUpdates_PreservesPendingBlocks: merging must not lose an update
// just because its causal ancestor is absent from the set being merged.
//
// Before this, MergeUpdates applied everything to a scratch document and
// re-encoded it, so a block parked in the pending buffer never reached the
// output: merging a lone gap delta returned an EMPTY update and the edit was
// gone with no error anywhere. yjs 13.6.32 returns that delta unchanged
// (measured), because its mergeUpdates preserves unintegrated structs and
// writes a Skip run for the hole.
func TestMergeUpdates_PreservesPendingBlocks(t *testing.T) {
	first, late, filler, full := gapUpdates(t)

	t.Run("a lone gap delta survives the merge", func(t *testing.T) {
		merged, err := ygo.MergeUpdates([][]byte{late})
		if err != nil {
			t.Fatal(err)
		}
		// Nothing to merge it with and nothing integrable in it, so the only
		// correct answer is the delta itself. yjs returns it unchanged too.
		// Before the pending buffer was included in the encoding this
		// returned a two-byte empty update.
		if !bytes.Equal(merged, late) {
			t.Fatalf("merging a lone delta changed it:\n got %d (%d bytes)\nwant %d (%d bytes)", merged, len(merged), late, len(late))
		}
		// And it still carries its edit, once the rest of the history shows
		// up. The filler alone is not enough: it starts above the very first
		// edit, so the whole document has to arrive for the hole to close.
		d := ygo.NewDoc()
		if err := ygo.ApplyUpdate(d, merged); err != nil {
			t.Fatal(err)
		}
		if !ygo.HasPending(d) {
			t.Fatal("a delta with a clock gap did not queue")
		}
		if err := ygo.ApplyUpdate(d, full); err != nil {
			t.Fatal(err)
		}
		if got := rootText(t, d, "C"); got != "ccccc" {
			t.Fatalf("C = %q after the history arrived; the merge lost the delta", got)
		}
	})

	t.Run("a set spanning a hole keeps both sides", func(t *testing.T) {
		merged, err := ygo.MergeUpdates([][]byte{first, late})
		if err != nil {
			t.Fatal(err)
		}
		d := ygo.NewDoc()
		if err := ygo.ApplyUpdate(d, merged); err != nil {
			t.Fatal(err)
		}
		// The near side integrates; the far side must WAIT rather than
		// integrate over the hole.
		if got := rootText(t, d, "A"); got != "aaaaa" {
			t.Fatalf("A = %q, want the integrated near side", got)
		}
		if got := rootText(t, d, "C"); got != "" {
			t.Fatalf("C = %q before the gap was filled; it must stay queued", got)
		}
		if !ygo.HasPending(d) {
			t.Fatal("nothing pending: the far side was integrated over the hole instead of queued")
		}
		if err := ygo.ApplyUpdate(d, filler); err != nil {
			t.Fatal(err)
		}
		for root, want := range map[string]string{"A": "aaaaa", "B": "bbbbb", "C": "ccccc"} {
			if got := rootText(t, d, root); got != want {
				t.Fatalf("%s = %q, want %q after the gap was filled", root, got, want)
			}
		}
	})

	t.Run("the hole is emitted as a Skip run", func(t *testing.T) {
		merged, err := ygo.MergeUpdates([][]byte{first, late})
		if err != nil {
			t.Fatal(err)
		}
		// yjs emits `item, Skip(len), item` for this shape and so must we:
		// closing the hole instead would let a receiver integrate over it,
		// which is the data-loss path TestApplyUpdate_HoldsClockGapPending
		// covers from the other side. 10 is BLOCK_SKIP_REF_NUMBER, followed
		// by the run length; here the hole is root B's five characters.
		// Assert on the structure, not on a substring: {10, 5} can occur
		// inside content by coincidence. The header is clientCount,
		// blockCount, clientID, startClock; the Skip has to be the block
		// that follows the first item, and the run has to declare three
		// blocks rather than two.
		if merged[0] != 1 {
			t.Fatalf("client count = %d, want 1", merged[0])
		}
		if merged[1] != 3 {
			t.Fatalf("client run declares %d blocks, want 3 (item, Skip, item)", merged[1])
		}
		// item(A) is info byte 4 then parent info then 5 chars of 'a'; the
		// Skip follows it immediately as ref 10 with the hole's length.
		i := bytes.Index(merged, []byte("aaaaa"))
		if i < 0 {
			t.Fatalf("first item missing from the merged output: % d", merged)
		}
		if rest := merged[i+5:]; len(rest) < 2 || rest[0] != 10 || rest[1] != 5 {
			t.Fatalf("block after the first item is % d, want a Skip run {10, 5}", rest[:min(2, len(rest))])
		}
	})

	t.Run("a complete set is unchanged in shape", func(t *testing.T) {
		// Nothing queued: the merge must go down the ordinary encoder path,
		// so this stays byte-identical to what every fixture pins.
		merged, err := ygo.MergeUpdates([][]byte{first})
		if err != nil {
			t.Fatal(err)
		}
		d := ygo.NewDoc()
		if err := ygo.ApplyUpdate(d, first); err != nil {
			t.Fatal(err)
		}
		if want := ygo.EncodeStateAsUpdate(d); !bytes.Equal(merged, want) {
			t.Fatalf("merging a complete set changed the bytes:\n got %d\nwant %d", merged, want)
		}
	})
}

// TestMergeUpdatesV2_PreservesPendingBlocks is the V2 twin. The formats differ
// in how a Skip carries its length - V2 puts it in the rest stream rather than
// the len column - so the emitters are separate code and need separate proof.
func TestMergeUpdatesV2_PreservesPendingBlocks(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 77})
	a := ygo.NewText(d, "A")
	txn := d.WriteTxn()
	if err := a.Insert(txn, 0, "aaaaa"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	first := ygo.EncodeStateAsUpdateV2(d)
	svAfterA := ygo.EncodeStateVector(d)

	b := ygo.NewText(d, "B")
	txn = d.WriteTxn()
	if err := b.Insert(txn, 0, "bbbbb"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	svAfterB := ygo.EncodeStateVector(d)

	// The filler is captured HERE, before the far side exists, so it closes
	// the hole and carries NOTHING ELSE. Taken later it would also carry root
	// C, and every assertion below would pass whether or not the merge
	// preserved anything - which is exactly what this test did until a
	// mutation run caught it.
	filler, err := ygo.EncodeDiffV2(d, svAfterA)
	if err != nil {
		t.Fatal(err)
	}

	c := ygo.NewText(d, "C")
	txn = d.WriteTxn()
	if err := c.Insert(txn, 0, "ccccc"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()

	late, err := ygo.EncodeDiffV2(d, svAfterB)
	if err != nil {
		t.Fatal(err)
	}

	merged, err := ygo.MergeUpdatesV2([][]byte{first, late})
	if err != nil {
		t.Fatal(err)
	}
	r := ygo.NewDoc()
	if err := ygo.ApplyUpdateV2(r, merged); err != nil {
		t.Fatal(err)
	}
	if got := rootText(t, r, "A"); got != "aaaaa" {
		t.Fatalf("A = %q", got)
	}
	if got := rootText(t, r, "C"); got != "" {
		t.Fatalf("C = %q before the gap was filled; it must stay queued", got)
	}
	if !ygo.HasPending(r) {
		t.Fatal("nothing pending after applying the merged V2 update: the far side was dropped at merge time, not queued")
	}
	if err := ygo.ApplyUpdateV2(r, filler); err != nil {
		t.Fatal(err)
	}
	for root, want := range map[string]string{"A": "aaaaa", "B": "bbbbb", "C": "ccccc"} {
		if got := rootText(t, r, root); got != want {
			t.Fatalf("%s = %q, want %q after the gap was filled", root, got, want)
		}
	}

	// Same no-pending guarantee as V1.
	solo, err := ygo.MergeUpdatesV2([][]byte{first})
	if err != nil {
		t.Fatal(err)
	}
	clean := ygo.NewDoc()
	if err := ygo.ApplyUpdateV2(clean, first); err != nil {
		t.Fatal(err)
	}
	if want := ygo.EncodeStateAsUpdateV2(clean); !bytes.Equal(solo, want) {
		t.Fatal("merging a complete V2 set changed the bytes")
	}
}

// TestMergeUpdates_ReconcilesOverlappingBlocks: two updates in a merge set can
// describe the same clocks differently, and emitting both would corrupt the
// result rather than merely bloat it.
//
// Neither wire format carries a per-block clock - a receiver derives each
// block's clock by accumulating lengths from the run's declared start - so a
// second, overlapping record is RELABELLED at the end of the first. It lands on
// clocks its client never minted, duplicates content, and shifts every later
// block in the run. Both ygo and yjs accept the result without error, which is
// what makes it worth a test: the failure is silent.
//
// The scenario is ordinary. One peer publishes a client's run whole; another,
// having spliced into the middle of it, publishes the same run split in two.
// Above a causal hole neither integrates, so both reach the merge plan. This
// produced "cccccXccc" before reconciliation existed.
func TestMergeUpdates_ReconcilesOverlappingBlocks(t *testing.T) {
	author := ygo.NewDocWithOptions(ygo.Options{ClientID: 42})
	a := ygo.NewText(author, "A")
	txn := author.WriteTxn()
	if err := a.Insert(txn, 0, "aaaaa"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	b := ygo.NewText(author, "B")
	txn = author.WriteTxn()
	if err := b.Insert(txn, 0, "bbbbb"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	svAfterB := ygo.EncodeStateVector(author)
	c := ygo.NewText(author, "C")
	txn = author.WriteTxn()
	if err := c.Insert(txn, 0, "ccccc"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()

	// A peer that holds the history and splices into the middle of C, which
	// splits the author's run in its store.
	peer := ygo.NewDoc()
	if err := ygo.ApplyUpdate(peer, ygo.EncodeStateAsUpdate(author)); err != nil {
		t.Fatal(err)
	}
	pc := ygo.NewText(peer, "C")
	txn = peer.WriteTxn()
	if err := pc.Insert(txn, 2, "X"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	want := rootText(t, peer, "C")
	if want != "ccXccc" {
		t.Fatalf("setup: peer reads C = %q", want)
	}

	fromAuthor, err := ygo.EncodeDiff(author, svAfterB)
	if err != nil {
		t.Fatal(err)
	}
	fromPeer, err := ygo.EncodeDiff(peer, svAfterB)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := ygo.MergeUpdates([][]byte{fromAuthor, fromPeer})
	if err != nil {
		t.Fatal(err)
	}

	full := ygo.EncodeStateAsUpdate(author)
	viaMerge := ygo.NewDoc()
	if err := ygo.ApplyUpdate(viaMerge, merged); err != nil {
		t.Fatal(err)
	}
	if err := ygo.ApplyUpdate(viaMerge, full); err != nil {
		t.Fatal(err)
	}

	// The control is the same updates applied without merging: whatever that
	// converges on is what the merge must also produce.
	control := ygo.NewDoc()
	for _, u := range [][]byte{fromAuthor, fromPeer, full} {
		if err := ygo.ApplyUpdate(control, u); err != nil {
			t.Fatal(err)
		}
	}
	if got := rootText(t, control, "C"); got != want {
		t.Fatalf("control diverged on its own: %q, want %q", got, want)
	}
	if got := rootText(t, viaMerge, "C"); got != want {
		t.Fatalf("merging overlapping views of one client's run gave C = %q, want %q; the overlap was emitted twice and relabelled", got, want)
	}
}

// TestMergeUpdates_PreservesPendingDeletes: a delete whose target has not
// arrived is queued in the same buffer as an unintegrated block, and merging
// has to carry it for the same reason - the sender handed it to us.
//
// This is the second wire-shape change the preserving encoder makes and the
// one with no obvious symptom: a dropped pending delete does not corrupt
// anything, it just quietly resurrects content the author deleted once the
// insert finally shows up.
func TestMergeUpdates_PreservesPendingDeletes(t *testing.T) {
	author := ygo.NewDocWithOptions(ygo.Options{ClientID: 55})
	txt := ygo.NewText(author, "t")
	txn := author.WriteTxn()
	if err := txt.Insert(txn, 0, "abcd"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	insert := ygo.EncodeStateAsUpdate(author)
	svAfterInsert := ygo.EncodeStateVector(author)

	txn = author.WriteTxn()
	if err := txt.Delete(txn, 1, 2); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	deleteOnly, err := ygo.EncodeDiff(author, svAfterInsert)
	if err != nil {
		t.Fatal(err)
	}
	want := rootText(t, author, "t")
	if want != "ad" {
		t.Fatalf("setup: author reads %q", want)
	}

	// Merge the delete alone: its target is absent, so it can only be queued.
	merged, err := ygo.MergeUpdates([][]byte{deleteOnly})
	if err != nil {
		t.Fatal(err)
	}
	d := ygo.NewDoc()
	if err := ygo.ApplyUpdate(d, merged); err != nil {
		t.Fatal(err)
	}
	if err := ygo.ApplyUpdate(d, insert); err != nil {
		t.Fatal(err)
	}
	if got := rootText(t, d, "t"); got != want {
		t.Fatalf("read %q after the insert arrived, want %q; the merge dropped the queued delete and the text came back", got, want)
	}
}
