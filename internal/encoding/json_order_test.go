package encoding

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"github.com/Deln0r/ygo/internal/anyval"
	"github.com/Deln0r/ygo/internal/block"
)

// refJSONOrder is jsonOrder built on encoding/json's own tokenizer, whose
// keys are the keys of the value json.Unmarshal returns: the order of each
// object is its keys in the order they first came, put in JavaScript
// enumeration order, with the order of each key's last value.
func refJSONOrder(t *testing.T, text string) *block.KeyOrder {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var val func() *block.KeyOrder
	val = func() *block.KeyOrder {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		switch tok {
		case json.Delim('{'):
			var keys []string
			subs := map[string]*block.KeyOrder{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					t.Fatalf("%q: %v", text, err)
				}
				k := kt.(string)
				sub := val()
				if _, seen := subs[k]; !seen {
					keys = append(keys, k)
				}
				subs[k] = sub
			}
			if _, err := dec.Token(); err != nil {
				t.Fatal(err)
			}
			js := anyval.JSOrder(keys)
			var list []*block.KeyOrder
			for _, k := range js {
				if subs[k] != nil {
					list = make([]*block.KeyOrder, len(js))
					break
				}
			}
			for i, k := range js {
				if list != nil {
					list[i] = subs[k]
				}
			}
			if list == nil && anyval.IsFallbackOrder(js) {
				return nil
			}
			return &block.KeyOrder{Keys: js, Sub: list}
		case json.Delim('['):
			var subs []*block.KeyOrder
			for dec.More() {
				subs = append(subs, val())
			}
			if _, err := dec.Token(); err != nil {
				t.Fatal(err)
			}
			for len(subs) > 0 && subs[len(subs)-1] == nil {
				subs = subs[:len(subs)-1]
			}
			if len(subs) == 0 {
				return nil
			}
			return &block.KeyOrder{Sub: subs}
		}
		return nil
	}
	return val()
}

func sameOrder(a, b *block.KeyOrder) bool {
	if a == nil || b == nil {
		return a == b
	}
	if !slices.Equal(a.Keys, b.Keys) || len(a.Sub) != len(b.Sub) {
		return false
	}
	for i := range a.Sub {
		if !sameOrder(a.Sub[i], b.Sub[i]) {
			return false
		}
	}
	return true
}

func orderString(o *block.KeyOrder) string {
	if o == nil {
		return "-"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%q[", o.Keys)
	for _, s := range o.Sub {
		b.WriteString(orderString(s) + " ")
	}
	return b.String() + "]"
}

// Keys as JSON text: escapes, keys past ASCII, raw bytes that are not
// UTF-8 (only bytes ygo stored before 1.26 hold them), lone surrogates,
// array-index keys and keys that only look like one, and keys that decode
// to the same string.
var jsonKeyTexts = []string{
	`"a"`, `"b"`, `"c"`, `"\u0061"`, `"a\"q"`, `"\\"`, `""`,
	`"0"`, `"1"`, `"9"`, `"10"`, `"01"`, `"4294967294"`, `"4294967295"`, `"-1"`,
	`"é"`, `"\u00e9"`, `"z\u00e9"`, `"\ud83d\ude00"`, `"😀"`,
	`"\ud800"`, `"\udc00"`, `"\ufffd"`, "\"\xed\xa0\x80\"", "\"\xff\"", "\"\xef\xbf\xbd\"",
	`"__proto__"`, `"href"`, `"target"`,
}

func randomJSON(r *rand.Rand, b *strings.Builder, depth int) {
	ws := func() {
		if r.Intn(4) == 0 {
			b.WriteString([]string{" ", "\n", "\t", "\r\n  "}[r.Intn(4)])
		}
	}
	ws()
	switch k := r.Intn(10); {
	case depth < 4 && k < 4:
		b.WriteByte('{')
		n := r.Intn([]int{22, 8, 4, 3}[depth])
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			ws()
			b.WriteString(jsonKeyTexts[r.Intn(len(jsonKeyTexts))])
			ws()
			b.WriteByte(':')
			randomJSON(r, b, depth+1)
		}
		ws()
		b.WriteByte('}')
	case depth < 4 && k < 6:
		b.WriteByte('[')
		n := r.Intn(6)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			randomJSON(r, b, depth+1)
		}
		ws()
		b.WriteByte(']')
	default:
		b.WriteString([]string{`1`, `-2.5e3`, `"s"`, `"{\"x\":1}"`, `true`, `false`, `null`, `"\u0000]"`}[r.Intn(8)])
	}
	ws()
}

// TestJSONOrder_MatchesEncodingJSON: the order jsonOrder reads from JSON
// text has the keys encoding/json gives the value, decoded the same way
// (escapes, U+FFFD for each byte that is not UTF-8 and for each lone
// surrogate), each once, in its first place, with the order of its last
// value, over 10,000 random texts.
func TestJSONOrder_MatchesEncodingJSON(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 10000; i++ {
		var b strings.Builder
		randomJSON(r, &b, 0)
		text := b.String()
		var v any
		if err := json.Unmarshal([]byte(text), &v); err != nil {
			t.Fatalf("generator wrote %q: %v", text, err)
		}
		var tab orderTable
		got, want := jsonOrder(text, &tab), refJSONOrder(t, text)
		if !sameOrder(got, want) {
			t.Fatalf("%q:\n got %s\nwant %s", text, orderString(got), orderString(want))
		}
	}
}

// TestJSONOrder_NoAllocationInFallbackOrder: text whose objects are in the
// fallback order, the common case (a link {href, target}), is read without
// allocating.
func TestJSONOrder_NoAllocationInFallbackOrder(t *testing.T) {
	for _, text := range []string{
		`{"href":"https://example.com/?a=1&b=2","target":"_blank"}`,
		`[{"a":1,"b":[{"c":2,"d":3}]},1,"x"]`,
		`{"0":1,"10":2,"a":{"b":1,"c":2},"é":null}`,
	} {
		var tab orderTable
		if o := jsonOrder(text, &tab); o != nil {
			t.Fatalf("%s: order %s, want none", text, orderString(o))
		}
		if n := testing.AllocsPerRun(100, func() { jsonOrder(text, &tab) }); n != 0 {
			t.Errorf("%s: %v allocations", text, n)
		}
	}
}

// TestJSONOrder_LargeObjectsInOrder: an object of many keys in the
// fallback order allocates only to hold its keys, and builds no map of
// their positions; it built one for every object past 16 keys.
func TestJSONOrder_LargeObjectsInOrder(t *testing.T) {
	var b strings.Builder
	b.WriteByte('{')
	for i := range 200 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"k%03d":%d`, i, i)
	}
	b.WriteByte('}')
	text := b.String()
	var tab orderTable
	if o := jsonOrder(text, &tab); o != nil {
		t.Fatalf("order %s, want none", orderString(o))
	}
	// The keys slice grows from 8 to 256: 5 allocations.
	if n := testing.AllocsPerRun(20, func() { jsonOrder(text, &tab) }); n > 5 {
		t.Errorf("%v allocations for 200 keys in order", n)
	}
}

// TestJSONOrder_KeysDoNotPinTheText: the keys of an order read from JSON
// text are copies, so a stored order does not keep the whole text alive.
func TestJSONOrder_KeysDoNotPinTheText(t *testing.T) {
	text := `{"b":1,"a":{"y":"` + strings.Repeat("z", 4096) + `","x":2}}`
	var tab orderTable
	o := jsonOrder(text, &tab)
	if o == nil || o.Sub[1] == nil {
		t.Fatalf("order %s", orderString(o))
	}
	lo := uintptr(unsafe.Pointer(unsafe.StringData(text)))
	hi := lo + uintptr(len(text))
	for _, k := range append(append([]string(nil), o.Keys...), o.Sub[1].Keys...) {
		if p := uintptr(unsafe.Pointer(unsafe.StringData(k))); p >= lo && p < hi {
			t.Errorf("key %q points into the JSON text", k)
		}
	}
}
