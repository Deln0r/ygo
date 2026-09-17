package encoding

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/types"
)

// TestFixtures_DecodeApplyJSYjsV2Updates is the V2 binary-protocol-
// compat proof. For each scenario captured by gen-yjs-update-v2.mjs:
//  1. Decode JS-emitted V2 update bytes via DecodeUpdateV2.
//  2. Apply to a fresh Doc + branch via Update.Apply (shared with V1).
//  3. Verify every expected key/element/text via the typed accessors.
//
// Direction proven: JS Yjs → Go for V2 (mirrors the V1 fixture test).
// Reverse direction (Go encodes V2 → JS Y.applyUpdateV2 decodes) is
// tracked under "Reverse direction Go → JS" in tech-debt.md.
//
// Skipped if the fixture file is absent (fresh checkout without
// Node); CI regenerates via testdata/gen/gen-yjs-update-v2.mjs.
//
// Reuses fixtureFile / fixtureScenario from fixture_test.go.
func TestFixtures_DecodeApplyJSYjsV2Updates(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "yjs-update-v2-fixtures.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			t.Skip("testdata/yjs-update-v2-fixtures.json not present; run testdata/gen/gen-yjs-update-v2.mjs to regenerate")
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

			u, err := DecodeUpdateV2(updateBytes)
			if err != nil {
				t.Fatalf("DecodeUpdateV2 failed: %v", err)
			}

			d := doc.NewDoc()
			branch := d.Branch(sc.RootName)

			txn := d.WriteTxn()
			if err := u.Apply(txn); err != nil {
				t.Fatalf("Apply failed: %v", err)
			}
			txn.Commit()

			rootKind := sc.RootKind
			if rootKind == "" {
				rootKind = "map"
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
