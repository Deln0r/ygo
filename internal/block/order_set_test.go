package block

import "testing"

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
