package ygo_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/internal/block"
)

// update-event-fixtures.json is written by testdata/gen/gen-update-events.mjs:
// every 'update' / 'updateV2' event yjs emits for a sequence of transactions,
// and Y.encodeStateAsUpdate / V2 against state vectors that end inside a
// block. TestUpdateEvents_Fixtures replays the same steps through ygo.

type updateEventFile struct {
	Generator string                `json:"generator"`
	Scenarios []updateEventScenario `json:"scenarios"`
}

type updateEventScenario struct {
	Name        string               `json:"name"`
	Description string               `json:"description"`
	GC          bool                 `json:"gc"`
	Clients     []uint64             `json:"clients"`
	UndoScope   string               `json:"undo_scope"`
	UndoGroup   bool                 `json:"undo_group"`
	Managers    []updateEventManager `json:"managers"`
	RootKeys    []string             `json:"root_keys"`
	Steps       []updateEventStep    `json:"steps"`
	Events      []updateEvent        `json:"events"`
	// Relays are documents that only forward updates: nothing reads
	// their roots, so none is typed (no undo scope, no state).
	Relays []int `json:"relays"`
	// Changed asks for each event's changed types (updateEvent.Changed);
	// on for every scenario but the pinned one, whose pin lists bytes only.
	Changed  bool         `json:"changed"`
	DiffsDoc int          `json:"diffs_doc"`
	Diffs    []updateDiff `json:"diffs"`
}

type updateEventStep struct {
	Doc    int             `json:"doc"`
	Origin *string         `json:"origin"`
	Ops    []updateEventOp `json:"ops"`
	Apply  *struct {
		From   int     `json:"from"`
		To     int     `json:"to"`
		Origin *string `json:"origin"`
		What   string  `json:"what"`
		SV     svJSON  `json:"sv"`
		Replay *int    `json:"replay"`
		Update string  `json:"update"`
		// UpdateV2 is the same update in V2 (yjs
		// convertUpdateFormatV1ToV2), for the V2 and mixed replays.
		UpdateV2 string `json:"update_v2"`
	} `json:"apply"`
	Undo   *updateEventUndoRef `json:"undo"`
	Redo   *updateEventUndoRef `json:"redo"`
	Manage *int                `json:"manage"`
	Clear  *updateEventUndoRef `json:"clear"`
	Stop   *updateEventUndoRef `json:"stop"`
	// Result is what yjs's undo / redo returned (a stack item or null),
	// State the toJSON of each document's roots after the step, Stacks
	// each manager's [canUndo, canRedo] after it (null before it exists).
	Result *bool           `json:"result"`
	State  json.RawMessage `json:"state"`
	Stacks json.RawMessage `json:"stacks"`
}

// updateEventUndoRef names the manager an undo / redo / clear step acts on:
// Um indexes the scenario's managers; without it, the document's legacy
// undo_scope manager.
type updateEventUndoRef struct {
	Doc int  `json:"doc"`
	Um  *int `json:"um"`
}

// updateEventManager is an UndoManager a scenario creates at its manage step,
// on Scope (a path), tracking Origins (null for the nil origin; absent means
// [null]), one step per transaction unless Group.
type updateEventManager struct {
	Doc     int             `json:"doc"`
	Scope   json.RawMessage `json:"scope"`
	Origins []*string       `json:"origins"`
	Group   bool            `json:"group"`
}

type updateEventOp struct {
	Op     string          `json:"op"`
	Path   json.RawMessage `json:"path"`
	Index  uint64          `json:"index"`
	Len    uint64          `json:"len"`
	Text   string          `json:"text"`
	Attrs  json.RawMessage `json:"attrs"`
	Value  json.RawMessage `json:"value"`
	Values json.RawMessage `json:"values"`
	Key    string          `json:"key"`
	Kind   string          `json:"kind"`
	Name   string          `json:"name"`
	Delta  json.RawMessage `json:"delta"`
}

type updateEvent struct {
	Step   int     `json:"step"`
	Doc    int     `json:"doc"`
	Origin *string `json:"origin"`
	V1     string  `json:"v1"`
	V2     string  `json:"v2"`
	// Changed lists the types the transaction changed, in the order it
	// first changed them (yjs transaction.changed, ygo ChangedTypes), as
	// paths: a root by name, a nested type by its parent's path and map key
	// or array index, "<deleted>" once deleted. Only for scenarios with
	// "changed".
	Changed []string `json:"changed"`
}

func (e updateEvent) String() string {
	o := "nil"
	if e.Origin != nil {
		o = strconv.Quote(*e.Origin)
	}
	out := fmt.Sprintf("step %d doc %d origin %s\n   v1 %s\n   v2 %s", e.Step, e.Doc, o, e.V1, e.V2)
	if e.Changed != nil {
		out += "\n   changed " + strings.Join(e.Changed, ", ")
	}
	return out
}

// branchPath is typePath of gen-update-events.mjs for a ygo branch.
func branchPath(br *block.Branch) string {
	it := br.Item
	if it == nil {
		return br.Name
	}
	if it.IsDeleted() {
		return "<deleted>"
	}
	parent := it.Parent.Branch
	if it.ParentSub != nil {
		return branchPath(parent) + "/" + *it.ParentSub
	}
	var i uint64
	for x := parent.Start; x != nil && x != it; x = x.Right {
		if !x.IsDeleted() && x.IsCountable() {
			i += x.Len
		}
	}
	return fmt.Sprintf("%s/%d", branchPath(parent), i)
}

type updateDiff struct {
	SV svJSON `json:"sv"`
	V1 string `json:"v1"`
	V2 string `json:"v2"`
}

// svJSON is a state vector as the generator writes it: client ID (as a
// decimal string key) to clock.
type svJSON map[string]uint64

// encode writes the state vector in the lib0 wire form EncodeDiff takes:
// varuint count, then varuint (client, clock) pairs.
func (sv svJSON) encode(t *testing.T) []byte {
	t.Helper()
	clients := make([]uint64, 0, len(sv))
	for k := range sv {
		c, err := strconv.ParseUint(k, 10, 64)
		if err != nil {
			t.Fatalf("state vector key %q: %v", k, err)
		}
		clients = append(clients, c)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] > clients[j] })
	buf := binary.AppendUvarint(nil, uint64(len(clients)))
	for _, c := range clients {
		buf = binary.AppendUvarint(buf, c)
		buf = binary.AppendUvarint(buf, sv[strconv.FormatUint(c, 10)])
	}
	return buf
}

// knownUpdateEventDivergences pins the scenarios where ygo's events do not yet
// match yjs, with the exact events ygo emits today. A scenario that starts to
// match, or diverges in another way, fails the test.
var knownUpdateEventDivergences = map[string]struct {
	reason string
	events []updateEvent
}{
	"insert-into-root-typed-after-formatting": {
		reason: "yjs builds a fresh YText when an untyped root is first read as text, with search markers on, though the root already holds format markers (ContentFormat.integrate turns them off as it integrates); the next insert starts at a marker with no attributes in effect and steps over the closing bold:null, so \"!\" is plain. ygo's text keeps walking from the start, collects bold and inserts before the closing marker, so \"!\" is bold (docs/tech-debt.md, \"A root typed after formatted text arrived inserts as a formatted text\")",
		events: pinnedEvents(
			"0|0|nil|0103cdcc83da0b000401017402787946cdcc83da0b0004626f6c64047472756586cdcc83da0b0104626f6c64046e756c6c00|0002000206cd9987b4170101020100050400460086100b747879626f6c64626f6c640102440001010000010300787e00",
			"1|1|remote|0103cdcc83da0b000401017402787946cdcc83da0b0004626f6c64047472756586cdcc83da0b0104626f6c64046e756c6c00|0002000206cd9987b4170101020100050400460086100b747879626f6c64626f6c640102440001010000010300787e00",
			"2|1|nil|0101d689cf810100c4cdcc83da0b01cdcc83da0b03012100|00000b96939e8302cd9987b417000102010601c40301210100000001010000",
			"3|0|remote|0101d689cf81010084cdcc83da0b03012100|00000a96939e83028d9987b41701060001840301210100000001010000",
		),
	},
}

// pinnedEvents parses "step|doc|origin|v1|v2" lines, origin "nil" for none.
func pinnedEvents(lines ...string) []updateEvent {
	out := make([]updateEvent, 0, len(lines))
	for _, l := range lines {
		f := strings.Split(l, "|")
		if len(f) != 5 {
			panic("pinned event needs 5 fields: " + l)
		}
		step, err1 := strconv.Atoi(f[0])
		doc, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			panic("bad pinned event: " + l)
		}
		e := updateEvent{Step: step, Doc: doc, V1: f[3], V2: f[4]}
		if f[2] != "nil" {
			o := f[2]
			e.Origin = &o
		}
		out = append(out, e)
	}
	return out
}

func TestUpdateEvents_Fixtures(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "update-event-fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f updateEventFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Scenarios) == 0 {
		t.Fatal("no update-event scenarios; the fixture file is empty or its shape changed")
	}
	seen := map[string]bool{}
	for _, sc := range f.Scenarios {
		seen[sc.Name] = true
		t.Run(sc.Name, func(t *testing.T) {
			docs, events := replayUpdateEvents(t, sc, "v1")
			known, isKnown := knownUpdateEventDivergences[sc.Name]
			switch {
			case !isKnown:
				compareUpdateEvents(t, events, sc.Events)
			case updateEventsEqual(events, sc.Events):
				t.Errorf("now matches yjs; remove it from knownUpdateEventDivergences (listed as: %s)", known.reason)
			case !updateEventsEqual(events, known.events):
				t.Errorf("known divergence (%s) now diverges differently", known.reason)
				compareUpdateEvents(t, events, known.events)
			}
			// Diffs forward, then backward: an encoder that cut the
			// live item instead of a copy would still pass an ascending
			// pass, each later suffix being right, while the content
			// before it disappeared. The full state must not move.
			d := docs[sc.DiffsDoc]
			before := ygo.EncodeStateAsUpdate(d)
			for pass := 0; pass < 2; pass++ {
				for i := range sc.Diffs {
					df := sc.Diffs[i]
					if pass == 1 {
						df = sc.Diffs[len(sc.Diffs)-1-i]
					}
					checkUpdateDiff(t, d, df)
				}
			}
			if after := ygo.EncodeStateAsUpdate(d); !bytes.Equal(before, after) {
				t.Errorf("encoding diffs changed the document:\n before %x\n after  %x", before, after)
			}
		})
	}
	for name := range knownUpdateEventDivergences {
		if !seen[name] {
			t.Errorf("knownUpdateEventDivergences names %q, which is not a scenario", name)
		}
	}
}

// TestUpdateEvents_FixturesV2 replays every scenario with its updates
// applied as V2, and as V1 and V2 by turns: both decoders feed the same
// integration, which must emit the same events.
func TestUpdateEvents_FixturesV2(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "update-event-fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f updateEventFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"v2", "mixed"} {
		for _, sc := range f.Scenarios {
			t.Run(format+"/"+sc.Name, func(t *testing.T) {
				_, events := replayUpdateEvents(t, sc, format)
				want := sc.Events
				if known, isKnown := knownUpdateEventDivergences[sc.Name]; isKnown {
					want = known.events
				}
				compareUpdateEvents(t, events, want)
			})
		}
	}
}

func checkUpdateDiff(t *testing.T, d *ygo.Doc, df updateDiff) {
	t.Helper()
	sv := df.SV.encode(t)
	v1, err := ygo.EncodeDiff(d, sv)
	if err != nil {
		t.Fatalf("EncodeDiff %v: %v", df.SV, err)
	}
	if got := hex.EncodeToString(v1); got != df.V1 {
		t.Errorf("EncodeDiff against %v:\n ygo %s\n yjs %s", df.SV, got, df.V1)
	}
	v2, err := ygo.EncodeDiffV2(d, sv)
	if err != nil {
		t.Fatalf("EncodeDiffV2 %v: %v", df.SV, err)
	}
	if got := hex.EncodeToString(v2); got != df.V2 {
		t.Errorf("EncodeDiffV2 against %v:\n ygo %s\n yjs %s", df.SV, got, df.V2)
	}
}

func updateEventsEqual(a, b []updateEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].String() != b[i].String() {
			return false
		}
	}
	return true
}

func compareUpdateEvents(t *testing.T, got, want []updateEvent) {
	t.Helper()
	n := len(got)
	if len(want) > n {
		n = len(want)
	}
	bad := 0
	for i := 0; i < n && bad < 5; i++ {
		switch {
		case i >= len(got):
			t.Errorf("event %d missing in ygo; yjs:\n %s", i, want[i])
			bad++
		case i >= len(want):
			t.Errorf("event %d extra in ygo:\n %s", i, got[i])
			bad++
		case got[i].String() != want[i].String():
			t.Errorf("event %d differs:\n ygo %s\n yjs %s", i, got[i], want[i])
			bad++
		}
	}
}

// updateEventDoc is one replayed document with its root types, created up
// front: the root constructors take the document lock, which an open write
// transaction already holds.
type updateEventDoc struct {
	d     *ygo.Doc
	roots map[string]any
	um    *ygo.UndoManager
}

// root returns the root type name ("text:t") of ed, typing it on first
// use, as the generator's yjs documents type a root only when an op, a
// state snapshot or an UndoManager first reads it (doc.getText). A
// document that only receives updates keeps untyped roots, which matters
// to the format cleanup: yjs runs it for a YText, not an untyped root.
func (ed *updateEventDoc) root(name string) any {
	if r, ok := ed.roots[name]; ok {
		return r
	}
	kind, n, _ := strings.Cut(name, ":")
	var r any
	switch kind {
	case "text":
		r = ygo.NewText(ed.d, n)
	case "array":
		r = ygo.NewArray(ed.d, n)
	case "map":
		r = ygo.NewMap(ed.d, n)
	case "xml":
		r = ygo.NewXmlFragment(ed.d, n)
	default:
		panic("unknown root kind " + kind)
	}
	ed.roots[name] = r
	return r
}

// replayUpdateEvents replays sc, applying updates as V1, as V2, or
// ("mixed") V1 and V2 by turns.
func replayUpdateEvents(t *testing.T, sc updateEventScenario, format string) ([]*ygo.Doc, []updateEvent) {
	t.Helper()
	rootNames := map[string]bool{}
	if sc.UndoScope != "" {
		rootNames[sc.UndoScope] = true
	}
	for _, st := range sc.Steps {
		for _, op := range st.Ops {
			segs := decodeUpdateEventPath(t, op.Path)
			rootNames[segs[0].(string)] = true
		}
	}
	for _, m := range sc.Managers {
		rootNames[decodeUpdateEventPath(t, m.Scope)[0].(string)] = true
	}
	for _, r := range sc.RootKeys {
		rootNames[r] = true
	}
	managers := make([]*ygo.UndoManager, len(sc.Managers))
	var events []updateEvent
	step := -1
	docs := make([]*updateEventDoc, len(sc.Clients))
	plain := make([]*ygo.Doc, len(sc.Clients))
	for i, client := range sc.Clients {
		d := ygo.NewDocWithOptions(ygo.Options{ClientID: client, DisableGC: !sc.GC})
		ed := &updateEventDoc{d: d, roots: map[string]any{}}
		for name := range rootNames {
			switch kind, _, _ := strings.Cut(name, ":"); kind {
			case "text", "array", "map", "xml":
			default:
				t.Fatalf("unknown root kind %q", kind)
			}
		}
		relay := slices.Contains(sc.Relays, i)
		if sc.UndoScope != "" && !relay {
			// undo_group: one step for everything; otherwise one step per
			// transaction.
			timeout := time.Duration(-1)
			if sc.UndoGroup {
				timeout = time.Hour
			}
			ed.um = ygo.NewUndoManagerWithOptions(d, ygo.UndoManagerOptions{CaptureTimeout: timeout}, ed.root(sc.UndoScope).(ygo.UndoScope))
		}
		docIdx := i
		originName := func(o any) *string {
			switch v := o.(type) {
			case nil:
				return nil
			case string:
				return &v
			}
			if ed.um != nil && o == any(ed.um) {
				s := "<undo-manager>"
				return &s
			}
			for k, m := range managers {
				if m != nil && o == any(m) {
					s := fmt.Sprintf("<um:%d>", k)
					return &s
				}
			}
			t.Fatalf("unexpected origin %T", o)
			return nil
		}
		var lastChanged []string
		if sc.Changed {
			d.OnAfterTransaction(func(tr *ygo.TransactionMut) {
				lastChanged = lastChanged[:0:0]
				for _, br := range tr.ChangedTypes() {
					lastChanged = append(lastChanged, branchPath(br))
				}
			})
		}
		d.OnUpdate(func(u []byte, origin any) {
			e := updateEvent{Step: step, Doc: docIdx, Origin: originName(origin), V1: hex.EncodeToString(u)}
			if sc.Changed {
				e.Changed = append([]string{}, lastChanged...)
			}
			events = append(events, e)
		})
		d.OnUpdateV2(func(u []byte, origin any) {
			for k := len(events) - 1; k >= 0; k-- {
				e := &events[k]
				if e.Step == step && e.Doc == docIdx && e.V2 == "" {
					e.V2 = hex.EncodeToString(u)
					return
				}
			}
			t.Errorf("step %d doc %d: updateV2 without a matching update", step, docIdx)
		})
		docs[i] = ed
		plain[i] = d
	}
	applies := 0 // for "mixed": V1 and V2 by turns over the applies, not the steps
	for k, st := range sc.Steps {
		step = k
		switch {
		case st.Apply != nil:
			a := st.Apply
			raw, err := hex.DecodeString(a.Update)
			if err != nil {
				t.Fatal(err)
			}
			// ygo's own encoding of the same request must be what yjs
			// sent. A replay re-sends bytes an earlier step applied.
			var own []byte
			switch {
			case a.Replay != nil, a.What == "raw":
				own = raw
			case a.What == "state":
				own = ygo.EncodeStateAsUpdate(plain[a.From])
			case a.What == "diff":
				own, err = ygo.EncodeDiff(plain[a.From], ygo.EncodeStateVector(plain[a.To]))
			case a.What == "since":
				own, err = ygo.EncodeDiff(plain[a.From], a.SV.encode(t))
			default:
				t.Fatalf("unknown apply %q", a.What)
			}
			if err != nil {
				t.Fatalf("step %d: encode %s: %v", k, a.What, err)
			}
			// A pinned scenario's documents already differ from yjs's,
			// so its own encodings may too.
			if _, pinned := knownUpdateEventDivergences[sc.Name]; !pinned && !bytes.Equal(own, raw) {
				t.Errorf("step %d: ygo's %s update differs from yjs's:\n ygo %x\n yjs %x", k, a.What, own, raw)
			}
			var origin any
			if a.Origin != nil {
				origin = *a.Origin
			}
			applyV2 := format == "v2" || (format == "mixed" && applies%2 == 1)
			applies++
			if applyV2 {
				v2, err := hex.DecodeString(a.UpdateV2)
				if err != nil || len(v2) == 0 {
					t.Fatalf("step %d: no V2 update in the fixture", k)
				}
				err = ygo.ApplyUpdateV2WithOrigin(plain[a.To], v2, origin)
				if err != nil {
					t.Fatalf("step %d: apply V2: %v", k, err)
				}
			} else if err := ygo.ApplyUpdateWithOrigin(plain[a.To], raw, origin); err != nil {
				t.Fatalf("step %d: apply: %v", k, err)
			}
		case st.Undo != nil || st.Redo != nil:
			ref, op := st.Undo, "undo"
			if ref == nil {
				ref, op = st.Redo, "redo"
			}
			um := docs[ref.Doc].um
			if ref.Um != nil {
				um = managers[*ref.Um]
			}
			var got bool
			if op == "undo" {
				got = um.Undo()
			} else {
				got = um.Redo()
			}
			if st.Result != nil && got != *st.Result {
				t.Errorf("step %d: %s returned %v, yjs %v", k, op, got, *st.Result)
			}
		case st.Manage != nil:
			m := sc.Managers[*st.Manage]
			scope, err := resolveUpdateEventPath(docs[m.Doc], decodeUpdateEventPath(t, m.Scope))
			if err != nil {
				t.Fatalf("step %d: manager scope: %v", k, err)
			}
			opts := ygo.UndoManagerOptions{CaptureTimeout: -1}
			if m.Group {
				opts.CaptureTimeout = time.Hour
			}
			if m.Origins != nil {
				opts.TrackedOrigins = map[any]struct{}{}
				for _, o := range m.Origins {
					if o == nil {
						opts.TrackedOrigins[nil] = struct{}{}
					} else {
						opts.TrackedOrigins[*o] = struct{}{}
					}
				}
			}
			managers[*st.Manage] = ygo.NewUndoManagerWithOptions(docs[m.Doc].d, opts, scope.(ygo.UndoScope))
		case st.Clear != nil:
			managers[*st.Clear.Um].Clear()
		case st.Stop != nil:
			managers[*st.Stop.Um].StopCapturing()
		default:
			ed := docs[st.Doc]
			for _, op := range st.Ops { // typed before the transaction: NewText takes the doc lock
				ed.root(decodeUpdateEventPath(t, op.Path)[0].(string))
			}
			txn := ed.d.WriteTxn()
			if st.Origin != nil {
				txn.Origin = *st.Origin
			}
			for j, op := range st.Ops {
				if err := applyUpdateEventOp(t, txn, ed, op); err != nil {
					txn.Commit()
					t.Fatalf("step %d op %d (%s): %v", k, j, op.Op, err)
				}
			}
			txn.Commit()
		}
		if len(st.State) > 0 {
			got := make([]any, len(docs))
			for i, ed := range docs {
				if slices.Contains(sc.Relays, i) {
					continue
				}
				state := map[string]any{}
				for _, r := range sc.RootKeys {
					state[r] = renderUpdateEventState(ed.root(r))
				}
				got[i] = state
			}
			if g, w, ok := sameUpdateEventJSON(got, st.State); !ok {
				t.Errorf("step %d: state\n ygo %s\n yjs %s", k, g, w)
			}
		}
		if len(st.Stacks) > 0 {
			got := make([]any, len(managers))
			for i, m := range managers {
				if m != nil {
					got[i] = []bool{m.CanUndo(), m.CanRedo()}
				}
			}
			if g, w, ok := sameUpdateEventJSON(got, st.Stacks); !ok {
				t.Errorf("step %d: [canUndo canRedo] per manager\n ygo %s\n yjs %s", k, g, w)
			}
		}
	}
	return plain, events
}

// renderUpdateEventState is the toJSON yjs gives a shared type: a map as an
// object, an array as a list, a text as its plain string, XML as its markup.
func renderUpdateEventState(v any) any {
	switch x := v.(type) {
	case *ygo.Map:
		out := map[string]any{}
		x.Range(func(k string, val any) bool {
			out[k] = renderUpdateEventState(val)
			return true
		})
		return out
	case *ygo.Array:
		out := []any{}
		x.Range(func(_ uint64, val any) bool {
			out = append(out, renderUpdateEventState(val))
			return true
		})
		return out
	case *ygo.Text:
		return x.String()
	case *ygo.XmlFragment:
		return x.ToString()
	case *ygo.XmlElement:
		return x.ToString()
	case *ygo.XmlText:
		return x.String()
	default:
		return v
	}
}

// sameUpdateEventJSON compares got with the fixture's JSON after one
// round through encoding/json each, so key order and number spelling agree.
func sameUpdateEventJSON(got any, want json.RawMessage) (string, string, bool) {
	g, err := json.Marshal(got)
	if err != nil {
		return err.Error(), string(want), false
	}
	dec := json.NewDecoder(bytes.NewReader(want))
	dec.UseNumber()
	var w any
	if err := dec.Decode(&w); err != nil {
		return string(g), err.Error(), false
	}
	wb, _ := json.Marshal(w)
	return string(g), string(wb), bytes.Equal(g, wb)
}

// resolveUpdateEventPath walks a fixture path ("kind:name", then map keys
// and child indices) to a shared type.
func resolveUpdateEventPath(ed *updateEventDoc, segs []any) (any, error) {
	target := ed.root(segs[0].(string))
	for _, seg := range segs[1:] {
		switch v := target.(type) {
		case *ygo.Map:
			target = v.Get(seg.(string))
		case *ygo.Array:
			target = v.Get(uint64(seg.(float64)))
		case *ygo.XmlFragment:
			target = v.Get(uint64(seg.(float64)))
		case *ygo.XmlElement:
			target = v.Get(uint64(seg.(float64)))
		default:
			return nil, fmt.Errorf("cannot descend into %T", target)
		}
	}
	return target, nil
}

func decodeUpdateEventPath(t *testing.T, raw json.RawMessage) []any {
	t.Helper()
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []any{one}
	}
	var segs []any
	if err := json.Unmarshal(raw, &segs); err != nil || len(segs) == 0 {
		t.Fatalf("bad path %s", raw)
	}
	if _, ok := segs[0].(string); !ok {
		t.Fatalf("path %s does not start with a root", raw)
	}
	return segs
}

func applyUpdateEventOp(t *testing.T, txn *ygo.TransactionMut, ed *updateEventDoc, op updateEventOp) error {
	t.Helper()
	target, err := resolveUpdateEventPath(ed, decodeUpdateEventPath(t, op.Path))
	if err != nil {
		return err
	}
	if x, ok := target.(*ygo.XmlText); ok {
		target = &x.Text // an XmlText takes the text ops: insert, delete, format, embed, delta
	}
	switch v := target.(type) {
	case *ygo.XmlFragment:
		switch op.Op {
		case "xmlElement":
			v.InsertXmlElement(txn, op.Index, op.Name)
			return nil
		case "xmlText":
			v.InsertXmlText(txn, op.Index)
			return nil
		case "delete":
			v.Delete(txn, op.Index, op.Len)
			return nil
		}
	case *ygo.XmlElement:
		switch op.Op {
		case "xmlElement":
			v.InsertXmlElement(txn, op.Index, op.Name)
			return nil
		case "xmlText":
			v.InsertXmlText(txn, op.Index)
			return nil
		case "setAttr":
			v.SetAttribute(txn, op.Key, jsonValue(t, op.Value).(string))
			return nil
		case "delete":
			v.Delete(txn, op.Index, op.Len)
			return nil
		}
	case *ygo.Text:
		switch op.Op {
		case "insert":
			if len(op.Attrs) > 0 {
				return v.InsertWithAttributes(txn, op.Index, op.Text, jsonAttrs(t, op.Attrs))
			}
			return v.Insert(txn, op.Index, op.Text)
		case "delete":
			return v.Delete(txn, op.Index, op.Len)
		case "format":
			return v.Format(txn, op.Index, op.Len, jsonAttrs(t, op.Attrs))
		case "embed":
			if len(op.Attrs) > 0 {
				return v.InsertEmbedWithAttributes(txn, op.Index, jsonValue(t, op.Value), jsonAttrs(t, op.Attrs))
			}
			return v.InsertEmbed(txn, op.Index, jsonValue(t, op.Value))
		case "delta":
			return v.ApplyDelta(txn, jsonDelta(t, op.Delta))
		}
	case *ygo.Array:
		switch op.Op {
		case "push":
			v.Push(txn, jsonValue(t, op.Values).([]any)...)
			return nil
		case "insertValues":
			v.InsertRange(txn, op.Index, jsonValue(t, op.Values).([]any))
			return nil
		case "delete":
			v.Delete(txn, op.Index, op.Len)
			return nil
		case "insertType":
			switch op.Kind {
			case "map":
				v.InsertMap(txn, op.Index)
			case "array":
				v.InsertArray(txn, op.Index)
			case "text":
				v.InsertText(txn, op.Index)
			default:
				return fmt.Errorf("unknown type kind %q", op.Kind)
			}
			return nil
		}
	case *ygo.Map:
		switch op.Op {
		case "set":
			v.Set(txn, op.Key, jsonValue(t, op.Value))
			return nil
		case "mapDelete":
			v.Delete(txn, op.Key)
			return nil
		case "mapClear":
			v.Clear(txn)
			return nil
		case "setType":
			switch op.Kind {
			case "map":
				v.SetMap(txn, op.Key)
			case "array":
				v.SetArray(txn, op.Key)
			case "text":
				v.SetText(txn, op.Key)
			default:
				return fmt.Errorf("unknown type kind %q", op.Kind)
			}
			return nil
		}
	}
	return fmt.Errorf("op %s on %T", op.Op, target)
}

// jsonValue decodes a fixture value the way yjs wrote it: integral numbers
// become int64 (lib0 writes them as varints), others float64.
func jsonValue(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("bad value %s: %v", raw, err)
	}
	return numbersToGo(v)
}

func numbersToGo(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	case []any:
		for i := range x {
			x[i] = numbersToGo(x[i])
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = numbersToGo(x[k])
		}
		return x
	default:
		return v
	}
}

// jsonDelta converts a Quill delta as the generator records it.
func jsonDelta(t *testing.T, raw json.RawMessage) []ygo.DeltaOp {
	t.Helper()
	list, ok := jsonValue(t, raw).([]any)
	if !ok {
		t.Fatalf("delta %s is not an array", raw)
	}
	ops := make([]ygo.DeltaOp, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("delta op %v is not an object", item)
		}
		var op ygo.DeltaOp
		switch v := m["insert"].(type) {
		case string:
			op.Insert = v
		case nil:
		default:
			op.Embed = v
		}
		if n, ok := m["retain"].(int64); ok {
			op.Retain = uint64(n)
		}
		if n, ok := m["delete"].(int64); ok {
			op.Delete = uint64(n)
		}
		if a, ok := m["attributes"].(map[string]any); ok {
			op.Attributes = ygo.Attrs(a)
		}
		ops = append(ops, op)
	}
	return ops
}

func jsonAttrs(t *testing.T, raw json.RawMessage) ygo.Attrs {
	t.Helper()
	m, ok := jsonValue(t, raw).(map[string]any)
	if !ok {
		t.Fatalf("attributes %s are not an object", raw)
	}
	return ygo.Attrs(m)
}
