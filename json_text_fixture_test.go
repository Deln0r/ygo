package ygo_test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/Deln0r/ygo"
)

// jsonTextScenario is one entry of testdata/json-text-fixtures.json
// (testdata/gen/gen-json-text.mjs, yjs 13.6.33).
type jsonTextScenario struct {
	Name     string          `json:"name"`
	Kind     string          `json:"kind"`
	Key      string          `json:"key"`
	Value    json.RawMessage `json:"value"`
	ReplayV2 bool            `json:"replayV2"`
	V1       string          `json:"v1"`
	V2       string          `json:"v2"`
}

type jsonTextCase struct {
	Raw  string `json:"raw"`
	Want string `json:"want"`
	V1   string `json:"v1"`
	V2   string `json:"v2"`
}

func loadJSONTextFixtures(t *testing.T) ([]jsonTextScenario, []jsonTextCase) {
	t.Helper()
	raw, err := os.ReadFile("testdata/json-text-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Scenarios []jsonTextScenario `json:"scenarios"`
		Texts     []jsonTextCase     `json:"texts"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Scenarios) == 0 || len(f.Texts) == 0 {
		t.Fatal("no scenarios")
	}
	return f.Scenarios, f.Texts
}

// untagJSONText is untag plus the generator's tag for a Uint8Array.
func untagJSONText(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if m, ok := v.(map[string]any); ok && m["$"] == "bytes" {
		var out []byte
		for _, b := range m["v"].([]any) {
			out = append(out, byte(b.(float64)))
		}
		return out
	}
	return untagValue(t, v)
}

func buildJSONTextDoc(t *testing.T, s jsonTextScenario, value any) *ygo.Doc {
	t.Helper()
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	var m *ygo.Map
	var x *ygo.Text
	if s.Kind == "map" {
		m = ygo.NewMap(d, "m")
	} else {
		x = ygo.NewText(d, "t")
	}
	txn := d.WriteTxn()
	switch s.Kind {
	case "map":
		m.Set(txn, s.Key, value)
	case "embed":
		if err := x.InsertEmbed(txn, 0, value); err != nil {
			t.Fatal(err)
		}

	case "format":
		if err := x.Insert(txn, 0, "ab"); err != nil {
			t.Fatal(err)
		}
		if err := x.Format(txn, 0, 2, ygo.Attrs{s.Key: value}); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown kind %q", s.Kind)
	}
	txn.Commit()
	return d
}

// TestJSONText_Fixtures: ygo re-encodes a document yjs wrote to yjs's
// bytes, where JSON.stringify and encoding/json write different text for
// the same value (& < > U+2028 U+2029, NaN and Infinity next to other
// values, exponent forms, array-index keys first, a Uint8Array). A
// document read from V1 is compared in V1 only: JSON turned NaN into null
// and the bytes into an object there, in yjs as in ygo.
func TestJSONText_Fixtures(t *testing.T) {
	scenarios, _ := loadJSONTextFixtures(t)
	for _, s := range scenarios {
		t.Run(s.Name, func(t *testing.T) {
			d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
			if err := ygo.ApplyUpdate(d, mustHex(t, s.V1)); err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != s.V1 {
				t.Fatalf("applied V1, re-encoded V1:\n got %s\nwant %s", got, s.V1)
			}
			d = ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
			if err := ygo.ApplyUpdateV2(d, mustHex(t, s.V2)); err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != s.V1 {
				t.Fatalf("applied V2, re-encoded V1:\n got %s\nwant %s", got, s.V1)
			}
			if got := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(d)); got != s.V2 {
				t.Fatalf("applied V2, re-encoded V2:\n got %s\nwant %s", got, s.V2)
			}
		})
	}
}

// TestJSONText_WrittenFromGo: the same values set through ygo's API, with
// the same client ID, give yjs's bytes. V2 is skipped where the value holds
// a NaN, whose bits differ between Go and JavaScript.
func TestJSONText_WrittenFromGo(t *testing.T) {
	scenarios, _ := loadJSONTextFixtures(t)
	for _, s := range scenarios {
		t.Run(s.Name, func(t *testing.T) {
			d := buildJSONTextDoc(t, s, untagJSONText(t, s.Value))
			if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != s.V1 {
				t.Fatalf("V1:\n got %s\nwant %s", got, s.V1)
			}
			if !s.ReplayV2 {
				return
			}
			if got := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(d)); got != s.V2 {
				t.Fatalf("V2:\n got %s\nwant %s", got, s.V2)
			}
		})
	}
}

// TestJSONText_RawMessage: an embed given as JSON text is written as yjs
// writes JSON.parse of that text, in both formats: JSON.stringify's text
// in V1 (array-index keys first, a repeated key once with its last value,
// escapes undone except where JSON.stringify writes them, lone surrogates
// kept, numbers in JavaScript's form), and the same object in V2.
func TestJSONText_RawMessage(t *testing.T) {
	_, texts := loadJSONTextFixtures(t)
	for _, c := range texts {
		t.Run(c.Raw, func(t *testing.T) {
			d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
			x := ygo.NewText(d, "t")
			txn := d.WriteTxn()
			if err := x.InsertEmbed(txn, 0, json.RawMessage(c.Raw)); err != nil {
				t.Fatal(err)
			}
			txn.Commit()
			if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != c.V1 {
				t.Fatalf("V1 (want text %s):\n got %s\nwant %s", c.Want, got, c.V1)
			}
			if got := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(d)); got != c.V2 {
				t.Fatalf("V2:\n got %s\nwant %s", got, c.V2)
			}
		})
	}
}
