package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// idleCountStore is a minimal persist.Store counting Flush calls, so the
// park/evict paths can be asserted durably without sqlite.
type idleCountStore struct{ flushes atomic.Int64 }

func (s *idleCountStore) StoreUpdate(ctx context.Context, doc string, u []byte) error { return nil }
func (s *idleCountStore) GetUpdates(ctx context.Context, doc string) ([][]byte, error) {
	return nil, nil
}
func (s *idleCountStore) Flush(ctx context.Context, doc string) error {
	s.flushes.Add(1)
	return nil
}
func (s *idleCountStore) DocumentExists(ctx context.Context, doc string) (bool, error) {
	return false, nil
}
func (s *idleCountStore) ListDocuments(ctx context.Context) ([]string, error) { return nil, nil }
func (s *idleCountStore) ClearDocument(ctx context.Context, doc string) error { return nil }
func (s *idleCountStore) Close() error                                        { return nil }

// TestReleaseConn_ParksInsteadOfEvicting: with DocIdleTimeout set, the last
// disconnect parks the document — still registered, idleSince stamped, the
// going-idle flush written, and the backplane subscription NOT stopped (an
// idle doc keeps applying foreign updates; that is the point of staying warm).
func TestReleaseConn_ParksInsteadOfEvicting(t *testing.T) {
	cs := &idleCountStore{}
	s := &Server{docs: map[string]*docState{}, opts: Options{DocIdleTimeout: time.Hour, Store: cs}}

	c, state, ok, err := s.admitConn(context.Background(), "room", nil)
	if err != nil || !ok {
		t.Fatalf("admit: ok=%v err=%v", ok, err)
	}
	var unsubbed atomic.Bool
	state.backplaneUnsub = func() { unsubbed.Store(true) }

	s.releaseConn(context.Background(), state, c)

	s.docsMu.Lock()
	reg, resident := s.docs["room"]
	idle := resident && !reg.idleSince.IsZero()
	s.docsMu.Unlock()
	if !resident || !idle {
		t.Fatalf("resident=%v idleStamped=%v; want parked (both true)", resident, idle)
	}
	if unsubbed.Load() {
		t.Fatal("backplane unsubscribed on park; an idle doc must keep its subscription")
	}
	if got := cs.flushes.Load(); got != 1 {
		t.Fatalf("flushes on park = %d, want 1 (durability matches eviction)", got)
	}
}

// TestAdmitConn_UnparksTheDocument: a reconnect inside the window clears
// idleSince under docsMu, so the sweep can never evict a re-admitted doc.
func TestAdmitConn_UnparksTheDocument(t *testing.T) {
	s := &Server{docs: map[string]*docState{}, opts: Options{DocIdleTimeout: time.Hour}}
	c, state, _, _ := s.admitConn(context.Background(), "room", nil)
	s.releaseConn(context.Background(), state, c)

	// Age the parked doc far past any timeout, then re-admit.
	s.docsMu.Lock()
	state.idleSince = time.Now().Add(-24 * time.Hour)
	s.docsMu.Unlock()
	if _, state2, ok, _ := s.admitConn(context.Background(), "room", nil); !ok || state2 != state {
		t.Fatalf("re-admit: ok=%v same=%v; want warm reuse of the parked state", ok, state2 == state)
	}

	s.sweepIdle(context.Background()) // must be a no-op: the doc has a connection again
	s.docsMu.Lock()
	_, still := s.docs["room"]
	cleared := state.idleSince.IsZero()
	s.docsMu.Unlock()
	if !still || !cleared {
		t.Fatalf("resident=%v idleCleared=%v after re-admit + sweep; want both", still, cleared)
	}
}

// TestSweepIdle_EvictsExpiredKeepsFresh: only documents idle past the
// timeout are evicted; a freshly parked one survives, and eviction finishes
// with the backplane unsubscribe and a final flush.
func TestSweepIdle_EvictsExpiredKeepsFresh(t *testing.T) {
	cs := &idleCountStore{}
	s := &Server{docs: map[string]*docState{}, opts: Options{DocIdleTimeout: time.Minute, Store: cs}}

	mk := func(name string) *docState {
		c, st, ok, err := s.admitConn(context.Background(), name, nil)
		if err != nil || !ok {
			t.Fatalf("admit %s: %v", name, err)
		}
		s.releaseConn(context.Background(), st, c) // park
		return st
	}
	old := mk("old")
	fresh := mk("fresh")
	var oldUnsub atomic.Bool
	old.backplaneUnsub = func() { oldUnsub.Store(true) }

	s.docsMu.Lock()
	old.idleSince = time.Now().Add(-2 * time.Minute) // expired
	s.docsMu.Unlock()
	_ = fresh

	flushesBefore := cs.flushes.Load()
	s.sweepIdle(context.Background())

	s.docsMu.Lock()
	_, oldThere := s.docs["old"]
	_, freshThere := s.docs["fresh"]
	s.docsMu.Unlock()
	if oldThere || !freshThere {
		t.Fatalf("old resident=%v fresh resident=%v; want evicted/kept", oldThere, freshThere)
	}
	if !oldUnsub.Load() {
		t.Fatal("eviction must stop the backplane subscription")
	}
	if cs.flushes.Load() != flushesBefore+1 {
		t.Fatalf("eviction flushes = %d, want exactly one more than %d", cs.flushes.Load(), flushesBefore)
	}
}

// TestMaxIdleDocs_EvictsLeastRecentlyIdle: parking past the bound evicts the
// OLDEST idle document immediately, keeping the newly parked one warm.
func TestMaxIdleDocs_EvictsLeastRecentlyIdle(t *testing.T) {
	cs := &idleCountStore{}
	s := &Server{docs: map[string]*docState{}, opts: Options{DocIdleTimeout: time.Hour, MaxIdleDocs: 1, Store: cs}}

	cA, stA, _, _ := s.admitConn(context.Background(), "a", nil)
	cB, stB, _, _ := s.admitConn(context.Background(), "b", nil)
	var aUnsub, bUnsub atomic.Bool
	stA.backplaneUnsub = func() { aUnsub.Store(true) }
	stB.backplaneUnsub = func() { bUnsub.Store(true) }

	s.releaseConn(context.Background(), stA, cA) // a parks (idle count 1, within bound)
	s.docsMu.Lock()
	stA.idleSince = time.Now().Add(-time.Minute) // make a strictly older
	s.docsMu.Unlock()
	flushesBefore := cs.flushes.Load()           // a's park flush
	s.releaseConn(context.Background(), stB, cB) // b parks -> bound exceeded -> a evicted

	s.docsMu.Lock()
	_, aThere := s.docs["a"]
	_, bThere := s.docs["b"]
	s.docsMu.Unlock()
	if aThere || !bThere {
		t.Fatalf("a resident=%v b resident=%v; want LRU eviction of a, b kept", aThere, bThere)
	}
	// The off-lock half of the LRU eviction must actually finish the victim:
	// stop ITS backplane subscription (and only its) and write its final
	// flush alongside b's park flush. Without these assertions, deleting the
	// finishEviction call passes every test while leaking a live
	// subscription on every MaxIdleDocs eviction - verified by mutation.
	if !aUnsub.Load() {
		t.Fatal("LRU victim's backplane subscription was not stopped")
	}
	if bUnsub.Load() {
		t.Fatal("survivor's backplane subscription must stay alive while parked")
	}
	if got := cs.flushes.Load(); got != flushesBefore+2 {
		t.Fatalf("flushes after LRU park+evict = %d, want %d (b park + a final)", got, flushesBefore+2)
	}
}

// TestClose_DrainsParkedDocsAndBlocksNewParks: Close evicts idle-parked
// documents (their backplane subscriptions must not outlive the server) and
// a disconnect racing or following Close evicts instead of parking into a
// registry no sweeper will scan again.
func TestClose_DrainsParkedDocsAndBlocksNewParks(t *testing.T) {
	cs := &idleCountStore{}
	s := &Server{docs: map[string]*docState{}, opts: Options{DocIdleTimeout: time.Hour, Store: cs}}

	cA, stA, _, _ := s.admitConn(context.Background(), "parked", nil)
	var aUnsub atomic.Bool
	stA.backplaneUnsub = func() { aUnsub.Store(true) }
	s.releaseConn(context.Background(), stA, cA) // parked before Close

	cB, stB, _, _ := s.admitConn(context.Background(), "live", nil) // still connected at Close

	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !aUnsub.Load() {
		t.Fatal("Close left the parked doc's backplane subscription alive")
	}
	s.docsMu.Lock()
	_, parkedThere := s.docs["parked"]
	s.docsMu.Unlock()
	if parkedThere {
		t.Fatal("parked doc still registered after Close")
	}

	// A disconnect AFTER Close must evict, not park.
	s.releaseConn(context.Background(), stB, cB)
	s.docsMu.Lock()
	_, liveThere := s.docs["live"]
	s.docsMu.Unlock()
	if liveThere {
		t.Fatal("post-Close disconnect parked the doc instead of evicting it")
	}
}

// TestStats_CountsIdleDocuments: a parked doc still counts as resident and
// is reported via IdleDocuments.
func TestStats_CountsIdleDocuments(t *testing.T) {
	s := &Server{docs: map[string]*docState{}, opts: Options{DocIdleTimeout: time.Hour}}
	c, st, _, _ := s.admitConn(context.Background(), "room", nil)
	s.releaseConn(context.Background(), st, c)

	got := s.Stats()
	if got.Documents != 1 || got.IdleDocuments != 1 || got.Connections != 0 {
		t.Fatalf("Stats = %+v; want Documents=1 IdleDocuments=1 Connections=0", got)
	}
}
