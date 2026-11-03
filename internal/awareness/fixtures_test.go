package awareness

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

// awarenessFixture mirrors the shape of testdata/awareness-fixtures.json
// emitted by testdata/gen/gen-awareness.mjs.
type awarenessFixture struct {
	Description    string         `json:"description"`
	JSClientID     uint64         `json:"js_client_id"`
	UpdateHex      string         `json:"update_hex"`
	ExpectedStates map[string]any `json:"expected_states"`
}

func loadAwarenessFixtures(t *testing.T) []awarenessFixture {
	t.Helper()
	candidates := []string{
		"../../testdata/awareness-fixtures.json",
		"../../../testdata/awareness-fixtures.json",
	}
	var raw []byte
	var err error
	for _, p := range candidates {
		raw, err = os.ReadFile(p)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("read awareness-fixtures.json from any of %v: %v", candidates, err)
	}
	var fixtures []awarenessFixture
	if err := json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatalf("parse awareness-fixtures.json: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatal("no fixtures loaded")
	}
	return fixtures
}

// TestFixtures_DecodeApplyJSAwarenessUpdates is the cross-language
// proof: every bytes-snapshot produced by y-protocols/awareness in
// gen-awareness.mjs must decode + apply on the Go side and yield
// the same state map.
//
// State equality is compared by JSON-Unmarshal of each expected
// JSON value vs our stored raw JSON — bytes-level equality would
// be fragile (JS may reorder keys) but JSON-decoded equality is
// the semantic invariant.
func TestFixtures_DecodeApplyJSAwarenessUpdates(t *testing.T) {
	fixtures := loadAwarenessFixtures(t)
	for _, fix := range fixtures {
		fix := fix
		t.Run(fix.Description, func(t *testing.T) {
			bytes, err := hex.DecodeString(fix.UpdateHex)
			if err != nil {
				t.Fatalf("hex decode: %v", err)
			}

			// Apply to a fresh Awareness (local clientID 999 — not
			// referenced in any fixture so self-eviction defense
			// stays out of the way).
			aw := New(999)
			if _, err := aw.Apply(bytes, "test"); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			gotStates := aw.States()

			// Verify every expected client appears with matching
			// JSON content. nil-valued expected entries denote
			// removed clients (state cleared, clock retained) —
			// they should NOT appear in States().
			for k, expectedVal := range fix.ExpectedStates {
				clientID, err := strconv.ParseUint(k, 10, 64)
				if err != nil {
					t.Fatalf("non-integer client key %q: %v", k, err)
				}

				if expectedVal == nil {
					if _, present := gotStates[clientID]; present {
						t.Errorf("client %d expected removed but present", clientID)
					}
					if _, _, ok := aw.Meta(clientID); !ok {
						t.Errorf("client %d expected to have meta retained", clientID)
					}
					continue
				}

				got, present := gotStates[clientID]
				if !present {
					t.Errorf("client %d missing from States(); got %v", clientID, gotStates)
					continue
				}
				var gotDecoded any
				if err := json.Unmarshal(got, &gotDecoded); err != nil {
					t.Errorf("client %d state %q not valid JSON: %v", clientID, got, err)
					continue
				}
				if !reflect.DeepEqual(gotDecoded, expectedVal) {
					t.Errorf("client %d state mismatch:\n got  %+v\n want %+v",
						clientID, gotDecoded, expectedVal)
				}
			}

			// Verify there are no extra clients in States beyond
			// the expected set.
			liveExpected := 0
			for _, v := range fix.ExpectedStates {
				if v != nil {
					liveExpected++
				}
			}
			if len(gotStates) != liveExpected {
				t.Errorf("States count = %d, want %d (expected live: %d, total in fixture: %d)",
					len(gotStates), liveExpected, liveExpected, len(fix.ExpectedStates))
			}
		})
	}
}

// TestFixtures_EncodeMatchesJSStructurally is the reverse-direction
// half: take the same expected state, build it locally, encode via
// our wire format, then decode-and-compare structurally against
// the JS bytes. Byte-level equality is NOT enforced (HashMap
// iteration order is unspecified — see awareness.md gotcha 7); we
// compare per-client (clock, json) tuples instead.
func TestFixtures_EncodeMatchesJSStructurally(t *testing.T) {
	fixtures := loadAwarenessFixtures(t)
	for _, fix := range fixtures {
		fix := fix
		// Skip fixtures we cannot reproduce with a single
		// setLocalState call. Multi-client requires injecting a
		// peer's state via Apply (we'd then be testing decode
		// again). Removal-only requires SetLocalState+RemoveLocalState
		// to match the JS clock-bump-twice pattern — the decode
		// test already covers this fixture's wire bytes.
		if len(fix.ExpectedStates) != 1 {
			continue
		}
		hasLiveState := false
		for _, v := range fix.ExpectedStates {
			if v != nil {
				hasLiveState = true
				break
			}
		}
		if !hasLiveState {
			continue
		}
		t.Run(fix.Description, func(t *testing.T) {
			jsBytes, err := hex.DecodeString(fix.UpdateHex)
			if err != nil {
				t.Fatalf("hex: %v", err)
			}
			// Build local awareness mirroring the fixture's state.
			aw := New(fix.JSClientID)
			for k, v := range fix.ExpectedStates {
				if v == nil {
					continue
				}
				_ = k
				payload, _ := json.Marshal(v)
				aw.SetLocalState(payload)
			}
			ourBytes := aw.Encode(nil)

			// Decode both and compare per-client (clock, json).
			ours, _, err := decodeUpdate(ourBytes)
			if err != nil {
				t.Fatalf("decode ours: %v", err)
			}
			theirs, _, err := decodeUpdate(jsBytes)
			if err != nil {
				t.Fatalf("decode JS: %v", err)
			}
			if len(ours) != len(theirs) {
				t.Errorf("entry count differs: ours=%d theirs=%d", len(ours), len(theirs))
				return
			}
			ourMap := map[uint64]wireEntry{}
			for _, e := range ours {
				ourMap[e.ClientID] = e
			}
			for _, e := range theirs {
				gotOur, ok := ourMap[e.ClientID]
				if !ok {
					t.Errorf("client %d in JS but not ours", e.ClientID)
					continue
				}
				// Decode both JSON payloads and compare structurally.
				var ourJSON, theirJSON any
				if err := json.Unmarshal(gotOur.JSON, &ourJSON); err != nil {
					t.Errorf("client %d ours JSON invalid: %v", e.ClientID, err)
					continue
				}
				if err := json.Unmarshal(e.JSON, &theirJSON); err != nil {
					t.Errorf("client %d theirs JSON invalid: %v", e.ClientID, err)
					continue
				}
				if !reflect.DeepEqual(ourJSON, theirJSON) {
					t.Errorf("client %d JSON mismatch:\n ours   %+v\n theirs %+v",
						e.ClientID, ourJSON, theirJSON)
				}
				// The clock too: a first state at clock 0 was dropped by
				// every y-protocols receiver, and this comparison, which
				// said it checked the clock, did not.
				if gotOur.Clock != e.Clock {
					t.Errorf("client %d clock: ours %d, theirs %d", e.ClientID, gotOur.Clock, e.Clock)
				}
			}
		})
	}
}

// guard against test data getting out of sync between gen and Go
// without anyone noticing.
func TestFixtures_AwarenessFileExists(t *testing.T) {
	for _, p := range []string{
		"../../testdata/awareness-fixtures.json",
		"../../../testdata/awareness-fixtures.json",
	} {
		if _, err := os.Stat(filepath.Clean(p)); err == nil {
			return
		}
	}
	t.Error("awareness-fixtures.json missing — run testdata/gen/gen-awareness.mjs")
}
