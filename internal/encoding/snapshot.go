package encoding

import (
	"sort"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/lib0"
	"github.com/Deln0r/ygo/internal/store"
)

// Snapshot is a point-in-time marker of a document's history: the set
// of deleted ID ranges (DS) plus the per-client clock heads (SV) as of
// the snapshot moment. Replaying the document up to SV, with DS applied
// as the deletions-so-far, reconstructs exactly the state the document
// had when the snapshot was taken.
//
// Mirrors yjs Snapshot (src/utils/Snapshot.js): a {ds, sv} pair. The
// wire form is writeDeleteSet(ds) followed by writeStateVector(sv).
type Snapshot struct {
	DS *IdSet
	SV store.StateVector
}

// CreateSnapshot captures the current delete set and state vector of
// the store. Mirrors yjs's `snapshot(doc)`.
//
// For snapshots to be useful for time-travel the doc should be created
// with GC disabled, otherwise deleted content the snapshot references
// may have been collected. The capture itself does not require it.
func CreateSnapshot(bs *store.BlockStore) Snapshot {
	sv := bs.GetStateVector()
	ds := buildDeleteSetFromStore(bs, sv)
	return Snapshot{DS: ds, SV: sv}
}

// EqualSnapshots reports whether two snapshots carry the same delete
// set and state vector. Mirrors yjs equalSnapshots.
func EqualSnapshots(a, b Snapshot) bool {
	if len(a.SV) != len(b.SV) {
		return false
	}
	for c, clk := range a.SV {
		if b.SV[c] != clk {
			return false
		}
	}
	ac, bc := a.DS.clients, b.DS.clients
	if len(ac) != len(bc) {
		return false
	}
	for c, ar := range ac {
		br, ok := bc[c]
		if !ok || len(ar) != len(br) {
			return false
		}
		for i := range ar {
			if ar[i] != br[i] {
				return false
			}
		}
	}
	return true
}

// EncodeSnapshot returns the V1 wire encoding of s: the delete set
// followed by the state vector. Byte-compatible with yjs
// `encodeSnapshot` (DSEncoderV1).
func EncodeSnapshot(s Snapshot) []byte {
	var buf []byte
	buf = s.DS.Encode(buf) // V1 delete set, descending client order
	buf = EncodeStateVector(s.SV, buf)
	return buf
}

// DecodeSnapshot parses a V1 snapshot produced by EncodeSnapshot or
// yjs `encodeSnapshot`.
func DecodeSnapshot(buf []byte) (Snapshot, error) {
	ds, rest, err := DecodeIdSet(buf)
	if err != nil {
		return Snapshot{}, err
	}
	sv, _, err := DecodeStateVector(rest)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{DS: ds, SV: sv}, nil
}

// SplitAtSnapshotBoundaries forces a clean cell boundary at each
// client's snapshot clock, so EncodeSnapshotAsUpdateV1 can emit exactly
// the clocks [0, snapshotClock) without including content created after
// the snapshot. Mirrors yjs's getItemCleanStart pass in
// createDocFromSnapshot. Mutates the store (splitting is semantically
// transparent), so the caller must hold a write transaction.
func SplitAtSnapshotBoundaries(bs *store.BlockStore, snap Snapshot) {
	for c, clock := range snap.SV {
		if clock == 0 {
			continue
		}
		cell, ok := bs.GetBlock(block.ID{Client: c, Clock: clock})
		if ok && cell.Kind == store.CellKindItem && cell.Item.ID.Clock < clock {
			bs.SplitBlock(cell.Item, clock-cell.Item.ID.Clock)
		}
	}
}

// EncodeSnapshotAsUpdateV1 builds a V1 update that contains, for each
// client, only the cells with clock strictly below the snapshot's clock
// for that client, plus the snapshot's delete set. Applying this update
// to a fresh document reconstructs the exact state captured by the
// snapshot.
//
// Call SplitAtSnapshotBoundaries first so no cell straddles a snapshot
// boundary. The wire shape matches EncodeDiff (per-client runs in
// descending client order, then the delete set).
func EncodeSnapshotAsUpdateV1(bs *store.BlockStore, snap Snapshot) []byte {
	type run struct {
		client uint64
		cells  []store.BlockCell
	}
	clients := make([]uint64, 0, len(snap.SV))
	for c, clock := range snap.SV {
		if clock > 0 {
			clients = append(clients, c)
		}
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] > clients[j] })

	var runs []run
	for _, c := range clients {
		clock := snap.SV[c]
		list := bs.GetClient(c)
		if list == nil {
			continue
		}
		var cells []store.BlockCell
		for i := 0; i < list.Len(); i++ {
			cell, _ := list.Get(i)
			if cell.ClockEnd() < clock {
				cells = append(cells, cell)
			}
		}
		if len(cells) > 0 {
			runs = append(runs, run{client: c, cells: cells})
		}
	}

	buf := lib0.WriteVarUint(nil, uint64(len(runs)))
	for _, r := range runs {
		buf = lib0.WriteVarUint(buf, uint64(len(r.cells)))
		buf = lib0.WriteVarUint(buf, r.client)
		buf = lib0.WriteVarUint(buf, r.cells[0].ClockStart())
		for _, cell := range r.cells {
			buf = encodeCell(buf, cell)
		}
	}
	buf = snap.DS.Encode(buf) // V1 delete set, descending client order
	return buf
}

// UpdateAddsNothing reports whether applying a V1 update to d would change
// nothing: every block lies within d's state vector and every deletion is
// already in d's delete set. It is yjs snapshotContainsUpdate against the
// document's current snapshot, which a Hocuspocus server uses to answer a
// read-only client's SyncStep2 with SyncStatus 1 when it carries nothing
// new.
func UpdateAddsNothing(d *doc.Doc, update []byte) (bool, error) {
	u, _, err := DecodeUpdate(update)
	if err != nil {
		return false, err
	}
	rtxn := d.ReadTxn()
	defer rtxn.Close()
	bs := rtxn.Store()
	sv := bs.GetStateVector()
	for client, blocks := range u.Blocks {
		for _, b := range blocks {
			clock, length := b.ID.Clock, b.Len
			if b.Kind == WireBlockItem && b.Item != nil {
				clock, length = b.Item.ID.Clock, b.Item.Len
			}
			if sv[client] < clock+length {
				return false, nil
			}
		}
	}
	have := buildDeleteSetFromStore(bs, sv)
	covered := true
	u.DeleteSet.Iterate(func(client uint64, ranges []Range) {
		for _, r := range ranges {
			if !rangeCovered(have.ClientRanges(client), r) {
				covered = false
			}
		}
	})
	return covered, nil
}

// rangeCovered reports whether r lies within the union of sorted,
// non-overlapping ranges.
func rangeCovered(ranges []Range, r Range) bool {
	at := r.Start
	for _, x := range ranges {
		if x.End() <= at {
			continue
		}
		if x.Start > at {
			return false
		}
		at = x.End()
		if at >= r.End() {
			return true
		}
	}
	return r.Length == 0
}
