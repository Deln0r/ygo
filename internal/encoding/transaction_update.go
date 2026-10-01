package encoding

import (
	"github.com/Deln0r/ygo/internal/doc"
)

func init() {
	doc.UpdateEncoder = func(t *doc.TransactionMut, v2 bool) []byte {
		if v2 {
			return EncodeTransactionUpdateV2(t)
		}
		return EncodeTransactionUpdate(t)
	}
}

// EncodeTransactionUpdate returns the V1 update for one committed
// transaction, or nil when the transaction created no structs and
// deleted nothing. Mirrors yjs writeUpdateMessageFromTransaction, the
// bytes of doc.on('update'): every struct from the transaction's start
// state on (the first of each client cut at that clock, as in
// EncodeDiff) and the delete set of what this transaction deleted, not
// the whole store's.
//
// Call it after Commit's squash and garbage collection, as Doc's
// update handlers do, so the bytes describe the final layout.
func EncodeTransactionUpdate(t *doc.TransactionMut) []byte {
	if !transactionChanged(t) {
		return nil
	}
	buf := writeClientsStructs(nil, t.Store(), t.BeforeState())
	return transactionDeleteSet(t).Encode(buf)
}

// EncodeTransactionUpdateV2 is EncodeTransactionUpdate in V2, the
// bytes of doc.on('updateV2').
func EncodeTransactionUpdateV2(t *doc.TransactionMut) []byte {
	if !transactionChanged(t) {
		return nil
	}
	enc := NewEncoderV2()
	writeClientsStructsV2(enc, t.Store(), t.BeforeState())
	writeDeleteSetV2(enc, transactionDeleteSet(t))
	return enc.Bytes()
}

// transactionChanged is yjs's emit condition: the transaction deleted
// something or some client's clock moved.
func transactionChanged(t *doc.TransactionMut) bool {
	if len(t.DeletedRanges()) > 0 {
		return true
	}
	before := t.BeforeState()
	for c, clock := range t.Store().GetStateVector() {
		if clock != before[c] {
			return true
		}
	}
	return false
}

// transactionDeleteSet is the transaction's own delete set, sorted and
// merged as yjs sortAndMergeDeleteSet leaves it (IdSet.Insert merges
// overlapping and adjacent ranges).
func transactionDeleteSet(t *doc.TransactionMut) *IdSet {
	ds := NewIdSet()
	for _, r := range t.DeletedRanges() {
		ds.Insert(r.ID.Client, r.ID.Clock, r.Len)
	}
	return ds
}
