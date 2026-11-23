package difffuzz

import (
	"encoding/json"
	"math/rand/v2"
	"sort"
	"strconv"
)

// The values and ops the generator leaves out, each a documented difference
// between ygo and yjs rather than a bug (README "Strings and values",
// docs/tech-debt.md):
//   - whole-number floats set from Go (written as float64, yjs writes an
//     integer): floats here always have a fraction;
//   - lone surrogates and invalid UTF-8;
//   - object keys out of the order ygo writes a Go-built object, and a
//     __proto__ key;
//   - undefined, BigInt and -0 (their own fixtures cover them);
//   - zero retains in a delta ("Rich-text inserts: differences from yjs
//     that stay" (1)), a null embed (yjs throws), shared types as embeds;
//   - attribute keys other than ascending, and attribute objects shared
//     between ops;
//   - XML attribute values other than ASCII strings, non-ASCII XML names;
//   - subdocuments (random GUIDs), relative positions, snapshots.

var clientPool = []uint64{1, 2, 3, 7, 100, 101, 1<<31 - 1, 1<<32 + 5, 1<<53 - 1}

// Alphabets of 1-, 2-, 3- and 4-byte UTF-8 (the last two-unit UTF-16, so
// positions cut inside surrogate pairs).
var alphabets = [][]rune{[]rune("abcde"), []rune("αβγ"), []rune("日本語"), []rune("😀🎉🙂")}

var (
	mapKeys   = []string{"k0", "k1", "k2", "k3"}
	objKeys   = []string{"0", "1", "a", "b", "c", "x"}
	attrKeys  = []string{"bold", "color", "italic", "link"}
	xmlTags   = []string{"p", "div", "span"}
	xmlAttrs  = []string{"class", "id", "style"}
	attrColor = []string{"red", "blue"}
)

// GenOptions changes what Generate draws.
type GenOptions struct {
	// BMPOnly leaves characters outside the BMP out of strings. Two peers
	// that cut one surrogate pair in different places end apart in yjs
	// (each replaces the halves it cut with U+FFFD), so a check of
	// convergence without yjs to compare with needs strings without pairs.
	BMPOnly bool
}

// Generate builds the scenario of a seed.
func Generate(seed uint64) Scenario { return GenerateWith(seed, GenOptions{}) }

// GenerateWith is Generate with options.
func GenerateWith(seed uint64, opts GenOptions) Scenario {
	r := rand.New(rand.NewPCG(seed, 0x6b32646966667a7a))
	g := &gen{r: r, opts: opts}
	n := 2 + r.IntN(4)
	perm := r.Perm(len(clientPool))
	noUndo := r.IntN(4) == 0
	s := Scenario{Seed: seed, Observe: r.IntN(10) < 3}
	for i := 0; i < n; i++ {
		s.Peers = append(s.Peers, Peer{
			Client: clientPool[perm[i]],
			GC:     r.IntN(5) != 0,
			Undo:   !noUndo && r.IntN(5) < 3,
		})
	}
	steps := 30 + r.IntN(120)
	for i := 0; i < steps; i++ {
		s.Steps = append(s.Steps, g.step(s.Peers))
	}
	s.Steps = append(s.Steps, Step{K: StepFinal}, Step{K: StepUnwind})
	return s
}

type gen struct {
	r    *rand.Rand
	opts GenOptions
}

func (g *gen) pick(n int) int { return g.r.IntN(n) }

func (g *gen) step(peers []Peer) Step {
	n := len(peers)
	pair := func() (int, int) {
		a := g.pick(n)
		b := (a + 1 + g.pick(n-1)) % n
		return a, b
	}
	var undoPeers []int
	for i, p := range peers {
		if p.Undo {
			undoPeers = append(undoPeers, i)
		}
	}
	x := g.pick(100)
	switch {
	case x < 55:
		st := Step{K: StepTxn, P: g.pick(n)}
		for k := 0; k < 1+g.pick(3); k++ {
			st.Ops = append(st.Ops, g.op())
		}
		return st
	case x < 70:
		a, b := pair()
		orders := []string{OrderIn, OrderIn, OrderRev, OrderShuf, OrderShuf, OrderDup, OrderPart, OrderPart, OrderLate, OrderLate}
		return Step{K: StepDeliver, From: a, To: b, V2: g.pick(2) == 0, Order: orders[g.pick(len(orders))], N: g.pick(8), R: g.r.Uint32()}
	case x < 76:
		a, b := pair()
		return Step{K: StepSync, From: a, To: b, V2: g.pick(2) == 0}
	case x < 81:
		a, b := pair()
		return Step{K: StepMerge, From: a, To: b, V2: g.pick(2) == 0}
	case x < 85:
		a, b := pair()
		return Step{K: StepDiff, From: a, To: b, V2: g.pick(2) == 0}
	}
	if len(undoPeers) == 0 {
		st := Step{K: StepTxn, P: g.pick(n)}
		st.Ops = append(st.Ops, g.op())
		return st
	}
	p := undoPeers[g.pick(len(undoPeers))]
	switch y := g.pick(15); {
	case y < 6:
		return Step{K: StepUndo, P: p}
	case y < 10:
		return Step{K: StepRedo, P: p}
	case y < 14:
		return Step{K: StepStop, P: p}
	default:
		return Step{K: StepUClear, P: p}
	}
}

func (g *gen) op() Op {
	roots := []string{"t", "a", "m", "x"}
	root := roots[g.pick(len(roots))]
	op := Op{Root: root}
	depth := 0
	if root != "t" {
		switch d := g.pick(10); {
		case d < 6:
			depth = 0
		case d < 9:
			depth = 1
		default:
			depth = 2
		}
	}
	for i := 0; i < depth; i++ {
		first := i == 0
		switch {
		case first && root == "m":
			op.Path = append(op.Path, mapKeys[g.pick(len(mapKeys))])
		case first:
			op.Path = append(op.Path, "#"+strconv.Itoa(g.pick(20)))
		case g.pick(2) == 0:
			op.Path = append(op.Path, mapKeys[g.pick(len(mapKeys))])
		default:
			op.Path = append(op.Path, "#"+strconv.Itoa(g.pick(20)))
		}
	}
	switch {
	case depth == 0 && root == "t":
		op.Want = WantText
	case depth == 0 && root == "a":
		op.Want = WantArray
	case depth == 0 && root == "m":
		op.Want = WantMap
	case depth == 0:
		op.Want = WantXml
	case root == "x":
		op.Want = []string{WantXml, WantXmlEl, WantXmlText}[g.pick(3)]
	default:
		op.Want = []string{WantText, WantArray, WantMap}[g.pick(3)]
	}
	g.fill(&op)
	return op
}

func (g *gen) fill(op *Op) {
	switch op.Want {
	case WantText, WantXmlText:
		g.textOp(op)
	case WantArray:
		switch g.pick(7) {
		case 0, 1:
			op.O, op.Pos, op.V = OpIns, g.pick(50), g.values()
		case 2:
			op.O, op.V = OpPush, g.values()
		case 3:
			op.O, op.Pos, op.Len = OpDel, g.pick(50), 1+g.pick(3)
		case 4:
			op.O, op.Pos = OpInsMap, g.pick(50)
		case 5:
			op.O, op.Pos = OpInsArr, g.pick(50)
		default:
			op.O, op.Pos = OpInsText, g.pick(50)
		}
	case WantMap:
		switch x := g.pick(20); {
		case x < 9:
			op.O, op.Key, op.V = OpSet, mapKeys[g.pick(len(mapKeys))], g.value(0)
		case x < 13:
			op.O, op.Key = OpMapDel, mapKeys[g.pick(len(mapKeys))]
		case x < 15:
			op.O, op.Key = OpSetMap, mapKeys[g.pick(len(mapKeys))]
		case x < 17:
			op.O, op.Key = OpSetArr, mapKeys[g.pick(len(mapKeys))]
		case x < 19:
			op.O, op.Key = OpSetText, mapKeys[g.pick(len(mapKeys))]
		default:
			op.O = OpClear
		}
	case WantXml:
		switch g.pick(5) {
		case 0, 1:
			op.O, op.Pos, op.S = OpInsEl, g.pick(20), xmlTags[g.pick(len(xmlTags))]
		case 2, 3:
			op.O, op.Pos = OpInsText, g.pick(20)
		default:
			op.O, op.Pos, op.Len = OpDel, g.pick(20), 1+g.pick(2)
		}
	case WantXmlEl:
		op.Key = xmlAttrs[g.pick(len(xmlAttrs))]
		if g.pick(5) == 0 {
			op.O = OpRmAttr
		} else {
			op.O, op.S = OpSetAttr, g.ascii(1+g.pick(5))
		}
	}
}

func (g *gen) textOp(op *Op) {
	switch x := g.pick(20); {
	case x < 8:
		op.O, op.Pos, op.S = OpIns, g.pick(60), g.str(1+g.pick(5))
		if g.pick(3) == 0 {
			op.Attrs = g.attrs(false)
		}
	case x < 12:
		op.O, op.Pos, op.Len = OpDel, g.pick(60), 1+g.pick(4)
	case x < 16:
		op.O, op.Pos, op.Len = OpFmt, g.pick(60), 1+g.pick(6)
		op.Attrs = g.attrs(true)
	case x < 18:
		op.O, op.Pos, op.V = OpEmb, g.pick(60), g.object(1)
		if g.pick(2) == 0 {
			op.Attrs = g.attrs(false)
		}
	default:
		op.O = OpDelta
		for k := 0; k < 1+g.pick(4); k++ {
			var d DeltaStep
			switch g.pick(4) {
			case 0:
				h := g.pick(20)
				d.R = &h
				if g.pick(2) == 0 {
					d.A = g.attrs(true)
				}
			case 1:
				h := g.pick(5)
				d.D = &h
			case 2:
				s := g.str(1 + g.pick(4))
				d.I = &s
				if g.pick(3) == 0 {
					d.A = g.attrs(false)
				}
			default:
				d.E = g.object(1)
				if g.pick(3) == 0 {
					d.A = g.attrs(false)
				}
			}
			op.Delta = append(op.Delta, d)
		}
	}
}

// attrs gives 0-2 attributes in ascending key order; withNull allows null
// values (a format removing an attribute). An insert's attributes may be
// empty (given, but none).
func (g *gen) attrs(withNull bool) *[]KV {
	keys := append([]string(nil), attrKeys...)
	g.r.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	keys = keys[:g.pick(3)]
	sort.Strings(keys)
	out := []KV{}
	for _, k := range keys {
		var v json.RawMessage
		switch x := g.pick(6); {
		case withNull && x == 0:
			v = tagNull()
		case x < 3:
			v = tagBool(true)
		case x < 5:
			v = tagStr(attrColor[g.pick(len(attrColor))])
		default:
			v = g.object(1)
		}
		out = append(out, KV{K: k, V: v})
	}
	return &out
}

func (g *gen) values() json.RawMessage {
	items := make([]json.RawMessage, 1+g.pick(3))
	for i := range items {
		items[i] = g.value(0)
	}
	return tagArr(items)
}

func (g *gen) value(depth int) json.RawMessage {
	max := 7
	if depth >= 2 {
		max = 5
	}
	switch g.pick(max) {
	case 0:
		return tagInt(int64(g.pick(2001) - 1000))
	case 1:
		return tagFloat(g.float())
	case 2:
		return tagStr(g.str(g.pick(5)))
	case 3:
		return tagBool(g.pick(2) == 0)
	case 4:
		return tagNull()
	case 5:
		items := make([]json.RawMessage, g.pick(4))
		for i := range items {
			items[i] = g.value(depth + 1)
		}
		return tagArr(items)
	default:
		return g.object(depth + 1)
	}
}

// float has a fraction: some exactly float32 (written as float32 by both),
// some not.
func (g *gen) float() float64 {
	k := float64(g.pick(4000) - 2000)
	if g.pick(2) == 0 {
		return k + 0.25 + 0.5*float64(g.pick(2))
	}
	return k + 0.1 + float64(g.pick(1000))/1013.0
}

func (g *gen) object(depth int) json.RawMessage {
	keys := append([]string(nil), objKeys...)
	g.r.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	keys = keys[:g.pick(4)]
	keys = goObjectOrder(keys)
	vals := make([]json.RawMessage, len(keys))
	for i := range keys {
		vals[i] = g.value(depth + 1)
	}
	return tagObj(keys, vals)
}

// goObjectOrder is the order ygo writes a Go-built object's keys: array
// index keys first, ascending, then the rest sorted (anyval.SortedJSOrder).
func goObjectOrder(keys []string) []string {
	out := append([]string(nil), keys...)
	sort.Slice(out, func(i, j int) bool {
		ai, aok := arrayIndex(out[i])
		bi, bok := arrayIndex(out[j])
		switch {
		case aok && bok:
			return ai < bi
		case aok != bok:
			return aok
		}
		return out[i] < out[j]
	})
	return out
}

func arrayIndex(k string) (uint64, bool) {
	if k == "" || (len(k) > 1 && k[0] == '0') {
		return 0, false
	}
	n, err := strconv.ParseUint(k, 10, 32)
	return n, err == nil && n < 1<<32-1
}

func (g *gen) str(n int) string {
	k := len(alphabets)
	if g.opts.BMPOnly {
		k-- // the last alphabet is outside the BMP
	}
	a := alphabets[g.pick(k)]
	out := make([]rune, n)
	for i := range out {
		out[i] = a[g.pick(len(a))]
	}
	return string(out)
}

func (g *gen) ascii(n int) string {
	const a = "abcXYZ-_"
	out := make([]byte, n)
	for i := range out {
		out[i] = a[g.pick(len(a))]
	}
	return string(out)
}
