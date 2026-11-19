package ygo_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/internal/encoding"
)

// pendingStateScenario is one entry of testdata/pending-state-fixtures.json
// (testdata/gen/gen-pending-state.mjs, yjs 13.6.33).
type pendingStateScenario struct {
	Name    string   `json:"name"`
	GC      bool     `json:"gc"`
	Updates []string `json:"updates"`
	V1      string   `json:"v1"`
	V2      string   `json:"v2"`
	Diffs   []struct {
		SV string `json:"sv"`
		V1 string `json:"v1"`
		V2 string `json:"v2"`
	} `json:"diffs"`
}

// TestPendingState_Fixtures: a document holding updates that wait for what
// they depend on writes them into its state, whole and as diffs against a
// state vector, V1 and V2, as yjs encodeStateAsUpdate appends its pending
// structs and pending delete set. ygo left them out, so a peer synced from
// it never got what it held but could not integrate yet.
func TestPendingState_Fixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/pending-state-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Scenarios []pendingStateScenario `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Scenarios) == 0 {
		t.Fatal("no scenarios")
	}
	for _, s := range f.Scenarios {
		t.Run(s.Name, func(t *testing.T) {
			d := ygo.NewDocWithOptions(ygo.Options{ClientID: 99, DisableGC: !s.GC})
			for _, u := range s.Updates {
				b, _ := hex.DecodeString(u)
				if err := ygo.ApplyUpdate(d, b); err != nil {
					t.Fatal(err)
				}
			}
			if !ygo.HasPending(d) {
				t.Fatal("nothing pending")
			}
			if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != s.V1 {
				t.Errorf("state V1:\n got %s\nwant %s", got, s.V1)
			}
			if got := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(d)); got != s.V2 {
				t.Errorf("state V2:\n got %s\nwant %s", got, s.V2)
			}
			for _, df := range s.Diffs {
				sv, _ := hex.DecodeString(df.SV)
				g1, err := ygo.EncodeDiff(d, sv)
				if err != nil {
					t.Fatal(err)
				}
				g2, err := ygo.EncodeDiffV2(d, sv)
				if err != nil {
					t.Fatal(err)
				}
				if got := hex.EncodeToString(g1); got != df.V1 {
					t.Errorf("diff V1 against %s:\n got %s\nwant %s", df.SV, got, df.V1)
				}
				if got := hex.EncodeToString(g2); got != df.V2 {
					t.Errorf("diff V2 against %s:\n got %s\nwant %s", df.SV, got, df.V2)
				}
			}
		})
	}
}

// TestPendingState_SnapshotLeavesThePendingBufferOut: a snapshot holds
// what the document has integrated, as yjs snapshots do; restoring one of
// a document with updates waiting gives a document with nothing waiting,
// the store's state.
func TestPendingState_SnapshotLeavesThePendingBufferOut(t *testing.T) {
	raw, err := os.ReadFile("testdata/pending-state-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Scenarios []pendingStateScenario `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	for _, s := range f.Scenarios {
		if s.GC {
			continue // snapshots need GC off
		}
		d := ygo.NewDocWithOptions(ygo.Options{ClientID: 99, DisableGC: true})
		for _, u := range s.Updates {
			b, _ := hex.DecodeString(u)
			if err := ygo.ApplyUpdate(d, b); err != nil {
				t.Fatal(err)
			}
		}
		r, err := ygo.RestoreSnapshot(d, ygo.CreateSnapshot(d))
		if err != nil {
			t.Fatal(err)
		}
		if ygo.HasPending(r) {
			t.Errorf("%s: the restored document has updates waiting", s.Name)
		}
		if got, want := ygo.EncodeStateAsUpdate(r), encoding.EncodeStateAsUpdate(d); !bytes.Equal(got, want) {
			t.Errorf("%s: restored state %x, want the store's %x", s.Name, got, want)
		}
	}
}
