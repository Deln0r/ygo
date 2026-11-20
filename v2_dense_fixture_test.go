package ygo_test

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/Deln0r/ygo"
)

// TestV2Dense_Fixtures: V2 run-length encodes most columns, so a valid
// update can hold more structs than bytes (10,000 empty nested arrays in 32
// bytes, a million deleted characters in 42). ygo reads them as yjs does and
// writes yjs's bytes back, up to a budget of max(bytes, 1<<20) structs per
// update; past it the update is refused with ErrUpdateTooDense, before any
// struct is decoded when the first client's count alone is past it.
func TestV2Dense_Fixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/v2-dense-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Queued struct {
			A                string `json:"a"`
			B                string `json:"b"`
			AMergedWithEmpty string `json:"aMergedWithEmpty"`
		} `json:"queued"`
		Pairs []struct {
			Name    string `json:"name"`
			A       string `json:"a"`
			B       string `json:"b"`
			Refused bool   `json:"refused"`
		} `json:"pairs"`
		Scenarios []struct {
			Name      string `json:"name"`
			Structs   int    `json:"structs"`
			Refused   bool   `json:"refused"`
			Early     bool   `json:"early"`
			State     string `json:"state"`
			RoundTrip string `json:"roundTrip"`
		} `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Scenarios) == 0 || len(f.Pairs) == 0 {
		t.Fatal("no scenarios or no pairs")
	}
	// The bounds past the floor are tested with a floor of 20,000
	// (MergeUpdatesV2Floor), on updates of 12,000 structs: the same code
	// as with 1,048,576, at a fiftieth of the cost (the race detector and
	// coverage make a million structs take minutes).
	const testFloor = 20000
	// Updates whose structs are all queued (each lacks its first): one
	// merged with an empty update gives yjs's bytes; two of 11,999 queued
	// structs each are past the bound together.
	t.Run("merge queued", func(t *testing.T) {
		a, _ := hex.DecodeString(f.Queued.A)
		b, _ := hex.DecodeString(f.Queued.B)
		empty := ygo.EncodeStateAsUpdateV2(ygo.NewDoc())
		merged, err := ygo.MergeUpdatesV2Floor([][]byte{a, empty}, testFloor)
		if err != nil {
			t.Fatalf("one queued update: %v", err)
		}
		if got := hex.EncodeToString(merged); got != f.Queued.AMergedWithEmpty {
			t.Fatalf("merged %d bytes, yjs %d", len(got)/2, len(f.Queued.AMergedWithEmpty)/2)
		}
		// Refused while they are held, before any output is written.
		if _, err := ygo.MergeUpdatesV2Floor([][]byte{a, b}, testFloor); !errors.Is(err, ygo.ErrUpdateTooDense) || !strings.Contains(err.Error(), "the inputs hold") {
			t.Fatalf("two queued updates: %v, want ErrUpdateTooDense from what the inputs hold", err)
		}
	})
	// MergeUpdatesV2 of two dense updates of different clients: the merge
	// applies as the two do, and holds no more than one update of both
	// inputs' bytes may (max(bytes, the floor) structs).
	for _, p := range f.Pairs {
		t.Run("merge "+p.Name, func(t *testing.T) {
			a, _ := hex.DecodeString(p.A)
			b, _ := hex.DecodeString(p.B)
			merged, err := ygo.MergeUpdatesV2([][]byte{a, b})
			if p.Refused {
				// Under the real floor the pair merges.
				if err != nil {
					t.Fatalf("merged under the floor of 1,048,576: %v", err)
				}
				merge := func(u ...[]byte) ([]byte, error) { return ygo.MergeUpdatesV2Floor(u, testFloor) }
				if _, err := merge(a, b); !errors.Is(err, ygo.ErrUpdateTooDense) {
					t.Fatalf("merged: %v, want ErrUpdateTooDense", err)
				}
				// The merge stops at the input that takes it past the
				// bound: a broken input after it is never read.
				if _, err := merge(a, b, []byte{0xff}); !errors.Is(err, ygo.ErrUpdateTooDense) {
					t.Fatalf("merged with a broken third input: %v, want ErrUpdateTooDense", err)
				}
				// Bytes that do not survive the merge (an input given
				// twice) raise the inputs' allowance, not the output's:
				// a merge ApplyUpdateV2 would refuse is refused.
				text := func(n int) []byte {
					d := ygo.NewDocWithOptions(ygo.Options{ClientID: 3})
					txt := ygo.NewText(d, "big")
					w := d.WriteTxn()
					if err := txt.Insert(w, 0, strings.Repeat("x", n)); err != nil {
						t.Fatal(err)
					}
					w.Commit()
					return ygo.EncodeStateAsUpdateV2(d)
				}
				c := text(14000)
				if _, err := merge(a, b, c, c); !errors.Is(err, ygo.ErrUpdateTooDense) || !strings.Contains(err.Error(), "the merged update") {
					t.Fatalf("a merge past its own budget: %v, want ErrUpdateTooDense from the merged update", err)
				}
				// Past the floor, bytes that do survive (a text of 26 KB)
				// carry the structs, as one update of them may.
				merged, err := merge(a, b, text(26000))
				if err != nil {
					t.Fatalf("a merge past the floor its bytes carry: %v", err)
				}
				m := ygo.NewDocWithOptions(ygo.Options{ClientID: 99})
				if err := ygo.ApplyUpdateV2(m, merged); err != nil {
					t.Fatalf("applying it: %v", err)
				}
				if got := ygo.NewText(m, "big").String(); len(got) != 26000 {
					t.Fatalf("the merged text has %d characters, want 26000", len(got))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			both := ygo.NewDocWithOptions(ygo.Options{ClientID: 99})
			for _, u := range [][]byte{a, b} {
				if err := ygo.ApplyUpdateV2(both, u); err != nil {
					t.Fatal(err)
				}
			}
			m := ygo.NewDocWithOptions(ygo.Options{ClientID: 99})
			if err := ygo.ApplyUpdateV2(m, merged); err != nil {
				t.Fatalf("applying the merge: %v", err)
			}
			if got, want := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(m)), hex.EncodeToString(ygo.EncodeStateAsUpdateV2(both)); got != want {
				t.Fatalf("the merge applies to %s, the two inputs to %s", got, want)
			}
		})
	}
	for _, s := range f.Scenarios {
		t.Run(s.Name, func(t *testing.T) {
			state, _ := hex.DecodeString(s.State)
			d := ygo.NewDocWithOptions(ygo.Options{ClientID: 99})
			// ValidateUpdate (V1) diagnoses V2 bytes it refuses; that must not
			// expand them (a network path calls it).
			var v0, v1 runtime.MemStats
			runtime.ReadMemStats(&v0)
			if err := ygo.ValidateUpdate(state); err == nil {
				t.Fatal("ValidateUpdate accepted V2 bytes")
			}
			runtime.ReadMemStats(&v1)
			if mb := (v1.TotalAlloc - v0.TotalAlloc) >> 20; mb > 4 {
				t.Fatalf("ValidateUpdate allocated %d MB", mb)
			}
			if s.Refused {
				var m0, m1 runtime.MemStats
				runtime.ReadMemStats(&m0)
				err := ygo.ApplyUpdateV2(d, state)
				runtime.ReadMemStats(&m1)
				if !errors.Is(err, ygo.ErrUpdateTooDense) {
					t.Fatalf("%d structs in %d bytes: err %v, want ErrUpdateTooDense", s.Structs, len(state), err)
				}
				// A million structs cost about 900 MB to decode; refusing on
				// the first count must cost next to nothing. A later
				// client's count comes after the clients before it, which
				// the budget bounds.
				if mb := (m1.TotalAlloc - m0.TotalAlloc) >> 20; s.Early && mb > 16 {
					t.Fatalf("refusing allocated %d MB", mb)
				}
				if _, err := ygo.MergeUpdatesV2([][]byte{state, state}); !errors.Is(err, ygo.ErrUpdateTooDense) {
					t.Fatalf("MergeUpdatesV2: %v, want ErrUpdateTooDense", err)
				}
				return
			}
			if err := ygo.ApplyUpdateV2(d, state); err != nil {
				t.Fatalf("%d structs in %d bytes: %v", s.Structs, len(state), err)
			}
			if got := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(d)); got != s.RoundTrip {
				t.Errorf("state written back:\n got %s\nwant %s", got, s.RoundTrip)
			}
			// The state merged with itself; past 200,000 structs that
			// costs minutes under the race detector and coverage, and
			// the merges past the floor are tested above.
			if s.Structs > 200000 {
				return
			}
			merged, err := ygo.MergeUpdatesV2([][]byte{state, state})
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(merged); got != s.RoundTrip {
				t.Errorf("MergeUpdatesV2 of the state twice:\n got %s\nwant %s", got, s.RoundTrip)
			}
		})
	}
}

// TestMergeUpdatesV2_ManyInputs: 100,000 updates, each of its own client
// and root type, merge in time linear in their number. Two things made it
// quadratic: the held bound counted the store per input (a state vector of
// every client so far), and every applied update reset the search markers
// of every type changed so far in the merge's transaction. No time is
// asserted here; a quadratic merge takes minutes.
func TestMergeUpdatesV2_ManyInputs(t *testing.T) {
	const n = 100000
	updates := make([][]byte, 0, n)
	for i := range n {
		d := ygo.NewDocWithOptions(ygo.Options{ClientID: uint64(i + 1)})
		txt := ygo.NewText(d, fmt.Sprintf("t%d", i))
		w := d.WriteTxn()
		if err := txt.Insert(w, 0, "x"); err != nil {
			t.Fatal(err)
		}
		w.Commit()
		updates = append(updates, ygo.EncodeStateAsUpdateV2(d))
	}
	merged, err := ygo.MergeUpdatesV2(updates)
	if err != nil {
		t.Fatal(err)
	}
	d := ygo.NewDoc()
	if err := ygo.ApplyUpdateV2(d, merged); err != nil {
		t.Fatal(err)
	}
	if got := ygo.NewText(d, fmt.Sprintf("t%d", n-1)).String(); got != "x" {
		t.Fatalf("the last root reads %q", got)
	}
}
