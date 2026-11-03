package gomobile

import (
	"sync"
	"testing"
)

type nopTextListener struct{}

func (nopTextListener) OnTextChange([]byte) {}

type nopMapListener struct{}

func (nopMapListener) OnMapChange([]byte) {}

type nopArrayListener struct{}

func (nopArrayListener) OnArrayChange([]byte) {}

// Concurrent ObserveChanges on one handle replace one another: one
// listener stays registered, none is left behind without a removal.
func TestObserveChanges_ConcurrentReplace(t *testing.T) {
	d := NewDoc()
	text, m, arr := d.Text("t"), d.Map("m"), d.Array("a")
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); text.ObserveChanges(nopTextListener{}) }()
		go func() { defer wg.Done(); m.ObserveChanges(nopMapListener{}) }()
		go func() { defer wg.Done(); arr.ObserveChanges(nopArrayListener{}) }()
	}
	wg.Wait()
	for name, n := range map[string]int{
		"text":  len(text.inner.Branch().Observers),
		"map":   len(m.inner.Branch().Observers),
		"array": len(arr.inner.Branch().Observers),
	} {
		if n != 1 {
			t.Errorf("%s: %d observers registered, want 1", name, n)
		}
	}
}
