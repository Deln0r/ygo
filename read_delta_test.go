package ygo_test

import (
	"fmt"
	"testing"

	"github.com/Deln0r/ygo"
)

// concurrentBold returns the updates of two peers bolding the same "x"
// of a base text at key k of root map "m" (nested) or of root text "t".
func concurrentBold(t *testing.T, nested bool) [][]byte {
	t.Helper()
	text := func(d *ygo.Doc) *ygo.Text {
		if nested {
			return ygo.NewMap(d, "m").Get("k").(*ygo.Text)
		}
		return ygo.NewText(d, "t")
	}
	base := ygo.NewDocWithOptions(ygo.Options{ClientID: 50})
	if nested {
		m := ygo.NewMap(base, "m")
		w := base.WriteTxn()
		if err := m.SetText(w, "k").Insert(w, 0, "x"); err != nil {
			t.Fatal(err)
		}
		w.Commit()
	} else {
		txt := ygo.NewText(base, "t")
		w := base.WriteTxn()
		if err := txt.Insert(w, 0, "x"); err != nil {
			t.Fatal(err)
		}
		w.Commit()
	}
	b0 := ygo.EncodeStateAsUpdateV2(base)
	updates := [][]byte{b0}
	for _, id := range []uint64{1, 2} {
		p := ygo.NewDocWithOptions(ygo.Options{ClientID: id})
		if err := ygo.ApplyUpdateV2(p, b0); err != nil {
			t.Fatal(err)
		}
		sv := ygo.EncodeStateVector(p)
		txt := text(p)
		w := p.WriteTxn()
		if err := txt.Format(w, 0, 1, ygo.Attrs{"bold": true}); err != nil {
			t.Fatal(err)
		}
		w.Commit()
		u, err := ygo.EncodeDiffV2(p, sv)
		if err != nil {
			t.Fatal(err)
		}
		updates = append(updates, u)
	}
	return updates
}

// TestReadDelta_FirstReadOnly: only the first ReadDelta of an event opens
// a transaction (it deletes the duplicate marker); reading it again, or
// the copy a deep observer of the parent gets, opens none, as yjs caches
// event.delta on its one event object. The trace of one read per event
// is the fixtures' (TestEventDelta_Fixtures).
func TestReadDelta_FirstReadOnly(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprint("nested=", nested), func(t *testing.T) {
			run := func(again bool) (local, deleted int) {
				d := ygo.NewDocWithOptions(ygo.Options{ClientID: 99})
				d.OnAfterTransaction(func(tr *ygo.TransactionMut) {
					if tr.Local {
						local++
						deleted += len(tr.DeletedRanges())
					}
				})
				read := func(e *ygo.TextEvent) {
					a := e.ReadDelta()
					if fmt.Sprint(a) != fmt.Sprint(e.Delta) {
						t.Errorf("ReadDelta %v, Delta %v", a, e.Delta)
					}
					if again && fmt.Sprint(e.ReadDelta()) != fmt.Sprint(a) {
						t.Errorf("a second read differs from the first")
					}
				}
				var m *ygo.Map
				if nested {
					m = ygo.NewMap(d, "m")
					if again {
						m.ObserveDeep(func(evs []any) {
							for _, ev := range evs {
								if e, ok := ev.(*ygo.TextEvent); ok {
									read(e)
								}
							}
						})
					}
				} else {
					ygo.NewText(d, "t").Observe(read)
				}
				for i, u := range concurrentBold(t, nested) {
					if err := ygo.ApplyUpdateV2WithOrigin(d, u, "remote"); err != nil {
						t.Fatal(err)
					}
					if i == 0 && nested {
						m.Get("k").(*ygo.Text).Observe(read)
					}
				}
				return local, deleted
			}
			once, deletedOnce := run(false)
			twice, deletedTwice := run(true)
			if once != twice || deletedOnce != 1 || deletedTwice != 1 {
				t.Fatalf("one read per event: %d local transactions, %d deletions; more reads: %d and %d; want the same, one deletion", once, deletedOnce, twice, deletedTwice)
			}
		})
	}
}

// TestReadDelta_AfterTheCommit: an event kept and read after the commit
// that delivered it deletes nothing (yjs would open a transaction of its
// own) and returns Delta.
func TestReadDelta_AfterTheCommit(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 99})
	var kept []*ygo.TextEvent
	ygo.NewText(d, "t").Observe(func(e *ygo.TextEvent) { kept = append(kept, e) })
	for _, u := range concurrentBold(t, false) {
		if err := ygo.ApplyUpdateV2(d, u); err != nil {
			t.Fatal(err)
		}
	}
	before := ygo.EncodeStateAsUpdateV2(d)
	updates := 0
	d.OnUpdateV2(func([]byte, any) { updates++ })
	for _, e := range kept {
		if got := e.ReadDelta(); fmt.Sprint(got) != fmt.Sprint(e.Delta) {
			t.Fatalf("ReadDelta %v, Delta %v", got, e.Delta)
		}
	}
	if updates != 0 || string(ygo.EncodeStateAsUpdateV2(d)) != string(before) {
		t.Fatalf("reading after the commit changed the document (%d updates)", updates)
	}
}

// TestReadDelta_OnAnotherGoroutineAfterTheCommit: events handed to another
// goroutine and read there after the commit returned change nothing and
// race with nothing (run under -race).
func TestReadDelta_OnAnotherGoroutineAfterTheCommit(t *testing.T) {
	d := ygo.NewDocWithOptions(ygo.Options{ClientID: 99})
	ch := make(chan *ygo.TextEvent, 8)
	ygo.NewText(d, "t").Observe(func(e *ygo.TextEvent) { ch <- e })
	for _, u := range concurrentBold(t, false) {
		if err := ygo.ApplyUpdateV2(d, u); err != nil {
			t.Fatal(err)
		}
	}
	close(ch)
	before := string(ygo.EncodeStateAsUpdateV2(d))
	done := make(chan []string)
	go func() {
		var got []string
		for e := range ch {
			got = append(got, fmt.Sprint(e.ReadDelta()), fmt.Sprint(e.Delta))
		}
		done <- got
	}()
	got := <-done
	for i := 0; i < len(got); i += 2 {
		if got[i] != got[i+1] {
			t.Fatalf("event %d: ReadDelta %s, Delta %s", i/2, got[i], got[i+1])
		}
	}
	if string(ygo.EncodeStateAsUpdateV2(d)) != before {
		t.Fatal("reading after the commit changed the document")
	}
}
