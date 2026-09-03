package ygo_test

import (
	"testing"

	"github.com/Deln0r/ygo"
)

// varuintBytes writes n the way the V1 wire format does, so these tests can
// hand-build updates no encoder would produce.
func varuintBytes(buf []byte, n uint64) []byte {
	for n > 0x7f {
		buf = append(buf, byte(n&0x7f)|0x80)
		n >>= 7
	}
	return append(buf, byte(n))
}

// gcBlockUpdate builds a V1 update carrying a single GC block of the declared
// length: clientCount, [blockCount, clientID, startClock, block], deleteSet.
func gcBlockUpdate(client, clock, length uint64) []byte {
	b := varuintBytes(nil, 1)
	b = varuintBytes(b, 1)
	b = varuintBytes(b, client)
	b = varuintBytes(b, clock)
	b = append(b, 0x00) // BLOCK_GC_REF_NUMBER
	b = varuintBytes(b, length)
	return varuintBytes(b, 0) // empty delete set
}

// deleteSetUpdate builds a V1 update with no blocks and one delete range.
func deleteSetUpdate(client, start, length uint64) []byte {
	b := varuintBytes(nil, 0)
	b = varuintBytes(b, 1)
	b = varuintBytes(b, client)
	b = varuintBytes(b, 1)
	b = varuintBytes(b, start)
	return varuintBytes(b, length)
}

// TestDecodeUpdate_RejectsUnrepresentableClockRange pins the decoder to the
// clock space the reference implementation can represent, in BOTH directions.
//
// yjs keeps clocks in JavaScript numbers. Handed a run of 2^64-1 it throws
// "Integer out of Range"; handed 2^40 it accepts (measured against yjs
// 13.6.32). Accepting what the reference refuses buys nothing, and refusing
// what it accepts would break interoperability, so the bound sits exactly at
// Number.MAX_SAFE_INTEGER and the test asserts both sides of it.
func TestDecodeUpdate_RejectsUnrepresentableClockRange(t *testing.T) {
	const maxSafe = uint64(1)<<53 - 1

	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"GC block", gcBlockUpdate(99, 0, ^uint64(0))},
		{"GC block at the boundary", gcBlockUpdate(99, 1, maxSafe)},
		{"delete range", deleteSetUpdate(99, 0, ^uint64(0))},
		{"delete range at the boundary", deleteSetUpdate(99, 1, maxSafe)},
	} {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			if err := ygo.ValidateUpdate(tc.raw); err == nil {
				t.Fatal("accepted a clock range the wire format cannot represent")
			}
			if err := ygo.ApplyUpdate(ygo.NewDoc(), tc.raw); err == nil {
				t.Fatal("applied a clock range the wire format cannot represent")
			}
		})
	}

	// The other direction matters just as much: a large but representable run
	// is legal, and yjs takes it. Rejecting it here would be a divergence.
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"GC block", gcBlockUpdate(99, 0, 1<<40)},
		{"delete range", deleteSetUpdate(99, 0, 1<<40)},
	} {
		t.Run("still accepts a representable "+tc.name, func(t *testing.T) {
			if err := ygo.ValidateUpdate(tc.raw); err != nil {
				t.Fatalf("rejected a run yjs accepts: %v", err)
			}
			if err := ygo.ApplyUpdate(ygo.NewDoc(), tc.raw); err != nil {
				t.Fatalf("failed to apply a run yjs accepts: %v", err)
			}
		})
	}
}

// TestUpdateFormat_DeletesAreNotAuthenticated documents a property of the Yjs
// update format that no amount of input validation changes, so that nobody
// reads the validation elsewhere in this repo as a security boundary.
//
// Anyone who can hand a document an update can destroy another client's
// content and stop that client from ever being heard again - with a dozen
// legal bytes, or equally by making an ordinary edit. yjs 13.6.32 behaves
// identically; this was measured in both directions before the test was
// written, with the victim's client ID pinned on both sides (getting that
// wrong is what makes such a comparison silently meaningless).
//
// The test exists to keep the documentation honest. If a future change makes
// ygo refuse these, it diverges from the reference implementation, and this
// test says so rather than letting the divergence ship.
func TestUpdateFormat_DeletesAreNotAuthenticated(t *testing.T) {
	const victim = 99
	author := ygo.NewDocWithOptions(ygo.Options{ClientID: victim})
	txt := ygo.NewText(author, "t")
	txn := author.WriteTxn()
	if err := txt.Insert(txn, 0, "hello"); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	legit := ygo.EncodeStateAsUpdate(author)

	t.Run("a delete range erases content the sender never wrote", func(t *testing.T) {
		peer := ygo.NewDoc()
		if err := ygo.ApplyUpdate(peer, legit); err != nil {
			t.Fatal(err)
		}
		if got := textOf(t, peer, "t"); got != "hello" {
			t.Fatalf("setup: peer holds %q", got)
		}
		if err := ygo.ApplyUpdate(peer, deleteSetUpdate(victim, 0, 1<<20)); err != nil {
			t.Fatal(err)
		}
		if got := textOf(t, peer, "t"); got != "" {
			t.Fatalf("content survived an unauthenticated delete range (%q); if this now passes, ygo has diverged from yjs and the README's trust-model note is wrong", got)
		}
	})

	t.Run("a GC run silences a client's future writes", func(t *testing.T) {
		peer := ygo.NewDoc()
		if err := ygo.ApplyUpdate(peer, gcBlockUpdate(victim, 0, 1<<20)); err != nil {
			t.Fatal(err)
		}
		if err := ygo.ApplyUpdate(peer, legit); err != nil {
			t.Fatal(err)
		}
		if got := textOf(t, peer, "t"); got != "" {
			t.Fatalf("the victim was still heard (%q); ygo has diverged from yjs and the trust-model note needs revisiting", got)
		}
	})

	// Control: without the hostile update, the same legitimate bytes land.
	clean := ygo.NewDoc()
	if err := ygo.ApplyUpdate(clean, legit); err != nil {
		t.Fatal(err)
	}
	if got := textOf(t, clean, "t"); got != "hello" {
		t.Fatalf("control: a legitimate update alone reads %q, so the assertions above prove nothing", got)
	}
}
