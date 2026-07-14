package server

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// TestServer_Stats_CountsDocsAndConns checks the snapshot against a known
// sequence of admissions and a release: the document count tracks
// resident rooms and the connection count sums conns across them, and an
// eviction (last conn of a room departing) is reflected.
func TestServer_Stats_CountsDocsAndConns(t *testing.T) {
	s := &Server{docs: map[string]*docState{}}
	ctx := context.Background()

	if st := s.Stats(); st.Documents != 0 || st.Connections != 0 {
		t.Fatalf("empty server: %+v, want {0 0}", st)
	}

	// Two connections on "a", one on "b": 2 docs, 3 conns.
	_, _, _, _ = s.admitConn(ctx, "a", nil)
	_, _, _, _ = s.admitConn(ctx, "a", nil)
	cb, sb, _, _ := s.admitConn(ctx, "b", nil)
	if st := s.Stats(); st.Documents != 2 || st.Connections != 3 {
		t.Fatalf("after admits: %+v, want {2 3}", st)
	}

	// Releasing b's only connection evicts the room.
	s.releaseConn(ctx, sb, cb)
	if st := s.Stats(); st.Documents != 1 || st.Connections != 2 {
		t.Fatalf("after releasing b: %+v, want {1 2}", st)
	}
}

// TestServer_Stats_PerDocBreakdown checks Stats.Docs: one entry per
// resident room, sorted by name (not insertion order), each carrying that
// room's connection count, with the aggregates equal to their sum.
func TestServer_Stats_PerDocBreakdown(t *testing.T) {
	s := &Server{docs: map[string]*docState{}}
	ctx := context.Background()

	// Admit "b" before "a" so a sorted result proves the output order is
	// by name, not by insertion. "a" gets two conns, "b" one.
	_, _, _, _ = s.admitConn(ctx, "b", nil)
	_, _, _, _ = s.admitConn(ctx, "a", nil)
	_, _, _, _ = s.admitConn(ctx, "a", nil)

	st := s.Stats()
	want := []DocStat{{Name: "a", Connections: 2}, {Name: "b", Connections: 1}}
	if len(st.Docs) != len(want) {
		t.Fatalf("Docs = %+v, want %+v", st.Docs, want)
	}
	for i, ds := range st.Docs {
		if ds != want[i] {
			t.Fatalf("Docs[%d] = %+v, want %+v", i, ds, want[i])
		}
	}
	if st.Documents != 2 || st.Connections != 3 {
		t.Fatalf("aggregates = {%d %d}, want {2 3}", st.Documents, st.Connections)
	}
}

// TestServer_Stats_ConcurrentWithChurn calls Stats concurrently with
// admit/release churn across a few rooms under the race detector. It must
// not race or deadlock, and once the churn settles every room has evicted
// so the snapshot returns to zero.
func TestServer_Stats_ConcurrentWithChurn(t *testing.T) {
	s := &Server{docs: map[string]*docState{}}
	ctx := context.Background()

	stop := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
				_ = s.Stats()
			}
		}
	}()

	var churn sync.WaitGroup
	for g := 0; g < 8; g++ {
		g := g
		churn.Add(1)
		go func() {
			defer churn.Done()
			name := fmt.Sprintf("d%d", g%3)
			for i := 0; i < 50; i++ {
				c, st, ok, err := s.admitConn(ctx, name, nil)
				if ok && err == nil {
					s.releaseConn(ctx, st, c)
				}
			}
		}()
	}
	churn.Wait()
	close(stop)
	<-readerDone

	// Every admitted conn was released, so all rooms evicted.
	if st := s.Stats(); st.Documents != 0 || st.Connections != 0 {
		t.Fatalf("after churn settled: %+v, want {0 0}", st)
	}
}
