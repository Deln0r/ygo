// Command perfsmoke times five workloads on ygo's public API, for the
// perf-smoke CI job (.github/workflows/perf.yml), which builds it against
// two commits and compares them. It prints one line per workload,
// "name ns_per_op", and exits non-zero when a workload's document does not
// end as its plain-string model does, so a build that does less is not
// mistaken for a faster one.
//
// The edits are synthetic and shaped like the B4 trace (one author typing
// a paper): mostly single characters at a cursor that walks, backspaces,
// now and then a jump or a pasted burst. Seeded, so both builds replay the
// same edits.
package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"runtime"
	"time"

	ygo "github.com/Deln0r/ygo"
)

type edit struct {
	pos uint64
	del uint64
	ins string
}

// trace returns n edits and the text they leave, applied to a plain
// string (the model every workload is checked against).
func trace(n int, seed uint64) ([]edit, string) {
	r := rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15))
	const letters = "etaoinshrdlucmfwypvbgkjqxz      ,.\n"
	text := []rune{}
	cursor := 0
	edits := make([]edit, 0, n)
	for len(edits) < n {
		p := r.Float64()
		switch {
		case p < 0.05 && len(text) > 0:
			cursor = r.IntN(len(text) + 1)
			continue
		case p < 0.15 && cursor > 0:
			edits = append(edits, edit{pos: uint64(cursor - 1), del: 1})
			text = append(text[:cursor-1], text[cursor:]...)
			cursor--
		default:
			k := 1
			if r.Float64() < 0.02 {
				k = 20 + r.IntN(61)
			}
			b := make([]rune, k)
			for i := range b {
				b[i] = rune(letters[r.IntN(len(letters))])
			}
			edits = append(edits, edit{pos: uint64(cursor), ins: string(b)})
			text = append(text[:cursor], append(b, text[cursor:]...)...)
			cursor += k
		}
	}
	return edits, string(text)
}

func applyEdit(t *ygo.Text, txn *ygo.TransactionMut, e edit) {
	if e.del > 0 {
		_ = t.Delete(txn, e.pos, e.del)
	}
	if e.ins != "" {
		_ = t.Insert(txn, e.pos, e.ins)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "perfsmoke: "+format+"\n", args...)
	os.Exit(1)
}

// w1: every edit into one Text in one transaction (the B4 path).
func w1(edits []edit, want string) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	t := ygo.NewText(d, "t")
	txn := d.WriteTxn()
	for _, e := range edits {
		applyEdit(t, txn, e)
	}
	txn.Commit()
	if t.String() != want {
		fail("w1: the text differs from the model")
	}
}

// w2: one transaction per edit, collecting every update.
func w2(edits []edit, want string) [][]byte {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	t := ygo.NewText(d, "t")
	updates := make([][]byte, 0, len(edits))
	d.OnUpdate(func(u []byte, _ any) { updates = append(updates, u) })
	for _, e := range edits {
		txn := d.WriteTxn()
		applyEdit(t, txn, e)
		txn.Commit()
	}
	if t.String() != want {
		fail("w2: the text differs from the model")
	}
	return updates
}

// w3: w2's updates applied, in order, to a fresh document.
func w3(updates [][]byte, want string) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 2})
	t := ygo.NewText(d, "t")
	for _, u := range updates {
		if err := ygo.ApplyUpdate(d, u); err != nil {
			fail("w3: %v", err)
		}
	}
	if t.String() != want {
		fail("w3: the text differs from the model")
	}
}

// w4: the whole state encoded once and applied to a fresh document.
func w4(edits []edit, want string) func() {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	t := ygo.NewText(d, "t")
	txn := d.WriteTxn()
	for _, e := range edits {
		applyEdit(t, txn, e)
	}
	txn.Commit()
	return func() {
		u := ygo.EncodeStateAsUpdate(d)
		e := ygo.NewDocWithOptions(ygo.Options{ClientID: 2})
		te := ygo.NewText(e, "t")
		if err := ygo.ApplyUpdate(e, u); err != nil {
			fail("w4: %v", err)
		}
		if te.String() != want {
			fail("w4: the text differs from the model")
		}
	}
}

// w5setup: four writers share the edits, each applying its turn at a
// position clamped to its own text, and exchange their updates every 50
// edits. It returns every update they made and the text they converge on.
func w5setup(edits []edit) ([][]byte, string) {
	const writers = 4
	docs := make([]*ygo.Doc, writers)
	texts := make([]*ygo.Text, writers)
	outbox := make([][][]byte, writers)
	var all [][]byte
	for i := range docs {
		docs[i] = ygo.NewDocWithOptions(ygo.Options{ClientID: uint64(101 + i)})
		texts[i] = ygo.NewText(docs[i], "t")
		k := i
		docs[i].OnUpdate(func(u []byte, origin any) {
			if origin == nil {
				outbox[k] = append(outbox[k], u)
				all = append(all, u)
			}
		})
	}
	exchange := func() {
		for i := range docs {
			for j := range docs {
				if i == j {
					continue
				}
				for _, u := range outbox[i] {
					if err := ygo.ApplyUpdateWithOrigin(docs[j], u, "peer"); err != nil {
						fail("w5: %v", err)
					}
				}
			}
		}
		for i := range outbox {
			outbox[i] = nil
		}
	}
	for n, e := range edits {
		w := n % writers
		l := texts[w].Length()
		e.pos = min(e.pos, l)
		if e.del > 0 && e.pos+e.del > l {
			e.del = 0
		}
		txn := docs[w].WriteTxn()
		applyEdit(texts[w], txn, e)
		txn.Commit()
		if n%50 == 49 {
			exchange()
		}
	}
	exchange()
	want := texts[0].String()
	for i := 1; i < writers; i++ {
		if texts[i].String() != want {
			fail("w5: the writers did not converge")
		}
	}
	return all, want
}

// w5: the writers' updates applied to a fresh document in a shuffled
// order, so blocks wait on others and the queue drains by retries.
func w5(updates [][]byte, want string, seed uint64) {
	r := rand.New(rand.NewPCG(seed, 7))
	order := r.Perm(len(updates))
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 200})
	t := ygo.NewText(d, "t")
	for _, i := range order {
		if err := ygo.ApplyUpdate(d, updates[i]); err != nil {
			fail("w5: %v", err)
		}
	}
	if t.String() != want || ygo.HasPending(d) {
		fail("w5: the shuffled replay did not converge")
	}
}

// measure runs f until at least minDur has passed and returns its
// fastest run in ns. A short workload runs many times in a sample, and a
// collection that lands in one of them says nothing about the code.
func measure(f func(), minDur time.Duration) int64 {
	runtime.GC()
	best := int64(math.MaxInt64)
	start := time.Now()
	for {
		t0 := time.Now()
		f()
		if d := time.Since(t0).Nanoseconds(); d < best {
			best = d
		}
		if time.Since(start) >= minDur {
			return best
		}
	}
}

func main() {
	n := flag.Int("edits", 20000, "number of edits")
	seed := flag.Uint64("seed", 1, "trace seed")
	minDur := flag.Duration("min", 200*time.Millisecond, "minimum time per workload sample")
	shuffled := flag.Int("w5-updates", 6000, "edits shared by the four writers of w5")
	only := flag.String("only", "", "run one workload (w1..w5); all when empty")
	flag.Parse()
	edits, want := trace(*n, *seed)
	run := func(name string, f func()) {
		if *only == "" || *only == name {
			f() // warm-up, untimed
			fmt.Printf("%s %d\n", name, measure(f, *minDur))
		}
	}
	run("w1", func() { w1(edits, want) })
	run("w2", func() { w2(edits, want) })
	if *only == "" || *only == "w3" {
		updates := w2(edits, want)
		run("w3", func() { w3(updates, want) })
	}
	if *only == "" || *only == "w4" {
		run("w4", w4(edits, want))
	}
	if *only == "" || *only == "w5" {
		fifth, fifthWant := w5setup(edits[:*shuffled])
		run("w5", func() { w5(fifth, fifthWant, *seed) })
	}
}
