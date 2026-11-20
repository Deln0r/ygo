package ygo_test

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/Deln0r/ygo"
)

// formatUndefinedScenario is one entry of
// testdata/format-undefined-fixtures.json
// (testdata/gen/gen-format-undefined.mjs, yjs 13.6.33).
type formatUndefinedScenario struct {
	Name     string            `json:"name"`
	Setup    []string          `json:"setup"`
	Txns     []json.RawMessage `json:"txns"`
	Undo     bool              `json:"undo"`
	Writer   formatUndefinedSide
	Receiver formatUndefinedSide
}

type formatUndefinedSide struct {
	Events  []any    `json:"events"`
	Updates []string `json:"updates"`
	State   string   `json:"state"`
	Delta   any      `json:"delta"`
	String  string   `json:"string"`
	V1      *string  `json:"v1"`
}

// TestFormatUndefined_Fixtures: a format attribute whose value is undefined
// itself is a marker of its own, as in yjs, and the formatting code gives it
// yjs's meaning at each place it reads a value. The writer replays the
// scenario's calls through ygo's API with the writer's client ID and must
// emit yjs's V2 updates and event deltas and end with yjs's state and
// delta; the receiver applies the V2 updates yjs emitted and must end the
// same way, with the same events and its own cleanup updates.
func TestFormatUndefined_Fixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/format-undefined-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Scenarios []formatUndefinedScenario `json:"scenarios"`
		Snapshots []struct {
			Name   string             `json:"name"`
			Before [][]map[string]any `json:"before"`
			After  [][]map[string]any `json:"after"`
			State  string             `json:"state"`
			Delta  any                `json:"delta"`
		} `json:"snapshots"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Scenarios) == 0 || len(f.Snapshots) == 0 {
		t.Fatal("no scenarios or no snapshots")
	}
	for _, s := range f.Scenarios {
		t.Run(s.Name+"/writer", func(t *testing.T) { replayFormatUndefinedWriter(t, s) })
		t.Run(s.Name+"/receiver", func(t *testing.T) { replayFormatUndefinedReceiver(t, s) })
	}
	// A snapshot is restored through V2, as yjs createDocFromSnapshot does,
	// so an undefined marker survives it.
	for _, s := range f.Snapshots {
		t.Run(s.Name+"/snapshot", func(t *testing.T) {
			d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1, DisableGC: true})
			txt := ygo.NewText(d, "t")
			run := func(txns [][]map[string]any) {
				for i, ops := range txns {
					w := d.WriteTxn()
					for j, op := range ops {
						if err := applyFormatUndefinedOp(txt, w, op); err != nil {
							w.Commit()
							t.Fatalf("transaction %d op %d: %v", i, j, err)
						}
					}
					w.Commit()
				}
			}
			run(s.Before)
			snap := ygo.CreateSnapshot(d)
			run(s.After)
			r, err := ygo.RestoreSnapshot(d, snap)
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(r)); got != s.State {
				t.Errorf("restored V2 state:\n got %s\nwant %s", got, s.State)
			}
			if g, w := normalize(t, markDelta(ygo.NewText(r, "t").ToDelta())), normalize(t, s.Delta); !reflect.DeepEqual(g, w) {
				gj, _ := json.Marshal(g)
				wj, _ := json.Marshal(w)
				t.Errorf("restored delta:\n got %s\nwant %s", gj, wj)
			}
		})
	}
}

func replayFormatUndefinedWriter(t *testing.T, s formatUndefinedScenario) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	txt := ygo.NewText(d, "t")
	var events []any
	var updates []string
	txt.Observe(func(e *ygo.TextEvent) { events = append(events, markDelta(e.Delta)) })
	d.OnUpdateV2(func(u []byte, _ any) { updates = append(updates, hex.EncodeToString(u)) })
	var um *ygo.UndoManager
	if s.Undo {
		um = ygo.NewUndoManagerWithOptions(d, ygo.UndoManagerOptions{CaptureTimeout: -1}, txt)
	}
	for _, h := range s.Setup {
		b, _ := hex.DecodeString(h)
		if err := ygo.ApplyUpdateV2(d, b); err != nil {
			t.Fatal(err)
		}
	}
	for i, rawTxn := range s.Txns {
		var cmd string
		if json.Unmarshal(rawTxn, &cmd) == nil {
			switch cmd {
			case "undo":
				um.Undo()
			case "redo":
				um.Redo()
			default:
				t.Fatalf("transaction %d: %q", i, cmd)
			}
			continue
		}
		var ops []map[string]any
		if err := json.Unmarshal(rawTxn, &ops); err != nil {
			t.Fatal(err)
		}
		w := d.WriteTxn()
		for j, op := range ops {
			if err := applyFormatUndefinedOp(txt, w, op); err != nil {
				w.Commit()
				t.Fatalf("transaction %d op %d: %v", i, j, err)
			}
		}
		w.Commit()
	}
	compareFormatUndefinedSide(t, d, txt, s.Writer, events, updates)
	if s.Writer.V1 != nil {
		if got := hex.EncodeToString(ygo.EncodeStateAsUpdate(d)); got != *s.Writer.V1 {
			t.Errorf("V1 state:\n got %s\nwant %s", got, *s.Writer.V1)
		}
	}
}

func replayFormatUndefinedReceiver(t *testing.T, s formatUndefinedScenario) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 99})
	txt := ygo.NewText(d, "t")
	var events []any
	var updates []string
	txt.Observe(func(e *ygo.TextEvent) { events = append(events, markDelta(e.Delta)) })
	d.OnUpdateV2(func(u []byte, _ any) { updates = append(updates, hex.EncodeToString(u)) })
	for _, h := range s.Writer.Updates {
		b, _ := hex.DecodeString(h)
		if err := ygo.ApplyUpdateV2(d, b); err != nil {
			t.Fatal(err)
		}
	}
	compareFormatUndefinedSide(t, d, txt, s.Receiver, events, updates)
}

func compareFormatUndefinedSide(t *testing.T, d *ygo.Doc, txt *ygo.Text, want formatUndefinedSide, events []any, updates []string) {
	t.Helper()
	if !slices.Equal(updates, want.Updates) {
		t.Errorf("V2 update events:\n got %v\nwant %v", updates, want.Updates)
	}
	if len(events) != len(want.Events) {
		t.Errorf("%d text events, want %d", len(events), len(want.Events))
	} else {
		for i := range events {
			w := want.Events[i]
			if m, ok := w.(map[string]any); ok && m["$throws"] == true {
				// yjs's delta getter throws on an undefined embed; ygo emits
				// the delta, which the generator records as expect.
				w = m["expect"]
			}
			if g, w := normalize(t, events[i]), normalize(t, w); !reflect.DeepEqual(g, w) {
				gj, _ := json.Marshal(g)
				wj, _ := json.Marshal(w)
				t.Errorf("event %d delta:\n got %s\nwant %s", i, gj, wj)
			}
		}
	}
	if got := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(d)); got != want.State {
		t.Errorf("V2 state:\n got %s\nwant %s", got, want.State)
	}
	if g, w := normalize(t, markDelta(txt.ToDelta())), normalize(t, want.Delta); !reflect.DeepEqual(g, w) {
		gj, _ := json.Marshal(g)
		wj, _ := json.Marshal(w)
		t.Errorf("delta:\n got %s\nwant %s", gj, wj)
	}
	r := d.ReadTxn()
	s := txt.String()
	r.Close()
	if s != want.String {
		t.Errorf("string %q, want %q", s, want.String)
	}
}

// reviveUndefined turns {"$undefined": true} into ygo.Undefined{}.
func reviveUndefined(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 1 && x["$undefined"] == true {
			return ygo.Undefined{}
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = reviveUndefined(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = reviveUndefined(e)
		}
		return out
	}
	return v
}

// markUndefined is the reverse, for comparison with the fixture.
func markUndefined(v any) any {
	switch x := v.(type) {
	case ygo.Undefined:
		return map[string]any{"$undefined": true}
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = markUndefined(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = markUndefined(e)
		}
		return out
	}
	return v
}

func markDelta(ops []ygo.DeltaOp) []any {
	out := []any{}
	for _, op := range ops {
		m := map[string]any{}
		switch {
		case op.Embed != nil:
			m["insert"] = markUndefined(op.Embed)
		case op.Insert != "":
			m["insert"] = op.Insert
		case op.Retain > 0:
			m["retain"] = op.Retain
		case op.Delete > 0:
			m["delete"] = op.Delete
		}
		if len(op.Attributes) > 0 {
			m["attributes"] = markUndefined(map[string]any(op.Attributes))
		}
		out = append(out, m)
	}
	return out
}

func normalize(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func applyFormatUndefinedOp(txt *ygo.Text, txn *ygo.TransactionMut, op map[string]any) error {
	num := func(k string) uint64 { n, _ := op[k].(float64); return uint64(n) }
	attrs := func() (ygo.Attrs, bool) {
		a, ok := op["attrs"]
		if !ok {
			return nil, false
		}
		m, _ := reviveUndefined(a).(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		return ygo.Attrs(m), true
	}
	switch op["op"] {
	case "insert":
		text, _ := op["text"].(string)
		if a, ok := attrs(); ok {
			return txt.InsertWithAttributes(txn, num("index"), text, a)
		}
		return txt.Insert(txn, num("index"), text)
	case "embed":
		e := reviveUndefined(op["embed"])
		if a, ok := attrs(); ok {
			return txt.InsertEmbedWithAttributes(txn, num("index"), e, a)
		}
		return txt.InsertEmbed(txn, num("index"), e)
	case "format":
		a, _ := attrs()
		return txt.Format(txn, num("index"), num("length"), a)
	case "delete":
		return txt.Delete(txn, num("index"), num("length"))
	case "delta":
		list, _ := op["delta"].([]any)
		ops := make([]ygo.DeltaOp, 0, len(list))
		for _, raw := range list {
			m, _ := reviveUndefined(raw).(map[string]any)
			var d ygo.DeltaOp
			switch v := m["insert"].(type) {
			case string:
				d.Insert = v
			case nil:
			default:
				d.Embed = v
			}
			if n, ok := m["retain"].(float64); ok {
				d.Retain = uint64(n)
			}
			if n, ok := m["delete"].(float64); ok {
				d.Delete = uint64(n)
			}
			if a, ok := m["attributes"].(map[string]any); ok {
				d.Attributes = ygo.Attrs(a)
			}
			ops = append(ops, d)
		}
		return txt.ApplyDelta(txn, ops)
	}
	return fmt.Errorf("unknown op %v", op["op"])
}
