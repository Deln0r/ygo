package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestAdmitConn_RegistersAtomically pins admitConn's core invariant: an
// admitted connection is always inserted into the docState currently
// registered under its name (never an orphan), and a second admission
// joins that same registered state rather than a divergent one. Holding
// docsMu across the find/create and the insert is what guarantees this;
// the test locks the post-condition so a future split of that critical
// section (the original split-room bug) is caught.
func TestAdmitConn_RegistersAtomically(t *testing.T) {
	s := &Server{docs: map[string]*docState{}}

	c, state, ok, err := s.admitConn(context.Background(), "room", nil)
	if err != nil || !ok {
		t.Fatalf("admitConn: ok=%v err=%v", ok, err)
	}

	s.docsMu.Lock()
	reg := s.docs["room"]
	s.docsMu.Unlock()
	if reg != state {
		t.Fatalf("admitted state %p is not the registered one %p (orphan)", state, reg)
	}

	state.connsMu.RLock()
	_, present := state.conns[c]
	state.connsMu.RUnlock()
	if !present {
		t.Fatal("admitted conn is not in the docState conn set")
	}

	// A second admission must join the SAME registered state.
	c2, state2, ok2, err2 := s.admitConn(context.Background(), "room", nil)
	if err2 != nil || !ok2 {
		t.Fatalf("second admitConn: ok=%v err=%v", ok2, err2)
	}
	if state2 != state {
		t.Fatal("second admission created a divergent docState for the same name")
	}
	state.connsMu.RLock()
	n := len(state.conns)
	_, has2 := state.conns[c2]
	state.connsMu.RUnlock()
	if n != 2 || !has2 {
		t.Fatalf("conn set = %d (has c2=%v), want 2", n, has2)
	}
}

// TestAdmitConn_RejectsAtCap confirms the per-doc cap is enforced through
// admitConn and that a rejected admission neither registers its conn nor
// disturbs the established one.
func TestAdmitConn_RejectsAtCap(t *testing.T) {
	s := &Server{docs: map[string]*docState{}, opts: Options{MaxConnsPerDoc: 1}}

	if _, _, ok, _ := s.admitConn(context.Background(), "room", nil); !ok {
		t.Fatal("first admission under cap 1 was rejected")
	}
	_, state, ok, err := s.admitConn(context.Background(), "room", nil)
	if err != nil {
		t.Fatalf("second admitConn err: %v", err)
	}
	if ok {
		t.Fatal("second admission exceeded cap 1")
	}
	state.connsMu.RLock()
	n := len(state.conns)
	state.connsMu.RUnlock()
	if n != 1 {
		t.Fatalf("conn set after rejected admission = %d, want 1", n)
	}
}

// releaseConn returns only after the connection's writer has exited, so
// OnDisconnect, which serveWS calls after it, never runs beside a writer that
// is still closing the socket.
func TestReleaseConn_WaitsForWriter(t *testing.T) {
	s := New(Options{})
	defer func() { _ = s.Close(context.Background()) }()
	c, state, ok, err := s.admitConn(context.Background(), "room", nil)
	if !ok || err != nil {
		t.Fatalf("admitConn: ok=%v err=%v", ok, err)
	}
	// Stand in for a writer that takes a while to finish its close.
	c.queue.done = make(chan struct{})
	var exited atomic.Bool
	go func() {
		time.Sleep(100 * time.Millisecond)
		exited.Store(true)
		close(c.queue.done)
	}()
	s.releaseConn(context.Background(), state, c)
	if !exited.Load() {
		t.Fatal("releaseConn returned before the connection's writer exited")
	}
	if got := c.queue.terminalCause(); got != causeReleased {
		t.Fatalf("queue cause after release = %d, want causeReleased", got)
	}
}
