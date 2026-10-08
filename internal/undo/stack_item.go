package undo

import "github.com/Deln0r/ygo/internal/encoding"

// StackItem records one captured change for Undo or Redo.
//
// Insertions covers item ID ranges created during the captured
// transaction window. Undo deletes everything in Insertions; Redo
// recreates it.
//
// Deletions covers item ID ranges tombstoned during the captured
// window. Undo restores them (via the redoItem chain in Item.Redone);
// Redo re-deletes the restorations.
//
// Meta is an arbitrary user payload — typically used by editor
// integrations to store and restore a selection range across an
// Undo / Redo round-trip. The core UndoManager logic never reads or
// writes Meta; only the consumer does.
type StackItem struct {
	Insertions *encoding.IdSet
	Deletions  *encoding.IdSet
	Meta       map[string]any

	// delOrder lists the clients of every deletion in the captured
	// transactions, in or out of scope, in the order they first deleted
	// something, across grouped transactions. Restoration walks clients
	// in this order, as yjs walks the delete set it keeps (the whole
	// transaction's, a Map in insertion order): which item is restored
	// first decides whether a later one conflicts with it.
	delOrder []uint64

	// insOrder lists the clients that created anything in the captured
	// transactions, in or out of scope: per transaction in the order they
	// first joined the document's store (yjs builds insertions from
	// afterState, which iterates store.clients), and across grouped
	// transactions the first one's clients followed by clients new in
	// later ones (yjs mergeDeleteSets). Undo collects inserted items in
	// this order and deletes them in reverse.
	insOrder []uint64
}

// noteDeletion records client in the deletion order if it is new.
func (si *StackItem) noteDeletion(client uint64) {
	for _, c := range si.delOrder {
		if c == client {
			return
		}
	}
	si.delOrder = append(si.delOrder, client)
}

// noteInsertion records client in the insertion order if it is new.
func (si *StackItem) noteInsertion(client uint64) {
	for _, c := range si.insOrder {
		if c == client {
			return
		}
	}
	si.insOrder = append(si.insOrder, client)
}

// deletionClients returns the clients of Deletions in deletion order,
// then any not recorded there in ascending order.
func (si *StackItem) deletionClients() []uint64 {
	return orderedClients(si.delOrder, si.Deletions)
}

// insertionClients returns the clients of Insertions in insertion order,
// then any not recorded there in ascending order.
func (si *StackItem) insertionClients() []uint64 {
	return orderedClients(si.insOrder, si.Insertions)
}

// orderedClients returns order, then the clients of set missing from it
// in ascending order.
func orderedClients(order []uint64, set *encoding.IdSet) []uint64 {
	out := append([]uint64(nil), order...)
	set.Iterate(func(client uint64, _ []encoding.Range) {
		for _, c := range out {
			if c == client {
				return
			}
		}
		out = append(out, client)
	})
	return out
}

// newStackItem returns a StackItem with empty IdSets ready to accept
// Insert calls and a nil Meta map (callers populate Meta on demand).
func newStackItem() *StackItem {
	return &StackItem{
		Insertions: encoding.NewIdSet(),
		Deletions:  encoding.NewIdSet(),
	}
}

// merge folds the ranges from other into si. Used when an
// AfterTransaction handler decides to append to the existing top of
// stack rather than open a new StackItem (capture-timeout grouping).
// Meta is not merged: the most recent captured Meta wins via
// last-writer; if the consumer set Meta on a previous item and wants
// to preserve it, they should set it again on the next change.
func (si *StackItem) merge(other *StackItem) {
	mergeIdSets(si.Insertions, other.Insertions)
	mergeIdSets(si.Deletions, other.Deletions)
	for _, c := range other.delOrder {
		si.noteDeletion(c)
	}
	for _, c := range other.insOrder {
		si.noteInsertion(c)
	}
	if other.Meta != nil {
		si.Meta = other.Meta
	}
}

// mergeIdSets inserts every range from src into dst. Existing ranges
// are auto-merged by IdSet.Insert.
func mergeIdSets(dst, src *encoding.IdSet) {
	if src == nil {
		return
	}
	src.Iterate(func(client uint64, ranges []encoding.Range) {
		for _, r := range ranges {
			dst.Insert(client, r.Start, r.Length)
		}
	})
}
