// gen-go-fixtures generates reverse-direction wire-format fixtures:
// Go encodes Doc state via EncodeStateAsUpdate (V1) and
// EncodeStateAsUpdateV2, captures the bytes + expected state, and
// writes them as JSON. The companion Node validator reads the
// JSON, applies the bytes via Y.applyUpdate / Y.applyUpdateV2, and
// asserts the JS-side state matches what Go wrote.
//
// This closes the binary-protocol-compat loop: pre-existing
// JS→Go fixtures (testdata/yjs-updates.json,
// yjs-update-v2-fixtures.json) prove Go can read what JS writes;
// these fixtures prove JS can read what Go writes.
//
// Output: testdata/go-updates.json, testdata/go-update-v2-fixtures.json
//
// Run: go run ./cmd/gen-go-fixtures
//
// Determinism: every scenario constructs the Doc with a pinned
// ClientID via NewDocWithOptions so bytes are reproducible across
// re-runs (CI's git-diff drift check depends on byte-level
// stability).
package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Deln0r/ygo"
)

// encoder selects between V1 and V2 wire formats.
type encoder int

const (
	encV1 encoder = iota
	encV2
)

// must wraps an error-returning mutation. Fixture generation is a
// build-time tool — any failure here means a bug in the generator,
// not a runtime concern, so panic fast.
func must(err error) {
	if err != nil {
		panic(err)
	}
}

func (e encoder) encode(d *ygo.Doc) []byte {
	if e == encV1 {
		return ygo.EncodeStateAsUpdate(d)
	}
	return ygo.EncodeStateAsUpdateV2(d)
}

// scenario is the on-disk shape — mirrors fixture_test.go's
// fixtureScenario so the JS-side validator can use the same field
// names without translation.
type scenario struct {
	Description    string                 `json:"description"`
	GoClientID     uint64                 `json:"go_client_id"`
	RootKind       string                 `json:"root_kind"`
	RootName       string                 `json:"root_name"`
	UpdateHex      string                 `json:"update_hex"`
	ExpectedMap    map[string]interface{} `json:"expected_map,omitempty"`
	ExpectedArray  []interface{}          `json:"expected_array,omitempty"`
	ExpectedText   string                 `json:"expected_text,omitempty"`
	ExpectedLength uint64                 `json:"expected_length,omitempty"`
	// ExpectedXmlChildren lists each XmlFragment child as
	// {kind:"element"|"text", name:"div", text:"hello"}. Only
	// shallow children — nested element trees out of scope for
	// MVP cross-language validation.
	ExpectedXmlChildren []map[string]interface{} `json:"expected_xml_children,omitempty"`
	// FollowUpHex is a second update applied after UpdateHex before the
	// expectation is checked. It exists for one thing: an update that
	// deliberately carries blocks the receiver cannot integrate yet, where
	// the point being proved is that the far side SURVIVES until the rest of
	// the history arrives.
	FollowUpHex string `json:"follow_up_hex,omitempty"`
}

type fixtureFile struct {
	Generator string     `json:"generator"`
	Scenarios []scenario `json:"scenarios"`
}

// captureMap mutates a Doc's Map under a pinned clientID and
// records the wire bytes + the live state as the expected JSON.
// Mutation closure is responsible for adding entries in
// deterministic order — Map iteration in Go is random, so the
// expected_map is recorded by querying explicit keys not via Range.
func captureMap(enc encoder, description, rootName string, clientID uint64, keys []string, mutate func(*ygo.Map, *ygo.TransactionMut)) scenario {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: clientID})
	m := ygo.NewMap(d, rootName)
	txn := d.WriteTxn()
	mutate(m, txn)
	txn.Commit()

	bytes := enc.encode(d)
	expected := map[string]interface{}{}
	for _, k := range keys {
		v := m.Get(k)
		if v == nil && !m.Has(k) {
			continue
		}
		expected[k] = v
	}
	return scenario{
		Description: description,
		GoClientID:  clientID,
		RootKind:    "map",
		RootName:    rootName,
		UpdateHex:   hex.EncodeToString(bytes),
		ExpectedMap: expected,
	}
}

func captureArray(enc encoder, description, rootName string, clientID uint64, mutate func(*ygo.Array, *ygo.TransactionMut)) scenario {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: clientID})
	a := ygo.NewArray(d, rootName)
	txn := d.WriteTxn()
	mutate(a, txn)
	txn.Commit()

	bytes := enc.encode(d)
	return scenario{
		Description:   description,
		GoClientID:    clientID,
		RootKind:      "array",
		RootName:      rootName,
		UpdateHex:     hex.EncodeToString(bytes),
		ExpectedArray: a.ToSlice(),
	}
}

func captureText(enc encoder, description, rootName string, clientID uint64, mutate func(*ygo.Text, *ygo.TransactionMut)) scenario {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: clientID})
	t := ygo.NewText(d, rootName)
	txn := d.WriteTxn()
	mutate(t, txn)
	txn.Commit()

	bytes := enc.encode(d)
	return scenario{
		Description:    description,
		GoClientID:     clientID,
		RootKind:       "text",
		RootName:       rootName,
		UpdateHex:      hex.EncodeToString(bytes),
		ExpectedText:   t.String(),
		ExpectedLength: t.Length(),
	}
}

// captureXml records a shallow XmlFragment snapshot. We list
// children as {kind:"element"|"text", name:?, text:?} so the
// validator can verify both child kinds without instantiating
// a parallel type system.
func captureXml(enc encoder, description, rootName string, clientID uint64, mutate func(*ygo.XmlFragment, *ygo.TransactionMut)) scenario {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: clientID})
	f := ygo.NewXmlFragment(d, rootName)
	txn := d.WriteTxn()
	mutate(f, txn)
	txn.Commit()

	bytes := enc.encode(d)

	var children []map[string]interface{}
	f.Range(func(_ uint64, child any) bool {
		switch c := child.(type) {
		case *ygo.XmlElement:
			children = append(children, map[string]interface{}{
				"kind": "element",
				"name": c.NodeName(),
			})
		case *ygo.XmlText:
			children = append(children, map[string]interface{}{
				"kind": "text",
				"text": c.String(),
			})
		}
		return true
	})
	return scenario{
		Description:         description,
		GoClientID:          clientID,
		RootKind:            "xml",
		RootName:            rootName,
		UpdateHex:           hex.EncodeToString(bytes),
		ExpectedXmlChildren: children,
	}
}

// captureAll runs the same scenario list for both encoders so V1
// and V2 fixtures cover identical surface area.
// captureMergedWithPending records what MergeUpdates emits when the set it is
// given spans a hole, so the reference implementation gets to say whether our
// output is readable.
//
// This is the cross-language half of the merge-preserves-pending work. The
// merged update contains a Skip run standing in for clocks nobody supplied;
// yjs must read it, integrate the near side, hold the far side, and converge
// once the rest of the history arrives. Two scenarios, because one update
// cannot assert both halves: the first checks the near side landed, the second
// applies the remaining history and checks the far side was kept rather than
// dropped at merge time.
//
// Separate roots on purpose - a single Text run squashes into one cell at
// commit and a diff would then carry the whole run, leaving no hole to test.
func captureMergedWithPending(enc encoder) []scenario {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 900})

	a := ygo.NewText(d, "A")
	txn := d.WriteTxn()
	must(a.Insert(txn, 0, "aaaaa"))
	txn.Commit()
	first := enc.encode(d)
	svAfterA := ygo.EncodeStateVector(d)

	b := ygo.NewText(d, "B")
	txn = d.WriteTxn()
	must(b.Insert(txn, 0, "bbbbb"))
	txn.Commit()
	svAfterB := ygo.EncodeStateVector(d)

	// The filler is captured HERE, before the third root exists, so it holds
	// the hole and NOTHING ELSE. Taking it later would also carry the far
	// side, and the second scenario below would then pass whether or not the
	// merge preserved anything - which is exactly what it did on the first
	// attempt, caught by deliberately removing the feature and watching the
	// fixtures stay green.
	var filler []byte
	var err error
	if enc == encV1 {
		filler, err = ygo.EncodeDiff(d, svAfterA)
	} else {
		filler, err = ygo.EncodeDiffV2(d, svAfterA)
	}
	must(err)

	c := ygo.NewText(d, "C")
	txn = d.WriteTxn()
	must(c.Insert(txn, 0, "ccccc"))
	txn.Commit()

	var late, merged []byte
	if enc == encV1 {
		late, err = ygo.EncodeDiff(d, svAfterB)
		must(err)
		merged, err = ygo.MergeUpdates([][]byte{first, late})
		must(err)
	} else {
		late, err = ygo.EncodeDiffV2(d, svAfterB)
		must(err)
		merged, err = ygo.MergeUpdatesV2([][]byte{first, late})
		must(err)
	}

	return []scenario{
		{
			Description:    "merged across a hole: the near side integrates",
			GoClientID:     900,
			RootKind:       "text",
			RootName:       "A",
			UpdateHex:      hex.EncodeToString(merged),
			ExpectedText:   "aaaaa",
			ExpectedLength: 5,
		},
		{
			Description:    "merged across a hole: the far side survives until the history arrives",
			GoClientID:     900,
			RootKind:       "text",
			RootName:       "C",
			UpdateHex:      hex.EncodeToString(merged),
			FollowUpHex:    hex.EncodeToString(filler),
			ExpectedText:   "ccccc",
			ExpectedLength: 5,
		},
	}
}

func captureAll(enc encoder) []scenario {
	return append(baseScenarios(enc), captureMergedWithPending(enc)...)
}

func baseScenarios(enc encoder) []scenario {
	return []scenario{
		// --- Map ---
		captureMap(enc, "empty Map", "x", 100, nil, func(_ *ygo.Map, _ *ygo.TransactionMut) {}),

		captureMap(enc, "single Map.Set string", "settings", 101,
			[]string{"color"},
			func(m *ygo.Map, txn *ygo.TransactionMut) {
				m.Set(txn, "color", "red")
			}),

		captureMap(enc, "Map across primitive value types", "x", 103,
			[]string{"s", "i", "f", "b_true", "b_false", "nullval"},
			func(m *ygo.Map, txn *ygo.TransactionMut) {
				m.Set(txn, "s", "hello")
				m.Set(txn, "i", int64(42))
				m.Set(txn, "f", 3.14)
				m.Set(txn, "b_true", true)
				m.Set(txn, "b_false", false)
				m.Set(txn, "nullval", nil)
			}),

		captureMap(enc, "Map LWW chain on same key", "x", 104,
			[]string{"k"},
			func(m *ygo.Map, txn *ygo.TransactionMut) {
				m.Set(txn, "k", "first")
				m.Set(txn, "k", "second")
				m.Set(txn, "k", "third")
			}),

		captureMap(enc, "Map.Set + Map.Delete", "x", 105,
			[]string{"a", "b"},
			func(m *ygo.Map, txn *ygo.TransactionMut) {
				m.Set(txn, "a", "alpha")
				m.Set(txn, "b", "beta")
				m.Delete(txn, "a")
			}),

		captureMap(enc, "Map with unicode keys and values", "x", 107,
			[]string{"ключ", "emoji"},
			func(m *ygo.Map, txn *ygo.TransactionMut) {
				m.Set(txn, "ключ", "значение")
				m.Set(txn, "emoji", "ok")
			}),

		// --- Array ---
		captureArray(enc, "empty Array", "x", 200, func(_ *ygo.Array, _ *ygo.TransactionMut) {}),

		captureArray(enc, "Array.Push single", "x", 201, func(a *ygo.Array, txn *ygo.TransactionMut) {
			a.Push(txn, "only")
		}),

		captureArray(enc, "Array.Push batch packed", "x", 202, func(a *ygo.Array, txn *ygo.TransactionMut) {
			a.InsertRange(txn, 0, []any{"a", "b", "c", "d", "e"})
		}),

		captureArray(enc, "Array with various value types", "x", 203, func(a *ygo.Array, txn *ygo.TransactionMut) {
			a.InsertRange(txn, 0, []any{"s", int64(42), 3.14, true, false, nil})
		}),

		captureArray(enc, "Array insert in middle (split)", "x", 204, func(a *ygo.Array, txn *ygo.TransactionMut) {
			a.InsertRange(txn, 0, []any{"a", "b", "c", "d"})
			a.Insert(txn, 2, "X")
		}),

		captureArray(enc, "Array delete range across packed run", "x", 206, func(a *ygo.Array, txn *ygo.TransactionMut) {
			a.InsertRange(txn, 0, []any{"a", "b", "c", "d", "e", "f"})
			a.Delete(txn, 2, 3)
		}),

		// --- Text ---
		captureText(enc, "empty Text", "x", 300, func(_ *ygo.Text, _ *ygo.TransactionMut) {}),

		captureText(enc, "Text insert ASCII", "x", 301, func(t *ygo.Text, txn *ygo.TransactionMut) {
			must(t.Insert(txn, 0, "hello"))
		}),

		captureText(enc, "Text two inserts", "x", 302, func(t *ygo.Text, txn *ygo.TransactionMut) {
			must(t.Insert(txn, 0, "hello"))
			must(t.Insert(txn, 5, " world"))
		}),

		captureText(enc, "Text Cyrillic (BMP)", "x", 306, func(t *ygo.Text, txn *ygo.TransactionMut) {
			must(t.Insert(txn, 0, "привет"))
		}),

		captureText(enc, "Text emoji (non-BMP surrogate pair)", "x", 307, func(t *ygo.Text, txn *ygo.TransactionMut) {
			must(t.Insert(txn, 0, "a😀b"))
		}),

		captureText(enc, "Text delete range", "x", 305, func(t *ygo.Text, txn *ygo.TransactionMut) {
			must(t.Insert(txn, 0, "hello world"))
			must(t.Delete(txn, 5, 6))
		}),

		// --- XmlFragment ---
		captureXml(enc, "XmlFragment empty", "frag", 400, func(_ *ygo.XmlFragment, _ *ygo.TransactionMut) {}),

		captureXml(enc, "XmlFragment with two elements", "frag", 401, func(f *ygo.XmlFragment, txn *ygo.TransactionMut) {
			f.InsertXmlElement(txn, 0, "div")
			f.InsertXmlElement(txn, 1, "span")
		}),

		captureXml(enc, "XmlFragment with element + text child", "frag", 402, func(f *ygo.XmlFragment, txn *ygo.TransactionMut) {
			f.InsertXmlElement(txn, 0, "p")
			xt := f.InsertXmlText(txn, 1)
			must(xt.Insert(txn, 0, "hello"))
		}),

		// --- Edge scenarios (mirror V1/V2 forward set so reverse counts cross 100 too) ---
		captureMap(enc, "Map empty-string key", "x", 109,
			[]string{"", "nonempty"},
			func(m *ygo.Map, txn *ygo.TransactionMut) {
				m.Set(txn, "", "empty-key-value")
				m.Set(txn, "nonempty", "for contrast")
			}),

		captureArray(enc, "Array delete entire range", "x", 209, func(a *ygo.Array, txn *ygo.TransactionMut) {
			a.InsertRange(txn, 0, []any{"a", "b", "c"})
			a.Delete(txn, 0, 3)
		}),

		captureText(enc, "Text non-BMP combining + ZWJ family", "x", 310, func(t *ygo.Text, txn *ygo.TransactionMut) {
			must(t.Insert(txn, 0, "é🧑‍💻"))
		}),
	}
}

func writeFixture(path, generator string, scenarios []scenario) error {
	ff := fixtureFile{
		Generator: generator,
		Scenarios: scenarios,
	}
	data, err := json.MarshalIndent(ff, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func main() {
	// Resolve paths relative to module root by walking up from cwd.
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "getwd:", err)
		os.Exit(1)
	}
	// `go run ./cmd/gen-go-fixtures` runs from the module root, so
	// testdata is just under root. If invoked from a subdirectory,
	// the user can pass the root path as argv[1].
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	v1Path := filepath.Join(root, "testdata", "go-updates.json")
	v2Path := filepath.Join(root, "testdata", "go-update-v2-fixtures.json")

	v1 := captureAll(encV1)
	v2 := captureAll(encV2)

	if err := writeFixture(v1Path, "ygo (V1)", v1); err != nil {
		fmt.Fprintln(os.Stderr, "write V1:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %d V1 scenarios to %s\n", len(v1), v1Path)

	if err := writeFixture(v2Path, "ygo (V2)", v2); err != nil {
		fmt.Fprintln(os.Stderr, "write V2:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %d V2 scenarios to %s\n", len(v2), v2Path)
}
