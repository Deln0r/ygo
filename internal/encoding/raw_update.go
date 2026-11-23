package encoding

import (
	"errors"
	"fmt"
	"slices"

	"github.com/Deln0r/ygo/internal/lib0"
	"github.com/Deln0r/ygo/internal/store"
)

// This file is yjs's merge and diff of update bytes (utils/updates.js
// mergeUpdatesV2, diffUpdateV2): they read an update's structs one after
// another as written and its delete set as written, and write what they
// keep without integrating anything.

// rawUpdate is an update as yjs's LazyStructReader and readDeleteSet see
// it: every struct in wire order across the client sections (a client may
// have two), Skips included, and the delete set as read.
type rawUpdate struct {
	stream []Block
	ds     *rawDeleteSet
}

// rawDeleteSet is yjs readDeleteSet's result: the clients in the order they
// were first read, each client's ranges in the order read (a client's
// sections appended), zero-length ranges kept, a client read with no
// ranges left out.
type rawDeleteSet struct {
	clients []uint64
	ranges  map[uint64][]Range
}

func newRawDeleteSet() *rawDeleteSet { return &rawDeleteSet{ranges: map[uint64][]Range{}} }

func (d *rawDeleteSet) add(client uint64, rs []Range) {
	if len(rs) == 0 {
		return
	}
	if _, ok := d.ranges[client]; !ok {
		d.clients = append(d.clients, client)
	}
	d.ranges[client] = append(d.ranges[client], rs...)
}

// decodeRawDeleteSet reads a V1 delete set as written, with DecodeIdSet's
// bounds.
func decodeRawDeleteSet(buf []byte) (*rawDeleteSet, []byte, error) {
	clientCount, n, err := lib0.ReadVarUint(buf)
	if err != nil {
		return nil, buf, err
	}
	buf = buf[n:]
	if err := checkDecodeCount(clientCount, len(buf)); err != nil {
		return nil, buf, err
	}
	ds := newRawDeleteSet()
	for i := uint64(0); i < clientCount; i++ {
		client, n, err := lib0.ReadVarUint(buf)
		if err != nil {
			return nil, buf, err
		}
		buf = buf[n:]
		rangeCount, n, err := lib0.ReadVarUint(buf)
		if err != nil {
			return nil, buf, err
		}
		buf = buf[n:]
		if err := checkDecodeCount(rangeCount, len(buf)); err != nil {
			return nil, buf, err
		}
		ranges := make([]Range, rangeCount)
		for j := range ranges {
			start, n, err := lib0.ReadVarUint(buf)
			if err != nil {
				return nil, buf, err
			}
			buf = buf[n:]
			length, n, err := lib0.ReadVarUint(buf)
			if err != nil {
				return nil, buf, err
			}
			buf = buf[n:]
			if err := checkClockRange(start, length); err != nil {
				return nil, buf, err
			}
			ranges[j] = Range{Start: start, Length: length}
		}
		ds.add(client, ranges)
	}
	return ds, buf, nil
}

// readRawDeleteSetV2 reads a V2 delete set as written, with
// readDeleteSetV2's bounds. A V2 range is never empty (readDsLen adds one).
func readRawDeleteSetV2(dec *DecoderV2) (*rawDeleteSet, error) {
	clientCount, err := dec.ReadVarUint()
	if err != nil {
		return nil, fmt.Errorf("readDeleteSetV2 client count: %w", err)
	}
	if err := checkDecodeCount(clientCount, len(dec.ReadRestBytes())); err != nil {
		return nil, fmt.Errorf("readDeleteSetV2 client count: %w", err)
	}
	ds := newRawDeleteSet()
	for i := uint64(0); i < clientCount; i++ {
		c, err := dec.ReadVarUint()
		if err != nil {
			return nil, fmt.Errorf("readDeleteSetV2 client: %w", err)
		}
		rangeCount, err := dec.ReadVarUint()
		if err != nil {
			return nil, fmt.Errorf("readDeleteSetV2 range count: %w", err)
		}
		if err := checkDecodeCount(rangeCount, len(dec.ReadRestBytes())); err != nil {
			return nil, fmt.Errorf("readDeleteSetV2 range count: %w", err)
		}
		var dsCurr uint64
		ranges := make([]Range, 0, rangeCount)
		for j := uint64(0); j < rangeCount; j++ {
			clockDiff, err := dec.ReadVarUint()
			if err != nil {
				return nil, fmt.Errorf("readDeleteSetV2 clock diff: %w", err)
			}
			if err := checkClockRange(dsCurr, clockDiff); err != nil {
				return nil, fmt.Errorf("readDeleteSetV2 clock diff: %w", err)
			}
			dsCurr += clockDiff
			start := dsCurr
			lenMinus1, err := dec.ReadVarUint()
			if err != nil {
				return nil, fmt.Errorf("readDeleteSetV2 len: %w", err)
			}
			if lenMinus1 > MaxClock {
				return nil, fmt.Errorf("readDeleteSetV2 len: %w: length %d+1 exceeds the %d-clock space", lib0.ErrTruncated, lenMinus1, MaxClock)
			}
			l := lenMinus1 + 1
			if err := checkClockRange(start, l); err != nil {
				return nil, fmt.Errorf("readDeleteSetV2 range: %w", err)
			}
			ranges = append(ranges, Range{Start: start, Length: l})
			dsCurr += l
		}
		ds.add(c, ranges)
	}
	return ds, nil
}

// mergeRawDeleteSets is yjs mergeDeleteSets: a client's ranges from every
// set, in set order, then sorted by clock (stably) and joined where they
// touch or overlap (sortAndMergeDeleteSet), a zero-length range that
// touches nothing kept.
//
// yjs collects a client's ranges by scanning the sets after the first that
// has it; this collects them in one pass, in the same order.
func mergeRawDeleteSets(dss []*rawDeleteSet) *rawDeleteSet {
	out := newRawDeleteSet()
	for _, ds := range dss {
		for _, c := range ds.clients {
			if _, ok := out.ranges[c]; !ok {
				out.clients = append(out.clients, c)
			}
			out.ranges[c] = append(out.ranges[c], ds.ranges[c]...)
		}
	}
	for _, c := range out.clients {
		all := out.ranges[c]
		slices.SortStableFunc(all, func(a, b Range) int {
			switch {
			case a.Start < b.Start:
				return -1
			case a.Start > b.Start:
				return 1
			}
			return 0
		})
		j := 1
		for i := 1; i < len(all); i++ {
			left, right := &all[j-1], all[i]
			if left.Start+left.Length >= right.Start {
				left.Length = max(left.Length, right.Start+right.Length-left.Start)
			} else {
				all[j] = right
				j++
			}
		}
		out.ranges[c] = all[:j]
	}
	return out
}

// descending is the client order yjs writeDeleteSet writes in.
func (d *rawDeleteSet) descending() []uint64 {
	cs := slices.Clone(d.clients)
	slices.SortFunc(cs, func(a, b uint64) int {
		switch {
		case a > b:
			return -1
		case a < b:
			return 1
		}
		return 0
	})
	return cs
}

// encodeRawDeleteSet is yjs writeDeleteSet with the V1 encoder.
func encodeRawDeleteSet(buf []byte, d *rawDeleteSet) []byte {
	cs := d.descending()
	buf = lib0.WriteVarUint(buf, uint64(len(cs)))
	for _, c := range cs {
		rs := d.ranges[c]
		buf = lib0.WriteVarUint(buf, c)
		buf = lib0.WriteVarUint(buf, uint64(len(rs)))
		for _, r := range rs {
			buf = lib0.WriteVarUint(buf, r.Start)
			buf = lib0.WriteVarUint(buf, r.Length)
		}
	}
	return buf
}

// errEmptyRangeV2 is yjs UpdateEncoderV2.writeDsLen's refusal of a
// zero-length range.
var errEmptyRangeV2 = errors.New("encoding: a V2 delete set cannot hold an empty range")

// writeRawDeleteSetV2 is yjs writeDeleteSet with the V2 encoder: each clock
// as the difference from where the range before ended. A range that starts
// below that (only in a set read as written, not sorted) is written as lib0
// writes a negative varuint: its low seven bits.
func writeRawDeleteSetV2(enc *EncoderV2, d *rawDeleteSet) error {
	cs := d.descending()
	enc.WriteVarUint(uint64(len(cs)))
	for _, c := range cs {
		rs := d.ranges[c]
		enc.WriteVarUint(c)
		enc.WriteVarUint(uint64(len(rs)))
		var dsCurr uint64
		for _, r := range rs {
			if r.Length == 0 {
				return errEmptyRangeV2
			}
			if r.Start >= dsCurr {
				enc.WriteVarUint(r.Start - dsCurr)
			} else {
				enc.WriteVarUint((r.Start - dsCurr) & 0x7f)
			}
			dsCurr = r.Start
			enc.WriteVarUint(r.Length - 1)
			dsCurr += r.Length
		}
	}
	return nil
}

// sectionsOf splits structs into client sections where the client changes,
// as yjs's LazyStructWriter starts one.
func sectionsOf(structs []Block) []mergeSection {
	var plan []mergeSection
	for i := 0; i < len(structs); {
		c := blockClient(&structs[i])
		j := i + 1
		for j < len(structs) && blockClient(&structs[j]) == c {
			j++
		}
		plan = append(plan, mergeSection{client: c, blocks: structs[i:j]})
		i = j
	}
	return plan
}

// writeSections writes plan's sections in V1, a section's start clock being
// its first struct's.
func writeSections(buf []byte, plan []mergeSection) []byte {
	buf = lib0.WriteVarUint(buf, uint64(len(plan)))
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
	return buf
}

// writeSectionsV2 is the V2 twin of writeSections.
func writeSectionsV2(enc *EncoderV2, plan []mergeSection) {
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
}

// decodeRaw reads an update as yjs's lazy reader does. A V2 update gets
// its own budget (max(its length, floor) structs).
func decodeRaw(buf []byte, v2 bool, floor int) (*rawUpdate, error) {
	raw := &rawUpdate{}
	if v2 {
		if _, err := decodeUpdateV2Into(buf, len(buf), max(len(buf), floor), raw); err != nil {
			return nil, err
		}
		return raw, nil
	}
	if _, _, err := decodeUpdateInto(buf, strictStrings, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// merged is the stream a merge reader reads: Skips dropped (yjs's reader
// filters them), empty structs kept.
func (r *rawUpdate) merged() []Block {
	out := make([]Block, 0, len(r.stream))
	for _, b := range r.stream {
		if b.Kind == WireBlockSkip || (b.Kind == WireBlockItem && b.Item == nil) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// mergeSortBudget is the sorting a merge of total input bytes may do where a
// GC and an item tie at one clock: 16 comparisons a byte, at least 16 << 20.
// yjs sorts every reader at every step; ygo only around such a tie, so only
// inputs that hold one while many other readers merge come near it.
func mergeSortBudget(total int) int64 {
	return 16 * int64(max(total, 1<<20))
}

// MergeUpdateBytes is yjs mergeUpdates (v2 false) or mergeUpdatesV2 of two
// updates or more: their structs merged without integrating them
// (mergeStreams), their delete sets merged. V2 inputs are decoded one after
// another, each within its own budget, and together may hold at most
// max(their bytes, floor) structs, else ErrUpdateTooDense before the next is
// read; an output holding more structs than max(its length, floor) must
// decode, as a receiver would check it.
func MergeUpdateBytes(updates [][]byte, v2 bool, floor int) ([]byte, error) {
	total := 0
	for _, u := range updates {
		total += len(u)
	}
	held := 0
	streams := make([][]Block, 0, len(updates))
	dss := make([]*rawDeleteSet, 0, len(updates))
	for i, u := range updates {
		raw, err := decodeRaw(u, v2, floor)
		if err != nil {
			return nil, fmt.Errorf("update %d: %w", i, err)
		}
		held += len(raw.stream)
		if v2 && held > max(total, floor) {
			return nil, fmt.Errorf("the inputs hold more than %d structs: %w", max(total, floor), ErrUpdateTooDense)
		}
		streams = append(streams, raw.merged())
		dss = append(dss, raw.ds)
	}
	structs, _, err := mergeStreams(streams, mergeSortBudget(total))
	if err != nil {
		return nil, err
	}
	return writeRawUpdate(structs, mergeRawDeleteSets(dss), v2, floor, "the merged update")
}

func writeRawUpdate(structs []Block, ds *rawDeleteSet, v2 bool, floor int, what string) ([]byte, error) {
	plan := sectionsOf(structs)
	if !v2 {
		return encodeRawDeleteSet(writeSections(nil, plan), ds), nil
	}
	enc := NewEncoderV2()
	writeSectionsV2(enc, plan)
	if err := writeRawDeleteSetV2(enc, ds); err != nil {
		return nil, err
	}
	out := enc.Bytes()
	if len(structs) > max(len(out), floor) {
		if _, err := decodeUpdateV2(out, len(out), max(len(out), floor)); err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
	}
	return out, nil
}

// DiffUpdateBytes is yjs diffUpdate (v2 false) or diffUpdateV2: update's
// structs from sv on, without integrating them, and its delete set as read.
// For each client's run of structs (until the client changes, across the
// sections of one client next to each other): a Skip where the run starts
// is passed, the structs ending at or before sv[client] are dropped, the
// first past it is written from sv[client] (an item cut there is written as
// yjs's Item.write with an offset writes it: its key bit kept), and every
// struct after it of that client, Skips included.
func DiffUpdateBytes(update []byte, sv store.StateVector, v2 bool, floor int) ([]byte, error) {
	raw, err := decodeRaw(update, v2, floor)
	if err != nil {
		return nil, err
	}
	st := raw.stream
	out := make([]Block, 0, len(st))
	for i := 0; i < len(st); {
		curr := st[i]
		client := blockClient(&curr)
		svClock := sv[client]
		if curr.Kind == WireBlockSkip {
			i++
			continue
		}
		start := blockStartClock(curr)
		if start+blockLen(curr) > svClock {
			if svClock > start {
				off := svClock - start
				if curr.Kind == WireBlockItem {
					cp := *curr.Item
					if err := sliceWireItemRight(&cp, off); err != nil {
						return nil, fmt.Errorf("diff of %v from offset %d: %w", curr.Item.ID, off, err)
					}
					curr = Block{Kind: WireBlockItem, Item: &cp}
				} else {
					curr.ID.Clock += off
					curr.Len -= off
				}
			}
			out = append(out, curr)
			i++
			for i < len(st) && blockClient(&st[i]) == client {
				out = append(out, st[i])
				i++
			}
			continue
		}
		for i < len(st) && blockClient(&st[i]) == client && blockStartClock(st[i])+blockLen(st[i]) <= svClock {
			i++
		}
	}
	for _, b := range out {
		if b.Kind == WireBlockItem && b.Item == nil {
			return nil, errors.New("encoding: diff of an update holding content ygo cannot write")
		}
	}
	return writeRawUpdate(out, raw.ds, v2, floor, "the diffed update")
}
