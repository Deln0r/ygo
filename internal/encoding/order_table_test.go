package encoding

import (
	"strings"
	"testing"
	"unsafe"

	"github.com/Deln0r/ygo/internal/block"
)

// TestOrderTable: within one table, objects of one shape share one order,
// arrays with the same element orders share one, and one-element contents
// share one Orders; the same keys with different orders inside are
// different shapes; a fresh table shares nothing with an old one.
func TestOrderTable(t *testing.T) {
	var tab orderTable
	inner1 := tab.object([]string{"y", "x"}, nil, false)
	inner2 := tab.object([]string{"q", "p"}, nil, false)
	if tab.object([]string{"y", "x"}, nil, false) != inner1 {
		t.Error("one shape built twice")
	}
	a := tab.object([]string{"b", "a"}, []*block.KeyOrder{inner1, nil}, false)
	b := tab.object([]string{"b", "a"}, []*block.KeyOrder{inner2, nil}, false)
	if a == b || a.Sub[0] != inner1 || b.Sub[0] != inner2 {
		t.Errorf("the same keys with other inner orders share an order: %v %v", a, b)
	}
	if tab.object([]string{"b", "a"}, []*block.KeyOrder{inner1, nil}, false) != a {
		t.Error("an object shape with orders inside built twice")
	}
	// A key list must not read as another one: ["ab"] and ["a", "b"].
	if tab.object([]string{"ab"}, nil, false) == tab.object([]string{"a", "b"}, nil, false) {
		t.Error("different key lists share an order")
	}

	arr := tab.array([]*block.KeyOrder{nil, inner1, nil, nil})
	if len(arr.Sub) != 2 || arr.Sub[1] != inner1 {
		t.Errorf("array order %v: want the trailing elements without an order left out", arr.Sub)
	}
	if tab.array([]*block.KeyOrder{nil, inner1}) != arr {
		t.Error("one array shape built twice")
	}
	if tab.array([]*block.KeyOrder{inner1}) == arr {
		t.Error("arrays with their orders at other elements share an order")
	}
	// The last array returned is another one: the table still finds arr.
	if tab.array([]*block.KeyOrder{nil, inner1}) != arr {
		t.Error("an array shape not found again after another one")
	}
	if one, two := tab.array([]*block.KeyOrder{inner1}), tab.array([]*block.KeyOrder{inner2}); one == two {
		t.Error("arrays with other orders inside share an order")
	}

	s := tab.single(a)
	if tab.single(a) != s || s.Of(0) != a || tab.single(nil) != nil {
		t.Error("one-element Orders not shared")
	}
	if sb := tab.single(b); sb == s || sb.Of(0) != b {
		t.Errorf("Orders of another order: %v", sb.List)
	}
	if tab.single(a) != s {
		t.Error("one-element Orders not found again after another one")
	}

	var fresh orderTable
	if fresh.object([]string{"y", "x"}, nil, false) == inner1 {
		t.Error("a new table returned an order of an old one")
	}

	// clone: the keys of a new order do not point into the text they
	// were read from.
	text := strings.Repeat("z", 1<<10) + `"k2""k1"`
	k2, k1 := text[1<<10+1:1<<10+3], text[1<<10+5:1<<10+7]
	var ctab orderTable
	o := ctab.object([]string{k2, k1}, nil, true)
	if o.Keys[0] != "k2" || o.Keys[1] != "k1" {
		t.Fatalf("keys %q", o.Keys)
	}
	if unsafe.StringData(o.Keys[0]) == unsafe.StringData(k2) {
		t.Error("clone kept a key pointing into its source text")
	}
}
