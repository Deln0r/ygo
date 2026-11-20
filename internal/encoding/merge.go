package encoding

import (
	"slices"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/lib0"
	"github.com/Deln0r/ygo/internal/store"
)

// This file exists for one job: encoding a document together with whatever is
// still queued in its pending buffer, so that a replica relays what it holds
// but cannot integrate yet, and merging updates cannot quietly lose the half
// that has not integrated.
//
// yjs encodeStateAsUpdateV2 (utils/encoding.js) does this for every state it
// writes: the integrated structs from the target state vector, then the
// pending delete set whole and the pending structs diffed against the vector,
// merged. ygo's public state encodings (ygo.EncodeStateAsUpdate*,
// ygo.EncodeDiff*) and the sync handler's SyncStep2 go through
// EncodeDiffWithPending. The store-only EncodeDiff stays for what yjs sends
// from its update event, which never carries the pending buffer: a client's
// routine sends and its local log. yjs `mergeUpdates` likewise preserves
// unintegrated structs and writes Skip runs for the gaps between them, so
// merging a lone delta whose ancestor is absent returns that delta rather than
// an empty update.
//
// Wire shape, checked against yjs 13.6.32 on the same scenario: one client run
// whose declared start clock is the first block emitted, then blocks in
// clock-ascending order, with `Skip` (ref 10) covering any clocks in between
// that nobody has. yjs emits `item, Skip(5), item`; so does this.

// mergeSection is one client run of a merged update: the client, then
// its structs in clock order, Skips included, as yjs's LazyStructWriter
// writes a run.
type mergeSection struct {
	client uint64
	blocks []Block
}

// buildMergePlan is what yjs encodeStateAsUpdateV2 merges when the
// document holds updates that wait: the store part written from sv, read
// back as an update (storeStream), merged with the queue diffed against sv
// (diffUpdateV2, queueStream), by mergeStructs. ok is false when nothing is
// queued (structs or deletes), which tells the caller to use the ordinary
// encoder and keep the common path byte-identical to what the fixtures pin.
func buildMergePlan(bs *store.BlockStore, pending *Pending, sv store.StateVector) (plan []mergeSection, ok bool) {
	if pending.IsEmpty() {
		return nil, false
	}
	merged := mergeStructs(storeStream(bs, sv), queueStream(pending.Blocks, sv))
	plan = []mergeSection{}
	for i := 0; i < len(merged); {
		c := blockClient(&merged[i])
		j := i + 1
		for j < len(merged) && blockClient(&merged[j]) == c {
			j++
		}
		plan = append(plan, mergeSection{client: c, blocks: merged[i:j]})
		i = j
	}
	return plan, true
}

// storeStream lists what EncodeDiff writes from sv, client by client
// (descending), the first struct cut at sv, as yjs's merge reads it back:
// an item with an origin, the cut one included, carries no key (marked
// read; the store's items are not touched). So once anything waits, a map
// value that overwrote another is written without the key bit, as yjs
// writes it, though a plain diff keeps it.
func storeStream(bs *store.BlockStore, sv store.StateVector) []Block {
	clients := make([]uint64, 0)
	for client, clock := range bs.GetStateVector() {
		if clock > sv[client] {
			clients = append(clients, client)
		}
	}
	slices.Sort(clients)
	n := 0
	for _, client := range clients {
		n += bs.GetClient(client).Len()
	}
	out := make([]Block, 0, n)
	for i := len(clients) - 1; i >= 0; i-- {
		client := clients[i]
		list := bs.GetClient(client)
		from := sv[client]
		for j := 0; j < list.Len(); j++ {
			cell, ok := list.Get(j)
			if !ok {
				continue
			}
			start, end := cell.ClockStart(), cell.ClockEnd()+1
			if end <= from {
				continue // the remote has it
			}
			off := uint64(0)
			if start < from {
				off = from - start
			}
			switch cell.Kind {
			case store.CellKindItem:
				it := cell.Item
				if off > 0 {
					it = itemFrom(it, off)
				}
				out = append(out, Block{Kind: WireBlockItem, Item: it, read: true})
			case store.CellKindGC:
				out = append(out, Block{Kind: WireBlockGC, ID: block.ID{Client: client, Clock: start + off}, Len: end - start - off})
			}
		}
	}
	return out
}

// queueStream is yjs diffUpdateV2 of the queue against sv, as the merge
// reads it: per client (descending), the blocks that end at or before sv
// left out, the first one past it written from sv (an item then has an
// origin and comes back without parent and key), the rest as they are.
// The queue's items are not touched.
func queueStream(queue map[uint64][]Block, sv store.StateVector) []Block {
	all := clientStream(queue)
	out := make([]Block, 0, len(all))
	for i := 0; i < len(all); {
		client := blockClient(&all[i])
		svClock := sv[client]
		for i < len(all) && blockClient(&all[i]) == client && blockStartClock(all[i])+blockLen(all[i]) <= svClock {
			i++
		}
		first := true
		for ; i < len(all) && blockClient(&all[i]) == client; i++ {
			b := all[i]
			if start := blockStartClock(b); first && start < svClock {
				off := svClock - start
				if b.Kind == WireBlockItem {
					b = Block{Kind: WireBlockItem, Item: asRead(itemFrom(b.Item, off))}
				} else {
					b.ID.Clock += off
					b.Len -= off
				}
			}
			first = false
			out = append(out, b)
		}
	}
	return out
}

// EncodeStateAsUpdateWithPending is EncodeStateAsUpdate plus the pending
// buffer, in V1 wire format. See the file comment for why the two differ.
func EncodeStateAsUpdateWithPending(d *doc.Doc) []byte {
	txn := d.ReadTxn()
	defer txn.Close()
	return EncodeDiffWithPending(d, txn, nil)
}

// EncodeDiffWithPending is EncodeDiff plus the pending buffer from sv on, in
// the caller's transaction: the bytes of yjs encodeStateAsUpdate(doc, sv),
// the store part and the queue merged as yjs merges them (buildMergePlan).
// Where the store itself is laid out differently from yjs's (docs/tech-debt.md),
// so is this.
func EncodeDiffWithPending(d *doc.Doc, txn *doc.Transaction, sv store.StateVector) []byte {
	bs := txn.Store()
	plan, ok := buildMergePlan(bs, GetPending(txn), sv)
	if !ok { // nothing pending
		return EncodeDiff(d, txn, sv)
	}

	buf := lib0.WriteVarUint(nil, uint64(len(plan)))
	for _, sec := range plan {
		buf = lib0.WriteVarUint(buf, uint64(len(sec.blocks)))
		buf = lib0.WriteVarUint(buf, sec.client)
		buf = lib0.WriteVarUint(buf, blockStartClock(sec.blocks[0]))
		for _, b := range sec.blocks {
			switch b.Kind {
			case WireBlockItem:
				if b.read {
					buf = encodeItemInfo(buf, b.Item, readInfo(b.Item))
				} else {
					buf = encodeItem(buf, b.Item)
				}
			case WireBlockSkip:
				buf = lib0.WriteVarUint(buf, 10) // BLOCK_SKIP_REF_NUMBER
				buf = lib0.WriteVarUint(buf, b.Len)
			default:
				buf = lib0.WriteVarUint(buf, 0) // BLOCK_GC_REF_NUMBER
				buf = lib0.WriteVarUint(buf, b.Len)
			}
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
	return EncodeDiffV2WithPending(d, txn, nil)
}

// EncodeDiffV2WithPending is the V2 twin of EncodeDiffWithPending.
func EncodeDiffV2WithPending(d *doc.Doc, txn *doc.Transaction, sv store.StateVector) []byte {
	bs := txn.Store()
	plan, ok := buildMergePlan(bs, GetPending(txn), sv)
	if !ok { // nothing pending
		return EncodeDiffV2(d, txn, sv)
	}

	enc := NewEncoderV2()
	enc.WriteVarUint(uint64(len(plan)))
	for _, sec := range plan {
		enc.WriteVarUint(uint64(len(sec.blocks)))
		enc.WriteClient(sec.client)
		enc.WriteVarUint(blockStartClock(sec.blocks[0]))
		for _, b := range sec.blocks {
			switch b.Kind {
			case WireBlockItem:
				if b.read {
					encodeItemInfoV2(enc, b.Item, readInfo(b.Item))
				} else {
					encodeItemV2(enc, b.Item)
				}
			case WireBlockSkip:
				enc.WriteInfo(10)
				enc.WriteVarUint(b.Len)
			default:
				enc.WriteInfo(0)
				enc.WriteLen(b.Len)
			}
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
