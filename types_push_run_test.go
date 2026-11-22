package ygo_test

import (
	"runtime"
	"testing"

	"github.com/Deln0r/ygo"
)

// A run of single-value pushes in one transaction merges at commit with
// each value appended once: merging the run right to left with the
// payloads concatenated at every step copied the growing tail into each
// item and kept every copy until the end, quadratic in the run.
func TestSquash_PushRunAllocatesLinearly(t *testing.T) {
	const n = 20000
	d := ygo.NewDoc()
	a := ygo.NewArray(d, "a")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	txn := d.WriteTxn()
	for i := 0; i < n; i++ {
		a.Push(txn, float64(i))
	}
	txn.Commit()
	runtime.ReadMemStats(&after)
	if got := after.TotalAlloc - before.TotalAlloc; got > 256<<20 {
		t.Fatalf("%d pushes allocated %d MB, want far less (quadratic would be gigabytes)", n, got>>20)
	}
	if a.Len() != n {
		t.Fatalf("len %d", a.Len())
	}
}
