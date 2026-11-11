package types_test

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/internal/undo"
	"github.com/Deln0r/ygo/internal/utf16"
)

// checkMarkers verifies every search marker of br: its item is still in
// br's list, and its Index is the number of live countable units before
// the item, the count every position walk uses.
func checkMarkers(br *block.Branch) error {
	before := map[*block.Item]uint64{}
	var n uint64
	for it := br.Start; it != nil; it = it.Right {
		before[it] = n
		if !it.IsDeleted() && it.IsCountable() {
			n += it.Len
		}
	}
	for _, mk := range br.Markers.Snapshot() {
		want, ok := before[mk.Item]
		if !ok {
			return fmt.Errorf("marker at %d points at %v, which is not in the list", mk.Index, mk.Item.ID)
		}
		if mk.Index != want {
			return fmt.Errorf("marker on %v says %d, the item starts at %d", mk.Item.ID, mk.Index, want)
		}
	}
	return nil
}

// markerPair runs the same edits on two documents of the same client: one
// keeps its search markers, the other drops them before every edit, so
// each of its positions comes from a walk from the start. Both must end
// up byte for byte the same.
type markerPair struct {
	t          *testing.T
	seed       uint64
	docs       [2]*doc.Doc
	txns       [2]*doc.TransactionMut
	branch     [2]*block.Branch
	undo       [2]*undo.UndoManager
	remote     *doc.Doc
	name       string
	ops        int
}

func newMarkerPair(t *testing.T, seed uint64, name string) *markerPair {
	p := &markerPair{t: t, seed: seed, name: name, remote: doc.NewDocWithOptions(doc.Options{ClientID: 2})}
	for i := range p.docs {
		p.docs[i] = doc.NewDocWithOptions(doc.Options{ClientID: 1})
		p.branch[i] = p.docs[i].Branch(name)
		p.undo[i] = undo.NewUndoManager(p.docs[i], []*block.Branch{p.branch[i]}, undo.Options{CaptureTimeout: -1})
	}
	return p
}

// edit runs fn on both documents in their open transactions, opening them
// first, and checks the markers of the first one after it.
func (p *markerPair) edit(op string, fn func(i int, txn *doc.TransactionMut) error) {
	p.t.Helper()
	p.ops++
	var errs [2]error
	for i := range p.docs {
		if p.txns[i] == nil {
			p.txns[i] = p.docs[i].WriteTxn()
		}
		if i == 1 {
			p.branch[1].Markers.Invalidate()
		}
		errs[i] = fn(i, p.txns[i])
	}
	if (errs[0] == nil) != (errs[1] == nil) {
		p.t.Fatalf("seed %d op %d %s: errors differ: %v / %v", p.seed, p.ops, op, errs[0], errs[1])
	}
	if err := checkMarkers(p.branch[0]); err != nil {
		p.t.Fatalf("seed %d op %d %s: %v", p.seed, p.ops, op, err)
	}
}

// commit ends both transactions and compares the documents.
func (p *markerPair) commit() {
	p.t.Helper()
	for i := range p.txns {
		if p.txns[i] != nil {
			p.txns[i].Commit()
			p.txns[i] = nil
		}
	}
	if err := checkMarkers(p.branch[0]); err != nil {
		p.t.Fatalf("seed %d after the commit at op %d: %v", p.seed, p.ops, err)
	}
	a, b := encoding.EncodeStateAsUpdate(p.docs[0]), encoding.EncodeStateAsUpdate(p.docs[1])
	if !bytes.Equal(a, b) {
		p.t.Fatalf("seed %d after op %d: the documents differ (%d and %d bytes)", p.seed, p.ops, len(a), len(b))
	}
}

// fromRemote commits, lets another client catch up and make an edit, and
// applies that edit to both documents: an update moves indices the
// markers knew.
func (p *markerPair) fromRemote(edit func(txn *doc.TransactionMut, br *block.Branch)) {
	p.t.Helper()
	p.commit()
	if err := encoding.ApplyUpdate(p.remote, encoding.EncodeStateAsUpdate(p.docs[0])); err != nil {
		p.t.Fatal(err)
	}
	br := p.remote.Branch(p.name)
	txn := p.remote.WriteTxn()
	edit(txn, br)
	txn.Commit()
	u := encoding.EncodeStateAsUpdate(p.remote)
	for _, d := range p.docs {
		if err := encoding.ApplyUpdate(d, u); err != nil {
			p.t.Fatal(err)
		}
	}
	p.commit()
}

// undoStep commits and undoes (or redoes) the last step on both documents.
func (p *markerPair) undoStep(redo bool) {
	p.t.Helper()
	p.commit()
	for _, um := range p.undo {
		if redo {
			um.Redo()
		} else {
			um.Undo()
		}
	}
	p.commit()
}

var markerAlphabet = []string{"a", "b", "c", " ", "\n", "é", "😀"}

func randomText(r *rand.Rand, n int) string {
	var b bytes.Buffer
	for range n {
		b.WriteString(markerAlphabet[r.IntN(len(markerAlphabet))])
	}
	return b.String()
}

// Search markers stay exact through typing, backspace, cursor moves,
// pastes, large deletions, embeds, formatting, transaction commits (which
// squash items under the markers), remote edits and undo, and positions
// found from them are the positions a walk from the start finds.
func TestSearchMarkers_TextMatchesWalkFromStart(t *testing.T) {
	for seed := uint64(1); seed <= 300; seed++ {
		r := rand.New(rand.NewPCG(seed, 7))
		p := newMarkerPair(t, seed, "text")
		text := [2]*types.Text{types.NewText(p.branch[0]), types.NewText(p.branch[1])}
		formatting := seed%3 == 0
		perTxn := 1 + r.IntN(30)
		var cursor uint64
		for range 400 {
			length := text[0].Length()
			cursor = min(cursor, length)
			switch k := r.IntN(100); {
			case k < 40:
				s := randomText(r, 1+r.IntN(3))
				at := cursor
				p.edit("type", func(i int, txn *doc.TransactionMut) error { return text[i].Insert(txn, at, s) })
				cursor = min(at+utf16.Length(s), text[0].Length())
			case k < 55 && cursor > 0:
				n := min(cursor, uint64(1+r.IntN(2)))
				at := cursor - n
				p.edit("backspace", func(i int, txn *doc.TransactionMut) error { return text[i].Delete(txn, at, n) })
				cursor = at
			case k < 62 && cursor < length:
				n := min(length-cursor, uint64(1+r.IntN(3)))
				at := cursor
				p.edit("delete", func(i int, txn *doc.TransactionMut) error { return text[i].Delete(txn, at, n) })
			case k < 72:
				step := uint64(r.IntN(20))
				if r.IntN(2) == 0 {
					cursor = min(cursor+step, length)
				} else {
					cursor -= min(cursor, step)
				}
			case k < 77:
				cursor = uint64(r.IntN(int(length) + 1))
			case k < 82:
				s := randomText(r, 5+r.IntN(40))
				at := cursor
				p.edit("paste", func(i int, txn *doc.TransactionMut) error { return text[i].Insert(txn, at, s) })
			case k < 85 && length > 0:
				at := uint64(r.IntN(int(length)))
				n := 1 + uint64(r.IntN(int(length-at)))
				p.edit("cut", func(i int, txn *doc.TransactionMut) error { return text[i].Delete(txn, at, n) })
			case k < 88 && formatting:
				at := cursor
				p.edit("embed", func(i int, txn *doc.TransactionMut) error {
					return text[i].InsertEmbed(txn, at, map[string]any{"image": "x"})
				})
			case k < 92 && formatting && length > 0:
				at := uint64(r.IntN(int(length)))
				n := 1 + uint64(r.IntN(int(min(length-at, 30))))
				attrs := types.Attrs{"bold": true}
				if r.IntN(3) == 0 {
					attrs = types.Attrs{"bold": nil}
				}
				p.edit("format", func(i int, txn *doc.TransactionMut) error { return text[i].Format(txn, at, n, attrs) })
			case k < 94:
				p.fromRemote(func(txn *doc.TransactionMut, br *block.Branch) {
					rt := types.NewText(br)
					if n := rt.Length(); n > 0 && r.IntN(2) == 0 {
						at := uint64(r.IntN(int(n)))
						_ = rt.Delete(txn, at, min(n-at, uint64(1+r.IntN(5))))
						return
					}
					_ = rt.Insert(txn, uint64(r.IntN(int(rt.Length())+1)), randomText(r, 1+r.IntN(6)))
				})
			case k < 96:
				p.undoStep(r.IntN(3) == 0)
			}
			if r.IntN(perTxn) == 0 {
				p.commit()
			}
		}
		p.commit()
		if a, b := text[0].String(), text[1].String(); a != b {
			t.Fatalf("seed %d: texts differ", seed)
		}
	}
}

// The same for arrays: single inserts, ranges, pushes, nested types,
// deletions, commits, remote edits and undo.
func TestSearchMarkers_ArrayMatchesWalkFromStart(t *testing.T) {
	for seed := uint64(1); seed <= 300; seed++ {
		r := rand.New(rand.NewPCG(seed, 11))
		p := newMarkerPair(t, seed, "list")
		arr := [2]*types.Array{types.NewArray(p.branch[0]), types.NewArray(p.branch[1])}
		perTxn := 1 + r.IntN(30)
		var cursor uint64
		for range 400 {
			length := arr[0].Len()
			cursor = min(cursor, length)
			switch k := r.IntN(100); {
			case k < 35:
				at, v := cursor, r.IntN(1000)
				p.edit("insert", func(i int, txn *doc.TransactionMut) error { arr[i].Insert(txn, at, v); return nil })
				cursor = at + 1
			case k < 45:
				at, n := cursor, 2+r.IntN(6)
				vs := make([]any, n)
				for j := range vs {
					vs[j] = r.IntN(1000)
				}
				p.edit("range", func(i int, txn *doc.TransactionMut) error { arr[i].InsertRange(txn, at, vs); return nil })
			case k < 50:
				v := r.IntN(1000)
				p.edit("push", func(i int, txn *doc.TransactionMut) error { arr[i].Push(txn, v); return nil })
			case k < 55:
				at := cursor
				p.edit("nested", func(i int, txn *doc.TransactionMut) error { arr[i].InsertMap(txn, at); return nil })
			case k < 70 && cursor > 0:
				n := min(cursor, uint64(1+r.IntN(2)))
				at := cursor - n
				p.edit("backspace", func(i int, txn *doc.TransactionMut) error { arr[i].Delete(txn, at, n); return nil })
				cursor = at
			case k < 75 && length > 0:
				at := uint64(r.IntN(int(length)))
				n := 1 + uint64(r.IntN(int(length-at)))
				p.edit("cut", func(i int, txn *doc.TransactionMut) error { arr[i].Delete(txn, at, n); return nil })
			case k < 85:
				step := uint64(r.IntN(10))
				if r.IntN(2) == 0 {
					cursor = min(cursor+step, length)
				} else {
					cursor -= min(cursor, step)
				}
			case k < 90:
				cursor = uint64(r.IntN(int(length) + 1))
			case k < 93:
				p.fromRemote(func(txn *doc.TransactionMut, br *block.Branch) {
					ra := types.NewArray(br)
					if n := ra.Len(); n > 0 && r.IntN(2) == 0 {
						ra.Delete(txn, uint64(r.IntN(int(n))), 1)
						return
					}
					ra.Insert(txn, uint64(r.IntN(int(ra.Len())+1)), "remote")
				})
			case k < 95:
				p.undoStep(r.IntN(3) == 0)
			}
			if r.IntN(perTxn) == 0 {
				p.commit()
			}
		}
		p.commit()
	}
}
