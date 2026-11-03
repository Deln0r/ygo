package ygo_test

import (
	"sync"
	"testing"
	"time"

	ygo "github.com/Deln0r/ygo"
)

// withTimeout fails the test when f has not returned in five seconds:
// a root accessor that takes the document lock inside an open
// transaction blocks forever.
func withTimeout(t *testing.T, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked: the accessor waited for the lock the open transaction holds")
	}
}

// The *In forms work inside an open transaction, as yjs getMap does
// inside transact, and return the same root the document forms do.
func TestRootIn_InsideWriteTxn(t *testing.T) {
	d := ygo.NewDoc()
	withTimeout(t, func() {
		txn := d.WriteTxn()
		ygo.TextIn(txn, "t").Insert(txn, 0, "hi")
		ygo.MapIn(txn, "m").Set(txn, "k", int64(1))
		ygo.ArrayIn(txn, "a").Push(txn, "x")
		f := ygo.XmlFragmentIn(txn, "f")
		f.InsertXmlText(txn, 0)
		ygo.XmlTextIn(txn, "xt").Insert(txn, 0, "x")
		ygo.XmlElementIn(txn, "e").SetAttribute(txn, "k", "v")
		txn.Commit()
	})
	if got := ygo.NewText(d, "t").String(); got != "hi" {
		t.Errorf("text %q, want hi", got)
	}
	if got := ygo.NewMap(d, "m").Get("k"); got != int64(1) {
		t.Errorf("map k %v, want 1", got)
	}
	if got := ygo.NewArray(d, "a").Len(); got != 1 {
		t.Errorf("array length %d, want 1", got)
	}
	if got := ygo.NewXmlFragment(d, "f").Length(); got != 1 {
		t.Errorf("fragment length %d, want 1", got)
	}
	if got := ygo.NewXmlText(d, "xt").String(); got != "x" {
		t.Errorf("xml text %q, want x", got)
	}
	if got, ok := ygo.NewXmlElement(d, "e").GetAttribute("k"); !ok || got != "v" {
		t.Errorf("xml element attribute %q (ok %v), want v", got, ok)
	}
	txn := d.WriteTxn()
	inTxn := ygo.TextIn(txn, "t").Branch()
	txn.Commit()
	if ygo.NewText(d, "t").Branch() != inTxn {
		t.Error("TextIn and NewText returned different branches for one name")
	}
}

// A root is found and typed under the document lock, so typing it while
// another goroutine applies updates is not a data race (run with -race).
func TestNewText_TypesUnderTheLock(t *testing.T) {
	src := ygo.NewDoc()
	st := ygo.NewText(src, "t")
	var updates [][]byte
	src.OnUpdate(func(u []byte, _ any) { updates = append(updates, u) })
	for i := 0; i < 50; i++ {
		txn := src.WriteTxn()
		st.Insert(txn, 0, "x")
		txn.Commit()
	}
	d := ygo.NewDoc()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for _, u := range updates {
			if err := ygo.ApplyUpdate(d, u); err != nil {
				t.Error(err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = ygo.NewText(d, "t")
		}
	}()
	wg.Wait()
	if got := ygo.NewText(d, "t").Length(); got != 50 {
		t.Fatalf("length %d, want 50", got)
	}
}

// Reading a nested type wraps it without writing to its branch, so
// readers holding ReadTxn at the same time do not race (run with -race).
func TestNestedRead_ConcurrentReaders(t *testing.T) {
	d := ygo.NewDoc()
	m := ygo.NewMap(d, "m")
	txn := d.WriteTxn()
	m.SetText(txn, "t").Insert(txn, 0, "nested")
	txn.Commit()
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				rt := d.ReadTxn()
				if tx, ok := m.Get("t").(*ygo.Text); !ok || tx.String() != "nested" {
					t.Errorf("nested read got %v", m.Get("t"))
				}
				rt.Close()
			}
		}()
	}
	wg.Wait()
}
