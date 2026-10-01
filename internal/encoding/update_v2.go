package encoding

import (
	"fmt"
	"sort"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/store"
)

// V2 update wire format wiring.
//
// Mirrors V1's update.go but routes per-block field writes through
// EncoderV2 / DecoderV2 column buffers per docs/yrs-port-notes/
// update-v2.md. Block-level semantics (info-byte layout, parent
// dispatch, content kinds, integrate / pending / delete-set handling)
// are shared with V1 — only the byte routing differs. Reuses the
// existing Update struct + Update.Apply unchanged.
//
// Wire structure (writeClientsStructs + writeDeleteSet against
// UpdateEncoderV2 in yjs/src/utils/encoding.js + DeleteSet.js):
//
//	[V2 header]                            // EncoderV2.Bytes prefix
//	[varuint client_count]                 // -> rest
//	for each client (desc by clientID):
//	  [varuint block_count]                // -> rest
//	  [WriteClient(client)]                // -> client column
//	  [varuint clock_start]                // -> rest
//	  for each cell:
//	    [WriteInfo(info)]                  // -> info column
//	    case info==0  (GC):
//	      [WriteLen(len)]                  // -> len column
//	    case info==10 (Skip):
//	      [varuint(len)]                   // -> rest (NOT len column — yjs anomaly)
//	    default (Item):
//	      if HAS_ORIGIN:       WriteLeftID(origin)
//	      if HAS_RIGHT_ORIGIN: WriteRightID(rightOrigin)
//	      if cantCopyParentInfo:
//	        WriteParentInfo(isRootName)
//	        if isRootName: WriteString(name)
//	        else:          WriteLeftID(parentItemID)
//	        if HAS_PARENT_SUB:
//	          WriteString(parentSub)       // -> string column (yjs uses writeString, NOT writeKey)
//	      EncodeContentV2(content)
//	[delete set]                           // see writeDeleteSetV2 below

// EncodeStateAsUpdateV2 returns V2 wire bytes for the doc's full
// state. Mirrors EncodeStateAsUpdate. Caller-side dispatch: V1 vs
// V2 is the caller's choice (no autodetect on the wire side).
func EncodeStateAsUpdateV2(d *doc.Doc) []byte {
	txn := d.ReadTxn()
	defer txn.Close()
	return EncodeDiffV2(d, txn, nil)
}

// EncodeDiffV2 returns V2 wire bytes for the blocks the local doc
// has that the remote (per remoteSV) does not: the V2 form of
// EncodeDiff, each client from the remote's clock with the first
// struct cut at that clock, then the whole store's delete set.
func EncodeDiffV2(d *doc.Doc, txn *doc.Transaction, remoteSV store.StateVector) []byte {
	bs := txn.Store()
	enc := NewEncoderV2()
	writeClientsStructsV2(enc, bs, remoteSV)
	// Delete set goes into the V2 rest stream as a V2-flavoured
	// diff stream (cumulative clock + len-1) — see writeDeleteSetV2.
	writeDeleteSetV2(enc, buildDeleteSetFromStore(bs, bs.GetStateVector()))
	return enc.Bytes()
}

// writeClientsStructsV2 is writeClientsStructs through the V2 column
// API.
func writeClientsStructsV2(enc *EncoderV2, bs *store.BlockStore, remoteSV store.StateVector) {
	runs := diffRuns(bs, remoteSV)
	enc.WriteVarUint(uint64(len(runs)))
	for _, run := range runs {
		enc.WriteVarUint(uint64(run.list.Len() - run.idx))
		enc.WriteClient(run.client)
		enc.WriteVarUint(run.clock)
		first, _ := run.list.Get(run.idx)
		encodeCellV2From(enc, first, run.clock-first.ClockStart())
		for i := run.idx + 1; i < run.list.Len(); i++ {
			cell, _ := run.list.Get(i)
			encodeCellV2(enc, cell)
		}
	}
}

// encodeCellV2 writes one whole cell (Item or GC) via the column API.
func encodeCellV2(enc *EncoderV2, cell store.BlockCell) {
	encodeCellV2From(enc, cell, 0)
}

// encodeCellV2From writes cell from offset, as encodeCellFrom does for
// V1. GC.write: writeInfo(0) + writeLen(length - offset).
func encodeCellV2From(enc *EncoderV2, cell store.BlockCell, offset uint64) {
	switch cell.Kind {
	case store.CellKindGC:
		enc.WriteInfo(0)
		enc.WriteLen(cell.GC.Len() - offset)
	case store.CellKindItem:
		it := cell.Item
		if offset > 0 {
			it = itemFrom(it, offset)
		}
		encodeItemV2(enc, it)
	default:
		panic(fmt.Sprintf("encoding.encodeCellV2From: unknown cell kind %d", cell.Kind))
	}
}

// encodeItemV2 writes a full Item record through the column API.
// Mirrors Item.write in yjs/src/structs/Item.js but routes field
// writes per the V2 column layout.
func encodeItemV2(enc *EncoderV2, it *block.Item) {
	info := it.Info()
	cantCopyParentInfo := info&(block.InfoHasOrigin|block.InfoHasRightOrigin) == 0
	enc.WriteInfo(info)
	if it.Origin != nil {
		enc.WriteLeftID(*it.Origin)
	}
	if it.RightOrigin != nil {
		enc.WriteRightID(*it.RightOrigin)
	}
	if cantCopyParentInfo {
		switch it.Parent.Kind {
		case block.ParentBranch:
			b := it.Parent.Branch
			if b == nil {
				panic("encoding.encodeItemV2: ParentBranch with nil Branch")
			}
			if b.Item != nil {
				enc.WriteParentInfo(false)
				enc.WriteLeftID(b.Item.ID)
			} else if b.Name != "" {
				enc.WriteParentInfo(true)
				enc.WriteString(b.Name)
			} else {
				panic("encoding.encodeItemV2: ParentBranch with neither nested item nor root name")
			}
		case block.ParentNamed:
			enc.WriteParentInfo(true)
			enc.WriteString(it.Parent.Named)
		case block.ParentID:
			enc.WriteParentInfo(false)
			enc.WriteLeftID(it.Parent.ID)
		case block.ParentUnknown:
			panic("encoding.encodeItemV2: ParentUnknown — cannot emit")
		}
		if it.ParentSub != nil {
			// yjs Item.write: parentSub uses writeString (string
			// column in V2), NOT writeKey. Verified against
			// testdata/gen/node_modules/yjs/src/structs/Item.js.
			enc.WriteString(*it.ParentSub)
		}
	}
	EncodeContentV2(enc, it.Content)
}

// EncodeContentV2 writes a Content payload via the V2 column API.
// Mirrors EncodeContent (V1) but routes per the per-ContentKind
// wire layout documented in yjs/src/structs/Content*.js.
func EncodeContentV2(enc *EncoderV2, c block.Content) {
	switch c.Kind {
	case block.KindAny:
		// ContentAny.write: writeLen(count) + writeAny per item
		// (verified against ContentAny.js).
		enc.WriteLen(uint64(len(c.Anys)))
		for _, v := range c.Anys {
			enc.WriteAny(v)
		}
	case block.KindString:
		enc.WriteString(c.Str)
	case block.KindBinary:
		enc.WriteVarBuf(c.Bytes)
	case block.KindDeleted:
		enc.WriteLen(c.DeletedLen)
	case block.KindEmbed:
		var v block.Any
		if len(c.Anys) > 0 {
			v = c.Anys[0]
		}
		enc.WriteJSON(v)
	case block.KindFormat:
		// ContentFormat.write: writeKey(key) + writeJSON(value).
		enc.WriteKey(c.FormatKey)
		var v block.Any
		if len(c.Anys) > 0 {
			v = c.Anys[0]
		}
		enc.WriteJSON(v)
	case block.KindType:
		if c.Branch == nil {
			panic("encoding.EncodeContentV2: KindType with nil Branch")
		}
		enc.WriteTypeRef(uint8(c.Branch.TypeRef))
		// XmlElement / XmlHook carry a tag name; verified against
		// YXmlElement._write / YXmlHook._write — they use writeKey
		// (NOT writeString) in V2. This routes through the key
		// table on encode (per-yjs bug: encoder keyMap never
		// populates) and the asymmetric decoder-side cache.
		switch c.Branch.TypeRef {
		case block.TypeRefXmlElement, block.TypeRefXmlHook:
			enc.WriteKey(c.Branch.Name)
		}
	default:
		panic(fmt.Sprintf("encoding.EncodeContentV2: unsupported kind %d", c.Kind))
	}
}

// writeDeleteSetV2 emits the DeleteSet in V2 format. Verified
// against yjs DeleteSet.writeDeleteSet using DSEncoderV2:
//
//	varuint(client_count)                  -> rest
//	for each client (desc):
//	  resetDsCurVal (dsCurr = 0)
//	  varuint(client)                      -> rest
//	  varuint(len = numRanges)             -> rest
//	  for each range:
//	    varuint(clock - dsCurr) ; dsCurr = clock
//	    varuint(len - 1)        ; dsCurr += len
//
// All three top-level varuints go to the V2 rest stream — the
// IntDiffOpt columns are NOT involved. The "diff" pattern here is
// hand-rolled per-client into the rest stream, not column-encoded.
func writeDeleteSetV2(enc *EncoderV2, ds *IdSet) {
	// Collect non-empty clients and emit in DESCENDING order per
	// yjs writeDeleteSet ("sort((a, b) => b[0] - a[0])"). Same-
	// package access to ds.clients keeps the helper in one place
	// without adding a Ranges accessor.
	clients := make([]uint64, 0, len(ds.clients))
	for c, ranges := range ds.clients {
		if len(ranges) > 0 {
			clients = append(clients, c)
		}
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] > clients[j] })

	enc.WriteVarUint(uint64(len(clients)))
	for _, c := range clients {
		ranges := ds.clients[c]
		enc.WriteVarUint(c)
		enc.WriteVarUint(uint64(len(ranges)))
		var dsCurr uint64
		for _, r := range ranges {
			enc.WriteVarUint(r.Start - dsCurr)
			dsCurr = r.Start
			// writeDsLen requires len > 0 per yjs source — IdSet
			// invariant should guarantee this (zero-length ranges
			// are never inserted).
			enc.WriteVarUint(r.Length - 1)
			dsCurr += r.Length
		}
	}
}

// DecodeUpdateV2 parses V2 wire bytes into an Update. The returned
// Update is ready for Apply but its blocks have nil Left/Right —
// Repair fills those at apply time. Mirrors DecodeUpdate (V1) but
// pulls per-field bytes from the column decoders.
func DecodeUpdateV2(buf []byte) (*Update, error) {
	// Block/client data is pulled from the V2 column streams, which are
	// all slices of buf, so the total input length is a valid upper
	// bound on any element count (each element consumes >= 1 byte from
	// some column). Without it, a wire-supplied count drove an
	// unbounded make()/decode loop on drained columns — a fuzzer
	// OOM-killed the process from a 69-byte input.
	total := len(buf)

	dec, err := NewDecoderV2(buf)
	if err != nil {
		return nil, fmt.Errorf("DecodeUpdateV2 header: %w", err)
	}

	clientCount, err := dec.ReadVarUint()
	if err != nil {
		return nil, fmt.Errorf("DecodeUpdateV2 client count: %w", err)
	}
	if err := checkDecodeCount(clientCount, total); err != nil {
		return nil, fmt.Errorf("DecodeUpdateV2 client count: %w", err)
	}

	u := NewUpdate()
	for i := uint64(0); i < clientCount; i++ {
		blockCount, err := dec.ReadVarUint()
		if err != nil {
			return nil, fmt.Errorf("DecodeUpdateV2 block count: %w", err)
		}
		if err := checkDecodeCount(blockCount, total); err != nil {
			return nil, fmt.Errorf("DecodeUpdateV2 block count: %w", err)
		}
		client, err := dec.ReadClient()
		if err != nil {
			return nil, fmt.Errorf("DecodeUpdateV2 client: %w", err)
		}
		clock, err := dec.ReadVarUint()
		if err != nil {
			return nil, fmt.Errorf("DecodeUpdateV2 clock start: %w", err)
		}

		blocks := make([]Block, 0, blockCount)
		for j := uint64(0); j < blockCount; j++ {
			b, err := decodeBlockV2(dec, block.ID{Client: client, Clock: clock})
			if err != nil {
				return nil, fmt.Errorf("DecodeUpdateV2 block %d: %w", j, err)
			}
			blocks = append(blocks, b)
			clock += b.length()
		}
		u.Blocks[client] = blocks
	}

	ds, err := readDeleteSetV2(dec)
	if err != nil {
		return nil, fmt.Errorf("DecodeUpdateV2 delete set: %w", err)
	}
	u.DeleteSet = ds
	return u, nil
}

// decodeBlockV2 parses one block record at the given starting ID
// from the V2 column streams.
func decodeBlockV2(dec *DecoderV2, id block.ID) (Block, error) {
	info, err := dec.ReadInfo()
	if err != nil {
		return Block{}, fmt.Errorf("decodeBlockV2 info: %w", err)
	}

	switch info {
	case 0: // GC — len from len column.
		l, err := dec.ReadLen()
		if err != nil {
			return Block{}, fmt.Errorf("decodeBlockV2 gc len: %w", err)
		}
		return Block{Kind: WireBlockGC, ID: id, Len: l}, nil
	case 10: // Skip — len from REST stream, NOT len column (yjs anomaly).
		l, err := dec.ReadVarUint()
		if err != nil {
			return Block{}, fmt.Errorf("decodeBlockV2 skip len: %w", err)
		}
		return Block{Kind: WireBlockSkip, ID: id, Len: l}, nil
	}

	cantCopyParentInfo := info&(block.InfoHasOrigin|block.InfoHasRightOrigin) == 0
	var origin, rightOrigin *block.ID
	if info&block.InfoHasOrigin != 0 {
		idVal, err := dec.ReadLeftID()
		if err != nil {
			return Block{}, fmt.Errorf("decodeBlockV2 origin: %w", err)
		}
		origin = &idVal
	}
	if info&block.InfoHasRightOrigin != 0 {
		idVal, err := dec.ReadRightID()
		if err != nil {
			return Block{}, fmt.Errorf("decodeBlockV2 right origin: %w", err)
		}
		rightOrigin = &idVal
	}

	parent := block.Parent{Kind: block.ParentUnknown}
	if cantCopyParentInfo {
		isRootName, err := dec.ReadParentInfo()
		if err != nil {
			return Block{}, fmt.Errorf("decodeBlockV2 parent info: %w", err)
		}
		if isRootName {
			name, err := dec.ReadString()
			if err != nil {
				return Block{}, fmt.Errorf("decodeBlockV2 parent name: %w", err)
			}
			parent = block.Parent{Kind: block.ParentNamed, Named: name}
		} else {
			pid, err := dec.ReadLeftID()
			if err != nil {
				return Block{}, fmt.Errorf("decodeBlockV2 parent id: %w", err)
			}
			parent = block.Parent{Kind: block.ParentID, ID: pid}
		}
	}

	var parentSub *string
	if cantCopyParentInfo && info&block.InfoHasParentSub != 0 {
		s, err := dec.ReadString()
		if err != nil {
			return Block{}, fmt.Errorf("decodeBlockV2 parent sub: %w", err)
		}
		parentSub = &s
	}

	content, err := DecodeContentV2(dec, info&block.InfoContentMask)
	if err != nil {
		return Block{}, fmt.Errorf("decodeBlockV2 content: %w", err)
	}

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
	return Block{Kind: WireBlockItem, Item: it}, nil
}

// DecodeContentV2 reads a Content payload from V2 column streams
// given the content ref-number (low nibble of info byte). Mirror
// of EncodeContentV2.
func DecodeContentV2(dec *DecoderV2, refNum uint8) (block.Content, error) {
	switch block.ContentKind(refNum) {
	case block.KindAny:
		count, err := dec.ReadLen()
		if err != nil {
			return block.Content{}, fmt.Errorf("DecodeContentV2 any count: %w", err)
		}
		// Elements come from the rest stream, so it bounds the count;
		// without this a wire count forces an unbounded make().
		if err := checkDecodeCount(count, len(dec.ReadRestBytes())); err != nil {
			return block.Content{}, fmt.Errorf("DecodeContentV2 any count: %w", err)
		}
		anys := make([]block.Any, count)
		for i := uint64(0); i < count; i++ {
			v, err := dec.ReadAny()
			if err != nil {
				return block.Content{}, fmt.Errorf("DecodeContentV2 any %d: %w", i, err)
			}
			anys[i] = v
		}
		return block.Content{Kind: block.KindAny, Anys: anys}, nil
	case block.KindString:
		s, err := dec.ReadString()
		if err != nil {
			return block.Content{}, fmt.Errorf("DecodeContentV2 string: %w", err)
		}
		return block.Content{Kind: block.KindString, Str: s}, nil
	case block.KindBinary:
		b, err := dec.ReadVarBuf()
		if err != nil {
			return block.Content{}, fmt.Errorf("DecodeContentV2 binary: %w", err)
		}
		return block.Content{Kind: block.KindBinary, Bytes: b}, nil
	case block.KindDeleted:
		v, err := dec.ReadLen()
		if err != nil {
			return block.Content{}, fmt.Errorf("DecodeContentV2 deleted: %w", err)
		}
		return block.Content{Kind: block.KindDeleted, DeletedLen: v}, nil
	case block.KindEmbed:
		v, err := dec.ReadJSON()
		if err != nil {
			return block.Content{}, fmt.Errorf("DecodeContentV2 embed: %w", err)
		}
		return block.Content{Kind: block.KindEmbed, Anys: []block.Any{v}}, nil
	case block.KindFormat:
		key, err := dec.ReadKey()
		if err != nil {
			return block.Content{}, fmt.Errorf("DecodeContentV2 format key: %w", err)
		}
		v, err := dec.ReadJSON()
		if err != nil {
			return block.Content{}, fmt.Errorf("DecodeContentV2 format value: %w", err)
		}
		return block.Content{Kind: block.KindFormat, FormatKey: key, Anys: []block.Any{v}}, nil
	case block.KindType:
		typeRefU, err := dec.ReadTypeRef()
		if err != nil {
			return block.Content{}, fmt.Errorf("DecodeContentV2 type ref: %w", err)
		}
		typeRef := block.TypeRef(typeRefU)
		br := &block.Branch{TypeRef: typeRef}
		switch typeRef {
		case block.TypeRefXmlElement, block.TypeRefXmlHook:
			name, err := dec.ReadKey()
			if err != nil {
				return block.Content{}, fmt.Errorf("DecodeContentV2 type name: %w", err)
			}
			br.Name = name
		}
		return block.Content{Kind: block.KindType, Branch: br}, nil
	default:
		return block.Content{}, fmt.Errorf("DecodeContentV2: unsupported content kind %d", refNum)
	}
}

// readDeleteSetV2 reads the V2 DeleteSet. Mirror of writeDeleteSetV2.
// All field bytes come from the rest stream.
func readDeleteSetV2(dec *DecoderV2) (*IdSet, error) {
	ds := NewIdSet()
	clientCount, err := dec.ReadVarUint()
	if err != nil {
		return nil, fmt.Errorf("readDeleteSetV2 client count: %w", err)
	}
	// All delete-set counts and range pairs come from the rest stream,
	// so it bounds them — same guard as the V1 DecodeIdSet twin.
	if err := checkDecodeCount(clientCount, len(dec.ReadRestBytes())); err != nil {
		return nil, fmt.Errorf("readDeleteSetV2 client count: %w", err)
	}
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
		for j := uint64(0); j < rangeCount; j++ {
			clockDiff, err := dec.ReadVarUint()
			if err != nil {
				return nil, fmt.Errorf("readDeleteSetV2 clock diff: %w", err)
			}
			dsCurr += clockDiff
			start := dsCurr
			lenMinus1, err := dec.ReadVarUint()
			if err != nil {
				return nil, fmt.Errorf("readDeleteSetV2 len: %w", err)
			}
			l := lenMinus1 + 1
			ds.Insert(c, start, l)
			dsCurr += l
		}
	}
	return ds, nil
}

// ApplyUpdateV2 is the top-level convenience entry point for V2
// wire bytes. Opens a write transaction on d, decodes raw, applies,
// returns. Pending-buffer semantics identical to ApplyUpdate (V1):
// items missing causal dependencies queue silently and drain on
// subsequent ApplyUpdate / ApplyUpdateV2 calls.
//
// V1 and V2 wire formats are NOT interchangeable. Calling
// ApplyUpdateV2 on V1 bytes (or vice versa) is undefined behaviour.
func ApplyUpdateV2(d *doc.Doc, raw []byte) error {
	return ApplyUpdateV2WithOrigin(d, raw, nil)
}

// ApplyUpdateV2WithOrigin is ApplyUpdateV2 with origin as the
// transaction's Origin.
func ApplyUpdateV2WithOrigin(d *doc.Doc, raw []byte, origin any) error {
	upd, err := DecodeUpdateV2(raw)
	if err != nil {
		return fmt.Errorf("ApplyUpdateV2 decode: %w", err)
	}
	txn := d.WriteTxn()
	txn.Origin = origin
	defer txn.Commit()
	return upd.Apply(txn)
}
