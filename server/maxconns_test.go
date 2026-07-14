package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// TestServer_MaxConns_CapsTotal verifies the global connection cap:
// connections are admitted up to the cap across documents, the next is
// refused with errServerConnsFull whether it targets a resident room or a
// new one, and a refused new-doc connection does not orphan a docState
// (the cap is checked before the room is created).
func TestServer_MaxConns_CapsTotal(t *testing.T) {
	s := &Server{docs: map[string]*docState{}, opts: Options{MaxConns: 3}}
	ctx := context.Background()

	// Two conns on "a", one on "b": three admitted, at the cap.
	for _, name := range []string{"a", "a", "b"} {
		if _, _, ok, err := s.admitConn(ctx, name, nil); !ok || err != nil {
			t.Fatalf("admit %q: ok=%v err=%v", name, ok, err)
		}
	}

	// A fourth connection to an already-resident room is refused.
	if _, _, ok, err := s.admitConn(ctx, "a", nil); ok || !errors.Is(err, errServerConnsFull) {
		t.Fatalf("admit resident past cap: ok=%v err=%v, want refused with errServerConnsFull", ok, err)
	}
	// A connection that would create a new room is also refused, and must
	// not have created the room on the way to the rejection.
	if _, _, ok, err := s.admitConn(ctx, "c", nil); ok || !errors.Is(err, errServerConnsFull) {
		t.Fatalf("admit new doc past cap: ok=%v err=%v, want refused", ok, err)
	}
	s.docsMu.Lock()
	n := len(s.docs)
	s.docsMu.Unlock()
	if n != 2 {
		t.Fatalf("resident docs = %d, want 2 (a rejected new-doc conn must not orphan a state)", n)
	}
}

// TestServer_MaxConns_ReleaseFreesSlot locks the cap as a live count: a
// released connection frees a global slot so a later connection is
// admitted, and the slot is reclaimed even though the room stays resident
// (another connection keeps it alive).
func TestServer_MaxConns_ReleaseFreesSlot(t *testing.T) {
	s := &Server{docs: map[string]*docState{}, opts: Options{MaxConns: 2}}
	ctx := context.Background()

	if _, _, ok, err := s.admitConn(ctx, "a", nil); !ok || err != nil {
		t.Fatalf("admit 1: ok=%v err=%v", ok, err)
	}
	c2, st2, ok, err := s.admitConn(ctx, "a", nil)
	if !ok || err != nil {
		t.Fatalf("admit 2: ok=%v err=%v", ok, err)
	}

	// At the cap.
	if _, _, ok, _ := s.admitConn(ctx, "a", nil); ok {
		t.Fatal("admitted past MaxConns=2")
	}

	// Release the second connection; the room stays resident (the first
	// keeps it alive) but a global slot frees, so a new connection is
	// admitted.
	s.releaseConn(ctx, st2, c2)
	if _, _, ok, err := s.admitConn(ctx, "a", nil); !ok || err != nil {
		t.Fatalf("admit after release: ok=%v err=%v, want admitted", ok, err)
	}
}

// TestServer_MaxConns_ZeroUnlimited confirms the zero value imposes no
// global cap (the opt-in default).
func TestServer_MaxConns_ZeroUnlimited(t *testing.T) {
	s := &Server{docs: map[string]*docState{}} // MaxConns 0 = unlimited
	ctx := context.Background()
	for i := 0; i < 64; i++ {
		if _, _, ok, err := s.admitConn(ctx, "room", nil); !ok || err != nil {
			t.Fatalf("admit %d under unlimited: ok=%v err=%v", i, ok, err)
		}
	}
	if got := s.totalConns.Load(); got != 64 {
		t.Fatalf("totalConns = %d, want 64", got)
	}
}

// TestServer_MaxConns_NegativeUnlimited confirms a negative value is also
// unlimited, matching the documented semantics.
func TestServer_MaxConns_NegativeUnlimited(t *testing.T) {
	s := &Server{docs: map[string]*docState{}, opts: Options{MaxConns: -1}}
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		if _, _, ok, err := s.admitConn(ctx, "room", nil); !ok || err != nil {
			t.Fatalf("admit %d under MaxConns=-1: ok=%v err=%v", i, ok, err)
		}
	}
}

// TestServer_MaxConns_NoOvershootUnderChurn drives concurrent admit/release
// churn against a small cap under the race detector: no admission may push
// totalConns above the cap, and once the churn settles the count returns to
// zero (every admitted connection was released, none double-counted). Run
// with -race -count to exercise the atomic and lock discipline.
func TestServer_MaxConns_NoOvershootUnderChurn(t *testing.T) {
	const limit = 4
	s := &Server{docs: map[string]*docState{}, opts: Options{MaxConns: limit}}
	ctx := context.Background()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("d%d", g%3)
			for i := 0; i < 200; i++ {
				c, st, ok, err := s.admitConn(ctx, name, nil)
				if err != nil && !errors.Is(err, errServerConnsFull) {
					t.Errorf("unexpected admit err: %v", err)
					return
				}
				if ok {
					if got := s.totalConns.Load(); got > limit {
						t.Errorf("totalConns = %d overshot cap %d", got, limit)
					}
					s.releaseConn(ctx, st, c)
				}
			}
		}()
	}
	wg.Wait()

	if got := s.totalConns.Load(); got != 0 {
		t.Fatalf("totalConns after churn = %d, want 0", got)
	}
}
