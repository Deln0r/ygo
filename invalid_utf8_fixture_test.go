package ygo_test

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Deln0r/ygo"
)

// TestInvalidUTF8_Fixtures replays testdata/invalid-utf8-fixtures.json
// (gen-invalid-utf8.mjs): bytes yjs wrote with a marker string at each place
// a string can sit, and the same bytes with the marker replaced by invalid
// UTF-8, which yjs, lib0 and y-protocols reject. ygo must reject every
// patched case with ErrInvalidUTF8 and accept every original, so a case that
// fails for another reason cannot pass as a rejection.
func TestInvalidUTF8_Fixtures(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "invalid-utf8-fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Cases []struct {
			Name       string `json:"name"`
			Kind       string `json:"kind"`
			Bytes      string `json:"bytes"`
			JSRejects  bool   `json:"js_rejects"`
			YgoError   string `json:"ygo_error"`
			ExpectText *struct {
				Root string `json:"root"`
				Text string `json:"text"`
			} `json:"expect_text"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) == 0 {
		t.Fatal("no cases; the fixture file is empty or its shape changed")
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			raw, err := hex.DecodeString(c.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			var errs []error
			d := ygo.NewDoc()
			switch c.Kind {
			case "update-v1":
				errs = append(errs, ygo.ApplyUpdate(d, raw), ygo.ValidateUpdate(raw))
				_, merr := ygo.MergeUpdates([][]byte{raw})
				errs = append(errs, merr)
			case "update-v2":
				errs = append(errs, ygo.ApplyUpdateV2(d, raw))
				_, merr := ygo.MergeUpdatesV2([][]byte{raw})
				errs = append(errs, merr)
			case "awareness":
				_, aerr := ygo.NewAwareness(1).Apply(raw, nil)
				errs = append(errs, aerr)
			case "relpos":
				_, rerr := ygo.DecodeRelativePosition(raw)
				errs = append(errs, rerr)
			default:
				t.Fatalf("unknown kind %q", c.Kind)
			}
			for i, err := range errs {
				switch {
				case c.YgoError == "split-surrogate":
					// yjs accepts these bytes; ygo refuses them on purpose
					// (see ErrSplitSurrogate).
					if !errors.Is(err, ygo.ErrSplitSurrogate) {
						t.Errorf("call %d: ygo returned %v, want ErrSplitSurrogate", i, err)
					}
				case c.YgoError != "":
					t.Fatalf("unknown ygo_error %q", c.YgoError)
				case c.JSRejects && !errors.Is(err, ygo.ErrInvalidUTF8):
					t.Errorf("call %d: yjs rejects these bytes; ygo returned %v, want ErrInvalidUTF8", i, err)
				case !c.JSRejects && err != nil:
					t.Errorf("call %d: yjs accepts these bytes; ygo returned %v", i, err)
				}
			}
			if c.ExpectText != nil {
				if got := ygo.NewText(d, c.ExpectText.Root).String(); got != c.ExpectText.Text {
					t.Errorf("text %q = %q, want %q (what yjs reads)", c.ExpectText.Root, got, c.ExpectText.Text)
				}
			}
		})
	}
	var originals int
	for _, c := range f.Cases {
		if !c.JSRejects {
			originals++
		}
		if c.JSRejects && !strings.Contains(c.Name, "/") {
			t.Errorf("%s: case name without a site", c.Name)
		}
	}
	if originals == 0 {
		t.Error("no original (accepted) cases: the positive control is missing")
	}
}
