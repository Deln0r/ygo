package encoding

import (
	"testing"

	"github.com/Deln0r/ygo/internal/block"
)

// TestPending_addBlock_KeepsTheLongerBlockAtOneClock: two blocks can start at
// the same clock and cover different amounts of it, because peers split a
// client's run at different points.
//
// addBlock used to skip any block whose start clock was already queued, on the
// reasoning that re-applying an update must not double-queue. A LONGER block
// at a known start clock is not a duplicate though, and dropping it loses the
// tail outright - which surfaces once the pending buffer is encoded, as a Skip
// run over clocks the buffer had been handed.
//
// This is a unit test rather than one driven through MergeUpdates on purpose.
// ygo's own EncodeDiff emits whole client runs (documented tech debt), so it
// cannot produce a shorter-then-longer pair whose tail nobody else covers; a
// yjs peer, which slices at the state-vector boundary, can. The public-API
// version of this test was written first and passed with the fix removed - a
// mutation run caught it, and this is the honest replacement.
func TestPending_addBlock_KeepsTheLongerBlockAtOneClock(t *testing.T) {
	mk := func(clock, length uint64) Block {
		return Block{Kind: WireBlockGC, ID: block.ID{Client: 9, Clock: clock}, Len: length}
	}

	for _, tc := range []struct {
		name  string
		order []Block
	}{
		{"shorter first", []Block{mk(10, 2), mk(10, 5)}},
		{"longer first", []Block{mk(10, 5), mk(10, 2)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPending()
			for _, b := range tc.order {
				p.addBlock(9, b)
			}
			list := p.Blocks[9]
			if len(list) != 1 {
				t.Fatalf("queued %d blocks at one clock, want 1", len(list))
			}
			if got := blockLen(list[0]); got != 5 {
				t.Fatalf("kept a block covering %d clocks, want the longer one (5); the tail was discarded", got)
			}
		})
	}

	// A genuine duplicate must still not double-queue.
	p := NewPending()
	p.addBlock(9, mk(10, 5))
	p.addBlock(9, mk(10, 5))
	if len(p.Blocks[9]) != 1 {
		t.Fatalf("re-applying the same block queued it %d times", len(p.Blocks[9]))
	}
}

// TestReconcile_RemovesOverlap covers the reconciliation the wire format makes
// mandatory: clocks are implicit and cumulative on decode, so two records
// covering overlapping clocks cannot both be emitted.
func TestReconcile_RemovesOverlap(t *testing.T) {
	rec := func(clock, length uint64) mergeRecord {
		return mergeRecord{clock: clock, length: length, kind: WireBlockGC}
	}
	for _, tc := range []struct {
		name string
		in   []mergeRecord
		want []mergeRecord
	}{
		{"no overlap is untouched", []mergeRecord{rec(0, 5), rec(5, 5)}, []mergeRecord{rec(0, 5), rec(5, 5)}},
		{"a hole is preserved", []mergeRecord{rec(0, 5), rec(10, 5)}, []mergeRecord{rec(0, 5), rec(10, 5)}},
		{"a fully covered record is dropped", []mergeRecord{rec(10, 5), rec(10, 2)}, []mergeRecord{rec(10, 5)}},
		{"an identical record is dropped", []mergeRecord{rec(10, 5), rec(10, 5)}, []mergeRecord{rec(10, 5)}},
		{"a straddling GC record is trimmed", []mergeRecord{rec(10, 2), rec(11, 5)}, []mergeRecord{rec(10, 2), rec(12, 4)}},
		{"three-way overlap collapses", []mergeRecord{rec(10, 5), rec(10, 2), rec(12, 3)}, []mergeRecord{rec(10, 5)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := reconcile(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d records, want %d: %+v", len(got), len(tc.want), got)
			}
			var next uint64
			for i, g := range got {
				if g.clock != tc.want[i].clock || g.length != tc.want[i].length {
					t.Fatalf("record %d = (clock %d, len %d), want (clock %d, len %d)", i, g.clock, g.length, tc.want[i].clock, tc.want[i].length)
				}
				if i > 0 && g.clock < next {
					t.Fatalf("record %d starts at %d, inside the previous run ending at %d; the emitted run would overlap itself", i, g.clock, next)
				}
				next = g.clock + g.length
			}
		})
	}
}
