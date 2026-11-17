package encoding

import (
	"testing"

	"github.com/Deln0r/ygo/internal/block"
)

// TestInternOrder: objects of one shape share one order; the same keys
// with different orders inside are different shapes.
func TestInternOrder(t *testing.T) {
	inner1 := internOrder([]string{"y", "x"}, nil)
	inner2 := internOrder([]string{"q", "p"}, nil)
	if internOrder([]string{"y", "x"}, nil) != inner1 {
		t.Error("one shape built twice")
	}
	a := internOrder([]string{"b", "a"}, []*block.KeyOrder{inner1, nil})
	b := internOrder([]string{"b", "a"}, []*block.KeyOrder{inner2, nil})
	if a == b || a.Sub[0] != inner1 || b.Sub[0] != inner2 {
		t.Errorf("the same keys with other inner orders share an order: %v %v", a, b)
	}
}
