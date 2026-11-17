package block

import (
	"fmt"
	"testing"
)

// TestOrderSet: orders of one shape, built apart, come back as one, with
// their children first; the same keys with other orders inside, and an
// array with the same children, stay apart; an Orders whose orders change
// is a new one, the one passed in is not written.
func TestOrderSet(t *testing.T) {
	var s OrderSet
	yx1 := &KeyOrder{Keys: []string{"y", "x"}}
	yx2 := &KeyOrder{Keys: []string{"y", "x"}}
	qp := &KeyOrder{Keys: []string{"q", "p"}}
	ba1 := &KeyOrder{Keys: []string{"b", "a"}, Sub: []*KeyOrder{yx1, nil}}
	ba2 := &KeyOrder{Keys: []string{"b", "a"}, Sub: []*KeyOrder{yx2, nil}}
	ba3 := &KeyOrder{Keys: []string{"b", "a"}, Sub: []*KeyOrder{qp, nil}}
	arr := &KeyOrder{Sub: []*KeyOrder{yx2}}

	o1 := s.Orders(&Orders{List: []*KeyOrder{ba1}})
	o2 := s.Orders(&Orders{List: []*KeyOrder{ba2}})
	if o1 != o2 || o1.List[0] != ba1 {
		t.Errorf("one shape built twice gave two Orders: %p %p", o1, o2)
	}
	if ba2.Sub[0] != yx2 {
		t.Error("an order passed in was written")
	}
	o3 := s.Orders(&Orders{List: []*KeyOrder{ba3}})
	if o3.List[0] == ba1 {
		t.Error("the same keys with other orders inside share an order")
	}
	in := &Orders{List: []*KeyOrder{arr, nil, ba2}}
	out := s.Orders(in)
	if out == in || out.List[0].Sub[0] != yx1 || out.List[2] != ba1 || in.List[0] != arr {
		t.Errorf("a list with orders to replace: %v", out.List)
	}
	if out.List[0] == yx1 || out.List[0].Keys != nil {
		t.Error("an array and an object of the same children share an order")
	}
	if s.Orders(nil) != nil {
		t.Error("no Orders gave some")
	}
	var other OrderSet
	if other.Orders(&Orders{List: []*KeyOrder{ba2}}).List[0] == ba1 {
		t.Error("two sets share an order")
	}
}

// TestOrderSet_Bounded: the set keeps no shape too large for its key
// bound, at most maxOrderShapes entries and about maxOrderBytes, whatever
// it is given; an order it cannot keep comes back as it came, so the
// sharing an update brought is not undone by copies.
func TestOrderSet_Bounded(t *testing.T) {
	var s OrderSet
	big := &KeyOrder{Keys: make([]string, 200)}
	for i := range big.Keys {
		big.Keys[i] = fmt.Sprintf("key-%04d", 199-i)
	}
	in := &Orders{List: []*KeyOrder{big}}
	if out := s.Orders(in); out != in || s.Len() != 0 || s.bytes != 0 {
		t.Errorf("a 200-key shape was kept: %d shapes, %d bytes", s.Len(), s.bytes)
	}
	inner := &KeyOrder{Keys: []string{"y", "x"}}
	for i := range 10000 {
		o := &KeyOrder{Keys: []string{fmt.Sprintf("k%05d", i), "a"}, Sub: []*KeyOrder{{Keys: []string{"y", "x"}}, nil}}
		s.Orders(&Orders{List: []*KeyOrder{o}})
		s.Orders(&Orders{List: []*KeyOrder{inner, o}})
	}
	if n := len(s.shapes) + len(s.singles); n > maxOrderShapes || s.bytes > maxOrderBytes {
		t.Errorf("%d entries, %d bytes kept", n, s.bytes)
	}
	// Shapes of 30 keys fill the bytes before the count.
	var w OrderSet
	for i := range 3000 {
		o := &KeyOrder{Keys: make([]string, 30)}
		for j := range o.Keys {
			o.Keys[j] = fmt.Sprintf("%04d-key%02d", i, 29-j)
		}
		w.Orders(&Orders{List: []*KeyOrder{o}})
	}
	if w.bytes > maxOrderBytes || w.Len() >= 3000 {
		t.Errorf("%d shapes of 30 keys kept, %d bytes", w.Len(), w.bytes)
	}

	// Full: a new shape whose child the set knows comes back unchanged.
	child := &KeyOrder{Keys: []string{"y", "x"}}
	o := &KeyOrder{Keys: []string{"new", "a"}, Sub: []*KeyOrder{child, nil}}
	in = &Orders{List: []*KeyOrder{o, o}}
	if out := s.Orders(in); out != in || out.List[0] != o || o.Sub[0] != child {
		t.Error("an order the set could not keep was copied")
	}
}
