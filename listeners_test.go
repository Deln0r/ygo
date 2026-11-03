package ygo_test

import (
	"strings"
	"sync"
	"testing"

	ygo "github.com/Deln0r/ygo"
)

// Observers added or removed during a delivery follow yjs EventHandler:
// the delivery walks the array current when it started, an addition
// joins it until the first removal, and a removal (also of a listener
// already gone) leaves the walk on the old array. Expected logs are
// yjs 13.6.33's for the same scripts (map.observe / observeDeep):
// "|" separates the first change from the second.
func TestObservers_ChangedDuringDelivery(t *testing.T) {
	// Each script runs inside the first delivery: addLate registers a
	// new observer, offRemoved / offNever / offSelf call the removal of
	// the observer registered after the running one, of one removed
	// before the delivery (so absent), and of the running one.
	cases := []struct {
		name   string
		script func(addLate, offRemoved, offNever, offSelf func())
		want   string
	}{
		{"add then remove", func(addLate, offRemoved, _, _ func()) { addLate(); offRemoved() }, "first removed late | first late"},
		{"remove then add", func(addLate, offRemoved, _, _ func()) { offRemoved(); addLate() }, "first removed | first late"},
		{"remove an absent one then add", func(addLate, _, offNever, _ func()) { offNever(); addLate() }, "first removed | first removed late"},
		{"remove itself", func(_, _, _, offSelf func()) { offSelf() }, "first removed | removed"},
	}
	for _, deep := range []bool{false, true} {
		for _, c := range cases {
			name := "shallow/" + c.name
			if deep {
				name = "deep/" + c.name
			}
			t.Run(name, func(t *testing.T) {
				d := ygo.NewDoc()
				m := ygo.NewMap(d, "m")
				var log []string
				on := func(f func()) func() {
					if deep {
						return m.ObserveDeep(func([]any) { f() })
					}
					return m.Observe(func(*ygo.MapEvent) { f() })
				}
				neverUnsub := on(func() { log = append(log, "never") })
				neverUnsub()
				var selfUnsub, removedUnsub func()
				first := true
				selfUnsub = on(func() {
					log = append(log, "first")
					if first {
						first = false
						c.script(
							func() { on(func() { log = append(log, "late") }) },
							func() { removedUnsub() },
							func() { neverUnsub() },
							func() { selfUnsub() })
					}
				})
				removedUnsub = on(func() { log = append(log, "removed") })
				for _, k := range []string{"a", "b"} {
					txn := d.WriteTxn()
					m.Set(txn, k, int64(1))
					txn.Commit()
					if k == "a" {
						log = append(log, "|")
					}
				}
				if got := strings.Join(log, " "); got != c.want {
					t.Fatalf("log %q, want %q (yjs)", got, c.want)
				}
			})
		}
	}
}

// Observers registered and removed on other goroutines while commits
// deliver race neither the deliveries nor each other (run with -race).
// Two registering goroutines, so an unguarded registration meets another
// one, not only a delivery.
func TestObservers_RegisteredWhileDelivering(t *testing.T) {
	d := ygo.NewDoc()
	m := ygo.NewMap(d, "m")
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			txn := d.WriteTxn()
			m.Set(txn, "k", int64(i))
			txn.Commit()
		}
	}()
	stop := make(chan struct{})
	counted := make(chan struct{})
	go func() {
		defer close(counted)
		for {
			select {
			case <-stop:
				return
			default:
				_ = m.Branch().ObserverCount() + m.Branch().DeepObserverCount()
			}
		}
	}()
	defer func() { close(stop); <-counted }()
	for g := 0; g < 2; g++ {
		go func() {
			defer wg.Done()
			var kept []func()
			for i := 0; i < 300; i++ {
				un := m.Observe(func(*ygo.MapEvent) {})
				deepUn := m.ObserveDeep(func([]any) {})
				if i%3 == 0 {
					kept = append(kept, un, deepUn)
					continue
				}
				un()
				deepUn()
			}
			for _, un := range kept {
				un()
			}
		}()
	}
	wg.Wait()
	if n := m.Branch().ObserverCount() + m.Branch().DeepObserverCount(); n != 0 {
		t.Fatalf("%d observers left after every removal", n)
	}
}

// The update and updateV2 handler lists are each read right before
// their own delivery: an update handler that swaps the V2 handler
// reaches the new one in the same commit (yjs 13.6.33: "v1 new-v2").
func TestOnUpdate_V2ListReadAfterV1Delivery(t *testing.T) {
	d := ygo.NewDoc()
	var log []string
	var offOld func()
	first := true
	d.OnUpdate(func([]byte, any) {
		log = append(log, "v1")
		if first {
			first = false
			offOld()
			d.OnUpdateV2(func([]byte, any) { log = append(log, "new-v2") })
		}
	})
	offOld = d.OnUpdateV2(func([]byte, any) { log = append(log, "old-v2") })
	txn := d.WriteTxn()
	ygo.MapIn(txn, "m").Set(txn, "a", int64(1))
	txn.Commit()
	if got := strings.Join(log, " "); got != "v1 new-v2" {
		t.Fatalf("log %q, want \"v1 new-v2\" (yjs)", got)
	}
}

// A deep observer added during one deep delivery to a type whose deep
// delivery is already queued is called in it; the queue itself is
// settled before the first deep call (yjs 13.6.33: "m n-first n-late").
func TestDeepObservers_AddedToAQueuedTarget(t *testing.T) {
	d := ygo.NewDoc()
	m := ygo.NewMap(d, "m")
	txn := d.WriteTxn()
	n := m.SetMap(txn, "n")
	txn.Commit()
	var log []string
	n.ObserveDeep(func([]any) { log = append(log, "n-first") })
	first := true
	m.ObserveDeep(func([]any) {
		log = append(log, "m")
		if first {
			first = false
			n.ObserveDeep(func([]any) { log = append(log, "n-late") })
		}
	})
	txn = d.WriteTxn()
	m.Set(txn, "a", int64(1))
	n.Set(txn, "x", int64(1))
	txn.Commit()
	if got := strings.Join(log, " "); got != "m n-first n-late" {
		t.Fatalf("log %q, want \"m n-first n-late\" (yjs)", got)
	}
}
