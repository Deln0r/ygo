package encoding

import (
	"bytes"
	"errors"
	"testing"

	"github.com/Deln0r/ygo/internal/anyval"
)

// nestedAny returns the Any bytes of depth containers nested in each
// other around a null: arrays of one element, or objects of one key.
func nestedAny(depth int, object bool) []byte {
	var b bytes.Buffer
	for range depth {
		if object {
			b.Write([]byte{AnyTagObject, 1, 1, 'k'})
		} else {
			b.Write([]byte{AnyTagArray, 1})
		}
	}
	b.WriteByte(AnyTagNull)
	return b.Bytes()
}

// TestDecodeAny_NestingBound: arrays and objects nested anyval.MaxNesting
// levels decode, with and without the key order; one level more is
// refused with ErrAnyTooDeep, and so are a million levels, which used to
// overflow the goroutine stack, a crash no caller could recover from.
func TestDecodeAny_NestingBound(t *testing.T) {
	for _, object := range []bool{false, true} {
		for _, c := range []struct {
			depth int
			ok    bool
		}{{anyval.MaxNesting, true}, {anyval.MaxNesting + 1, false}, {1_000_000, false}} {
			buf := nestedAny(c.depth, object)
			_, tail, err := DecodeAny(buf)
			var tab orderTable
			_, _, otail, oerr := decodeAnyOrdered(buf, strictStrings, &tab, 0)
			for _, e := range []error{err, oerr} {
				if c.ok && (e != nil || len(tail) != 0 || len(otail) != 0) {
					t.Errorf("object=%v depth %d: %v, %d bytes left", object, c.depth, e, len(tail))
				}
				if !c.ok && !errors.Is(e, ErrAnyTooDeep) {
					t.Errorf("object=%v depth %d: err %v, want ErrAnyTooDeep", object, c.depth, e)
				}
			}
		}
	}
}

// TestDecodeAny_DeepRefusalIsCheap: refusing a value at the nesting bound
// costs about what reading it does. Each level used to wrap the error of
// the levels below, copying its message: over a gigabyte allocated for an
// update of 20 KB.
func TestDecodeAny_DeepRefusalIsCheap(t *testing.T) {
	buf := nestedAny(anyval.MaxNesting+1, false)
	var tab orderTable
	allocs := testing.AllocsPerRun(5, func() {
		if _, _, _, err := decodeAnyOrdered(buf, strictStrings, &tab, 0); !errors.Is(err, ErrAnyTooDeep) {
			t.Fatal(err)
		}
	})
	// One []any per level read, and the few wraps of the outer levels.
	if allocs > anyval.MaxNesting+20 {
		t.Errorf("%v allocations to refuse %d bytes", allocs, len(buf))
	}
}
