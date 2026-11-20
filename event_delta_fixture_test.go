package ygo_test

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/Deln0r/ygo"
)

type eventDeltaReceiver struct {
	Mode   string `json:"mode"`
	Events []struct {
		Update string `json:"update"`
		Local  bool   `json:"local"`
	} `json:"events"`
	Trace  []any `json:"trace"`
	Deltas []any `json:"deltas"`
	Stacks []struct {
		Step    string `json:"step"`
		Changed *bool  `json:"changed"`
		Undo    int    `json:"undo"`
		Redo    int    `json:"redo"`
		State   string `json:"state"`
	} `json:"stacks"`
	State string `json:"state"`
}

type eventDeltaScenario struct {
	Name      string               `json:"name"`
	Shape     string               `json:"shape"`
	Base      []map[string]any     `json:"base"`
	Peers     [][]map[string]any   `json:"peers"`
	Updates   []string             `json:"updates"`
	Receivers []eventDeltaReceiver `json:"receivers"`
}

// TestEventDelta_Fixtures: a receiver of concurrent formatting ends where
// a yjs 13.6.33 receiver set up the same way ends. An untyped root keeps
// every marker; a typed text gets the after-transaction cleanup, also
// when an observer never reads the delta; a text whose events are read
// (ReadDelta, from an observer of the text or a deep observer of its
// parent) loses the markers yjs's event delta getter deletes, in a local
// transaction of its own per event, before the cleanup's; under Undo and
// Redo those transactions go to the stacks yjs puts them on. Compared:
// every V2 update event and whether it is local, the after-transaction
// trace, every delta and changed key read, the stacks after each undo
// step, the final V2 state.
func TestEventDelta_Fixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/event-delta-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Scenarios []eventDeltaScenario `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Scenarios) == 0 {
		t.Fatal("no scenarios")
	}
	for _, s := range f.Scenarios {
		for _, want := range s.Receivers {
			t.Run(s.Name+"/"+want.Mode, func(t *testing.T) {
				replayEventDeltaReceiver(t, s, want)
			})
		}
	}
}

// keysRead is the fixture's form of a text event's changed keys: sorted
// [key, action, old value], an added key's old value undefined as in yjs.
func keysRead(keys map[string]ygo.KeyChange) []any {
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	out := []any{}
	for _, k := range names {
		c := keys[k]
		old := markUndefined(c.OldValue)
		if c.Action == "add" && c.OldValue == nil {
			old = map[string]any{"$undefined": true}
		}
		out = append(out, []any{k, c.Action, old})
	}
	return out
}

func replayEventDeltaReceiver(t *testing.T, s eventDeltaScenario, want eventDeltaReceiver) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 99})
	type event struct {
		update string
		local  bool
	}
	var events []event
	d.OnUpdateV2(func(u []byte, origin any) {
		events = append(events, event{hex.EncodeToString(u), origin != "remote"})
	})
	var um *ygo.UndoManager
	trace := []any{}
	d.OnAfterTransaction(func(tr *ygo.TransactionMut) {
		var origin any
		switch {
		case tr.Origin == nil:
		case tr.Origin == "remote":
			origin = "remote"
		case um != nil && tr.Origin == any(um):
			origin = "undo-manager"
		default:
			origin = "other"
		}
		deleted := uint64(0)
		for _, r := range tr.DeletedRanges() {
			deleted += r.Len
		}
		trace = append(trace, map[string]any{"origin": origin, "local": tr.Local, "deleted": deleted})
	})
	deltas := []any{}
	// readDelta is ReadDelta; read in the event's own callback it is the
	// Delta field (both are the getter's delta at that point).
	readDelta := func(e *ygo.TextEvent, own bool) []any {
		got := markDelta(e.ReadDelta())
		if g, w := normalize(t, markDelta(e.Delta)), normalize(t, got); own && !reflect.DeepEqual(g, w) {
			t.Errorf("Delta %v, ReadDelta %v", g, w)
		}
		return got
	}
	read := func(name string, e *ygo.TextEvent, own bool) {
		deltas = append(deltas, map[string]any{"text": name, "delta": readDelta(e, own), "keys": keysRead(e.Keys)})
	}
	reader := func(name string) func(*ygo.TextEvent) {
		return func(e *ygo.TextEvent) { read(name, e, true) }
	}
	// delayed: a remote transaction's event kept unread, and read from
	// the next callback of a transaction its commit runs after it. The
	// remote's after-transaction handler runs after its observers.
	remoteDone := false
	d.OnAfterTransaction(func(tr *ygo.TransactionMut) {
		if tr.Origin == "remote" {
			remoteDone = true
		}
	})
	observe := func(name string, txt *ygo.Text) {
		switch want.Mode {
		case "silent":
			txt.Observe(func(*ygo.TextEvent) {})
		case "observed", "undo":
			txt.Observe(reader(name))
		case "oneOfTwo":
			txt.Observe(func(*ygo.TextEvent) {})
			txt.Observe(reader(name))
		case "delayed":
			var pending *ygo.TextEvent
			txt.Observe(func(e *ygo.TextEvent) {
				if pending != nil && remoteDone {
					read(name, pending, false)
					pending = nil
				}
				if !remoteDone {
					pending = e
				}
			})
		case "delayedThenCurrent":
			// The older read's deletions change the newer delta, so the
			// newer one equals Delta only when no older read came first.
			var pending *ygo.TextEvent
			txt.Observe(func(e *ygo.TextEvent) {
				if !remoteDone {
					pending = e
					return
				}
				own := pending == nil
				if pending != nil {
					read(name, pending, false)
					pending = nil
				}
				read(name, e, own)
			})
		}
	}
	// Root texts, in the order the generator types them.
	var names []string
	for _, ops := range append([][]map[string]any{s.Base}, s.Peers...) {
		for _, op := range ops {
			name, _ := op["target"].(string)
			if name == "" {
				name = "t"
			}
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	slices.Sort(names)
	var m *ygo.Map
	var frag *ygo.XmlFragment
	texts := map[string]*ygo.Text{}
	if want.Mode != "untyped" {
		switch s.Shape {
		case "root":
			for _, n := range names {
				texts[n] = ygo.NewText(d, n)
				observe(n, texts[n])
			}
		case "map", "depth":
			m = ygo.NewMap(d, "m")
		case "xml":
			frag = ygo.NewXmlFragment(d, "x")
		}
	}
	if want.Mode == "deep" {
		m.ObserveDeep(func(evs []any) {
			for _, ev := range evs {
				if e, ok := ev.(*ygo.TextEvent); ok {
					deltas = append(deltas, map[string]any{"text": "", "path": append([]any{}, e.Path...), "delta": readDelta(e, true), "keys": keysRead(e.Keys)})
				}
			}
		})
	}
	if want.Mode == "undo" {
		um = ygo.NewUndoManagerWithOptions(d, ygo.UndoManagerOptions{CaptureTimeout: -1}, texts["t"])
	}
	for i, u := range s.Updates {
		b, err := hex.DecodeString(u)
		if err != nil {
			t.Fatal(err)
		}
		remoteDone = false
		if err := ygo.ApplyUpdateV2WithOrigin(d, b, "remote"); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
		// A nested text exists at the receiver once the base arrived.
		if i == 0 {
			switch s.Shape {
			case "map":
				observe("", m.Get("k").(*ygo.Text))
			case "xml":
				observe("", &frag.Get(0).(*ygo.XmlText).Text)
			}
		}
	}
	if um != nil {
		// The steps yjs took, then both stacks emptied a step at a time:
		// a step that changes nothing returns false (yjs: null) and ends
		// it, so the stacks' contents are compared step by step.
		for _, w := range want.Stacks {
			var changed bool
			switch strings.TrimSuffix(w.Step, " until empty") {
			case "undo":
				changed = um.Undo()
			case "redo":
				changed = um.Redo()
			}
			if w.Changed != nil && changed != *w.Changed {
				t.Errorf("%s: changed %v, yjs %v", w.Step, changed, *w.Changed)
			}
			if um.CanUndo() != (w.Undo > 0) || um.CanRedo() != (w.Redo > 0) {
				t.Errorf("after %s: CanUndo %v CanRedo %v, yjs's stacks hold %d and %d", w.Step, um.CanUndo(), um.CanRedo(), w.Undo, w.Redo)
			}
			if got := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(d)); got != w.State {
				t.Errorf("after %s, V2 state:\n got %s\nwant %s", w.Step, got, w.State)
			}
		}
	}
	wantEvents := make([]event, len(want.Events))
	for i, e := range want.Events {
		wantEvents[i] = event{e.Update, e.Local}
	}
	if !slices.Equal(events, wantEvents) {
		t.Errorf("update events:\n got %v\nwant %v", events, wantEvents)
	}
	for _, c := range []struct {
		what      string
		got, want any
	}{{"after-transaction trace", trace, want.Trace}, {"deltas read", deltas, want.Deltas}} {
		if g, w := normalize(t, c.got), normalize(t, c.want); !reflect.DeepEqual(g, w) {
			gj, _ := json.Marshal(g)
			wj, _ := json.Marshal(w)
			t.Errorf("%s:\n got %s\nwant %s", c.what, gj, wj)
		}
	}
	if got := hex.EncodeToString(ygo.EncodeStateAsUpdateV2(d)); got != want.State {
		t.Errorf("V2 state:\n got %s\nwant %s", got, want.State)
	}
}
