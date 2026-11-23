package difffuzz

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/internal/anyval"
)

// Trace is what one implementation did with a scenario.
type Trace struct {
	Schema int         `json:"schema"`
	Steps  []StepTrace `json:"steps"`
}

// StepTrace records one step.
type StepTrace struct {
	Skip   bool        `json:"skip,omitempty"`
	Err    string      `json:"err,omitempty"`
	Result *bool       `json:"result,omitempty"`
	Sent   string      `json:"sent,omitempty"`
	Events []Event     `json:"events,omitempty"`
	Peers  []PeerState `json:"peers,omitempty"`
	// Obs are the observer events of the step (Scenario.Observe), in the
	// order the callbacks ran; Batch numbers the callback each came in.
	Obs []ObsEvent `json:"obs,omitempty"`
	// Depths are, for an unwind step, each undo peer's count of undos that
	// changed something until none did, then of redos.
	Depths []int `json:"depths,omitempty"`
}

// ObsEvent is one observer event: where it fired, the path of its target
// below the root, the target's kind and the change, as JSON.
type ObsEvent struct {
	Batch  int    `json:"batch"`
	Peer   int    `json:"peer"`
	Root   string `json:"root"`
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Change string `json:"change"`
}

// Event is one update event: the peer, the format ("1" or "2"), the bytes.
type Event struct {
	Peer int    `json:"peer"`
	V    string `json:"v"`
	Hex  string `json:"hex"`
}

// PeerState is a touched peer after a step.
type PeerState struct {
	Peer    int    `json:"peer"`
	V1      string `json:"v1"`
	V2      string `json:"v2"`
	SV      string `json:"sv"`
	Pending bool   `json:"pending"`
	View    string `json:"view"`
	CanUndo bool   `json:"canUndo,omitempty"`
	CanRedo bool   `json:"canRedo,omitempty"`
}

// RunOptions are test hooks; the zero value replays the scenario as is.
type RunOptions struct {
	// Plant changes the Go trace on purpose (the harness's own tests): it
	// gets the step index and the step's trace after the step ran.
	Plant func(i int, st *StepTrace)
	// Stats, when set, counts what the run did.
	Stats *Stats
}

// Stats counts what runs did, to see what the generator reaches.
type Stats struct {
	Ops     map[string]int // op (want/op) that found its target
	NoOps   map[string]int // op whose path did not resolve
	Steps   map[string]int // step kinds that were not skipped
	Events  int
	Pending int // touched peers left with something pending
	UndoDid int // undo or redo that changed something
	Errors  int
	Nested  int // ops on a target below a root
}

func (st *Stats) init() {
	if st.Ops == nil {
		st.Ops, st.NoOps, st.Steps = map[string]int{}, map[string]int{}, map[string]int{}
	}
}

type event struct{ v1, v2 []byte }

type peer struct {
	doc  *ygo.Doc
	t    *ygo.Text
	a    *ygo.Array
	m    *ygo.Map
	x    *ygo.XmlFragment
	um   *ygo.UndoManager
	log  []event
	sent []map[int]bool // per destination: indexes of log delivered
}

type runner struct {
	peers []*peer
	cur   *StepTrace
	stats *Stats
	batch int // observer callbacks so far in the step
}

// Run replays s through ygo and returns its trace.
func Run(s Scenario, opts RunOptions) Trace {
	r := &runner{stats: opts.Stats}
	if r.stats != nil {
		r.stats.init()
	}
	for i, p := range s.Peers {
		d := ygo.NewDocWithOptions(ygo.Options{ClientID: p.Client, DisableGC: !p.GC})
		pr := &peer{doc: d, t: ygo.NewText(d, "t"), a: ygo.NewArray(d, "a"), m: ygo.NewMap(d, "m"), x: ygo.NewXmlFragment(d, "x")}
		pr.sent = make([]map[int]bool, len(s.Peers))
		for j := range pr.sent {
			pr.sent[j] = map[int]bool{}
		}
		if p.Undo {
			pr.um = ygo.NewUndoManagerWithOptions(d, ygo.UndoManagerOptions{CaptureTimeout: time.Duration(math.MaxInt64)}, pr.t, pr.a, pr.m, pr.x)
		}
		idx := i
		d.OnUpdate(func(u []byte, _ any) {
			b := append([]byte(nil), u...)
			r.peers[idx].log = append(r.peers[idx].log, event{v1: b})
			if r.cur != nil {
				r.cur.Events = append(r.cur.Events, Event{Peer: idx, V: "1", Hex: hex.EncodeToString(b)})
			}
		})
		d.OnUpdateV2(func(u []byte, _ any) {
			b := append([]byte(nil), u...)
			l := r.peers[idx].log
			if len(l) > 0 && l[len(l)-1].v2 == nil {
				l[len(l)-1].v2 = b
			}
			if r.cur != nil {
				r.cur.Events = append(r.cur.Events, Event{Peer: idx, V: "2", Hex: hex.EncodeToString(b)})
			}
		})
		r.peers = append(r.peers, pr)
		if s.Observe {
			r.observe(idx, pr)
		}
	}
	tr := Trace{Schema: Schema}
	for i, st := range s.Steps {
		out := r.step(st)
		if r.stats != nil {
			if !out.Skip {
				r.stats.Steps[st.K]++
			}
			r.stats.Events += len(out.Events)
			if out.Err != "" {
				r.stats.Errors++
			}
			if out.Result != nil && *out.Result {
				r.stats.UndoDid++
			}
			for _, p := range out.Peers {
				if p.Pending {
					r.stats.Pending++
				}
			}
		}
		if opts.Plant != nil {
			opts.Plant(i, &out)
		}
		tr.Steps = append(tr.Steps, out)
		if out.Err != "" && len(out.Err) >= 5 && out.Err[:5] == "panic" {
			break
		}
	}
	return tr
}

func (r *runner) step(st Step) (out StepTrace) {
	r.cur = &out
	r.batch = 0
	defer func() {
		r.cur = nil
		if e := recover(); e != nil {
			out.Err = fmt.Sprintf("panic: %v", e)
		}
	}()
	var touched []int
	switch st.K {
	case StepTxn:
		p := r.peers[st.P]
		txn := p.doc.WriteTxn()
		var err error
		for _, op := range st.Ops {
			if err = r.op(p, txn, op); err != nil {
				break
			}
		}
		txn.Commit()
		if err != nil {
			out.Err = "error"
		}
		touched = []int{st.P}
	case StepUndo, StepRedo, StepStop, StepUClear:
		p := r.peers[st.P]
		if p.um == nil {
			out.Skip = true
			return out
		}
		switch st.K {
		case StepUndo:
			b := p.um.Undo()
			out.Result = &b
		case StepRedo:
			b := p.um.Redo()
			out.Result = &b
		case StepStop:
			p.um.StopCapturing()
		default:
			p.um.Clear()
		}
		touched = []int{st.P}
	case StepDeliver:
		from, to := r.peers[st.From], r.peers[st.To]
		idx := undelivered(from, st.To)
		if len(idx) == 0 {
			out.Skip = true
			return out
		}
		for _, i := range deliveryOrder(idx, st) {
			from.sent[st.To][i] = true
			if err := apply(to.doc, pick(from.log[i], st.V2), st.V2); err != nil {
				out.Err = "error"
				break
			}
		}
		touched = []int{st.To}
	case StepSync:
		from, to := r.peers[st.From], r.peers[st.To]
		sv := ygo.EncodeStateVector(to.doc)
		var u []byte
		var err error
		if st.V2 {
			u, err = ygo.EncodeDiffV2(from.doc, sv)
		} else {
			u, err = ygo.EncodeDiff(from.doc, sv)
		}
		if err == nil {
			out.Sent = hex.EncodeToString(u)
			err = apply(to.doc, u, st.V2)
		}
		if err != nil {
			out.Err = "error"
		}
		touched = []int{st.To}
	case StepMerge, StepDiff:
		from, to := r.peers[st.From], r.peers[st.To]
		idx := undelivered(from, st.To)
		if len(idx) == 0 {
			out.Skip = true
			return out
		}
		ups := make([][]byte, len(idx))
		for k, i := range idx {
			ups[k] = pick(from.log[i], st.V2)
			from.sent[st.To][i] = true
		}
		var u []byte
		var err error
		if st.V2 {
			u, err = ygo.MergeUpdatesV2(ups)
		} else {
			u, err = ygo.MergeUpdates(ups)
		}
		if err == nil && st.K == StepDiff {
			sv := ygo.EncodeStateVector(to.doc)
			if st.V2 {
				u, err = ygo.DiffUpdateV2(u, sv)
			} else {
				u, err = ygo.DiffUpdate(u, sv)
			}
		}
		if err == nil {
			out.Sent = hex.EncodeToString(u)
			err = apply(to.doc, u, st.V2)
		}
		if err != nil {
			out.Err = "error"
		}
		touched = []int{st.To}
	case StepFinal:
		for round := 0; round < 10; round++ {
			changed := false
			for a := range r.peers {
				for b := range r.peers {
					if a == b {
						continue
					}
					sv := ygo.EncodeStateVector(r.peers[b].doc)
					u, err := ygo.EncodeDiff(r.peers[a].doc, sv)
					if err == nil {
						err = apply(r.peers[b].doc, u, false)
					}
					if err != nil {
						out.Err = "error"
						return out
					}
					if string(ygo.EncodeStateVector(r.peers[b].doc)) != string(sv) {
						changed = true
					}
				}
			}
			if !changed {
				break
			}
		}
		for i := range r.peers {
			touched = append(touched, i)
		}
	case StepUnwind:
		for i, p := range r.peers {
			if p.um == nil {
				continue
			}
			u, d := 0, 0
			for u < unwindMax && p.um.Undo() {
				u++
			}
			for d < unwindMax && p.um.Redo() {
				d++
			}
			out.Depths = append(out.Depths, u, d)
			touched = append(touched, i)
		}
	default:
		out.Err = "unknown step " + st.K
		return out
	}
	for _, i := range touched {
		out.Peers = append(out.Peers, r.state(i))
	}
	return out
}

// unwindMax bounds an unwind step's undos and redos per peer.
const unwindMax = 1000

func pick(e event, v2 bool) []byte {
	if v2 {
		return e.v2
	}
	return e.v1
}

func apply(d *ygo.Doc, u []byte, v2 bool) error {
	if v2 {
		return ygo.ApplyUpdateV2WithOrigin(d, u, "remote")
	}
	return ygo.ApplyUpdateWithOrigin(d, u, "remote")
}

func undelivered(from *peer, to int) []int {
	var out []int
	for i := range from.log {
		if !from.sent[to][i] {
			out = append(out, i)
		}
	}
	return out
}

// deliveryOrder is the order a deliver step applies the undelivered events.
func deliveryOrder(idx []int, st Step) []int {
	out := append([]int(nil), idx...)
	switch st.Order {
	case OrderRev:
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	case OrderShuf, OrderPart:
		rnd := mulberry32(st.R)
		for i := len(out) - 1; i > 0; i-- {
			j := int(rnd() % uint32(i+1))
			out[i], out[j] = out[j], out[i]
		}
		if st.Order == OrderPart {
			out = out[:1+st.N%len(out)]
		}
	case OrderDup:
		d := make([]int, 0, 2*len(out))
		for _, i := range out {
			d = append(d, i, i)
		}
		out = d
	case OrderLate:
		if len(out) > 1 {
			out = out[1:]
		}
	}
	return out
}

// mulberry32 is the PRNG both sides use for shuffles.
func mulberry32(seed uint32) func() uint32 {
	a := seed
	return func() uint32 {
		a += 0x6D2B79F5
		t := a
		t = (t ^ t>>15) * (t | 1)
		t ^= t + (t^t>>7)*(t|61)
		return t ^ t>>14
	}
}

func (r *runner) state(i int) PeerState {
	p := r.peers[i]
	ps := PeerState{
		Peer:    i,
		V1:      sum(ygo.EncodeStateAsUpdate(p.doc)),
		V2:      sum(ygo.EncodeStateAsUpdateV2(p.doc)),
		SV:      hex.EncodeToString(ygo.EncodeStateVector(p.doc)),
		Pending: ygo.HasPending(p.doc),
		View:    string(anyval.AppendStringify(nil, view(p))),
	}
	if p.um != nil {
		ps.CanUndo, ps.CanRedo = p.um.CanUndo(), p.um.CanRedo()
	}
	return ps
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

// resolve finds op's target on p, or false when its path does not lead to
// the kind it wants.
func resolve(p *peer, op Op) (any, bool) {
	var cur any
	switch op.Root {
	case "t":
		cur = p.t
	case "a":
		cur = p.a
	case "m":
		cur = p.m
	case "x":
		cur = p.x
	default:
		return nil, false
	}
	for _, seg := range op.Path {
		hint := -1
		if len(seg) > 1 && seg[0] == '#' {
			n, err := strconv.Atoi(seg[1:])
			if err != nil {
				return nil, false
			}
			hint = n
		}
		switch c := cur.(type) {
		case *ygo.Map:
			if hint >= 0 {
				return nil, false
			}
			cur = c.Get(seg)
		case *ygo.Array:
			if hint < 0 || c.Len() == 0 {
				return nil, false
			}
			cur = c.Get(uint64(hint) % c.Len())
		case *ygo.XmlFragment:
			if hint < 0 || c.Length() == 0 {
				return nil, false
			}
			cur = c.Get(uint64(hint) % c.Length())
		case *ygo.XmlElement:
			if hint < 0 || c.Length() == 0 {
				return nil, false
			}
			cur = c.Get(uint64(hint) % c.Length())
		default:
			return nil, false
		}
	}
	switch op.Want {
	case WantText:
		_, ok := cur.(*ygo.Text)
		return cur, ok
	case WantXmlText:
		_, ok := cur.(*ygo.XmlText)
		return cur, ok
	case WantArray:
		_, ok := cur.(*ygo.Array)
		return cur, ok
	case WantMap:
		_, ok := cur.(*ygo.Map)
		return cur, ok
	case WantXml:
		switch cur.(type) {
		case *ygo.XmlFragment, *ygo.XmlElement:
			return cur, true
		}
	case WantXmlEl:
		_, ok := cur.(*ygo.XmlElement)
		return cur, ok
	}
	return nil, false
}

func insIdx(h int, l uint64) uint64 { return uint64(h) % (l + 1) }

// delRange is the range a delete or format of hint (h, n) covers on a
// length l, false when l is 0.
func delRange(h, n int, l uint64) (uint64, uint64, bool) {
	if l == 0 {
		return 0, 0, false
	}
	idx := uint64(h) % l
	return idx, min(uint64(max(n, 1)), l-idx), true
}

func (r *runner) op(p *peer, txn *ygo.TransactionMut, op Op) error {
	target, ok := resolve(p, op)
	if r.stats != nil {
		k := op.Want + "/" + op.O
		if ok {
			r.stats.Ops[k]++
			if len(op.Path) > 0 {
				r.stats.Nested++
			}
		} else {
			r.stats.NoOps[k]++
		}
	}
	if !ok {
		return nil
	}
	switch t := target.(type) {
	case *ygo.Text:
		return textOp(t, txn, op)
	case *ygo.XmlText:
		return textOp(&t.Text, txn, op)
	case *ygo.Array:
		l := t.Len()
		switch op.O {
		case OpIns, OpPush:
			v, err := DecodeValue(op.V)
			if err != nil {
				return err
			}
			vals, _ := v.([]any)
			if op.O == OpPush {
				t.Push(txn, vals...)
			} else {
				t.InsertRange(txn, insIdx(op.Pos, l), vals)
			}
		case OpDel:
			if i, n, ok := delRange(op.Pos, op.Len, l); ok {
				t.Delete(txn, i, n)
			}
		case OpInsMap:
			t.InsertMap(txn, insIdx(op.Pos, l))
		case OpInsArr:
			t.InsertArray(txn, insIdx(op.Pos, l))
		case OpInsText:
			t.InsertText(txn, insIdx(op.Pos, l))
		default:
			return fmt.Errorf("difffuzz: array op %q", op.O)
		}
	case *ygo.Map:
		switch op.O {
		case OpSet:
			v, err := DecodeValue(op.V)
			if err != nil {
				return err
			}
			t.Set(txn, op.Key, v)
		case OpMapDel:
			t.Delete(txn, op.Key)
		case OpSetMap:
			t.SetMap(txn, op.Key)
		case OpSetArr:
			t.SetArray(txn, op.Key)
		case OpSetText:
			t.SetText(txn, op.Key)
		case OpClear:
			t.Clear(txn)
		default:
			return fmt.Errorf("difffuzz: map op %q", op.O)
		}
	case *ygo.XmlFragment:
		return xmlOp(t.Length(), op, txn,
			func(i uint64, name string) { t.InsertXmlElement(txn, i, name) },
			func(i uint64) { t.InsertXmlText(txn, i) },
			func(i, n uint64) { t.Delete(txn, i, n) })
	case *ygo.XmlElement:
		switch op.O {
		case OpSetAttr:
			t.SetAttribute(txn, op.Key, op.S)
			return nil
		case OpRmAttr:
			t.RemoveAttribute(txn, op.Key)
			return nil
		}
		return xmlOp(t.Length(), op, txn,
			func(i uint64, name string) { t.InsertXmlElement(txn, i, name) },
			func(i uint64) { t.InsertXmlText(txn, i) },
			func(i, n uint64) { t.Delete(txn, i, n) })
	}
	return nil
}

func xmlOp(l uint64, op Op, _ *ygo.TransactionMut, insEl func(uint64, string), insText func(uint64), del func(uint64, uint64)) error {
	switch op.O {
	case OpInsEl:
		insEl(insIdx(op.Pos, l), op.S)
	case OpInsText:
		insText(insIdx(op.Pos, l))
	case OpDel:
		if i, n, ok := delRange(op.Pos, op.Len, l); ok {
			del(i, n)
		}
	default:
		return fmt.Errorf("difffuzz: xml op %q", op.O)
	}
	return nil
}

func textOp(t *ygo.Text, txn *ygo.TransactionMut, op Op) error {
	l := t.Length()
	attrs, err := decodeAttrs(op.Attrs)
	if err != nil {
		return err
	}
	switch op.O {
	case OpIns:
		if attrs == nil {
			return t.Insert(txn, insIdx(op.Pos, l), op.S)
		}
		return t.InsertWithAttributes(txn, insIdx(op.Pos, l), op.S, attrs)
	case OpDel:
		if i, n, ok := delRange(op.Pos, op.Len, l); ok {
			return t.Delete(txn, i, n)
		}
	case OpFmt:
		if i, n, ok := delRange(op.Pos, op.Len, l); ok {
			return t.Format(txn, i, n, attrs)
		}
	case OpEmb:
		v, err := DecodeValue(op.V)
		if err != nil {
			return err
		}
		if attrs == nil {
			return t.InsertEmbed(txn, insIdx(op.Pos, l), v)
		}
		return t.InsertEmbedWithAttributes(txn, insIdx(op.Pos, l), v, attrs)
	case OpDelta:
		ops, err := resolveDelta(op.Delta, l)
		if err != nil {
			return err
		}
		if len(ops) == 0 {
			return nil
		}
		return t.ApplyDelta(txn, ops)
	default:
		return fmt.Errorf("difffuzz: text op %q", op.O)
	}
	return nil
}

// resolveDelta turns hints into a delta on a text of length l.
func resolveDelta(steps []DeltaStep, l uint64) ([]ygo.DeltaOp, error) {
	var out []ygo.DeltaOp
	c := uint64(0)
	for _, d := range steps {
		attrs, err := decodeAttrs(d.A)
		if err != nil {
			return nil, err
		}
		switch {
		case d.R != nil:
			if l-c == 0 {
				continue
			}
			n := 1 + uint64(*d.R)%(l-c)
			out = append(out, ygo.DeltaOp{Retain: n, Attributes: attrs})
			c += n
		case d.D != nil:
			if l-c == 0 {
				continue
			}
			n := 1 + uint64(*d.D)%(l-c)
			out = append(out, ygo.DeltaOp{Delete: n})
			l -= n
		case d.I != nil:
			out = append(out, ygo.DeltaOp{Insert: *d.I, Attributes: attrs})
			n := utf16Len(*d.I)
			c += n
			l += n
		case d.E != nil:
			v, err := DecodeValue(d.E)
			if err != nil {
				return nil, err
			}
			out = append(out, ygo.DeltaOp{Embed: v, Attributes: attrs})
			c++
			l++
		}
	}
	return out, nil
}

func utf16Len(s string) uint64 {
	n := uint64(0)
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// view is the peer's four roots as a tagged tree, serialised as
// JSON.stringify writes it.
func view(p *peer) any {
	return map[string]any{
		"t": textView(p.t),
		"a": arrayView(p.a),
		"m": mapView(p.m),
		"x": xmlView(p.x),
	}
}

func valueView(v any) any {
	switch x := v.(type) {
	case *ygo.Map:
		return map[string]any{"$map": mapView(x)}
	case *ygo.Array:
		return map[string]any{"$array": arrayView(x)}
	case *ygo.Text:
		return map[string]any{"$text": textView(x)}
	case *ygo.XmlText:
		return map[string]any{"$xt": textView(&x.Text)}
	case *ygo.XmlElement:
		return xmlView(x)
	case *ygo.XmlFragment:
		return xmlView(x)
	}
	return v
}

func mapView(m *ygo.Map) any {
	out := map[string]any{}
	m.Range(func(k string, v any) bool {
		out[k] = valueView(v)
		return true
	})
	return out
}

func arrayView(a *ygo.Array) any {
	out := []any{}
	a.Range(func(_ uint64, v any) bool {
		out = append(out, valueView(v))
		return true
	})
	return out
}

func textView(t *ygo.Text) any {
	out := []any{}
	for _, d := range t.ToDelta() {
		op := map[string]any{}
		if d.Embed != nil {
			op["insert"] = valueView(d.Embed)
		} else {
			op["insert"] = d.Insert
		}
		if len(d.Attributes) > 0 {
			op["attributes"] = map[string]any(d.Attributes)
		}
		out = append(out, op)
	}
	return out
}

func xmlView(n any) any {
	var children []any
	add := func(_ uint64, c any) bool {
		children = append(children, valueView(c))
		return true
	}
	switch x := n.(type) {
	case *ygo.XmlFragment:
		x.Range(add)
		if children == nil {
			children = []any{}
		}
		return map[string]any{"$xml": children}
	case *ygo.XmlElement:
		x.Range(add)
		if children == nil {
			children = []any{}
		}
		attrs := map[string]any{}
		for k, v := range x.Attributes() {
			// Both reads of an attribute must agree (yjs has one, typeMapGet).
			if g, ok := x.GetAttribute(k); !ok || g != v {
				v = "!GetAttribute=" + g + " Attributes=" + v
			}
			attrs[k] = v
		}
		return map[string]any{"$el": x.NodeName(), "attrs": attrs, "ch": children}
	}
	return nil
}

// observe puts deep observers on peer i's array and map and an observer on
// its text (ygo's XML types have none).
func (r *runner) observe(i int, p *peer) {
	rec := func(root string, evs []any) {
		if r.cur == nil {
			return
		}
		r.batch++
		for _, ev := range evs {
			o := ObsEvent{Batch: r.batch, Peer: i, Root: root}
			switch e := ev.(type) {
			case *ygo.MapEvent:
				o.Path, o.Kind, o.Change = pathJSON(e.Path), "map", mapChange(e.Keys)
			case *ygo.ArrayEvent:
				o.Path, o.Kind, o.Change = pathJSON(e.Path), "array", arrayChange(e.Delta)
			case *ygo.TextEvent:
				o.Path, o.Kind, o.Change = pathJSON(e.Path), "text", textChange(e.ReadDelta())
			default:
				o.Kind = fmt.Sprintf("%T", ev)
			}
			r.cur.Obs = append(r.cur.Obs, o)
		}
	}
	p.a.ObserveDeep(func(evs []any) { rec("a", evs) })
	p.m.ObserveDeep(func(evs []any) { rec("m", evs) })
	p.t.Observe(func(e *ygo.TextEvent) { rec("t", []any{e}) })
}

func pathJSON(path []any) string {
	out := make([]any, len(path))
	for i, seg := range path {
		switch x := seg.(type) {
		case int:
			out[i] = int64(x)
		case uint64:
			out[i] = int64(x)
		default:
			out[i] = x
		}
	}
	return string(anyval.AppendStringify(nil, out))
}

// eventValue is a value in an event: a shared type by its kind and the ID
// of the item that made it (its content at callback time is not part of
// the change).
func eventValue(v any) any {
	kind := ""
	var br *ygo.Branch
	switch x := v.(type) {
	case *ygo.Map:
		kind, br = "$map", x.Branch()
	case *ygo.Array:
		kind, br = "$array", x.Branch()
	case *ygo.Text:
		kind, br = "$text", x.Branch()
	case *ygo.XmlText:
		kind, br = "$xt", x.Branch()
	case *ygo.XmlElement:
		kind, br = "$xml", x.Branch()
	case *ygo.XmlFragment:
		kind, br = "$xml", x.Branch()
	default:
		return v
	}
	if br == nil || br.Item == nil {
		return kind
	}
	return fmt.Sprintf("%s@%d:%d", kind, br.Item.ID.Client, br.Item.ID.Clock)
}

func mapChange(keys map[string]ygo.KeyChange) string {
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]any, len(names))
	for i, k := range names {
		c := keys[k]
		e := map[string]any{"key": k, "action": c.Action}
		if c.Action != "add" {
			e["old"] = eventValue(c.OldValue)
		}
		out[i] = e
	}
	return string(anyval.AppendStringify(nil, out))
}

func arrayChange(delta []ygo.ArrayDeltaOp) string {
	out := make([]any, 0, len(delta))
	for _, d := range delta {
		switch {
		case d.Insert != nil:
			ins := make([]any, len(d.Insert))
			for i, v := range d.Insert {
				ins[i] = eventValue(v)
			}
			out = append(out, map[string]any{"insert": ins})
		case d.Delete > 0:
			out = append(out, map[string]any{"delete": int64(d.Delete)})
		default:
			out = append(out, map[string]any{"retain": int64(d.Retain)})
		}
	}
	return string(anyval.AppendStringify(nil, out))
}

func textChange(delta []ygo.DeltaOp) string {
	out := make([]any, 0, len(delta))
	for _, d := range delta {
		op := map[string]any{}
		switch {
		case d.Retain > 0:
			op["retain"] = int64(d.Retain)
		case d.Delete > 0:
			op["delete"] = int64(d.Delete)
		case d.Embed != nil:
			op["insert"] = eventValue(d.Embed)
		default:
			op["insert"] = d.Insert
		}
		if len(d.Attributes) > 0 {
			op["attributes"] = map[string]any(d.Attributes)
		}
		out = append(out, op)
	}
	return string(anyval.AppendStringify(nil, out))
}
