package server_test

import (
	"context"
	"sync"
	"testing"

	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/server"
	"github.com/Deln0r/ygo/server/backplane"
)

// pushEdit appends value to the client's "items" array and sends it to the
// client's server as a SyncUpdate, returning nothing (the assertion is that
// the OTHER server's client observes it).
func pushEdit(t *testing.T, c *wsClient, value string) {
	t.Helper()
	arr := types.NewArray(c.doc.Branch("items"))
	txn := c.doc.WriteTxn()
	arr.Push(txn, value)
	txn.Commit()
	c.write(t, syncpkg.EncodeSyncUpdate(encoding.EncodeStateAsUpdate(c.doc)))
}

// awaitContains reads SyncUpdate frames on the client, applying each to the
// client's local doc, until its "items" array contains want, then returns
// the array contents. Reading until the value appears tolerates a stale
// self-echo (a client that edited earlier has its own broadcast queued
// ahead of the update it is now waiting for).
func awaitContains(t *testing.T, c *wsClient, want string) []any {
	t.Helper()
	for i := 0; i < 10; i++ {
		frame := c.readUntil(t, func(f *syncpkg.Frame) bool {
			return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncUpdate
		})
		if err := encoding.ApplyUpdate(c.doc, frame.Payload); err != nil {
			t.Fatal(err)
		}
		got := types.NewArray(c.doc.Branch("items")).ToSlice()
		if containsAll(got, want) {
			return got
		}
	}
	t.Fatalf("client never received an update containing %q", want)
	return nil
}

// TestServer_Backplane_ConvergesAcrossInstances: two independent servers
// share a document ONLY through an in-process backplane (no shared Store).
// An edit from a client of one server reaches a client of the other, in
// both directions, proving cross-instance publish + subscribe.
func TestServer_Backplane_ConvergesAcrossInstances(t *testing.T) {
	hub := backplane.NewMemory()
	t.Cleanup(func() { _ = hub.Close() })

	wsA, _ := startTestServer(t, server.Options{OriginPatterns: []string{"*"}, Backplane: hub.Conn()})
	wsB, _ := startTestServer(t, server.Options{OriginPatterns: []string{"*"}, Backplane: hub.Conn()})

	const docName = "shared"
	// Both clients connect first, so each server holds the document resident
	// and subscribed before any edit is published.
	a := dialClient(t, wsA, docName, 100)
	defer a.close()
	a.read(t) // server A's initial SyncStep1
	b := dialClient(t, wsB, docName, 200)
	defer b.close()
	b.read(t) // server B's initial SyncStep1

	// A -> B: edit on server A reaches B's client via the backplane.
	pushEdit(t, a, "from-A")
	if got := awaitContains(t, b, "from-A"); len(got) != 1 || got[0] != "from-A" {
		t.Fatalf("B did not converge on A's edit: %v, want [from-A]", got)
	}

	// B -> A: and back the other way. A has its own "from-A" self-echo
	// queued, so awaitContains reads past it to B's propagated edit; both
	// values must then be present (order across clients is not guaranteed).
	pushEdit(t, b, "from-B")
	if got := awaitContains(t, a, "from-B"); !containsAll(got, "from-A", "from-B") {
		t.Fatalf("A did not converge on B's edit: %v, want both from-A and from-B", got)
	}
}

// TestServer_Backplane_LateJoinerSyncsFromResidentDoc: a client that
// connects to a second server AFTER an edit was published still converges,
// because the backplane applied the edit to that server's resident copy, so
// its initial SyncStep2 already carries it.
func TestServer_Backplane_LateJoinerSyncsFromResidentDoc(t *testing.T) {
	hub := backplane.NewMemory()
	t.Cleanup(func() { _ = hub.Close() })

	wsA, _ := startTestServer(t, server.Options{OriginPatterns: []string{"*"}, Backplane: hub.Conn()})
	wsB, _ := startTestServer(t, server.Options{OriginPatterns: []string{"*"}, Backplane: hub.Conn()})

	const docName = "shared"
	// Keeper holds the document resident on server B so B stays subscribed.
	keeper := dialClient(t, wsB, docName, 1)
	defer keeper.close()
	keeper.read(t)

	a := dialClient(t, wsA, docName, 100)
	defer a.close()
	a.read(t)

	// A edits; the backplane applies it into B's resident copy.
	pushEdit(t, a, "from-A")
	// keeper observes it (confirms B applied it before the late joiner arrives).
	if got := awaitContains(t, keeper, "from-A"); len(got) != 1 || got[0] != "from-A" {
		t.Fatalf("keeper did not converge: %v", got)
	}

	// Late joiner connects to B and syncs from B's now-updated resident doc.
	late := dialClient(t, wsB, docName, 300)
	defer late.close()
	first := late.read(t)
	if first.Type != syncpkg.MessageSync || first.SyncSub != syncpkg.SyncStep1 {
		t.Fatalf("late joiner first frame = %d/%d, want SyncStep1", first.Type, first.SyncSub)
	}
	// Reply with an empty state vector; B answers with SyncStep2 carrying A's edit.
	late.write(t, syncpkg.EncodeSyncStep1(encoding.EncodeStateVector(map[uint64]uint64{}, nil)))
	step2 := late.readUntil(t, func(f *syncpkg.Frame) bool {
		return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncStep2
	})
	if err := encoding.ApplyUpdate(late.doc, step2.Payload); err != nil {
		t.Fatal(err)
	}
	if got := types.NewArray(late.doc.Branch("items")).ToSlice(); len(got) != 1 || got[0] != "from-A" {
		t.Fatalf("late joiner did not sync A's edit from B: %v", got)
	}
}

func containsAll(got []any, want ...string) bool {
	have := map[string]bool{}
	for _, v := range got {
		if s, ok := v.(string); ok {
			have[s] = true
		}
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

// orderBackplane and orderStore record the call order of Subscribe vs the
// Store load, so a test can assert getOrCreateDocLocked subscribes BEFORE it
// loads (closing the window where an update published during the load would
// reach neither the not-yet-live subscription nor the already-read copy).
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) note(ev string) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}
func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

type orderBackplane struct{ rec *recorder }

func (o orderBackplane) Publish(context.Context, string, []byte) error { return nil }
func (o orderBackplane) Subscribe(string, func([]byte)) (func(), error) {
	o.rec.note("subscribe")
	return func() {}, nil
}
func (o orderBackplane) Close() error { return nil }

type orderStore struct{ rec *recorder }

func (o orderStore) GetUpdates(context.Context, string) ([][]byte, error) {
	o.rec.note("load")
	return nil, nil
}
func (orderStore) StoreUpdate(context.Context, string, []byte) error    { return nil }
func (orderStore) Flush(context.Context, string) error                  { return nil }
func (orderStore) DocumentExists(context.Context, string) (bool, error) { return false, nil }
func (orderStore) ListDocuments(context.Context) ([]string, error)      { return nil, nil }
func (orderStore) ClearDocument(context.Context, string) error          { return nil }
func (orderStore) Close() error                                         { return nil }

// TestServer_Backplane_SubscribesBeforeLoad pins the ordering invariant: a
// document becoming resident subscribes to the backplane before it reads the
// Store. Reversing it reopens the drop-during-load divergence window.
func TestServer_Backplane_SubscribesBeforeLoad(t *testing.T) {
	rec := &recorder{}
	wsURL, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"},
		Backplane:      orderBackplane{rec},
		Store:          orderStore{rec},
	})

	c := dialClient(t, wsURL, "d", 1)
	defer c.close()
	c.read(t) // completes getOrCreateDocLocked (subscribe + load recorded)

	got := rec.snapshot()
	if len(got) < 2 || got[0] != "subscribe" {
		t.Fatalf("call order = %v, want subscribe before load", got)
	}
	sawLoad := false
	for _, ev := range got[1:] {
		if ev == "load" {
			sawLoad = true
		}
	}
	if !sawLoad {
		t.Fatalf("call order = %v, want a load after subscribe", got)
	}
}

// gateStore blocks the first GetUpdates (the first LoadDoc) until release is
// closed, signalling via entered when that load begins. It holds one
// instance inside getOrCreateDocLocked's load while another instance
// publishes, so a test can exercise the subscribe-before-load buffer path.
type gateStore struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGateStore() *gateStore {
	return &gateStore{entered: make(chan struct{}), release: make(chan struct{})}
}
func (g *gateStore) GetUpdates(context.Context, string) ([][]byte, error) {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	return nil, nil
}
func (*gateStore) StoreUpdate(context.Context, string, []byte) error    { return nil }
func (*gateStore) Flush(context.Context, string) error                  { return nil }
func (*gateStore) DocumentExists(context.Context, string) (bool, error) { return false, nil }
func (*gateStore) ListDocuments(context.Context) ([]string, error)      { return nil, nil }
func (*gateStore) ClearDocument(context.Context, string) error          { return nil }
func (*gateStore) Close() error                                         { return nil }

// TestServer_Backplane_BuffersUpdateDuringLoad exercises the subscribe-
// before-load buffer end to end: instance B is held inside its document load
// while instance A publishes an edit; B must not lose it. With the old
// load-then-subscribe order B would not yet be subscribed and the edit would
// be dropped, diverging B permanently.
func TestServer_Backplane_BuffersUpdateDuringLoad(t *testing.T) {
	hub := backplane.NewMemory()
	t.Cleanup(func() { _ = hub.Close() })

	wsA, _ := startTestServer(t, server.Options{OriginPatterns: []string{"*"}, Backplane: hub.Conn()})
	gate := newGateStore()
	wsB, _ := startTestServer(t, server.Options{OriginPatterns: []string{"*"}, Backplane: hub.Conn(), Store: gate})

	const docName = "shared"
	a := dialClient(t, wsA, docName, 100)
	defer a.close()
	a.read(t) // A holds the doc resident and subscribed

	// Dialing B accepts the WS, then B's serveWS blocks in getOrCreateDocLocked's
	// load (gateStore) — but only AFTER it has already subscribed.
	b := dialClient(t, wsB, docName, 200)
	defer b.close()
	<-gate.entered // B is subscribed and now blocked loading

	// A edits while B is mid-load; B's live subscription buffers it.
	pushEdit(t, a, "from-A")
	// A's self-echo is broadcast in the same goroutine that then Publishes to
	// the backplane (onAppliedUpdate runs right after the fan-out), so
	// receiving it means the edit reached the hub while B is still inside its
	// load. A reverted load-then-subscribe B would have no subscription yet
	// and drop the edit here, so this makes the buffer path a revert guard.
	a.readUntil(t, func(f *syncpkg.Frame) bool {
		return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncUpdate
	})

	// Release B's load; the buffered edit drains into B's doc.
	close(gate.release)

	// B's client must observe the edit, via its initial sync or a follow-up
	// SyncUpdate depending on buffer-vs-live timing.
	syncAndAwait(t, b, "from-A")
}

// syncAndAwait completes the initial SyncStep1 handshake and then applies any
// SyncStep2/SyncUpdate frames until the client's "items" array contains want.
func syncAndAwait(t *testing.T, c *wsClient, want string) {
	t.Helper()
	first := c.read(t)
	if first.Type != syncpkg.MessageSync || first.SyncSub != syncpkg.SyncStep1 {
		t.Fatalf("first frame = %d/%d, want SyncStep1", first.Type, first.SyncSub)
	}
	c.write(t, syncpkg.EncodeSyncStep1(encoding.EncodeStateVector(map[uint64]uint64{}, nil)))
	for i := 0; i < 10; i++ {
		f := c.readUntil(t, func(f *syncpkg.Frame) bool {
			return f.Type == syncpkg.MessageSync &&
				(f.SyncSub == syncpkg.SyncStep2 || f.SyncSub == syncpkg.SyncUpdate)
		})
		if len(f.Payload) > 0 {
			if err := encoding.ApplyUpdate(c.doc, f.Payload); err != nil {
				t.Fatal(err)
			}
		}
		if containsAll(types.NewArray(c.doc.Branch("items")).ToSlice(), want) {
			return
		}
	}
	t.Fatalf("client never received %q", want)
}
