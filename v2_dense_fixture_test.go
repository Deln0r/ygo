package ygo_test

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"runtime"
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
	if len(f.Scenarios) == 0 {
		t.Fatal("no scenarios")
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
