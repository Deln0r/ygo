package ygo_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Deln0r/ygo"
)

// updateUtilityFixtures is testdata/update-utility-fixtures.json
// (gen-update-utilities.mjs): yjs's conversion, state vector and meta of
// every input and merge of the merge fixtures (derived) and of updates
// written by hand (scenarios).
type updateUtilityFixtures struct {
	Derived []struct {
		Scenario int    `json:"scenario"`
		Slot     string `json:"slot"`
		utilityResults
	} `json:"derived"`
	Scenarios []struct {
		Name  string `json:"name"`
		Input string `json:"input"`
		// AppliedSV is the state vector of a document yjs applied the
		// input to (only for some scenarios).
		AppliedSV string `json:"applied_sv"`
		utilityResults
	} `json:"scenarios"`
}

type utilityResults struct {
	V2      bool       `json:"v2"`
	Convert yjsResult  `json:"convert"`
	SV      yjsResult  `json:"sv"`
	Meta    yjsMetaOut `json:"meta"`
}

type yjsResult struct {
	Out string `json:"out"`
	Err string `json:"err"`
}

type yjsMetaOut struct {
	From [][2]uint64 `json:"from"`
	To   [][2]uint64 `json:"to"`
	Err  string      `json:"err"`
}

// TestUpdateUtilities_Fixtures: ConvertUpdateFormatV1ToV2 / V2ToV1,
// EncodeStateVectorFromUpdate(V2) and ParseUpdateMeta(V2) give yjs's bytes
// and maps, and fail where yjs throws, but for the documented difference: a
// V2 format value that is undefined converts to V1 as null (yjs throws).
func TestUpdateUtilities_Fixtures(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "update-utility-fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx updateUtilityFixtures
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	mraw, err := os.ReadFile(filepath.Join("testdata", "merge-update-fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var merges mergeUpdateFixtures
	if err := json.Unmarshal(mraw, &merges); err != nil {
		t.Fatal(err)
	}
	if len(fx.Derived) == 0 || len(fx.Scenarios) == 0 {
		t.Fatal("empty fixtures")
	}
	for _, d := range fx.Derived {
		sc := merges.Scenarios[d.Scenario]
		var input string
		switch slot := d.Slot; {
		case slot == "merge_v1":
			input = sc.MergeV1
		case slot == "merge_v2":
			input = sc.MergeV2
		case strings.HasPrefix(slot, "input_v1/"):
			k, _ := strconv.Atoi(strings.TrimPrefix(slot, "input_v1/"))
			input = sc.InputsV1[k]
		case strings.HasPrefix(slot, "input_v2/"):
			k, _ := strconv.Atoi(strings.TrimPrefix(slot, "input_v2/"))
			input = sc.InputsV2[k]
		default:
			t.Fatalf("slot %q", slot)
		}
		checkUtilities(t, sc.Name+" "+d.Slot, mustHex(t, input), d.utilityResults, strings.Contains(sc.Name, "undefined"))
	}
	for _, sc := range fx.Scenarios {
		checkUtilities(t, sc.Name, mustHex(t, sc.Input), sc.utilityResults, false)
		if sc.AppliedSV == "" {
			continue
		}
		d := ygo.NewDoc()
		apply := ygo.ApplyUpdate
		if sc.V2 {
			apply = ygo.ApplyUpdateV2
		}
		if err := apply(d, mustHex(t, sc.Input)); err != nil {
			t.Errorf("%s: apply: %v", sc.Name, err)
		} else if got := ygo.EncodeStateVector(d); !bytes.Equal(got, mustHex(t, sc.AppliedSV)) {
			t.Errorf("%s: applied state vector %x, want %s", sc.Name, got, sc.AppliedSV)
		}
	}
}

func checkUtilities(t *testing.T, name string, input []byte, want utilityResults, undefinedValue bool) {
	t.Helper()
	label := name + " (V1)"
	convert, sv, meta := ygo.ConvertUpdateFormatV1ToV2, ygo.EncodeStateVectorFromUpdate, ygo.ParseUpdateMeta
	if want.V2 {
		label = name + " (V2)"
		convert, sv, meta = ygo.ConvertUpdateFormatV2ToV1, ygo.EncodeStateVectorFromUpdateV2, ygo.ParseUpdateMetaV2
	}
	got, err := convert(input)
	switch {
	case want.Convert.Err != "" && undefinedValue:
		// Documented: ygo writes null where yjs throws.
	case want.Convert.Err != "":
		if err == nil {
			t.Errorf("%s: conversion succeeded with %x; yjs throws %q", label, got, want.Convert.Err)
		}
	case err != nil:
		t.Errorf("%s: conversion: %v", label, err)
	case !bytes.Equal(got, mustHex(t, want.Convert.Out)):
		t.Errorf("%s: conversion\n got  %x\n want %s", label, got, want.Convert.Out)
	}
	gotSV, err := sv(input)
	switch {
	case want.SV.Err != "":
		if err == nil {
			t.Errorf("%s: state vector %x; yjs throws %q", label, gotSV, want.SV.Err)
		}
	case err != nil || !bytes.Equal(gotSV, mustHex(t, want.SV.Out)):
		t.Errorf("%s: state vector %x, %v; want %s", label, gotSV, err, want.SV.Out)
	}
	m, err := meta(input)
	if want.Meta.Err != "" {
		if err == nil {
			t.Errorf("%s: meta %v; yjs throws %q", label, m, want.Meta.Err)
		}
		return
	}
	if err != nil {
		t.Errorf("%s: meta: %v", label, err)
		return
	}
	pairs := func(mm map[uint64]uint64) [][2]uint64 {
		out := [][2]uint64{}
		for c, clock := range mm {
			out = append(out, [2]uint64{c, clock})
		}
		sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
		return out
	}
	wantFrom, wantTo := want.Meta.From, want.Meta.To
	if wantFrom == nil {
		wantFrom = [][2]uint64{}
	}
	if wantTo == nil {
		wantTo = [][2]uint64{}
	}
	if gf, gt := pairs(m.From), pairs(m.To); !equalPairs(gf, wantFrom) || !equalPairs(gt, wantTo) {
		t.Errorf("%s: meta from %v to %v; want from %v to %v", label, gf, gt, wantFrom, wantTo)
	}
}

func equalPairs(a, b [][2]uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
