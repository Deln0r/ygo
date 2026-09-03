package encoding

import (
	"fmt"
	"sort"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/lib0"
	"github.com/Deln0r/ygo/internal/store"
)

// BlockKind discriminates the three on-the-wire block variants:
// regular Item, GC tombstone range, Skip range.
type BlockKind uint8

const (
	WireBlockItem BlockKind = 0
	WireBlockGC   BlockKind = 1
	WireBlockSkip BlockKind = 2
)

// Block is one block-shaped record inside an Update. For WireBlockItem
// the Item field carries the parsed *block.Item with Origin /
// RightOrigin set but Left / Right nil (Repair fixes those at apply
// time). For WireBlockGC and WireBlockSkip only ID and Len matter.
type Block struct {
	Kind BlockKind
	Item *block.Item // WireBlockItem
	ID   block.ID    // WireBlockGC, WireBlockSkip
	Len  uint64      // WireBlockGC, WireBlockSkip
}

// Update is the parsed-but-not-yet-applied form of a V1 wire update.
// Blocks groups records by client; per-client lists are stored in
// emission order (clock-ascending within a client).
type Update struct {
	Blocks    map[uint64][]Block
	DeleteSet *IdSet
}

// NewUpdate returns an empty Update.
func NewUpdate() *Update {
	return &Update{
		Blocks:    map[uint64][]Block{},
		DeleteSet: NewIdSet(),
	}
}

// EncodeStateAsUpdate returns the V1 wire bytes for the doc's full
// state — equivalent to encoding against an empty remote state vector.
// The caller may pass an existing transaction for read access; if nil
// a fresh ReadTxn is acquired and released.
//
// Mirrors yrs Doc::encode_state_as_update_v1, simplified to omit the
// pending-update merge path (we have no pending buffer yet).
func EncodeStateAsUpdate(d *doc.Doc) []byte {
	txn := d.ReadTxn()
	defer txn.Close()
	return EncodeDiff(d, txn, nil)
}

// EncodeDiff returns the V1 wire bytes for the blocks the local doc
// has that the remote doc (per remoteSV) does not. A nil remoteSV is
// treated as the empty SV — emit everything.
//
// In this first port pass EncodeDiff emits whole blocks (no slicing
// at the SV boundary). yrs's Store::write_blocks_from clamps the
// first block of each client run via find_pivot + slice-trim; we
// emit the entire client list. The wire format is identical when
// the remote SV either knows the client fully (we skip them) or
// not at all (we emit from clock 0). Partial-knowledge clients
// (remote knows clocks [0, k) but not [k, end)) still get all
// blocks for that client emitted, so the receiver may see redundant
// blocks for clocks it already has. Those are silently rejected by
// integrate (state-vector check) so the result is correct, just
// chattier than yrs. Tracked in tech-debt.md.
//
// Block runs are emitted in DESCENDING clientID order per
// docs/yrs-port-notes/update-v1.md gotcha 1.
func EncodeDiff(d *doc.Doc, txn *doc.Transaction, remoteSV store.StateVector) []byte {
	bs := txn.Store()
	localSV := bs.GetStateVector()

	// diff = clients to emit. For each client present in localSV with
	// localClock > remoteClock, include with the remote clock as the
	// run start. Clients absent from remoteSV start at 0.
	type clientRun struct {
		client     uint64
		startClock uint64
	}
	var diff []clientRun
	for c, localClock := range localSV {
		remoteClock := uint64(0)
		if remoteSV != nil {
			remoteClock = remoteSV[c]
		}
		if localClock > remoteClock {
			diff = append(diff, clientRun{client: c, startClock: remoteClock})
		}
	}
	// Descending by clientID.
	sort.Slice(diff, func(i, j int) bool { return diff[i].client > diff[j].client })

	buf := lib0.WriteVarUint(nil, uint64(len(diff)))
	for _, run := range diff {
		clientList := bs.GetClient(run.client)
		startIdx := firstUnknownCell(clientList, run.startClock)
		count := clientList.Len() - startIdx
		buf = lib0.WriteVarUint(buf, uint64(count))
		buf = lib0.WriteVarUint(buf, run.client)
		first, _ := clientList.Get(startIdx)
		buf = lib0.WriteVarUint(buf, first.ClockStart())
		for i := startIdx; i < clientList.Len(); i++ {
			cell, _ := clientList.Get(i)
			buf = encodeCell(buf, cell)
		}
	}

	// Emit delete set: scan all cells, collect deleted ranges.
	ds := buildDeleteSetFromStore(bs, localSV)
	buf = ds.Encode(buf)

	return buf
}

// firstUnknownCell returns the index of the first cell in clientList
// whose clock range contains at least one clock the remote does not
// yet have. remoteClock follows state-vector semantics: it is the
// exclusive upper bound of the remote's known range, i.e. the remote
// has clocks [0, remoteClock). ClockEnd is inclusive.
//
// A cell is fully known to the remote iff cell.ClockEnd() <
// remoteClock. Cells that straddle remoteClock (ClockStart <
// remoteClock <= ClockEnd) are emitted whole; the receiver's
// integrate path silently rejects per-Item duplicates via the
// state-vector check, so the redundancy costs bandwidth but not
// correctness. Per-cell partial trim (split at remoteClock, emit
// only the right half) would match yrs's wire-byte output exactly
// for the straddling case; deferred — tracked in
// docs/tech-debt.md.
//
// Returns 0 if remoteClock is 0 (the remote has nothing for this
// client; full emission). Returns clientList.Len() if every cell
// is already known to the remote (no emission needed; caller
// should have filtered the client out of `diff` upstream, this is
// a defensive zero-block fallback).
func firstUnknownCell(clientList *store.ClientBlockList, remoteClock uint64) int {
	if remoteClock == 0 {
		return 0
	}
	n := clientList.Len()
	for i := 0; i < n; i++ {
		cell, _ := clientList.Get(i)
		if cell.ClockEnd() >= remoteClock {
			return i
		}
	}
	return n
}

// encodeCell writes one cell's wire record (info byte + conditional
// fields + content).
//
// Mirrors yrs ItemSlice::encode (slice.rs:181-233) for the
// adjacent-on-both-sides case. We do not yet support partial slices
// (start > 0 or end < len-1); first-block trimming is deferred per
// EncodeDiff's docstring.
func encodeCell(buf []byte, cell store.BlockCell) []byte {
	switch cell.Kind {
	case store.CellKindGC:
		buf = lib0.WriteVarUint(buf, uint64(0)) // BLOCK_GC_REF_NUMBER
		return lib0.WriteVarUint(buf, cell.GC.Len())
	case store.CellKindItem:
		return encodeItem(buf, cell.Item)
	default:
		panic(fmt.Sprintf("encoding.encodeCell: unknown cell kind %d", cell.Kind))
	}
}

// encodeItem writes a full Item record (no slicing).
func encodeItem(buf []byte, it *block.Item) []byte {
	info := it.Info()
	cantCopyParentInfo := info&(block.InfoHasOrigin|block.InfoHasRightOrigin) == 0
	buf = append(buf, info)
	if it.Origin != nil {
		buf = encodeID(buf, *it.Origin)
	}
	if it.RightOrigin != nil {
		buf = encodeID(buf, *it.RightOrigin)
	}
	if cantCopyParentInfo {
		switch it.Parent.Kind {
		case block.ParentBranch:
			b := it.Parent.Branch
			if b == nil {
				panic("encoding.encodeItem: ParentBranch with nil Branch")
			}
			if b.Item != nil {
				// Nested branch — emit as parent-by-ID.
				buf = append(buf, 0x00) // parent_info: false
				buf = encodeID(buf, b.Item.ID)
			} else if b.Name != "" {
				// Root branch — emit by name.
				buf = append(buf, 0x01) // parent_info: true
				buf = lib0.WriteVarString(buf, b.Name)
			} else {
				panic("encoding.encodeItem: ParentBranch with neither nested item nor root name")
			}
		case block.ParentNamed:
			buf = append(buf, 0x01)
			buf = lib0.WriteVarString(buf, it.Parent.Named)
		case block.ParentID:
			buf = append(buf, 0x00)
			buf = encodeID(buf, it.Parent.ID)
		case block.ParentUnknown:
			panic("encoding.encodeItem: ParentUnknown — cannot emit")
		}
		if it.ParentSub != nil {
			buf = lib0.WriteVarString(buf, *it.ParentSub)
		}
	}
	return EncodeContent(buf, it.Content)
}

func encodeID(buf []byte, id block.ID) []byte {
	buf = lib0.WriteVarUint(buf, id.Client)
	return lib0.WriteVarUint(buf, id.Clock)
}

// buildDeleteSetFromStore produces the wire DeleteSet by scanning
// every cell (Items and GC alike) and recording deleted ID ranges.
//
// Mirrors yrs IdSet::from_store (id_set.rs:432-448), simplified.
func buildDeleteSetFromStore(bs *store.BlockStore, sv store.StateVector) *IdSet {
	ds := NewIdSet()
	// Iterate clients in deterministic ascending order; IdSet.Insert
	// keeps per-client ranges sorted regardless.
	clients := make([]uint64, 0, len(sv))
	for c := range sv {
		clients = append(clients, c)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] < clients[j] })
	for _, c := range clients {
		l := bs.GetClient(c)
		if l == nil {
			continue
		}
		for i := 0; i < l.Len(); i++ {
			cell, _ := l.Get(i)
			if cell.IsDeleted() {
				ds.Insert(c, cell.ClockStart(), cell.Len())
			}
		}
	}
	return ds
}

// DecodeUpdate parses V1 wire bytes into an Update. The returned
// Update is ready for Apply but its blocks have nil Left/Right —
// Repair fills those at apply time.
//
// Mirrors yrs Update::decode (update.rs:497-516).
// checkDecodeCount guards a wire-supplied element count against the
// bytes still available to decode it. Every V1/V2 element consumes at
// least one input byte, so a count larger than the remaining input
// cannot come from a valid encoder; it is the length-prefix
// amplification DoS, where a handful of bytes name a huge count and
// force an unbounded make()/decode loop (a fuzzer turned a 9-byte
// update into a multi-terabyte allocation here). Reject it as the
// truncated input it effectively is.
func checkDecodeCount(count uint64, remaining int) error {
	if remaining < 0 || count > uint64(remaining) {
		return lib0.ErrTruncated
	}
	return nil
}

// MaxClock is the largest clock this decoder accepts from the wire.
//
// It is Number.MAX_SAFE_INTEGER, because the reference implementation keeps
// clocks in JavaScript numbers and cannot represent anything above it: a yjs
// peer handed a larger value throws "Integer out of Range" (measured against
// yjs 13.6.32). Accepting what the reference refuses buys nothing and lets a
// sender push a client's clock space somewhere no honest peer can follow.
//
// This is a fidelity bound, not a safety guarantee. A clock range BELOW it is
// still enough for a room member to silence another client or tombstone their
// content - yjs behaves identically, measured both ways - because the Yjs
// update format does not authenticate deletes. See the package documentation
// on what that means for untrusted transports.
const MaxClock = uint64(1)<<53 - 1

// checkClockRange rejects a wire-supplied run that would place a clock beyond
// what the reference implementation can represent. Overflow-safe: the sum is
// checked without computing it.
func checkClockRange(start, length uint64) error {
	if start > MaxClock || length > MaxClock || start > MaxClock-length {
		return fmt.Errorf("%w: clock range %d+%d exceeds the %d-clock space the wire format can represent", lib0.ErrTruncated, start, length, MaxClock)
	}
	return nil
}

func DecodeUpdate(buf []byte) (*Update, []byte, error) {
	clientCount, n, err := lib0.ReadVarUint(buf)
	if err != nil {
		return nil, buf, err
	}
	buf = buf[n:]
	if err := checkDecodeCount(clientCount, len(buf)); err != nil {
		return nil, buf, err
	}

	u := NewUpdate()
	for i := uint64(0); i < clientCount; i++ {
		blockCount, n, err := lib0.ReadVarUint(buf)
		if err != nil {
			return nil, buf, err
		}
		buf = buf[n:]
		client, n, err := lib0.ReadVarUint(buf)
		if err != nil {
			return nil, buf, err
		}
		buf = buf[n:]
		clock, n, err := lib0.ReadVarUint(buf)
		if err != nil {
			return nil, buf, err
		}
		buf = buf[n:]

		if err := checkDecodeCount(blockCount, len(buf)); err != nil {
			return nil, buf, err
		}
		blocks := make([]Block, 0, blockCount)
		for j := uint64(0); j < blockCount; j++ {
			b, tail, err := decodeBlock(buf, block.ID{Client: client, Clock: clock})
			if err != nil {
				return nil, buf, err
			}
			blocks = append(blocks, b)
			clock += b.length()
			buf = tail
		}
		u.Blocks[client] = blocks
	}

	ds, tail, err := DecodeIdSet(buf)
	if err != nil {
		return nil, buf, err
	}
	u.DeleteSet = ds
	return u, tail, nil
}

func (b Block) length() uint64 {
	if b.Kind == WireBlockItem {
		return b.Item.Len
	}
	return b.Len
}

// startClock is the block's first clock. For an Item the ID lives on the
// parsed Item; GC / Skip records carry it on the Block directly.
func (b Block) startClock() uint64 {
	if b.Kind == WireBlockItem {
		return b.Item.ID.Clock
	}
	return b.ID.Clock
}

// decodeBlock parses one block record at the given starting ID.
//
// Mirrors yrs Update::decode_block (update.rs:364-393).
func decodeBlock(buf []byte, id block.ID) (Block, []byte, error) {
	if len(buf) < 1 {
		return Block{}, buf, lib0.ErrTruncated
	}
	info := buf[0]
	buf = buf[1:]

	switch info {
	case 0: // BLOCK_GC_REF_NUMBER
		l, n, err := lib0.ReadVarUint(buf)
		if err != nil {
			return Block{}, buf, err
		}
		if err := checkClockRange(id.Clock, l); err != nil {
			return Block{}, buf, err
		}
		return Block{Kind: WireBlockGC, ID: id, Len: l}, buf[n:], nil
	case 10: // BLOCK_SKIP_REF_NUMBER
		l, n, err := lib0.ReadVarUint(buf)
		if err != nil {
			return Block{}, buf, err
		}
		if err := checkClockRange(id.Clock, l); err != nil {
			return Block{}, buf, err
		}
		return Block{Kind: WireBlockSkip, ID: id, Len: l}, buf[n:], nil
	}

	cantCopyParentInfo := info&(block.InfoHasOrigin|block.InfoHasRightOrigin) == 0
	var origin, rightOrigin *block.ID
	if info&block.InfoHasOrigin != 0 {
		idVal, tail, err := decodeID(buf)
		if err != nil {
			return Block{}, buf, err
		}
		origin = &idVal
		buf = tail
	}
	if info&block.InfoHasRightOrigin != 0 {
		idVal, tail, err := decodeID(buf)
		if err != nil {
			return Block{}, buf, err
		}
		rightOrigin = &idVal
		buf = tail
	}

	parent := block.Parent{Kind: block.ParentUnknown}
	if cantCopyParentInfo {
		if len(buf) < 1 {
			return Block{}, buf, lib0.ErrTruncated
		}
		parentInfo := buf[0]
		buf = buf[1:]
		if parentInfo == 0x01 {
			name, n, err := lib0.ReadVarString(buf)
			if err != nil {
				return Block{}, buf, err
			}
			parent = block.Parent{Kind: block.ParentNamed, Named: name}
			buf = buf[n:]
		} else {
			pid, tail, err := decodeID(buf)
			if err != nil {
				return Block{}, buf, err
			}
			parent = block.Parent{Kind: block.ParentID, ID: pid}
			buf = tail
		}
	}

	var parentSub *string
	if cantCopyParentInfo && info&block.InfoHasParentSub != 0 {
		s, n, err := lib0.ReadVarString(buf)
		if err != nil {
			return Block{}, buf, err
		}
		parentSub = &s
		buf = buf[n:]
	}

	content, tail, err := DecodeContent(buf, info&block.InfoContentMask)
	if err != nil {
		return Block{}, buf, err
	}
	buf = tail

	it := &block.Item{
		ID:          id,
		Len:         content.Len(block.OffsetUtf16),
		Origin:      origin,
		RightOrigin: rightOrigin,
		Content:     content,
		Parent:      parent,
		ParentSub:   parentSub,
	}
	if content.IsCountable() {
		it.SetCountable(true)
	}
	return Block{Kind: WireBlockItem, Item: it}, buf, nil
}

func decodeID(buf []byte) (block.ID, []byte, error) {
	client, n, err := lib0.ReadVarUint(buf)
	if err != nil {
		return block.ID{}, buf, err
	}
	buf = buf[n:]
	clock, n, err := lib0.ReadVarUint(buf)
	if err != nil {
		return block.ID{}, buf, err
	}
	return block.ID{Client: client, Clock: clock}, buf[n:], nil
}

// Apply integrates every block and delete-set entry in u into the
// doc represented by txn. Items whose causal dependencies the local
// store has not yet seen — Origin, RightOrigin, or Parent-by-ID
// references to unseen clocks — are queued in the doc's pending
// buffer (Pending) for automatic retry on subsequent Apply calls
// that satisfy them. Delete-set ranges targeting unseen IDs are
// likewise queued.
//
// Always returns nil. The previous ErrMissingDependency contract
// is retired; see docs/tech-debt.md "Pending update buffer not
// implemented" (now resolved). The signature stays
// `(*TransactionMut) error` for backwards compatibility with
// callers that check the error path.
//
// Mirrors yrs Update::integrate (update.rs:234-301) plus the
// pending-buffer retry loop yrs runs in `Store::try_integrate_pending`.
func (u *Update) Apply(txn *doc.TransactionMut) error {
	p := getOrCreatePending(txn)
	p.foldUpdate(u, txn.Store())
	for p.Drain(txn) > 0 {
		// Loop until fixed point. Drain returns 0 when no further
		// queued block could integrate this pass — the queue either
		// drained empty or is genuinely stuck on missing deps.
	}
	if p.IsEmpty() {
		txn.SetPendingState(nil)
	} else {
		txn.SetPendingState(p)
	}
	return nil
}

// getOrCreatePending returns the doc's pending buffer, lazily
// constructing one if none has been installed. Caller already
// holds the doc's write lock via txn.
func getOrCreatePending(txn *doc.TransactionMut) *Pending {
	if s := txn.PendingState(); s != nil {
		if p, ok := s.(*Pending); ok && p != nil {
			return p
		}
	}
	return NewPending()
}

// ApplyUpdate is the top-level convenience entry point. It opens a
// write transaction on d, decodes raw, integrates the result, and
// returns. Equivalent to:
//
//	upd, _, err := DecodeUpdate(raw)
//	if err != nil { return err }
//	txn := d.WriteTxn()
//	defer txn.Commit()
//	return upd.Apply(txn)
//
// Returns the decode error verbatim when raw is malformed. The
// pending buffer absorbs any missing-dependency items silently;
// inspect with GetPending afterwards if the caller cares.
func ApplyUpdate(d *doc.Doc, raw []byte) error {
	upd, _, err := DecodeUpdate(raw)
	if err != nil {
		return fmt.Errorf("ApplyUpdate decode: %w", err)
	}
	txn := d.WriteTxn()
	defer txn.Commit()
	return upd.Apply(txn)
}

// GetPending returns the doc's current pending buffer, or nil if
// no pending state is installed (queue is empty). Caller MUST hold
// at least a read transaction on d.
//
// The returned pointer is the live buffer — mutate at your peril.
// Use Pending.MissingSV and Pending.BlockCount for safe inspection.
func GetPending(t *doc.Transaction) *Pending {
	s := t.PendingState()
	if s == nil {
		return nil
	}
	p, _ := s.(*Pending)
	return p
}

// HasPending reports whether the doc has any queued items awaiting
// dependencies.
func HasPending(t *doc.Transaction) bool {
	p := GetPending(t)
	return !p.IsEmpty()
}

// MissingSV returns the state-vector of clocks the doc needs to
// receive in order to drain its pending buffer. Sync-protocol
// servers send this back to peers as a re-fetch request.
//
// Returns an empty SV when the pending buffer is empty.
func MissingSV(t *doc.Transaction) store.StateVector {
	p := GetPending(t)
	if p == nil {
		return make(store.StateVector)
	}
	return p.MissingSV(t.Store())
}
