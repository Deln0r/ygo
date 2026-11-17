package ygo_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"sync"
	"testing"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/internal/block"
	"github.com/Deln0r/ygo/internal/encoding"
)

// placeholderAny is the Any bytes of {p: 305419896}, which the tests below
// put in a document and then replace with bytes ygo's encoder does not
// write.
var placeholderAny = encoding.EncodeAny(nil, map[string]any{"p": int64(0x12345678)})

// recordAny is the Any bytes of {b: [{y: 1, x: 2}], a: 3}, keys in that
// order, as a JavaScript peer writes such a record.
var recordAny = []byte{
	encoding.AnyTagObject, 2,
	1, 'b', encoding.AnyTagArray, 1,
	encoding.AnyTagObject, 2, 1, 'y', encoding.AnyTagInteger, 1, 1, 'x', encoding.AnyTagInteger, 2,
	1, 'a', encoding.AnyTagInteger, 3,
}

// placeholderDoc returns the V1 and V2 updates of a document holding n
// records in one Array item and n more as Map values, with the
// placeholder in their place.
func placeholderDoc(t *testing.T, n int) (v1, v2 []byte) {
	t.Helper()
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	a, m := ygo.NewArray(d, "a"), ygo.NewMap(d, "m")
	txn := d.WriteTxn()
	vals := make([]any, n)
	for i := range vals {
		vals[i] = map[string]any{"p": int64(0x12345678)}
	}
	a.Push(txn, vals...)
	for i := range n {
		m.Set(txn, string(rune('A'+i)), map[string]any{"p": int64(0x12345678)})
	}
	txn.Commit()
	return ygo.EncodeStateAsUpdate(d), ygo.EncodeStateAsUpdateV2(d)
}

func itemContents(t *testing.T, u *encoding.Update) []block.Content {
	t.Helper()
	var out []block.Content
	for _, bs := range u.Blocks {
		for _, b := range bs {
			if b.Kind == encoding.WireBlockItem {
				out = append(out, b.Item.Content)
			}
		}
	}
	return out
}

// TestKeyOrder_SharedWithinOneUpdate: records of one shape decoded from one
// update share one order, also when the shape holds an array with an
// unsorted object inside, whether they are elements of one Array item or
// values of many Map entries, and the Map entries share one Orders. A
// second decode of the same bytes builds orders of its own: nothing is
// kept between updates, so nothing outlives the documents. Before, the
// orders were shared through a table of the process, which never shared a
// shape with an array in it and filled for good after 4096 shapes.
func TestKeyOrder_SharedWithinOneUpdate(t *testing.T) {
	const n = 5
	v1, v2 := placeholderDoc(t, n)
	v1 = bytes.ReplaceAll(v1, placeholderAny, recordAny)
	v2 = bytes.ReplaceAll(v2, placeholderAny, recordAny)
	decode := map[string]func() *encoding.Update{
		"V1": func() *encoding.Update {
			u, _, err := encoding.DecodeUpdate(v1)
			if err != nil {
				t.Fatal(err)
			}
			return u
		},
		"V2": func() *encoding.Update {
			u, err := encoding.DecodeUpdateV2(v2)
			if err != nil {
				t.Fatal(err)
			}
			return u
		},
	}
	for name, dec := range decode {
		t.Run(name, func(t *testing.T) {
			first := shapeOrder(t, dec(), n)
			if again := shapeOrder(t, dec(), n); again == first {
				t.Error("a second decode returned the first one's order")
			}
			if got := first.Keys; len(got) != 2 || got[0] != "b" || got[1] != "a" {
				t.Errorf("keys %q, want [b a]", got)
			}
			if inner := first.Sub[0].Sub[0]; inner == nil || inner.Keys[0] != "y" {
				t.Errorf("inner order %v, want [y x]", inner)
			}
		})
	}
}

// shapeOrder checks that every record in u has one shared order and that
// the Map entries share one Orders, and returns the order.
func shapeOrder(t *testing.T, u *encoding.Update, n int) *block.KeyOrder {
	t.Helper()
	var shared *block.KeyOrder
	var single *block.Orders
	records := 0
	for _, c := range itemContents(t, u) {
		if c.Kind != block.KindAny {
			continue
		}
		if len(c.Anys) == 1 {
			if single == nil {
				single = c.Orders
			} else if c.Orders != single {
				t.Errorf("Map entries of one shape hold different Orders")
			}
		}
		for i := range c.Anys {
			o := c.Orders.Of(i)
			if o == nil {
				t.Fatalf("record without an order")
			}
			if shared == nil {
				shared = o
			} else if o != shared {
				t.Errorf("records of one shape hold different orders")
			}
			records++
		}
	}
	if records != 2*n {
		t.Fatalf("%d records, want %d", records, 2*n)
	}
	return shared
}

// TestKeyOrder_NoOrderForSortedObjects: objects whose keys are in the
// order ygo would write anyway (array-index keys first, then sorted), at
// any depth and in every content that holds them, decode with no order at
// all, V1 and V2: they cost nothing.
func TestKeyOrder_NoOrderForSortedObjects(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 1})
	m, a, x := ygo.NewMap(d, "m"), ygo.NewArray(d, "a"), ygo.NewText(d, "t")
	txn := d.WriteTxn()
	m.Set(txn, "k", map[string]any{"0": 1, "10": 2, "a": []any{map[string]any{"c": 1, "d": 2}}, "b": map[string]any{"e": 1, "f": 2}})
	a.Push(txn, map[string]any{"x": 1}, map[string]any{"a": 1, "b": 2}, []any{map[string]any{"a": 1, "b": 2}})
	if err := x.Insert(txn, 0, "abcd"); err != nil {
		t.Fatal(err)
	}
	if err := x.InsertEmbed(txn, 4, map[string]any{"alt": "a", "src": "s"}); err != nil {
		t.Fatal(err)
	}
	if err := x.Format(txn, 0, 2, ygo.Attrs{"link": map[string]any{"href": "h", "target": "_blank"}}); err != nil {
		t.Fatal(err)
	}
	txn.Commit()
	u1, _, err := encoding.DecodeUpdate(ygo.EncodeStateAsUpdate(d))
	if err != nil {
		t.Fatal(err)
	}
	u2, err := encoding.DecodeUpdateV2(ygo.EncodeStateAsUpdateV2(d))
	if err != nil {
		t.Fatal(err)
	}
	for name, u := range map[string]*encoding.Update{"V1": u1, "V2": u2} {
		kinds := map[block.ContentKind]int{}
		for _, c := range itemContents(t, u) {
			kinds[c.Kind]++
			if c.Orders != nil {
				t.Errorf("%s: %v content %v has orders %v", name, c.Kind, c.Anys, c.Orders.List)
			}
		}
		if kinds[block.KindAny] == 0 || kinds[block.KindEmbed] == 0 || kinds[block.KindFormat] == 0 {
			t.Fatalf("%s: content kinds %v", name, kinds)
		}
	}
}

// TestMergeUpdates_SingleUpdateComesBackAsIs: yjs mergeUpdates returns a
// single update as it is, and so does ygo now, once it decodes; ygo
// re-encoded it before, which wrote a foreign update's keys in JavaScript
// order.
func TestMergeUpdates_SingleUpdateComesBackAsIs(t *testing.T) {
	var s keyOrderScenario
	for _, c := range loadKeyOrderFixtures(t) {
		if c.Name == "foreign: array-index keys after the others" {
			s = c
		}
	}
	u, u2 := mustHex(t, s.Updates[0]), mustHex(t, s.UpdatesV2[0])
	if got, err := ygo.MergeUpdates([][]byte{u}); err != nil || !bytes.Equal(got, u) {
		t.Errorf("MergeUpdates: %v, %x\nwant %x", err, got, u)
	}
	if got, err := ygo.MergeUpdatesV2([][]byte{u2}); err != nil || !bytes.Equal(got, u2) {
		t.Errorf("MergeUpdatesV2: %v, %x\nwant %x", err, got, u2)
	}
	if _, err := ygo.MergeUpdates([][]byte{{0x01, 0xff}}); err == nil {
		t.Error("MergeUpdates took a broken update")
	}
	if _, err := ygo.MergeUpdatesV2([][]byte{{0x00, 0xff}}); err == nil {
		t.Error("MergeUpdatesV2 took a broken update")
	}
}

// TestKeyOrder_NestingBound: an update whose Map value nests arrays
// deeper than 10000 levels is refused with ErrNestedTooDeep, V1 and V2,
// also a million levels deep, which used to crash the process; 10000
// levels apply and come back out unchanged.
func TestKeyOrder_NestingBound(t *testing.T) {
	v1, v2 := placeholderDoc(t, 1)
	nested := func(depth int) []byte {
		b := bytes.Repeat([]byte{encoding.AnyTagArray, 1}, depth)
		return append(b, encoding.AnyTagNull)
	}
	for _, c := range []struct {
		depth int
		ok    bool
	}{{10000, true}, {10001, false}, {1_000_000, false}} {
		in1 := bytes.ReplaceAll(v1, placeholderAny, nested(c.depth))
		in2 := bytes.ReplaceAll(v2, placeholderAny, nested(c.depth))
		d1, d2 := ygo.NewDoc(), ygo.NewDoc()
		err1, err2 := ygo.ApplyUpdate(d1, in1), ygo.ApplyUpdateV2(d2, in2)
		for name, err := range map[string]error{"V1": err1, "V2": err2} {
			if c.ok && err != nil {
				t.Errorf("%s depth %d: %v", name, c.depth, err)
			}
			if !c.ok && !errors.Is(err, ygo.ErrNestedTooDeep) {
				t.Errorf("%s depth %d: %v, want ErrNestedTooDeep", name, c.depth, err)
			}
		}
		if c.ok {
			if got := ygo.EncodeStateAsUpdate(d1); !bytes.Equal(got, in1) {
				t.Errorf("depth %d: V1 state differs from the update", c.depth)
			}
			if got := ygo.EncodeStateAsUpdateV2(d2); !bytes.Equal(got, in2) {
				t.Errorf("depth %d: V2 state differs from the update", c.depth)
			}
		}
	}
}

// TestKeyOrder_ConcurrentDecodes: documents decoded at the same time get
// yjs's bytes (run under -race in CI).
func TestKeyOrder_ConcurrentDecodes(t *testing.T) {
	scenarios := loadKeyOrderFixtures(t)
	var wg sync.WaitGroup
	errs := make(chan string, 8*len(scenarios))
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, s := range scenarios {
				d := ygo.NewDoc()
				for _, u := range s.Updates {
					b, _ := hex.DecodeString(u)
					if err := ygo.ApplyUpdate(d, b); err != nil {
						errs <- s.Name + ": " + err.Error()
					}
				}
				if hex.EncodeToString(ygo.EncodeStateAsUpdate(d)) != s.V1 || hex.EncodeToString(ygo.EncodeStateAsUpdateV2(d)) != s.V2 {
					errs <- s.Name + ": state differs from yjs's"
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
