package types

import (
	"sort"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
)

func init() {
	doc.FormatCleanupHook = formatCleanupFor
}

// formatCleanupFor is doc.FormatCleanupHook. yjs asks for a cleanup when a
// transaction that is not local changed a YText or YXmlText with
// formatting (YText._callObserver, for each changed type whose item is
// not deleted); the cleanup itself is cleanupTextAfterTransaction.
func formatCleanupFor(remote *doc.TransactionMut) func(*doc.TransactionMut) {
	for _, br := range remote.ChangedTypes() {
		if br.FormattedText && (br.Item == nil || !br.Item.IsDeleted()) {
			return func(cleanup *doc.TransactionMut) { cleanupTextAfterTransaction(remote, cleanup) }
		}
	}
	return nil
}

// cleanupTextAfterTransaction is yjs cleanupYTextAfterTransaction, run in
// the cleanup transaction while remote's deleted items still hold their
// content. A text that got a live format marker in remote, or lost one,
// gets the full cleanup; around other deleted content of a formatted
// text, the contextless one. The order matters and is yjs's: the set of
// texts for the full cleanup grows while the delete set is walked, and a
// text in it takes no more contextless cleanups.
func cleanupTextAfterTransaction(remote, cleanup *doc.TransactionMut) {
	var full []*block.Branch
	inFull := map[*block.Branch]bool{}
	addFull := func(br *block.Branch) {
		if !inFull[br] {
			inFull[br] = true
			full = append(full, br)
		}
	}
	bs := remote.Store()
	before, after := remote.BeforeState(), remote.AfterState()
	// The structs remote added, client by client in the order yjs
	// iterates afterState (the store's), each from its start state on.
	for _, client := range bs.ClientOrder() {
		from, to := before[client], after[client]
		if to == from {
			continue
		}
		eachBlock(remote, client, from, to, func(it *block.Item) {
			if !it.IsDeleted() && it.Content.Kind == block.KindFormat && it.Parent.IsResolved() {
				addFull(it.Parent.Branch)
			}
		})
	}
	// The structs remote deleted, as yjs walks transaction.deleteSet:
	// clients in the order of their first deletion, ranges sorted.
	for _, rg := range deletedByClientInOrder(remote) {
		for _, r := range rg.ranges {
			eachBlock(remote, rg.client, r[0], r[1], func(it *block.Item) {
				if !it.Parent.IsResolved() {
					return
				}
				parent := it.Parent.Branch
				if !hasFormatting(parent) || inFull[parent] {
					return
				}
				if it.Content.Kind == block.KindFormat {
					addFull(parent)
				} else {
					cleanupContextlessFormattingGap(cleanup, it)
				}
			})
		}
	}
	for _, br := range full {
		cleanupTextFormatting(cleanup, br)
	}
}

// hasFormatting is yjs's _hasFormatting of the object that stands for br,
// which the delete-set walk reads. A Text or XmlText is the YText that
// Doc.get built, flag false from the moment the root was typed
// (FormattedText); an untyped root is the placeholder that collected the
// format markers integrated into it, flag set by each (HasFormatting).
// Only the former asks for a cleanup, but once one runs, both take part.
func hasFormatting(br *block.Branch) bool {
	if br.TypeRef == block.TypeRefText || br.TypeRef == block.TypeRefXmlText {
		return br.FormattedText
	}
	return br.HasFormatting
}

// eachBlock calls f for every item among client's blocks covering clocks
// [from, to), each once; GC blocks are skipped, as yjs skips GC structs.
func eachBlock(txn *doc.TransactionMut, client, from, to uint64, f func(*block.Item)) {
	bs := txn.Store()
	for clock := from; clock < to; {
		cell, ok := bs.GetBlock(block.ID{Client: client, Clock: clock})
		if !ok {
			return
		}
		if it := cell.AsItem(); it != nil {
			f(it)
		}
		next := cell.ClockEnd() + 1
		if next <= clock {
			return
		}
		clock = next
	}
}

type clientRanges struct {
	client uint64
	ranges [][2]uint64 // half-open, sorted, merged
}

// deletedByClientInOrder returns txn's deleted ranges grouped by client,
// clients in the order of their first deletion, each client's ranges
// sorted and merged: yjs transaction.deleteSet after sortAndMergeDeleteSet.
func deletedByClientInOrder(txn *doc.TransactionMut) []clientRanges {
	var out []clientRanges
	at := map[uint64]int{}
	for _, d := range txn.DeletedRanges() {
		i, ok := at[d.ID.Client]
		if !ok {
			i = len(out)
			at[d.ID.Client] = i
			out = append(out, clientRanges{client: d.ID.Client})
		}
		out[i].ranges = append(out[i].ranges, [2]uint64{d.ID.Clock, d.ID.Clock + d.Len})
	}
	for i := range out {
		rs := out[i].ranges
		sort.Slice(rs, func(a, b int) bool { return rs[a][0] < rs[b][0] })
		merged := rs[:0]
		for _, r := range rs {
			if n := len(merged); n > 0 && r[0] <= merged[n-1][1] {
				if r[1] > merged[n-1][1] {
					merged[n-1][1] = r[1]
				}
				continue
			}
			merged = append(merged, r)
		}
		out[i].ranges = merged
	}
	return out
}

// cleanupContextlessFormattingGap is yjs cleanupContextlessFormattingGap:
// from item, go right to the last of the deleted or uncountable items
// that follow it, then left over that run, deleting every live format
// marker whose key a marker to its right in the run already set.
func cleanupContextlessFormattingGap(txn *doc.TransactionMut, item *block.Item) {
	for item != nil && item.Right != nil && (item.Right.IsDeleted() || !item.Right.IsCountable()) {
		item = item.Right
	}
	seen := map[string]bool{}
	for item != nil && (item.IsDeleted() || !item.IsCountable()) {
		if !item.IsDeleted() && item.Content.Kind == block.KindFormat {
			key := item.Content.FormatKey
			if seen[key] {
				txn.Delete(item)
			} else {
				seen[key] = true
			}
		}
		item = item.Left
	}
}

// cleanupTextFormatting is yjs cleanupYTextFormatting, ported as it is:
// at each live content item it runs cleanupFormattingGap from the
// previous one. Once start is a live content item, the gap's first loop
// stops on it at once, so in effect only the markers before the first
// live content are cleaned, and the ones after the last never are; yjs
// behaves so, and ygo with it.
func cleanupTextFormatting(txn *doc.TransactionMut, br *block.Branch) int {
	res := 0
	start := br.Start
	startAttrs := Attrs{}
	cur := &textPos{attrs: Attrs{}}
	for end := br.Start; end != nil; end = end.Right {
		if end.IsDeleted() {
			continue
		}
		if end.Content.Kind == block.KindFormat {
			cur.updateAttr(end.Content.FormatKey, formatValue(end))
			continue
		}
		res += cleanupFormattingGapFrom(txn, start, end, startAttrs, cur)
		startAttrs, _ = cur.currentAttrs()
		start = end
	}
	return res
}
