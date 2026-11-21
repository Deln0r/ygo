package encoding

import (
	"bytes"
	"errors"
	"runtime"
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
// update of 20 KB, and a few allocations per level.
//
// Reading it allocates one []any per level and a few more (10,013 to
// 10,017 counted, about 161 KB for 20,003 bytes, 20 Nov 2026). The counts
// are the whole process's (testing.AllocsPerRun, runtime.MemStats), so the
// runtime and the race detector add a few: the bounds leave room for that
// and still sit far below the old cost (a bound at MaxNesting+20 failed in
// CI by two allocations).
func TestDecodeAny_DeepRefusalIsCheap(t *testing.T) {
	buf := nestedAny(anyval.MaxNesting+1, false)
	var tab orderTable
	refuse := func() {
		if _, _, _, err := decodeAnyOrdered(buf, strictStrings, &tab, 0); !errors.Is(err, ErrAnyTooDeep) {
			t.Fatal(err)
		}
	}
	if allocs := testing.AllocsPerRun(5, refuse); allocs > anyval.MaxNesting+anyval.MaxNesting/10 {
		t.Errorf("%v allocations to refuse %d bytes", allocs, len(buf))
	}
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	const runs = 5
	for range runs {
		refuse()
	}
	runtime.ReadMemStats(&m1)
	if per := (m1.TotalAlloc - m0.TotalAlloc) / runs; per > 64*uint64(len(buf)) {
		t.Errorf("%d bytes allocated to refuse %d bytes", per, len(buf))
	}
}

// TestDecodeAny_ClaimedCountsMakeNoRoom: nested containers that each claim
// as many elements as there are bytes left are refused without making
// room for those counts first. Each used to make room for the count it
// claimed: a 112 KB update of 9000 nested arrays allocated 2.8 GB.
func TestDecodeAny_ClaimedCountsMakeNoRoom(t *testing.T) {
	for _, tag := range []byte{AnyTagArray, AnyTagObject} {
		const depth = 9000
		// Each level: tag, a 3-byte varuint count, and for an object a
		// one-byte key. The count claims everything after it.
		per := 4
		if tag == AnyTagObject {
			per = 6
		}
		total := depth * per
		buf := make([]byte, 0, total)
		for i := range depth {
			left := uint64(total - (i+1)*per + per - 4)
			buf = append(buf, tag)
			buf = append(buf, byte(left&0x7f|0x80), byte(left>>7&0x7f|0x80), byte(left>>14&0x7f))
			if tag == AnyTagObject {
				buf = append(buf, 1, 'k')
			}
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		var tab orderTable
		_, _, _, err := decodeAnyOrdered(buf, strictStrings, &tab, 0)
		runtime.ReadMemStats(&after)
		if err == nil {
			t.Fatalf("tag %d: a truncated value decoded", tag)
		}
		if got := after.TotalAlloc - before.TotalAlloc; got > 64<<20 {
			t.Errorf("tag %d: %d MB allocated to refuse %d KB", tag, got>>20, len(buf)>>10)
		}
	}
}
