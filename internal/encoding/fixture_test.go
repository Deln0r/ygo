package encoding

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/types"
)

// fixtureFile mirrors the JSON shape emitted by testdata/gen/gen-yjs-update.mjs.
type fixtureFile struct {
	Generator string            `json:"generator"`
	Scenarios []fixtureScenario `json:"scenarios"`
}

type fixtureScenario struct {
	Description    string                 `json:"description"`
	JsClientID     uint64                 `json:"js_client_id"`
	RootKind       string                 `json:"root_kind"` // "map" / "array" / "text"; defaults to "map" for back-compat
	RootName       string                 `json:"root_name"`
	UpdateHex      string                 `json:"update_hex"`
	ExpectedMap    map[string]interface{} `json:"expected_map,omitempty"`
	ExpectedArray  []interface{}          `json:"expected_array,omitempty"`
	ExpectedText   string                 `json:"expected_text,omitempty"`
	ExpectedLength uint64                 `json:"expected_length,omitempty"`
	// ExpectedDelta is the JS Y.Text.toDelta() output, for scenarios where the
	// flat string cannot tell right from wrong: a decoder that dropped every
	// attribute would still match ExpectedText and ExpectedLength.
	ExpectedDelta []any `json:"expected_delta,omitempty"`
}

// TestFixtures_DecodeApplyJSYjsUpdates is the binary-protocol-compat
// proof. For each scenario captured by gen-yjs-update.mjs we:
//  1. Decode the JS-emitted update bytes via our DecodeUpdate.
//  2. Apply to a fresh Doc + Map via our Update.Apply.
//  3. Read every expected key back via our Map.Get.
//
// If everything matches we have proven that bytes JS Yjs produces
// are bytes our pipeline correctly consumes — direction one of the
// prime directive. Direction two (Go encodes, JS decodes) is the
// follow-on test below if/when we wire a Node subprocess into the
// Go test runner.
//
// Skipped if the fixture file is absent (fresh checkout without Node);
// CI regenerates via gen-yjs-update.mjs before running tests.
func TestFixtures_DecodeApplyJSYjsUpdates(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "yjs-updates.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("testdata/yjs-updates.json not present; run testdata/gen/gen-yjs-update.mjs to regenerate")
		}
		t.Fatalf("read fixture: %v", err)
	}

	var ff fixtureFile
	if err := json.Unmarshal(data, &ff); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	if len(ff.Scenarios) == 0 {
		t.Fatal("fixture file has no scenarios")
	}

	for _, sc := range ff.Scenarios {
		t.Run(sc.Description, func(t *testing.T) {
			updateBytes, err := hex.DecodeString(sc.UpdateHex)
			if err != nil {
				t.Fatalf("invalid hex: %v", err)
			}

			// Decode the JS-emitted update.
			u, tail, err := DecodeUpdate(updateBytes)
			if err != nil {
				t.Fatalf("DecodeUpdate failed: %v", err)
			}
			if len(tail) != 0 {
				t.Errorf("DecodeUpdate left %d trailing bytes: % x", len(tail), tail)
			}

			// Apply to a fresh Doc.
			d := doc.NewDoc()
			branch := d.Branch(sc.RootName)

			txn := d.WriteTxn()
			if err := u.Apply(txn); err != nil {
				t.Fatalf("Apply failed: %v", err)
			}
			txn.Commit()

			rootKind := sc.RootKind
			if rootKind == "" {
				rootKind = "map" // back-compat with older fixtures
			}
			switch rootKind {
			case "map":
				verifyMapScenario(t, types.NewMap(branch), sc.ExpectedMap)
			case "array":
				verifyArrayScenario(t, types.NewArray(branch), sc.ExpectedArray)
			case "text":
				verifyTextScenario(t, types.NewText(branch), sc.ExpectedText, sc.ExpectedLength)
				if sc.ExpectedDelta != nil {
					verifyTextDelta(t, types.NewText(branch), sc.ExpectedDelta)
				}
			default:
				t.Fatalf("unknown root_kind %q", rootKind)
			}
		})
	}
}

func verifyMapScenario(t *testing.T, m *types.Map, expected map[string]interface{}) {
	t.Helper()
	for key, exp := range expected {
		got := m.Get(key)
		if !valueEqual(got, exp) {
			t.Errorf("key %q: got %v (%T), want %v (%T)", key, got, got, exp, exp)
		}
	}
	gotKeys := map[string]struct{}{}
	m.Range(func(k string, _ any) bool {
		gotKeys[k] = struct{}{}
		return true
	})
	if len(gotKeys) != len(expected) {
		t.Errorf("Map.Range visited %d live keys; want %d. seen=%v expected=%v",
			len(gotKeys), len(expected), keysOf(gotKeys), keysOfStr(expected))
	}
	for k := range expected {
		if _, ok := gotKeys[k]; !ok {
			t.Errorf("expected key %q not visible in Map.Range", k)
		}
	}
}

func verifyArrayScenario(t *testing.T, a *types.Array, expected []interface{}) {
	t.Helper()
	gotLen := a.Len()
	if gotLen != uint64(len(expected)) {
		t.Errorf("Array.Len = %d, want %d", gotLen, len(expected))
	}
	got := a.ToSlice()
	if len(got) != len(expected) {
		t.Errorf("Array.ToSlice length = %d, want %d (got=%v expected=%v)",
			len(got), len(expected), got, expected)
		return
	}
	for i := range expected {
		if !valueEqual(got[i], expected[i]) {
			t.Errorf("Array[%d] = %v (%T), want %v (%T)", i, got[i], got[i], expected[i], expected[i])
		}
	}
}

func verifyTextScenario(t *testing.T, txt *types.Text, expectedStr string, expectedLength uint64) {
	t.Helper()
	if got := txt.String(); got != expectedStr {
		t.Errorf("Text.String() = %q, want %q", got, expectedStr)
	}
	if got := txt.Length(); got != expectedLength {
		t.Errorf("Text.Length() = %d, want %d (UTF-16 code units)", got, expectedLength)
	}
}

// verifyTextDelta compares Text.ToDelta against the delta JS Yjs produced for
// the same document, in the JS shape: [{insert, attributes?}].
//
// Both sides go through JSON before comparison, so a number is a number
// whichever codec decoded it (JSON gives float64, lib0 Any gives int64) and
// only structure and values are compared.
func verifyTextDelta(t *testing.T, txt *types.Text, expected []any) {
	t.Helper()
	ops := txt.ToDelta()
	got := make([]any, 0, len(ops))
	for _, op := range ops {
		m := map[string]any{}
		if op.Embed != nil {
			m["insert"] = op.Embed
		} else {
			m["insert"] = op.Insert
		}
		if len(op.Attributes) > 0 {
			m["attributes"] = map[string]any(op.Attributes)
		}
		got = append(got, m)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal delta: %v", err)
	}
	wantJSON, err := json.Marshal(expected)
	if err != nil {
		t.Fatalf("marshal expected delta: %v", err)
	}
	var gotNorm, wantNorm any
	if err := json.Unmarshal(gotJSON, &gotNorm); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wantJSON, &wantNorm); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotNorm, wantNorm) {
		t.Errorf("Text.ToDelta() = %s\n                  want %s", gotJSON, wantJSON)
	}
}

// valueEqual compares two values across the JSON / Go-Any boundary.
// JSON unmarshals integers as float64 by default; our DecodeAny
// produces int64 for tag 125 and float64 for tag 123. Compare
// numerically rather than by type when one side came from JSON.
func valueEqual(got, expected any) bool {
	if got == nil && expected == nil {
		return true
	}
	if got == nil || expected == nil {
		return false
	}
	// Numeric cross-type comparison.
	gotF, gotIsNum := toFloat(got)
	expF, expIsNum := toFloat(expected)
	if gotIsNum && expIsNum {
		return gotF == expF
	}
	return got == expected
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float32:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

func keysOf(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keysOfStr(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
