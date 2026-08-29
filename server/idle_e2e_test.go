package server_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/server"
)

// loadCountStore counts document loads (GetUpdates) and flushes, and retains
// stored updates in memory so a reload actually replays history.
type loadCountStore struct {
	loads, flushes atomic.Int64
	mu             chan struct{} // 1-slot semaphore; e2e is low-concurrency
	updates        map[string][][]byte
}

func newLoadCountStore() *loadCountStore {
	s := &loadCountStore{mu: make(chan struct{}, 1), updates: map[string][][]byte{}}
	s.mu <- struct{}{}
	return s
}
func (s *loadCountStore) StoreUpdate(ctx context.Context, docName string, u []byte) error {
	<-s.mu
	s.updates[docName] = append(s.updates[docName], append([]byte(nil), u...))
	s.mu <- struct{}{}
	return nil
}
func (s *loadCountStore) GetUpdates(ctx context.Context, docName string) ([][]byte, error) {
	s.loads.Add(1)
	<-s.mu
	out := make([][]byte, len(s.updates[docName]))
	copy(out, s.updates[docName])
	s.mu <- struct{}{}
	return out, nil
}
func (s *loadCountStore) Flush(ctx context.Context, docName string) error {
	s.flushes.Add(1)
	return nil
}
func (s *loadCountStore) DocumentExists(ctx context.Context, docName string) (bool, error) {
	return false, nil
}
func (s *loadCountStore) ListDocuments(ctx context.Context) ([]string, error)     { return nil, nil }
func (s *loadCountStore) ClearDocument(ctx context.Context, docName string) error { return nil }
func (s *loadCountStore) Close() error                                            { return nil }

func editAndClose(t *testing.T, wsURL, docName string, clientID uint64, val string) {
	t.Helper()
	c := dialClient(t, wsURL, docName, clientID)
	defer c.close()
	c.read(t) // server SyncStep1
	arr := types.NewArray(c.doc.Branch("items"))
	txn := c.doc.WriteTxn()
	arr.Push(txn, val)
	txn.Commit()
	c.write(t, syncpkg.EncodeSyncUpdate(encoding.EncodeStateAsUpdate(c.doc)))
	// Await the self-broadcast so the server has definitely applied it.
	c.readUntil(t, func(f *syncpkg.Frame) bool {
		return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncUpdate
	})
}

// TestServer_DocIdleTimeout_WarmReconnectSkipsReload is the feature's
// headline: with keep-warm on, a disconnect+reconnect does NOT reload the
// document from the Store, yet the reconnecting client still receives the
// content. The control server (keep-warm off) reloads on reconnect,
// proving the counter would have caught it.
func TestServer_DocIdleTimeout_WarmReconnectSkipsReload(t *testing.T) {
	run := func(idle time.Duration) (*loadCountStore, string) {
		cs := newLoadCountStore()
		wsURL, srv := startTestServer(t, server.Options{
			OriginPatterns: []string{"*"},
			Store:          cs,
			DocIdleTimeout: idle,
		})
		editAndClose(t, wsURL, "warmdoc", 100, "from-first-session")
		// The server releases the connection asynchronously after observing
		// the close; without a barrier the reconnect can win that race, which
		// would make the control spuriously fail (doc never evicted) and the
		// warm assertion vacuous (doc never released). Poll Stats until THIS
		// run's disconnect has completed: parked for the warm server, evicted
		// for the control.
		deadline := time.Now().Add(5 * time.Second)
		for {
			st := srv.Stats()
			if idle > 0 && st.Connections == 0 && st.IdleDocuments == 1 {
				break
			}
			if idle == 0 && st.Documents == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("disconnect never settled: %+v (idle=%v)", st, idle)
			}
			time.Sleep(5 * time.Millisecond)
		}
		// Reconnect and pull the full state via an empty state vector.
		c := dialClient(t, wsURL, "warmdoc", 200)
		defer c.close()
		c.read(t)
		c.write(t, syncpkg.EncodeSyncStep1(encoding.EncodeStateVector(map[uint64]uint64{}, nil)))
		f := c.readUntil(t, func(f *syncpkg.Frame) bool {
			return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncStep2
		})
		d := doc.NewDoc()
		u, _, err := encoding.DecodeUpdate(f.Payload)
		if err != nil {
			t.Fatalf("decode step2: %v", err)
		}
		txn := d.WriteTxn()
		if err := u.Apply(txn); err != nil {
			t.Fatalf("apply step2: %v", err)
		}
		txn.Commit()
		arr := types.NewArray(d.Branch("items"))
		if arr.Len() != 1 {
			t.Fatalf("reconnect content: items len=%d, want 1", arr.Len())
		}
		return cs, arr.Get(0).(string)
	}

	warm, got := run(time.Hour)
	if got != "from-first-session" {
		t.Fatalf("warm reconnect content = %q", got)
	}
	if n := warm.loads.Load(); n != 1 {
		t.Fatalf("warm server loads = %d, want 1 (reconnect must reuse the resident doc)", n)
	}

	cold, _ := run(0) // control: keep-warm off must reload
	if n := cold.loads.Load(); n != 2 {
		t.Fatalf("control server loads = %d, want 2 (this proves the counter detects reloads)", n)
	}
}

// TestServer_FlushEvery_CompactsOnCadence: with FlushEvery=3, seven stored
// updates produce exactly two automatic flushes before the disconnect adds
// its going-idle/eviction flush.
func TestServer_FlushEvery_CompactsOnCadence(t *testing.T) {
	cs := newLoadCountStore()
	wsURL, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"},
		Store:          cs,
		FlushEvery:     3,
	})
	c := dialClient(t, wsURL, "cadence", 300)
	c.read(t)
	arr := types.NewArray(c.doc.Branch("items"))
	for i := 0; i < 7; i++ {
		txn := c.doc.WriteTxn()
		arr.Push(txn, i)
		txn.Commit()
		c.write(t, syncpkg.EncodeSyncUpdate(encoding.EncodeStateAsUpdate(c.doc)))
		c.readUntil(t, func(f *syncpkg.Frame) bool {
			return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncUpdate
		})
	}
	if n := cs.flushes.Load(); n != 2 {
		t.Fatalf("auto-flushes after 7 updates with FlushEvery=3: got %d, want 2", n)
	}
	c.close()
}
