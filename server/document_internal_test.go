package server

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestWaitNotClosing_AbortsOnSocketClose: a Hocuspocus admission waiting
// for a closing document stops when its socket's writer exits (abort), so
// its session can release the keys it holds on that document; without the
// signal it waits for the close.
func TestWaitNotClosing_AbortsOnSocketClose(t *testing.T) {
	s := New(Options{})
	defer func() { _ = s.Close(context.Background()) }()
	st := &docState{name: "doc", conns: map[*conn]struct{}{}, closing: make(chan struct{}), closeWake: make(chan struct{}, 1)}
	s.docsMu.Lock()
	s.docs["doc"] = st
	s.docsMu.Unlock()

	abort := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- s.waitNotClosing(context.Background(), "doc", false, abort) }()
	select {
	case err := <-done:
		t.Fatalf("returned while the document closes: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(abort)
	select {
	case err := <-done:
		if !errors.Is(err, errDocClosing) {
			t.Fatalf("aborted wait: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the wait did not stop when the socket closed")
	}

	// A server operation is woken by Close instead.
	op := make(chan error, 1)
	go func() { op <- s.waitNotClosing(context.Background(), "doc", true, nil) }()
	time.Sleep(20 * time.Millisecond)
	s.docsMu.Lock()
	s.closed = true
	close(s.closedCh)
	s.docsMu.Unlock()
	select {
	case err := <-op:
		if !errors.Is(err, ErrServerClosed) {
			t.Fatalf("operation wait: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not wake the operation")
	}
	// Let Close (deferred) not close closedCh twice.
	s.docsMu.Lock()
	s.closed = true
	s.docsMu.Unlock()
	close(st.closing)
}
