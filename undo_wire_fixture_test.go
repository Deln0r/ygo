package ygo_test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/Deln0r/ygo"
)

// undoWireScenario is one entry of testdata/undo-wire-fixtures.json
// (testdata/gen/gen-undo-wire.mjs, yjs 13.6.33).
type undoWireScenario struct {
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Updates []string `json:"updates"`
	Steps   []struct {
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
// step, V1 and V2. An update delivered twice after an undo cuts the item
// its origin points into, as yjs Item.getMissing does for every update it
// reads, and the pieces an undo brought back stay apart; ygo skipped the
// cut for a block it already had, and kept one item where yjs holds two.
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
		t.Run(s.Name, func(t *testing.T) {
			d := ygo.NewDocWithOptions(ygo.Options{ClientID: 99})
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
					u, _ := hex.DecodeString(s.Updates[st.I])
					if err := ygo.ApplyUpdateWithOrigin(d, u, "remote"); err != nil {
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
		})
	}
}
