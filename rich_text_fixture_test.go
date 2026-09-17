package ygo_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Deln0r/ygo"
)

// The fixtures in testdata/rich-text-fixtures.json record what yjs 13.6.32
// does with formatted text: the same insert, embed, format, delete and
// applyDelta calls, and the delta that comes out. They pin behaviour, not
// bytes - two implementations can agree on every attribute a reader sees and
// still write a different number of format markers.

type richTextFixtureFile struct {
	Generator string             `json:"generator"`
	Scenarios []richTextScenario `json:"scenarios"`
}

type richTextScenario struct {
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	Txns           [][]richTextOp  `json:"txns"`
	ExpectedDelta  json.RawMessage `json:"expected_delta"`
	ExpectedString string          `json:"expected_string"`
	ExpectedLength uint64          `json:"expected_length"`
	ExpectedEvents json.RawMessage `json:"expected_events"`
}

// richTextOp is one yjs call. Attrs is nil when the call had no attributes
// argument and an empty map when it had {} - yjs treats those differently.
type richTextOp struct {
	Op     string           `json:"op"`
	Index  uint64           `json:"index"`
	Length uint64           `json:"length"`
	Text   string           `json:"text"`
	Embed  any              `json:"embed"`
	Attrs  map[string]any   `json:"attrs"`
	Delta  []map[string]any `json:"delta"`
}

// knownRichTextDivergences lists the scenarios where ygo does not yet do what
// yjs does. Each entry pins the exact difference the test prints today, so a
// regression that makes the result wrong in another way fails instead of
// hiding behind the entry. The test also fails when a listed scenario starts
// to match, so an entry is removed by the change that fixes it and the list
// only shrinks.
type knownRichTextDivergence struct {
	reason string
	result string
}

var knownRichTextDivergences = map[string]knownRichTextDivergence{
	"insert-partial-attrs-inside-run": {
		reason: "InsertWithAttributes keeps inherited attributes it was not given",
		result: ` delta
  ygo: [{"attributes":{"bold":true},"insert":"he"},{"attributes":{"bold":true,"italic":true},"insert":"X"},{"attributes":{"bold":true},"insert":"llo"}]
  yjs: [{"attributes":{"bold":true},"insert":"he"},{"attributes":{"italic":true},"insert":"X"},{"attributes":{"bold":true},"insert":"llo"}]
 events
  ygo: [[{"attributes":{"bold":true},"insert":"he"},{"attributes":{"bold":true,"italic":true},"insert":"X"},{"attributes":{"bold":true},"insert":"llo"}]]
  yjs: [[{"attributes":{"bold":true},"insert":"he"},{"attributes":{"italic":true},"insert":"X"},{"attributes":{"bold":true},"insert":"llo"}]]
`,
	},
	"insert-empty-attrs-inside-run": {
		reason: "InsertWithAttributes with an empty map behaves like Insert",
		result: ` delta
  ygo: [{"attributes":{"bold":true},"insert":"heXllo"}]
  yjs: [{"attributes":{"bold":true},"insert":"he"},{"insert":"X"},{"attributes":{"bold":true},"insert":"llo"}]
 events
  ygo: [[{"attributes":{"bold":true},"insert":"heXllo"}]]
  yjs: [[{"attributes":{"bold":true},"insert":"he"},{"insert":"X"},{"attributes":{"bold":true},"insert":"llo"}]]
`,
	},
	"insert-plain-at-run-end": {
		reason: "Insert steps over the closing marker at the index instead of continuing the run",
		result: ` delta
  ygo: [{"attributes":{"bold":true},"insert":"hello"},{"insert":"!"}]
  yjs: [{"attributes":{"bold":true},"insert":"hello!"}]
 events
  ygo: [[{"attributes":{"bold":true},"insert":"hello"},{"insert":"!"}]]
  yjs: [[{"attributes":{"bold":true},"insert":"hello!"}]]
`,
	},
	"insert-plain-at-run-start": {
		reason: "Insert steps over the opening marker at the index and joins the run",
		result: ` delta
  ygo: [{"attributes":{"bold":true},"insert":"^hello"}]
  yjs: [{"insert":"^"},{"attributes":{"bold":true},"insert":"hello"}]
 events
  ygo: [[{"attributes":{"bold":true},"insert":"^hello"}]]
  yjs: [[{"insert":"^"},{"attributes":{"bold":true},"insert":"hello"}]]
`,
	},
	"insert-plain-at-run-end-next-transaction": {
		reason: "Insert steps over the closing marker at the index instead of continuing the run",
		result: ` delta
  ygo: [{"attributes":{"bold":true},"insert":"hello"},{"insert":"!"}]
  yjs: [{"attributes":{"bold":true},"insert":"hello!"}]
 events
  ygo: [[{"attributes":{"bold":true},"insert":"hello"}],[{"retain":5},{"insert":"!"}]]
  yjs: [[{"attributes":{"bold":true},"insert":"hello"}],[{"retain":5},{"attributes":{"bold":true},"insert":"!"}]]
`,
	},
	"embed-inside-run": {
		reason: "InsertEmbed inherits the formatting at the index; yjs inserts it unformatted",
		result: ` delta
  ygo: [{"attributes":{"bold":true},"insert":"he"},{"attributes":{"bold":true},"insert":{"image":"a"}},{"attributes":{"bold":true},"insert":"llo"}]
  yjs: [{"attributes":{"bold":true},"insert":"he"},{"insert":{"image":"a"}},{"attributes":{"bold":true},"insert":"llo"}]
 events
  ygo: [[{"attributes":{"bold":true},"insert":"he"},{"attributes":{"bold":true},"insert":{"image":"a"}},{"attributes":{"bold":true},"insert":"llo"}]]
  yjs: [[{"attributes":{"bold":true},"insert":"he"},{"insert":{"image":"a"}},{"attributes":{"bold":true},"insert":"llo"}]]
`,
	},
	"delete-across-boundary-then-insert": {
		reason: "Delete leaves format markers that no longer mark anything (yjs cleanupFormattingGap removes them), so the event splits the insert",
		result: ` events
  ygo: [[{"insert":"a"},{"insert":"Zf"}]]
  yjs: [[{"insert":"aZf"}]]
`,
	},
	"delete-whole-run": {
		reason: "Delete leaves format markers that no longer mark anything (yjs cleanupFormattingGap removes them), so the event splits the insert",
		result: ` events
  ygo: [[{"insert":"ab"},{"insert":"ef"}]]
  yjs: [[{"insert":"abef"}]]
`,
	},
	"delta-insert-partial-attrs": {
		reason: "ApplyDelta insert keeps inherited attributes it was not given",
		result: ` delta
  ygo: [{"attributes":{"bold":true},"insert":"he"},{"attributes":{"bold":true,"italic":true},"insert":"Z"},{"attributes":{"bold":true},"insert":"llo"}]
  yjs: [{"attributes":{"bold":true},"insert":"he"},{"attributes":{"italic":true},"insert":"Z"},{"attributes":{"bold":true},"insert":"llo"}]
 events
  ygo: [[{"attributes":{"bold":true},"insert":"he"},{"attributes":{"bold":true,"italic":true},"insert":"Z"},{"attributes":{"bold":true},"insert":"llo"}]]
  yjs: [[{"attributes":{"bold":true},"insert":"he"},{"attributes":{"italic":true},"insert":"Z"},{"attributes":{"bold":true},"insert":"llo"}]]
`,
	},
	"delta-insert-plain-inside-run": {
		reason: "ApplyDelta insert without attributes inherits the formatting at the cursor",
		result: ` delta
  ygo: [{"attributes":{"bold":true},"insert":"heWllo"}]
  yjs: [{"attributes":{"bold":true},"insert":"he"},{"insert":"W"},{"attributes":{"bold":true},"insert":"llo"}]
 events
  ygo: [[{"attributes":{"bold":true},"insert":"heWllo"}]]
  yjs: [[{"attributes":{"bold":true},"insert":"he"},{"insert":"W"},{"attributes":{"bold":true},"insert":"llo"}]]
`,
	},
}

func TestRichText_Fixtures(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "rich-text-fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f richTextFixtureFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Scenarios) == 0 {
		t.Fatal("no rich-text scenarios; the fixture file is empty or its shape changed")
	}

	seen := map[string]bool{}
	for _, sc := range f.Scenarios {
		seen[sc.Name] = true
		t.Run(sc.Name, func(t *testing.T) {
			mismatch, err := replayRichText(t, sc)
			known, isKnown := knownRichTextDivergences[sc.Name]
			switch {
			case err != nil:
				t.Errorf("%s: %v", sc.Description, err)
			case mismatch == "" && isKnown:
				t.Errorf("now matches yjs; remove it from knownRichTextDivergences (listed as: %s)", known.reason)
			case mismatch != "" && !isKnown:
				t.Errorf("%s\n%s", sc.Description, mismatch)
			case mismatch != known.result:
				t.Errorf("known divergence (%s) now diverges differently; a different wrong result is not the same known divergence\ngot:\n%s\npinned:\n%s", known.reason, mismatch, known.result)
			}
		})
	}
	for name := range knownRichTextDivergences {
		if !seen[name] {
			t.Errorf("knownRichTextDivergences names %q, which is not a scenario", name)
		}
	}
}

// replayRichText runs the scenario's calls through ygo and returns "" when
// the result matches yjs, or a description of the difference. A call ygo
// refuses is returned as an error, never as a difference.
func replayRichText(t *testing.T, sc richTextScenario) (string, error) {
	t.Helper()
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	txt := ygo.NewText(d, "t")
	var events []any
	txt.Observe(func(e *ygo.TextEvent) { events = append(events, eventDeltaJSON(e.Delta)) })
	for i, ops := range sc.Txns {
		txn := d.WriteTxn()
		for j, op := range ops {
			if err := applyRichTextOp(txt, txn, op); err != nil {
				txn.Commit()
				return "", fmt.Errorf("transaction %d op %d (%s): %w", i, j, op.Op, err)
			}
		}
		txn.Commit()
	}

	var want any
	if err := json.Unmarshal(sc.ExpectedDelta, &want); err != nil {
		t.Fatal(err)
	}
	got := deltaJSON(t, d, "t")
	want = normalizeJSON(t, want)

	rt := d.ReadTxn()
	gotString, gotLength := txt.String(), txt.Length()
	rt.Close()

	var diffs string
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		diffs += fmt.Sprintf(" delta\n  ygo: %s\n  yjs: %s\n", gotJSON, wantJSON)
	}
	if gotString != sc.ExpectedString {
		diffs += fmt.Sprintf(" string: ygo %q, yjs %q\n", gotString, sc.ExpectedString)
	}
	if gotLength != sc.ExpectedLength {
		diffs += fmt.Sprintf(" length: ygo %d, yjs %d\n", gotLength, sc.ExpectedLength)
	}
	var wantEvents any
	if err := json.Unmarshal(sc.ExpectedEvents, &wantEvents); err != nil {
		t.Fatal(err)
	}
	if gotEvents, wantEvents := normalizeJSON(t, events), normalizeJSON(t, wantEvents); !reflect.DeepEqual(gotEvents, wantEvents) {
		gotJSON, _ := json.Marshal(gotEvents)
		wantJSON, _ := json.Marshal(wantEvents)
		diffs += fmt.Sprintf(" events\n  ygo: %s\n  yjs: %s\n", gotJSON, wantJSON)
	}
	return diffs, nil
}

// eventDeltaJSON renders an event delta the way yjs serialises one.
func eventDeltaJSON(ops []ygo.DeltaOp) []any {
	out := []any{}
	for _, op := range ops {
		m := map[string]any{}
		switch {
		case op.Embed != nil:
			m["insert"] = op.Embed
		case op.Insert != "":
			m["insert"] = op.Insert
		case op.Retain > 0:
			m["retain"] = op.Retain
		case op.Delete > 0:
			m["delete"] = op.Delete
		}
		if len(op.Attributes) > 0 {
			m["attributes"] = map[string]any(op.Attributes)
		}
		out = append(out, m)
	}
	return out
}

func applyRichTextOp(txt *ygo.Text, txn *ygo.TransactionMut, op richTextOp) error {
	switch op.Op {
	case "insert":
		if op.Attrs == nil {
			return txt.Insert(txn, op.Index, op.Text)
		}
		return txt.InsertWithAttributes(txn, op.Index, op.Text, ygo.Attrs(op.Attrs))
	case "embed":
		if op.Attrs != nil {
			return fmt.Errorf("ygo has no call that inserts an embed with attributes")
		}
		return txt.InsertEmbed(txn, op.Index, op.Embed)
	case "format":
		return txt.Format(txn, op.Index, op.Length, ygo.Attrs(op.Attrs))
	case "delete":
		return txt.Delete(txn, op.Index, op.Length)
	case "delta":
		ops := make([]ygo.DeltaOp, 0, len(op.Delta))
		for _, raw := range op.Delta {
			ops = append(ops, deltaOpFromJSON(raw))
		}
		return txt.ApplyDelta(txn, ops)
	default:
		return fmt.Errorf("unknown op %q", op.Op)
	}
}

// deltaOpFromJSON converts one Quill delta op as JSON decodes it.
func deltaOpFromJSON(raw map[string]any) ygo.DeltaOp {
	var op ygo.DeltaOp
	switch v := raw["insert"].(type) {
	case string:
		op.Insert = v
	case nil:
	default:
		op.Embed = v
	}
	if n, ok := raw["retain"].(float64); ok {
		op.Retain = uint64(n)
	}
	if n, ok := raw["delete"].(float64); ok {
		op.Delete = uint64(n)
	}
	if attrs, ok := raw["attributes"].(map[string]any); ok {
		op.Attributes = ygo.Attrs(attrs)
	}
	return op
}
