package encoding

import (
	"sort"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/lib0"
	"github.com/Deln0r/ygo/internal/store"
)

// This file exists for one job: encoding a document together with whatever is
// still queued in its pending buffer, so that merging updates cannot quietly
// lose the half that has not integrated yet.
//
// EncodeStateAsUpdate deliberately does NOT do this. It answers "what is in
// this document", and a queued block is not in it. Merging answers a different
// question - "what did these updates carry" - and dropping the queued half
// loses data that was handed to us. The reference implementation draws the
// same line: yjs `mergeUpdates` preserves unintegrated structs and writes Skip
// runs for the gaps between them, so merging a lone delta whose ancestor is
// absent returns that delta rather than an empty update.
//
// Wire shape, checked against yjs 13.6.32 on the same scenario: one client run
// whose declared start clock is the first block emitted, then blocks in
// clock-ascending order, with `Skip` (ref 10) covering any clocks in between
// that nobody has. yjs emits `item, Skip(5), item`; so does this.

// mergeRecord is one emittable block, normalised so the V1 and V2 emitters can
// walk the same plan.
type mergeRecord struct {
	clock  uint64
	length uint64
	kind   BlockKind
	item   *block.Item // WireBlockItem only
}

// buildMergePlan orders every block the document can speak for - integrated
// and queued alike - by client and clock. Returns nil when nothing is queued,
// which tells the caller to use the ordinary encoder and keep the common path
// byte-identical to what the fixtures pin.
func buildMergePlan(bs *store.BlockStore, pending *Pending) ([]uint64, map[uint64][]mergeRecord) {
	if pending.IsEmpty() {
		return nil, nil
	}
	byClient := map[uint64][]mergeRecord{}

	for client := range bs.GetStateVector() {
		list := bs.GetClient(client)
		for i := 0; i < list.Len(); i++ {
			cell, ok := list.Get(i)
			if !ok {
				continue
			}
			rec := mergeRecord{clock: cell.ClockStart(), length: cell.ClockEnd() - cell.ClockStart() + 1}
			switch cell.Kind {
			case store.CellKindItem:
				rec.kind, rec.item = WireBlockItem, cell.Item
			case store.CellKindGC:
				rec.kind = WireBlockGC
			default:
				continue
			}
			byClient[client] = append(byClient[client], rec)
		}
	}

	for client, blocks := range pending.Blocks {
		known := bs.GetClock(client)
		for _, b := range blocks {
			rec := mergeRecord{kind: b.Kind, item: b.Item}
			switch b.Kind {
			case WireBlockItem:
				if b.Item == nil {
					continue
				}
				rec.clock, rec.length = b.Item.ID.Clock, b.Item.Len
			case WireBlockGC, WireBlockSkip:
				rec.clock, rec.length = b.ID.Clock, b.Len
			default:
				continue
			}
			// Below the store clock the block is already represented by the
			// integrated records above, or superseded by them. Emitting it as
			// well would overlap the run and corrupt the receiver's list.
			if rec.length == 0 || rec.clock < known {
				continue
			}
			byClient[client] = append(byClient[client], rec)
		}
	}

	clients := make([]uint64, 0, len(byClient))
	for c := range byClient {
		clients = append(clients, c)
	}
	// Descending clientID, matching EncodeDiff and yrs gotcha 1.
	sort.Slice(clients, func(i, j int) bool { return clients[i] > clients[j] })
	for _, c := range clients {
		recs := byClient[c]
		sort.Slice(recs, func(i, j int) bool { return recs[i].clock < recs[j].clock })
		byClient[c] = reconcile(recs)
	}
	return clients, byClient
}

// reconcile removes overlap from one client's clock-sorted records.
//
// This is not tidiness, it is the difference between a correct update and a
// silently corrupt one. Neither wire format carries a per-block clock: a
// receiver derives each block's clock by accumulating lengths from the run's
// declared start. So two records covering overlapping clocks cannot both be
// emitted - the second would be RELABELLED at the end of the first, landing on
// clocks its client never minted, duplicating content and shifting every later
// block in the run. Both ygo and yjs accept the result without complaint,
// which is what makes it dangerous.
//
// Overlap is ordinary in a merge set rather than exotic: one peer publishes a
// client's run whole while another, having spliced into the middle of it,
// publishes the same run split in two. Above a causal hole neither integrates,
// so both reach the plan. Measured before this existed: merging those two
// diffs produced "cccccXccc" where every other path gives "ccXccc".
//
// Fully covered records are dropped; a record straddling what is already
// covered is sliced, on a COPY, because Content.Split mutates its receiver and
// the records point into the document's live pending buffer.
func reconcile(recs []mergeRecord) []mergeRecord {
	if len(recs) < 2 {
		return recs
	}
	out := make([]mergeRecord, 0, len(recs))
	covered := recs[0].clock
	for _, r := range recs {
		if r.clock+r.length <= covered {
			continue // wholly inside what is already emitted
		}
		if r.clock < covered {
			off := covered - r.clock
			if r.kind == WireBlockItem {
				cp := *r.item
				if err := sliceWireItemRight(&cp, off); err != nil {
					// Non-splittable content at a partial boundary. Dropping
					// the record would lose data, so keep the wider one that
					// already covers this span and skip only the overlap.
					continue
				}
				r.item = &cp
			}
			r.clock += off
			r.length -= off
		}
		out = append(out, r)
		covered = r.clock + r.length
	}
	return out
}

// countWithSkips returns how many blocks a client's run will emit once the
// holes are filled with Skip records. The count is written before the blocks,
// so it has to be known up front.
func countWithSkips(recs []mergeRecord) uint64 {
	n := uint64(len(recs))
	next := recs[0].clock
	for _, r := range recs {
		if r.clock > next {
			n++
		}
		next = r.clock + r.length
	}
	return n
}

// EncodeStateAsUpdateWithPending is EncodeStateAsUpdate plus the pending
// buffer, in V1 wire format. See the file comment for why the two differ.
func EncodeStateAsUpdateWithPending(d *doc.Doc) []byte {
	txn := d.ReadTxn()
	defer txn.Close()
	bs := txn.Store()

	clients, byClient := buildMergePlan(bs, GetPending(txn))
	if clients == nil {
		return EncodeDiff(d, txn, nil)
	}

	buf := lib0.WriteVarUint(nil, uint64(len(clients)))
	for _, client := range clients {
		recs := byClient[client]
		buf = lib0.WriteVarUint(buf, countWithSkips(recs))
		buf = lib0.WriteVarUint(buf, client)
		buf = lib0.WriteVarUint(buf, recs[0].clock)

		next := recs[0].clock
		for _, r := range recs {
			if r.clock > next {
				buf = lib0.WriteVarUint(buf, 10) // BLOCK_SKIP_REF_NUMBER
				buf = lib0.WriteVarUint(buf, r.clock-next)
			}
			switch r.kind {
			case WireBlockItem:
				buf = encodeItem(buf, r.item)
			default: // GC and Skip share the shape: ref number, then length.
				ref := uint64(0)
				if r.kind == WireBlockSkip {
					ref = 10
				}
				buf = lib0.WriteVarUint(buf, ref)
				buf = lib0.WriteVarUint(buf, r.length)
			}
			next = r.clock + r.length
		}
	}
	return mergedDeleteSet(bs, GetPending(txn)).Encode(buf)
}

// EncodeStateAsUpdateV2WithPending is the V2 twin. The formats differ in how a
// Skip carries its length - V2 puts it in the rest stream rather than the len
// column, an anomaly inherited from yjs and already honoured by the decoder -
// so the two emitters cannot share a writer, only the plan.
func EncodeStateAsUpdateV2WithPending(d *doc.Doc) []byte {
	txn := d.ReadTxn()
	defer txn.Close()
	bs := txn.Store()

	clients, byClient := buildMergePlan(bs, GetPending(txn))
	if clients == nil {
		return EncodeDiffV2(d, txn, nil)
	}

	enc := NewEncoderV2()
	enc.WriteVarUint(uint64(len(clients)))
	for _, client := range clients {
		recs := byClient[client]
		enc.WriteVarUint(countWithSkips(recs))
		enc.WriteClient(client)
		enc.WriteVarUint(recs[0].clock)

		next := recs[0].clock
		for _, r := range recs {
			if r.clock > next {
				enc.WriteInfo(10)
				enc.WriteVarUint(r.clock - next)
			}
			switch r.kind {
			case WireBlockItem:
				encodeItemV2(enc, r.item)
			case WireBlockSkip:
				enc.WriteInfo(10)
				enc.WriteVarUint(r.length)
			default:
				enc.WriteInfo(0)
				enc.WriteLen(r.length)
			}
			next = r.clock + r.length
		}
	}
	writeDeleteSetV2(enc, mergedDeleteSet(bs, GetPending(txn)))
	return enc.Bytes()
}

// mergedDeleteSet combines the store's delete set with the ranges still queued
// against IDs nobody has yet. A queued delete is as much a part of what the
// updates carried as a queued block.
func mergedDeleteSet(bs *store.BlockStore, pending *Pending) *IdSet {
	ds := buildDeleteSetFromStore(bs, bs.GetStateVector())
	if pending != nil && pending.DeleteSet != nil {
		pending.DeleteSet.Iterate(func(client uint64, ranges []Range) {
			for _, r := range ranges {
				ds.Insert(client, r.Start, r.Length)
			}
		})
	}
	return ds
}
