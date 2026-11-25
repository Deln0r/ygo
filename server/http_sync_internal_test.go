package server

import (
	"context"
	"testing"
	"time"
)

// TestUnpinRead_OverlappingReads: a made-up name two reads overlap on is
// dropped when the last of them lets it go, whichever loaded it.
func TestUnpinRead_OverlappingReads(t *testing.T) {
	s := New(Options{DocIdleTimeout: time.Hour})
	defer s.Close(context.Background())
	a, err := s.pinRead(context.Background(), "made-up")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.pinRead(context.Background(), "made-up")
	if err != nil || b != a {
		t.Fatalf("second read: %v", err)
	}
	s.unpinRead(a)
	s.unpinRead(b)
	if st := s.Stats(); st.Documents != 0 {
		t.Fatalf("the document stayed: %+v", st)
	}
}

// TestUnpinRead_MaxIdleDocs: a read of a parked document keeps it out of
// MaxIdleDocs while it lasts; its end applies the bound again.
func TestUnpinRead_MaxIdleDocs(t *testing.T) {
	s := New(Options{DocIdleTimeout: time.Hour, MaxIdleDocs: 1})
	defer s.Close(context.Background())
	park := func(name string) {
		d, err := s.OpenDocument(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		d.Close()
	}
	park("a")
	st, err := s.pinRead(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	park("b")
	if got := s.Stats().IdleDocuments; got != 2 {
		t.Fatalf("while a is read: %d idle", got)
	}
	s.unpinRead(st)
	if got := s.Stats(); got.IdleDocuments != 1 || got.Documents != 1 {
		t.Fatalf("after the read: %+v", got)
	}
	s.docsMu.Lock()
	_, kept := s.docs["b"]
	s.docsMu.Unlock()
	if !kept {
		t.Fatal("the newer idle document was evicted")
	}
}
