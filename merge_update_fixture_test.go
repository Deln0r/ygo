package ygo_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Deln0r/ygo"
)

// mergeUpdateFixtures is testdata/merge-update-fixtures.json, written by
// testdata/gen/gen-merge-updates.mjs: yjs's mergeUpdates / mergeUpdatesV2 of
// lists of updates and diffUpdate / diffUpdateV2 of the result and of each
// input.
type mergeUpdateFixtures struct {
	Generator string `json:"generator"`
	Scenarios []struct {
		Name     string   `json:"name"`
		InputsV1 []string `json:"inputs_v1"`
		InputsV2 []string `json:"inputs_v2"`
		MergeV1  string   `json:"merge_v1"`
		MergeV2  string   `json:"merge_v2"`
		Diffs    []struct {
			SV string `json:"sv"`
			V1 string `json:"v1"`
			V2 string `json:"v2"`
		} `json:"diffs"`
		InputDiffs []struct {
			SV string `json:"sv"`
			V1 string `json:"v1"`
			V2 string `json:"v2"`
		} `json:"input_diffs"`
		// ApplyV1 is yjs's state after applying the inputs (V1 or V2, the
		// same), for inputs not written as yjs writes them.
		ApplyV1 string `json:"apply_v1"`
	} `json:"scenarios"`
}

// TestMergeUpdates_Fixtures: ygo merges and diffs updates to yjs's bytes, V1
// and V2, merging the updates' structs without integrating them; where an
// input is not written as yjs writes it, applying it gives yjs's state too.
func TestMergeUpdates_Fixtures(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "merge-update-fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx mergeUpdateFixtures
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Scenarios) == 0 {
		t.Fatal("no scenarios")
	}
	hexes := func(t *testing.T, hs []string) [][]byte {
		out := make([][]byte, len(hs))
		for i, h := range hs {
			out[i] = mustHex(t, h)
		}
		return out
	}
	same := func(t *testing.T, what string, got []byte, err error, want string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if w := mustHex(t, want); !bytes.Equal(got, w) {
			t.Fatalf("%s:\n got  %x\n want %x", what, got, w)
		}
	}
	for _, sc := range fx.Scenarios {
		t.Run(sc.Name, func(t *testing.T) {
			if len(sc.InputsV1) > 0 { // a V2-only scenario has none
				m1, err := ygo.MergeUpdates(hexes(t, sc.InputsV1))
				same(t, "MergeUpdates", m1, err, sc.MergeV1)
			}
			if len(sc.InputsV2) > 0 { // a V1-only scenario has none
				m2, err := ygo.MergeUpdatesV2(hexes(t, sc.InputsV2))
				same(t, "MergeUpdatesV2", m2, err, sc.MergeV2)
			}
			for _, d := range sc.Diffs {
				if d.V1 != "" {
					got, err := ygo.DiffUpdate(mustHex(t, sc.MergeV1), mustHex(t, d.SV))
					same(t, "DiffUpdate of the merge against "+d.SV, got, err, d.V1)
				}
				if d.V2 != "" {
					got, err := ygo.DiffUpdateV2(mustHex(t, sc.MergeV2), mustHex(t, d.SV))
					same(t, "DiffUpdateV2 of the merge against "+d.SV, got, err, d.V2)
				}
			}
			for i, d := range sc.InputDiffs {
				got, err := ygo.DiffUpdate(mustHex(t, sc.InputsV1[i]), mustHex(t, d.SV))
				same(t, "DiffUpdate of an input", got, err, d.V1)
				if d.V2 != "" {
					got, err = ygo.DiffUpdateV2(mustHex(t, sc.InputsV2[i]), mustHex(t, d.SV))
					same(t, "DiffUpdateV2 of an input", got, err, d.V2)
				}
			}
			if sc.ApplyV1 != "" {
				d := ygo.NewDoc()
				for _, u := range sc.InputsV1 {
					if err := ygo.ApplyUpdate(d, mustHex(t, u)); err != nil {
						t.Fatalf("ApplyUpdate: %v", err)
					}
				}
				same(t, "the state after applying the inputs", ygo.EncodeStateAsUpdate(d), nil, sc.ApplyV1)
				d2 := ygo.NewDoc()
				for _, u := range sc.InputsV2 {
					if err := ygo.ApplyUpdateV2(d2, mustHex(t, u)); err != nil {
						t.Fatalf("ApplyUpdateV2: %v", err)
					}
				}
				same(t, "the state after applying the V2 inputs", ygo.EncodeStateAsUpdate(d2), nil, sc.ApplyV1)
			}
		})
	}
}

// TestMergeUpdates_HeldTieSortingIsBounded: a GC and an item tied at
// client 1's clock 0 wait while 6,500 one-struct updates of higher clients
// merge, so every step sorts every input, as yjs's every step does; past 16
// comparisons a byte (at least 16,777,216) the merge stops with
// ErrUpdateTooDense. The fixtures hold the same shape with 300 inputs,
// merged as yjs merges it.
func TestMergeUpdates_HeldTieSortingIsBounded(t *testing.T) {
	var ups [][]byte
	for c := uint64(6501); c >= 2; c-- {
		u := []byte{1, 1}
		u = binary.AppendUvarint(u, c)
		ups = append(ups, append(u, 0, 0, 1, 0)) // clock 0, GC of 1, empty delete set
	}
	ups = append(ups,
		[]byte{1, 1, 1, 0, 0, 1, 0},                 // client 1: GC of 1 at 0
		[]byte{1, 1, 1, 0, 4, 1, 1, 't', 1, 'x', 0}, // client 1: "x" at 0 under root "t"
	)
	if _, err := ygo.MergeUpdates(ups); !errors.Is(err, ygo.ErrUpdateTooDense) || !strings.Contains(err.Error(), "tied at one clock") {
		t.Fatalf("err %v, want ErrUpdateTooDense", err)
	}
	if _, err := ygo.MergeUpdates(append(ups[6000:6500:6500], ups[6500:]...)); err != nil {
		t.Fatalf("500 inputs: %v", err)
	}
}
