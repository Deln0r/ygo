package ygo_test

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/Deln0r/ygo"
)

// undoWireScenario is one entry of testdata/undo-wire-fixtures.json
// (testdata/gen/gen-undo-wire.mjs, yjs 13.6.33).
type undoWireScenario struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	GC        bool     `json:"gc"`
	Updates   []string `json:"updates"`
	UpdatesV2 []string `json:"updatesV2"`
	Steps     []struct {
		T string `json:"t"`
		I uint64 `json:"i"`
		N uint64 `json:"n"`
		V string `json:"v"`
	} `json:"steps"`
	States []struct {
		V1 string `json:"v1"`
		V2 string `json:"v2"`
	} `json:"states"`
}

// TestUndoWire_Fixtures: a receiver with an UndoManager that deletes,
// undoes and redoes around a peer's updates writes yjs's bytes after every
// step, V1 and V2, with the updates delivered in V1 and in V2, garbage
// collection on and off. An update delivered twice after an undo cuts the
// item its origin points into, as yjs Item.getMissing does for every
// update it reads, and the pieces an undo brought back stay apart; ygo
// skipped the cut for a block it already had, and kept one item where yjs
// holds two.
func TestUndoWire_Fixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/undo-wire-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Scenarios []undoWireScenario `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Scenarios) == 0 {
		t.Fatal("no scenarios")
	}
	for _, s := range f.Scenarios {
		for _, format := range []string{"V1", "V2"} {
			t.Run(s.Name+" "+format, func(t *testing.T) { replayUndoWire(t, s, format) })
		}
	}
}

func replayUndoWire(t *testing.T, s undoWireScenario, format string) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 99, DisableGC: !s.GC})
	var shared ygo.SharedType
	var arr *ygo.Array
	var text *ygo.Text
	if s.Kind == "text" {
		text = ygo.NewText(d, "r")
		shared = text
	} else {
		arr = ygo.NewArray(d, "r")
		shared = arr
	}
	um := ygo.NewUndoManagerWithOptions(d, ygo.UndoManagerOptions{CaptureTimeout: 1 << 40}, shared)
	for i, st := range s.Steps {
		switch st.T {
		case "up":
			apply, list := ygo.ApplyUpdateWithOrigin, s.Updates
			if format == "V2" {
				apply, list = ygo.ApplyUpdateV2WithOrigin, s.UpdatesV2
			}
			u, _ := hex.DecodeString(list[st.I])
			if err := apply(d, u, "remote"); err != nil {
				t.Fatal(err)
			}
		case "del":
			txn := d.WriteTxn()
			var err error
			if text != nil {
				err = text.Delete(txn, st.I, st.N)
			} else {
				arr.Delete(txn, st.I, st.N)
			}
			txn.Commit()
			if err != nil {
				t.Fatal(err)
			}
		case "ins":
			txn := d.WriteTxn()
			var err error
			if text != nil {
				err = text.Insert(txn, st.I, st.V)
			} else {
				arr.Insert(txn, st.I, st.V)
			}
			txn.Commit()
			if err != nil {
				t.Fatal(err)
			}
		case "undo":
			um.Undo()
		case "redo":
			um.Redo()
		default:
			t.Fatalf("step %q", st.T)
		}
		if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != s.States[i].V1 {
			t.Fatalf("step %d (%s) V1:\n got %s\nwant %s", i, st.T, got, s.States[i].V1)
		}
		if got := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(d)); got != s.States[i].V2 {
			t.Fatalf("step %d (%s) V2:\n got %s\nwant %s", i, st.T, got, s.States[i].V2)
		}
	}
}

// TestUndoWire_KnownBlocksStayLinear: an update of known one-character
// blocks whose origins point inside one long text item, none of it
// brought back by an undo, costs in proportion to its blocks, also when
// the text holds a character outside the Basic Multilingual Plane. Cutting
// at each one and merging the pieces back, as yjs does, took 17 seconds
// for an update of 543 KB; the cuts come undone, so ygo makes only those
// that split a surrogate pair.
func TestUndoWire_KnownBlocksStayLinear(t *testing.T) {
	for _, prefix := range []string{"", "\U0001F600"} {
		cost := knownBlocksCost(t, prefix)
		small, large := cost(2000), cost(8000)
		// Linear is a ratio of about 4; cutting and merging back made it 16.
		if ratio := float64(large) / float64(small); ratio > 6 {
			t.Errorf("text %q...: 8000 known blocks allocated %.1f times what 2000 did (%d vs %d bytes)", prefix, ratio, large, small)
		}
	}
}

// knownBlocksCost returns what applying n known one-character blocks,
// their origins inside a text of prefix and n characters, allocates.
func knownBlocksCost(t *testing.T, prefix string) func(n int) uint64 {
	return func(n int) uint64 {
		src := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
		x := ygo.NewText(src, "t")
		txn := src.WriteTxn()
		if err := x.Insert(txn, 0, prefix+strings.Repeat("x", n)); err != nil {
			t.Fatal(err)
		}
		txn.Commit()
		// n blocks 1:i, one character each, origin 1:(n-1-i), as known
		// as the text they repeat (their own content does not matter).
		u := []byte{1}
		u = binary.AppendUvarint(u, uint64(n))
		u = append(u, 1, 0)
		for i := range n {
			u = append(u, 0x84) // origin, string content
			u = append(u, 1)
			u = binary.AppendUvarint(u, uint64(n-1-i))
			u = append(u, 1, 'x')
		}
		u = append(u, 0)
		d := ygo.NewDoc()
		if err := ygo.ApplyUpdate(d, ygo.EncodeStateAsUpdate(src)); err != nil {
			t.Fatal(err)
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		if err := ygo.ApplyUpdate(d, u); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
}
