package server

import (
	"context"
	"testing"

	"github.com/Deln0r/ygo/internal/awareness"
)

// presenceWire builds a single-client awareness wire update for clientID id with
// the given JSON state, as another instance would relay over the Backplane.
func presenceWire(id uint64, state string) []byte {
	a := awareness.New(id)
	a.SetLocalState([]byte(state))
	return a.Encode(nil)
}

// TestServer_BackplaneAwareness_HonorsForeignSubCap proves the origin partition
// end to end through the server: a document created by the real code path is
// wired with the resolved foreign sub-cap, and applyBackplaneAwareness routes
// relayed presence through ApplyForeign, so a second foreign client is dropped
// once the sub-cap is full while a local client still gets a slot. If either the
// creation wiring or the ApplyForeign routing regressed, the dropped foreign
// client would be admitted and this test would fail.
func TestServer_BackplaneAwareness_HonorsForeignSubCap(t *testing.T) {
	s := New(Options{MaxAwarenessClients: 3, MaxForeignAwarenessClients: 1})
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	s.docsMu.Lock()
	state, err := s.getOrCreateDocLocked(context.Background(), "d")
	s.docsMu.Unlock()
	if err != nil {
		t.Fatalf("getOrCreateDocLocked: %v", err)
	}

	// First relayed presence takes the single foreign slot.
	s.applyBackplaneAwareness(state, presenceWire(300, `{"n":1}`))
	if _, ok := state.awareness.States()[300]; !ok {
		t.Fatal("first relayed presence was not admitted")
	}
	// Second relayed presence is dropped: the foreign sub-cap is full.
	s.applyBackplaneAwareness(state, presenceWire(301, `{"n":2}`))
	if _, ok := state.awareness.States()[301]; ok {
		t.Fatal("second relayed presence admitted past the foreign sub-cap; " +
			"applyBackplaneAwareness must route through ApplyForeign against a wired cap")
	}

	// A local client (applied via the local path) still gets one of the two
	// slots reserved beyond the foreign sub-cap.
	if _, err := state.awareness.Apply(presenceWire(700, `{"n":3}`), "local"); err != nil {
		t.Fatalf("local Apply: %v", err)
	}
	if _, ok := state.awareness.States()[700]; !ok {
		t.Fatal("local presence was starved by relayed presence")
	}
}

// TestServer_BackplaneAwareness_DefaultReservesLocalFloor checks the zero-value
// default path (MaxForeignAwarenessClients unset): New must resolve a foreign
// sub-cap that reserves 1/8 of MaxAwarenessClients for local clients, and the
// created document must enforce it. With MaxAwarenessClients=8 the sub-cap is 7,
// so the 8th foreign client is dropped but a local client fills the last slot.
func TestServer_BackplaneAwareness_DefaultReservesLocalFloor(t *testing.T) {
	s := New(Options{MaxAwarenessClients: 8}) // foreign sub-cap defaults to 7
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	s.docsMu.Lock()
	state, err := s.getOrCreateDocLocked(context.Background(), "d")
	s.docsMu.Unlock()
	if err != nil {
		t.Fatalf("getOrCreateDocLocked: %v", err)
	}

	// Relay 8 foreign clients; only 7 are admitted (1 slot reserved for local).
	for id := uint64(300); id < 308; id++ {
		s.applyBackplaneAwareness(state, presenceWire(id, `{"n":1}`))
	}
	present := 0
	for id := uint64(300); id < 308; id++ {
		if _, ok := state.awareness.States()[id]; ok {
			present++
		}
	}
	if present != 7 {
		t.Fatalf("relayed presences admitted = %d, want 7 (foreign sub-cap = 8 - 8/8)", present)
	}

	// The reserved slot admits a local client.
	if _, err := state.awareness.Apply(presenceWire(700, `{"n":2}`), "local"); err != nil {
		t.Fatalf("local Apply: %v", err)
	}
	if _, ok := state.awareness.States()[700]; !ok {
		t.Fatal("reserved local slot did not admit a local client")
	}
}
