package ygo_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Deln0r/ygo"
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
	Name        string            `json:"name"`
	Description string            `json:"description"`
	GC          bool              `json:"gc"`
	Clients     []uint64          `json:"clients"`
	UndoScope   string            `json:"undo_scope"`
	UndoGroup   bool              `json:"undo_group"`
	Steps       []updateEventStep `json:"steps"`
	Events      []updateEvent     `json:"events"`
	DiffsDoc    int               `json:"diffs_doc"`
	Diffs       []updateDiff      `json:"diffs"`
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
	} `json:"apply"`
	Undo *struct {
		Doc int `json:"doc"`
	} `json:"undo"`
	Redo *struct {
		Doc int `json:"doc"`
	} `json:"redo"`
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
}

type updateEvent struct {
	Step   int     `json:"step"`
	Doc    int     `json:"doc"`
	Origin *string `json:"origin"`
	V1     string  `json:"v1"`
	V2     string  `json:"v2"`
}

func (e updateEvent) String() string {
	o := "nil"
	if e.Origin != nil {
		o = strconv.Quote(*e.Origin)
	}
	return fmt.Sprintf("step %d doc %d origin %s\n   v1 %s\n   v2 %s", e.Step, e.Doc, o, e.V1, e.V2)
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
	"nested-type-deleted": {
		reason: "deleting a nested type does not delete the items inside it (txn.Delete has no recursive delete), so the transaction's delete set holds the type's own item, [0,1), where yjs's holds [0,3)",
		events: pinnedEvents(
			"0|0|nil|0101cdcc83da0b002701016d056368696c640100|0000058d9987b4170000012709066d6368696c640105010101010001010000",
			"1|0|nil|0102cdcc83da0b012800cdcc83da0b000161017d012800cdcc83da0b000162017d0200|000006cd9987b417010201000001280502616241000100000241000102017d017d0200",
			"2|0|nil|0001cdcc83da0b010001|00000000000001000000000001cdcc83da0b010000",
		),
	},
	"nested-array-collected": {
		reason: "the same missing recursive delete: the delete set is [0,1), yjs's [0,6); the collected run itself, and every diff inside it, matches",
		events: pinnedEvents(
			"0|0|nil|0101cdcc83da0b002701016d036172720000|0000058d9987b4170000012707046d6172720103010101000001010000",
			"1|0|nil|0101cdcc83da0b010800cdcc83da0b00057d017d027d037d047d0500|000006cd9987b417000100000108010001000001050101017d017d027d037d047d0500",
			"2|0|nil|0001cdcc83da0b010001|00000000000001000000000001cdcc83da0b010000",
			"3|0|nil|0101cdcc83da0b062801016d056166746572017d0100|0000058d9987b4170000012809066d6166746572010501010001010101067d0100",
		),
	},
	"remote-collected-run-partly-known": {
		reason: "the same missing recursive delete in step 4 ([0,1) where yjs has [0,4)); the receiver's step 5, which integrates the unknown tail of the collected run, matches",
		events: pinnedEvents(
			"0|0|nil|0101cdcc83da0b002701016d01700200|0000058d9987b4170000012705026d704100010101020001010000",
			"1|0|nil|0101cdcc83da0b010400cdcc83da0b00016100|000006cd9987b417000100000104030161010100000001010100",
			"2|1|remote|0102cdcc83da0b002701016d0170020400cdcc83da0b00016100|000006cd9987b417000100000327000406036d706141010301000001020001020000",
			"3|0|nil|0101cdcc83da0b0284cdcc83da0b0102626300|000006cd9987b417000102000184040262630200000001010200",
			"4|0|nil|0001cdcc83da0b010001|00000000000001000000000001cdcc83da0b010000",
			"5|1|remote|0101cdcc83da0b02000201cdcc83da0b010002|0000058d9987b4170000010001000000010201010201cdcc83da0b010001",
		),
	},
	"concurrent-formatting": {
		reason: "after a remote update that duplicates a format, yjs runs cleanupYTextAfterTransaction and emits a second update of its own (origin nil) deleting the redundant marker; ygo has no such cleanup, so that event is missing and the later diff lacks the deletion",
		events: pinnedEvents(
			"0|0|nil|0101cdcc83da0b000401017402787900|0000058d9987b41700000104060374787901020101000001010000",
			"1|1|remote|0101cdcc83da0b000401017402787900|0000058d9987b41700000104060374787901020101000001010000",
			"2|0|nil|0102cdcc83da0b0246cdcc83da0b0004626f6c64047472756586cdcc83da0b0104626f6c64046e756c6c00|0002000206cd9987b4170101020100034600860b08626f6c64626f6c644400000000010202787e00",
			"3|1|nil|0102d689cf81010046cdcc83da0b0004626f6c64047472756586cdcc83da0b0104626f6c64046e756c6c00|000200020b96939e8302cd9987b4170001020100034600860b08626f6c64626f6c644400000000010200787e00",
			"4|0|remote|0102d689cf81010046cdcc83da0b0004626f6c64047472756586cdcc83da0b0104626f6c64046e756c6c00|000200020b96939e8302cd9987b4170001020100034600860b08626f6c64626f6c644400000000010200787e00",
			"5|1|remote|0102cdcc83da0b0246cdcc83da0b0004626f6c64047472756586cdcc83da0b0104626f6c64046e756c6c01d689cf8101010001|0002000206cd9987b4170101020100034600860b08626f6c64626f6c644400000000010202787e01d689cf8101010000",
		),
	},
	"undo-grouped-deletes": {
		reason: "tombstones an UndoManager keeps are not merged (yjs Item.mergeWith merges them and carries keep), so undo restores \"ab\" as two structs where yjs restores one; the text is the same. Merging them needs the restore pass to cut at the range ends first",
		events: pinnedEvents(
			"0|0|setup|0101cdcc83da0b000401017402616200|0000058d9987b41700000104060374616201020101000001010000",
			"1|0|nil|0001cdcc83da0b010101|00000000000001000000000001cdcc83da0b010100",
			"2|0|nil|0001cdcc83da0b010001|00000000000001000000000001cdcc83da0b010000",
			"3|0|<undo-manager>|0102cdcc83da0b0244cdcc83da0b000161c4cdcc83da0b00cdcc83da0b01016200|000006cd9987b417020100020002034400c405026162410000000001020200",
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
			docs, events := replayUpdateEvents(t, sc)
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

func replayUpdateEvents(t *testing.T, sc updateEventScenario) ([]*ygo.Doc, []updateEvent) {
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
	var events []updateEvent
	step := -1
	docs := make([]*updateEventDoc, len(sc.Clients))
	plain := make([]*ygo.Doc, len(sc.Clients))
	for i, client := range sc.Clients {
		d := ygo.NewDocWithOptions(ygo.Options{ClientID: client, DisableGC: !sc.GC})
		ed := &updateEventDoc{d: d, roots: map[string]any{}}
		for name := range rootNames {
			kind, n, _ := strings.Cut(name, ":")
			switch kind {
			case "text":
				ed.roots[name] = ygo.NewText(d, n)
			case "array":
				ed.roots[name] = ygo.NewArray(d, n)
			case "map":
				ed.roots[name] = ygo.NewMap(d, n)
			default:
				t.Fatalf("unknown root kind %q", kind)
			}
		}
		if sc.UndoScope != "" {
			// undo_group: one step for everything; otherwise one step per
			// transaction.
			timeout := time.Duration(-1)
			if sc.UndoGroup {
				timeout = time.Hour
			}
			ed.um = ygo.NewUndoManagerWithOptions(d, ygo.UndoManagerOptions{CaptureTimeout: timeout}, ed.roots[sc.UndoScope].(ygo.UndoScope))
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
			t.Fatalf("unexpected origin %T", o)
			return nil
		}
		d.OnUpdate(func(u []byte, origin any) {
			events = append(events, updateEvent{Step: step, Doc: docIdx, Origin: originName(origin), V1: hex.EncodeToString(u)})
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
			case a.Replay != nil:
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
			if err := ygo.ApplyUpdateWithOrigin(plain[a.To], raw, origin); err != nil {
				t.Fatalf("step %d: apply: %v", k, err)
			}
		case st.Undo != nil:
			docs[st.Undo.Doc].um.Undo()
		case st.Redo != nil:
			docs[st.Redo.Doc].um.Redo()
		default:
			ed := docs[st.Doc]
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
	}
	return plain, events
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
	segs := decodeUpdateEventPath(t, op.Path)
	target := ed.roots[segs[0].(string)]
	for _, seg := range segs[1:] {
		switch v := target.(type) {
		case *ygo.Map:
			target = v.Get(seg.(string))
		case *ygo.Array:
			target = v.Get(uint64(seg.(float64)))
		default:
			return fmt.Errorf("cannot descend into %T", target)
		}
	}
	switch v := target.(type) {
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
			return v.InsertEmbed(txn, op.Index, jsonValue(t, op.Value))
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

func jsonAttrs(t *testing.T, raw json.RawMessage) ygo.Attrs {
	t.Helper()
	m, ok := jsonValue(t, raw).(map[string]any)
	if !ok {
		t.Fatalf("attributes %s are not an object", raw)
	}
	return ygo.Attrs(m)
}
