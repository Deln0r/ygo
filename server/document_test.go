package server_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Deln0r/ygo"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/memstore"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/server"
	"github.com/Deln0r/ygo/server/backplane"
)

// pushServer is a server write that appends value to "items".
func pushServer(value string) func(txn *ygo.TransactionMut) error {
	return func(txn *ygo.TransactionMut) error {
		ygo.ArrayIn(txn, "items").Push(txn, value)
		return nil
	}
}

// stateItems decodes "items" of a document state.
func stateItems(t *testing.T, state []byte) []any {
	t.Helper()
	d := doc.NewDoc()
	if err := encoding.ApplyUpdate(d, state); err != nil {
		t.Fatal(err)
	}
	return types.NewArray(d.Branch("items")).ToSlice()
}

// syncClient completes a y-websocket client's handshake (step 1 from the
// server, step 2 for it), so later frames are broadcasts.
func syncClient(t *testing.T, c *wsClient) {
	t.Helper()
	c.readUntil(t, func(f *syncpkg.Frame) bool {
		return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncStep1
	})
}

// updateValues lists the values an update's items carry (an incremental
// update does not integrate into an empty document).
func updateValues(t *testing.T, u []byte) []any {
	t.Helper()
	upd, _, err := encoding.DecodeUpdate(u)
	if err != nil {
		t.Fatal(err)
	}
	var out []any
	for _, blocks := range upd.Blocks {
		for _, b := range blocks {
			if b.Item != nil {
				out = append(out, b.Item.Content.Anys...)
			}
		}
	}
	return out
}

func storeItems(t *testing.T, st *memstore.Store, name string) []any {
	t.Helper()
	ups, err := st.GetUpdates(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := ygo.MergeUpdates(ups)
	if err != nil {
		t.Fatal(err)
	}
	if merged == nil {
		return nil
	}
	return stateItems(t, merged)
}

// TestApply_ReachesEveryRecipient: a server write reaches a y-websocket
// client, a Hocuspocus document and a client of another instance on the
// same Backplane, is stored, and is reported to OnChange once, all with the
// update Apply returns.
func TestApply_ReachesEveryRecipient(t *testing.T) {
	hub := backplane.NewMemory()
	t.Cleanup(func() { _ = hub.Close() })
	store := memstore.New()
	var mu sync.Mutex
	var changes [][]byte
	urlA, a := startTestServer(t, server.Options{Store: store, Backplane: hub.Conn(), OnChange: func(name string, u []byte) {
		mu.Lock()
		changes = append(changes, append([]byte(nil), u...))
		mu.Unlock()
	}})
	urlB, _ := startTestServer(t, server.Options{Backplane: hub.Conn()})

	y := dialClient(t, urlA, "doc", 11)
	defer y.close()
	syncClient(t, y)
	hp := hpDial(t, urlA)
	hp.auth(t, "doc", "", syncpkg.ScopeReadWrite)
	hpSync(t, hp, "doc")
	other := dialClient(t, urlB, "doc", 12)
	defer other.close()
	syncClient(t, other)

	update, err := a.Apply(context.Background(), "doc", pushServer("from-server"))
	if err != nil || update == nil {
		t.Fatalf("Apply: %x, %v", update, err)
	}
	awaitContains(t, y, "from-server")
	if f := hp.want(t, "doc", syncpkg.MessageSync); !bytes.Equal(f.Payload, update) {
		t.Fatalf("Hocuspocus got %x, want %x", f.Payload, update)
	}
	awaitContains(t, other, "from-server")
	if got := storeItems(t, store, "doc"); fmt.Sprint(got) != "[from-server]" {
		t.Fatalf("stored %v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(changes) != 1 || !bytes.Equal(changes[0], update) {
		t.Fatalf("OnChange got %d updates", len(changes))
	}
}

// TestApply_NotResident: a document no client holds is loaded from the
// Store, written, stored and evicted again (parked with DocIdleTimeout); a
// client connecting next has the write.
func TestApply_NotResident(t *testing.T) {
	for _, idle := range []time.Duration{0, time.Hour} {
		t.Run(fmt.Sprint(idle), func(t *testing.T) {
			store := memstore.New()
			if err := store.StoreUpdate(context.Background(), "doc", hpUpdate(t, "stored")); err != nil {
				t.Fatal(err)
			}
			url, s := startTestServer(t, server.Options{Store: store, DocIdleTimeout: idle})
			if _, err := s.Apply(context.Background(), "doc", pushServer("written")); err != nil {
				t.Fatal(err)
			}
			st := s.Stats()
			if idle == 0 && st.Documents != 0 || idle > 0 && st.IdleDocuments != 1 {
				t.Fatalf("stats %+v", st)
			}
			if got := storeItems(t, store, "doc"); !containsAll(got, "stored", "written") {
				t.Fatalf("stored %v", got)
			}
			c := dialClient(t, url, "doc", 13)
			defer c.close()
			c.write(t, syncpkg.EncodeSyncStep1([]byte{0}))
			f := c.readUntil(t, func(f *syncpkg.Frame) bool {
				return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncStep2
			})
			if got := stateItems(t, f.Payload); !containsAll(got, "stored", "written") {
				t.Fatalf("client got %v", got)
			}
		})
	}
}

// TestApply_WithoutStoreRetains: without a Store a written document stays
// resident after the write, so a later reader has it; CloseDocument drops it.
func TestApply_WithoutStoreRetains(t *testing.T) {
	_, s := startTestServer(t, server.Options{})
	if _, err := s.Apply(context.Background(), "doc", pushServer("kept")); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.Documents != 1 {
		t.Fatalf("stats %+v", st)
	}
	d, err := s.OpenDocument(context.Background(), "doc")
	if err != nil {
		t.Fatal(err)
	}
	state, _ := d.State()
	d.Close()
	if got := stateItems(t, state); fmt.Sprint(got) != "[kept]" {
		t.Fatalf("state %v", got)
	}
	if err := s.CloseDocument(context.Background(), "doc", false); err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st.Documents != 0 {
		t.Fatalf("after CloseDocument: %+v", st)
	}
	// A document only read (no write) is not retained.
	d, _ = s.OpenDocument(context.Background(), "read-only")
	d.Close()
	if st := s.Stats(); st.Documents != 0 {
		t.Fatalf("a read document stayed: %+v", st)
	}
}

// TestApply_ErrorPanicAndNoChange: edits made before fn fails or panics are
// committed and sent; a write that changes nothing sends nothing.
func TestApply_ErrorPanicAndNoChange(t *testing.T) {
	store := memstore.New()
	calls := 0
	url, s := startTestServer(t, server.Options{Store: store, OnChange: func(string, []byte) { calls++ }})
	c := dialClient(t, url, "doc", 14)
	defer c.close()
	syncClient(t, c)

	boom := errors.New("boom")
	update, err := s.Apply(context.Background(), "doc", func(txn *ygo.TransactionMut) error {
		ygo.ArrayIn(txn, "items").Push(txn, "before-error")
		return boom
	})
	if !errors.Is(err, boom) || update == nil {
		t.Fatalf("Apply: %x, %v", update, err)
	}
	awaitContains(t, c, "before-error")

	func() {
		defer func() {
			if r := recover(); r != "panic in fn" {
				t.Fatalf("recovered %v", r)
			}
		}()
		_, _ = s.Apply(context.Background(), "doc", func(txn *ygo.TransactionMut) error {
			ygo.ArrayIn(txn, "items").Push(txn, "before-panic")
			panic("panic in fn")
		})
	}()
	awaitContains(t, c, "before-panic")

	update, err = s.Apply(context.Background(), "doc", func(*ygo.TransactionMut) error { return nil })
	if update != nil || err != nil {
		t.Fatalf("no change: %x, %v", update, err)
	}
	if calls != 2 {
		t.Fatalf("OnChange called %d times, want 2", calls)
	}
	if got := storeItems(t, store, "doc"); !containsAll(got, "before-error", "before-panic") || len(got) != 2 {
		t.Fatalf("stored %v", got)
	}
}

// TestApplyUpdate: a valid update is applied to the server's copy (a later
// client syncs it) and sent as given; undecodable and trailing bytes are
// refused with nothing applied.
func TestApplyUpdate(t *testing.T) {
	store := memstore.New()
	url, s := startTestServer(t, server.Options{Store: store})
	c := dialClient(t, url, "doc", 15)
	defer c.close()
	syncClient(t, c)

	u := hpUpdate(t, "injected")
	if err := s.ApplyUpdate(context.Background(), "doc", u); err != nil {
		t.Fatal(err)
	}
	f := c.readUntil(t, func(f *syncpkg.Frame) bool { return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncUpdate })
	if !bytes.Equal(f.Payload, u) {
		t.Fatalf("sent %x, want %x", f.Payload, u)
	}
	for _, bad := range [][]byte{{0xff, 0xff}, append(append([]byte(nil), u...), 0)} {
		if err := s.ApplyUpdate(context.Background(), "doc", bad); !errors.Is(err, server.ErrInvalidUpdate) {
			t.Fatalf("ApplyUpdate(%x): %v", bad, err)
		}
	}
	d, _ := s.OpenDocument(context.Background(), "doc")
	state, _ := d.State()
	d.Close()
	if got := stateItems(t, state); fmt.Sprint(got) != "[injected]" {
		t.Fatalf("server state %v", got)
	}
}

// TestOnServerWrite: the hook sees each write before it happens and can
// refuse it, leaving the document unchanged.
func TestOnServerWrite(t *testing.T) {
	var seen []server.ServerWrite
	refuse := errors.New("not now")
	_, s := startTestServer(t, server.Options{OnServerWrite: func(_ context.Context, w server.ServerWrite) error {
		seen = append(seen, w)
		if w.Op == "applyUpdate" {
			return refuse
		}
		return nil
	}})
	if _, err := s.Apply(context.Background(), "doc", pushServer("ok")); err != nil {
		t.Fatal(err)
	}
	u := hpUpdate(t, "refused")
	if err := s.ApplyUpdate(context.Background(), "doc", u); !errors.Is(err, server.ErrWriteRefused) || !errors.Is(err, refuse) {
		t.Fatalf("ApplyUpdate: %v", err)
	}
	if len(seen) != 2 || seen[0] != (server.ServerWrite{DocName: "doc", Op: "apply"}) || seen[1] != (server.ServerWrite{DocName: "doc", Op: "applyUpdate", Size: len(u)}) {
		t.Fatalf("seen %+v", seen)
	}
	d, _ := s.OpenDocument(context.Background(), "doc")
	state, _ := d.State()
	d.Close()
	if got := stateItems(t, state); fmt.Sprint(got) != "[ok]" {
		t.Fatalf("state %v", got)
	}
}

// TestCloseDocument: refusals, then a forced close that ends a y-websocket
// connection with 1001 and closes the Hocuspocus document while the
// session's other document stays; the next connection loads the stored
// state afresh.
func TestCloseDocument(t *testing.T) {
	store := memstore.New()
	url, s := startTestServer(t, server.Options{Store: store})
	if err := s.CloseDocument(context.Background(), "doc", false); !errors.Is(err, server.ErrDocumentNotFound) {
		t.Fatalf("not resident: %v", err)
	}
	y := dialClient(t, url, "doc", 16)
	defer y.close()
	syncClient(t, y)
	hp := hpDial(t, url)
	hp.auth(t, "doc", "", syncpkg.ScopeReadWrite)
	hpSync(t, hp, "doc")
	hp.auth(t, "other", "", syncpkg.ScopeReadWrite)
	hpSync(t, hp, "other")
	if _, err := s.Apply(context.Background(), "doc", pushServer("before-close")); err != nil {
		t.Fatal(err)
	}
	awaitContains(t, y, "before-close")
	hp.want(t, "doc", syncpkg.MessageSync)
	if err := s.CloseDocument(context.Background(), "doc", false); !errors.Is(err, server.ErrDocumentHasConnections) {
		t.Fatalf("with connections: %v", err)
	}

	// The y-websocket client reads on, so it answers the server's close.
	closed := make(chan websocket.StatusCode, 1)
	go func() {
		for {
			if _, _, err := y.conn.Read(y.ctx); err != nil {
				closed <- websocket.CloseStatus(err)
				return
			}
		}
	}()
	if err := s.CloseDocument(context.Background(), "doc", true); err != nil {
		t.Fatal(err)
	}
	if code := <-closed; code != websocket.StatusGoingAway {
		t.Fatalf("y-websocket closed with %v", code)
	}
	if f := hp.want(t, "doc", syncpkg.MessageClose); !strings.Contains(string(f.Payload), "closed") {
		t.Fatalf("Hocuspocus close: %q", f.Payload)
	}
	if st := s.Stats(); st.Documents != 1 || len(st.Docs) != 1 || st.Docs[0].Name != "other" {
		t.Fatalf("stats %+v", st)
	}
	// The session's other document still works.
	hp.send(t, "other", syncpkg.EncodeSyncUpdate(hpUpdate(t, "still-open")))
	hp.want(t, "other", syncpkg.MessageSyncStatus)
	// A new connection loads the stored state.
	y2 := dialClient(t, url, "doc", 17)
	defer y2.close()
	y2.write(t, syncpkg.EncodeSyncStep1([]byte{0}))
	f := y2.readUntil(t, func(f *syncpkg.Frame) bool { return f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncStep2 })
	if got := stateItems(t, f.Payload); fmt.Sprint(got) != "[before-close]" {
		t.Fatalf("reloaded %v", got)
	}
}

// TestDocument_HeldAcrossDisconnect: an open Document keeps its document
// resident when its last client leaves; closing it then evicts it.
func TestDocument_HeldAcrossDisconnect(t *testing.T) {
	store := memstore.New()
	url, s := startTestServer(t, server.Options{Store: store})
	d, err := s.OpenDocument(context.Background(), "doc")
	if err != nil {
		t.Fatal(err)
	}
	c := dialClient(t, url, "doc", 18)
	syncClient(t, c)
	c.close()
	time.Sleep(100 * time.Millisecond)
	if st := s.Stats(); st.Documents != 1 {
		t.Fatalf("held document gone: %+v", st)
	}
	if _, err := d.Apply(context.Background(), pushServer("held")); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if st := s.Stats(); st.Documents != 0 {
		t.Fatalf("after Close: %+v", st)
	}
	if _, err := d.Apply(context.Background(), pushServer("late")); !errors.Is(err, server.ErrDocumentClosed) {
		t.Fatalf("after Close: %v", err)
	}
	if got := storeItems(t, store, "doc"); fmt.Sprint(got) != "[held]" {
		t.Fatalf("stored %v", got)
	}
}

// TestServerClose_WaitsForDocuments: Close waits for open Documents (up to
// its ctx), and the document API is refused after it.
func TestServerClose_WaitsForDocuments(t *testing.T) {
	s := server.New(server.Options{})
	d, err := s.OpenDocument(context.Background(), "doc")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.Close(ctx); err == nil || !strings.Contains(err.Error(), "open documents") {
		t.Fatalf("Close with an open Document: %v", err)
	}
	d.Close()
	if _, err := s.OpenDocument(context.Background(), "doc"); !errors.Is(err, server.ErrServerClosed) {
		t.Fatalf("after Close: %v", err)
	}
	if _, err := s.Apply(context.Background(), "doc", pushServer("x")); !errors.Is(err, server.ErrServerClosed) {
		t.Fatalf("Apply after Close: %v", err)
	}
}

// TestApply_ConcurrentWithClients: server writes racing client edits,
// connects and disconnects; each Apply returns only its own update, and
// every client ends with the server's state.
func TestApply_ConcurrentWithClients(t *testing.T) {
	store := memstore.New()
	url, s := startTestServer(t, server.Options{Store: store})
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				v := fmt.Sprintf("s%d-%d", w, i)
				u, err := s.Apply(context.Background(), "doc", pushServer(v))
				if err != nil {
					t.Error(err)
					return
				}
				if got := updateValues(t, u); fmt.Sprint(got) != "["+v+"]" {
					t.Errorf("Apply %s returned an update of %v", v, got)
				}
			}
		}(w)
	}
	for k := 0; k < 3; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				c := dialClient(t, url, "doc", uint64(100+k*10+i))
				syncClient(t, c)
				pushEdit(t, c, fmt.Sprintf("c%d-%d", k, i))
				c.close()
			}
		}(k)
	}
	wg.Wait()
	d, err := s.OpenDocument(context.Background(), "doc")
	if err != nil {
		t.Fatal(err)
	}
	state, _ := d.State()
	d.Close()
	got := stateItems(t, state)
	if len(got) != 4*20+3*5 {
		t.Fatalf("server holds %d items", len(got))
	}
	if stored := storeItems(t, store, "doc"); len(stored) != len(got) {
		t.Fatalf("stored %d items, server %d", len(stored), len(got))
	}
}

// TestServerClose_DuringApply: Close waits for a write in progress to
// finish its effects (stored) before it returns.
func TestServerClose_DuringApply(t *testing.T) {
	store := memstore.New()
	s := server.New(server.Options{Store: store})
	inFn := make(chan struct{})
	release := make(chan struct{})
	applied := make(chan error, 1)
	go func() {
		_, err := s.Apply(context.Background(), "doc", func(txn *ygo.TransactionMut) error {
			close(inFn)
			<-release
			ygo.ArrayIn(txn, "items").Push(txn, "in-flight")
			return nil
		})
		applied <- err
	}()
	<-inFn
	closed := make(chan error, 1)
	go func() { closed <- s.Close(context.Background()) }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned during a write: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if got := storeItems(t, store, "doc"); fmt.Sprint(got) != "[in-flight]" {
		t.Fatalf("stored %v", got)
	}
}

// TestApply_ReachesReadOnlyAndUnsyncedClients: a read-only viewer receives
// a server write, and a client that has not finished its sync yet
// converges with it.
func TestApply_ReachesReadOnlyAndUnsyncedClients(t *testing.T) {
	url, s := startTestServer(t, server.Options{ReadOnly: func(_ string, r *http.Request) bool {
		return r.URL.Query().Get("view") == "1"
	}})
	viewer := dialClient(t, url, "doc?view=1", 19)
	defer viewer.close()
	syncClient(t, viewer)
	fresh := dialClient(t, url, "doc", 20)
	defer fresh.close()
	if _, err := s.Apply(context.Background(), "doc", pushServer("for-everyone")); err != nil {
		t.Fatal(err)
	}
	awaitContains(t, viewer, "for-everyone")
	// The fresh client syncs only now.
	fresh.write(t, syncpkg.EncodeSyncStep1([]byte{0}))
	for {
		f := fresh.readUntil(t, func(f *syncpkg.Frame) bool {
			return f.Type == syncpkg.MessageSync && (f.SyncSub == syncpkg.SyncStep2 || f.SyncSub == syncpkg.SyncUpdate)
		})
		if err := encoding.ApplyUpdate(fresh.doc, f.Payload); err != nil {
			t.Fatal(err)
		}
		if got := types.NewArray(fresh.doc.Branch("items")).ToSlice(); containsAll(got, "for-everyone") {
			break
		}
	}
}

// TestApplyUpdate_PendingOnly: an update whose dependency the server does
// not have is held in the server's pending queue, stored and sent as given;
// once the dependency arrives it integrates.
func TestApplyUpdate_PendingOnly(t *testing.T) {
	store := memstore.New()
	_, s := startTestServer(t, server.Options{Store: store})
	src := doc.NewDocWithOptions(doc.Options{ClientID: 50})
	arr := types.NewArray(src.Branch("items"))
	txn := src.WriteTxn()
	arr.Push(txn, "first")
	txn.Commit()
	sv := ygo.EncodeStateVector(src)
	txn = src.WriteTxn()
	arr.Push(txn, "second")
	txn.Commit()
	first := encoding.EncodeStateAsUpdate(src)
	second, err := ygo.EncodeDiff(src, sv)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyUpdate(context.Background(), "doc", second); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyUpdate(context.Background(), "doc", first); err != nil {
		t.Fatal(err)
	}
	d, _ := s.OpenDocument(context.Background(), "doc")
	state, _ := d.State()
	d.Close()
	if got := stateItems(t, state); fmt.Sprint(got) != "[first second]" {
		t.Fatalf("server holds %v", got)
	}
	// Each call evicted the document after it and the final flush compacted
	// the log; the waiting half survived that.
	if got := storeItems(t, store, "doc"); fmt.Sprint(got) != "[first second]" {
		t.Fatalf("stored %v", got)
	}
}

// TestApply_OnChangePanicStillPublishes: the Backplane publish comes
// before OnChange, so a panicking OnChange does not keep a write from other
// instances; the panic reaches the caller, and the document is not left
// held.
func TestApply_OnChangePanicStillPublishes(t *testing.T) {
	hub := backplane.NewMemory()
	t.Cleanup(func() { _ = hub.Close() })
	_, a := startTestServer(t, server.Options{Backplane: hub.Conn(), OnChange: func(string, []byte) { panic("OnChange") }})
	urlB, _ := startTestServer(t, server.Options{Backplane: hub.Conn()})
	other := dialClient(t, urlB, "doc", 21)
	defer other.close()
	syncClient(t, other)
	func() {
		defer func() {
			if r := recover(); r != "OnChange" {
				t.Fatalf("recovered %v", r)
			}
		}()
		_, _ = a.Apply(context.Background(), "doc", pushServer("published"))
	}()
	awaitContains(t, other, "published")
	if err := a.CloseDocument(context.Background(), "doc", false); err != nil {
		t.Fatalf("the document stayed held: %v", err)
	}
}

// TestCloseDocument_HocuspocusOnDisconnectPanic: OnDisconnect for a
// Hocuspocus document closed by CloseDocument runs on its session (whose
// panic net/http recovers), not on the closer, so a panicking hook does not
// take the process down; the server goes on serving.
func TestCloseDocument_HocuspocusOnDisconnectPanic(t *testing.T) {
	url, s := startTestServer(t, server.Options{OnDisconnect: func(string, string) { panic("OnDisconnect") }})
	hp := hpDial(t, url)
	hp.auth(t, "doc", "", syncpkg.ScopeReadWrite)
	hpSync(t, hp, "doc")
	if err := s.CloseDocument(context.Background(), "doc", true); err != nil {
		t.Fatal(err)
	}
	hp.want(t, "doc", syncpkg.MessageClose)
	// The session meets the closed key again: it drops it, OnDisconnect
	// panics there and net/http recovers.
	hp.send(t, "doc", syncpkg.EncodeSyncStep1([]byte{0}))
	other := dialClient(t, url, "other", 30)
	defer other.close()
	syncClient(t, other)
	if _, err := s.Apply(context.Background(), "other", pushServer("alive")); err != nil {
		t.Fatal(err)
	}
	awaitContains(t, other, "alive")
}

// TestOpenDocument_LoadPanic: a panicking OnLoadDocument reaches the caller
// and leaves the server usable.
func TestOpenDocument_LoadPanic(t *testing.T) {
	_, s := startTestServer(t, server.Options{OnLoadDocument: func(name string) ([]byte, error) {
		if name == "bad" {
			panic("load")
		}
		return nil, nil
	}})
	func() {
		defer func() {
			if r := recover(); r != "load" {
				t.Fatalf("recovered %v", r)
			}
		}()
		_, _ = s.OpenDocument(context.Background(), "bad")
	}()
	d, err := s.OpenDocument(context.Background(), "good")
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	_ = s.Stats()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestCloseDocument_SecondKeyWhileClosing: while CloseDocument waits for an
// open Document, the same Hocuspocus session attaches a second key to the
// document; its admission waits for the close without blocking the close,
// which releases the first key from outside the session.
func TestCloseDocument_SecondKeyWhileClosing(t *testing.T) {
	url, s := startTestServer(t, server.Options{})
	hp := hpDial(t, url)
	hp.auth(t, "doc", "", syncpkg.ScopeReadWrite)
	hpSync(t, hp, "doc")
	d, err := s.OpenDocument(context.Background(), "doc")
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- s.CloseDocument(context.Background(), "doc", true) }()
	hp.want(t, "doc", syncpkg.MessageClose)
	// The second key's admission now waits for the close.
	hp.send(t, "doc\x00two", syncpkg.EncodeAuthToken(""))
	select {
	case err := <-closed:
		t.Fatalf("CloseDocument returned with a Document open: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	d.Close()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CloseDocument deadlocked with the session's admission")
	}
	if f := hp.want(t, "doc\x00two", syncpkg.MessageAuth); f.AuthSub != syncpkg.AuthAuthenticated {
		t.Fatalf("second key: %+v", f)
	}
}

// TestDocument_CloseWaitsForCalls: Close waits for a call in progress, so
// the write lands on the document while it is still held.
func TestDocument_CloseWaitsForCalls(t *testing.T) {
	inHook := make(chan struct{})
	release := make(chan struct{})
	store := memstore.New()
	_, s := startTestServer(t, server.Options{Store: store, OnServerWrite: func(context.Context, server.ServerWrite) error {
		close(inHook)
		<-release
		return nil
	}})
	d, err := s.OpenDocument(context.Background(), "doc")
	if err != nil {
		t.Fatal(err)
	}
	applied := make(chan error, 1)
	go func() {
		_, err := d.Apply(context.Background(), pushServer("in-flight"))
		applied <- err
	}()
	<-inHook
	closed := make(chan struct{})
	go func() { d.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close returned during a call")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	<-closed
	if got := storeItems(t, store, "doc"); fmt.Sprint(got) != "[in-flight]" {
		t.Fatalf("stored %v", got)
	}
}

// TestServerClose_RefusesWaitersAndHandles: an OpenDocument waiting for a
// closing document returns ErrServerClosed when Close begins, so does a call
// on a Document opened before it; Close drains a retained document.
func TestServerClose_RefusesWaitersAndHandles(t *testing.T) {
	s := server.New(server.Options{})
	if _, err := s.Apply(context.Background(), "kept", pushServer("x")); err != nil {
		t.Fatal(err)
	}
	held, err := s.OpenDocument(context.Background(), "held")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.CloseDocument(context.Background(), "held", true) }()
	time.Sleep(50 * time.Millisecond) // CloseDocument now waits for held
	opened := make(chan error, 1)
	go func() {
		_, err := s.OpenDocument(context.Background(), "held")
		opened <- err
	}()
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = s.Close(ctx) // times out on held
	if err := <-opened; !errors.Is(err, server.ErrServerClosed) {
		t.Fatalf("waiting OpenDocument: %v", err)
	}
	if _, err := held.Apply(context.Background(), pushServer("late")); !errors.Is(err, server.ErrServerClosed) {
		t.Fatalf("Apply on a Document after Close: %v", err)
	}
	held.Close()
	// The pending CloseDocument finishes the held document's eviction now.
	deadline := time.Now().Add(2 * time.Second)
	for s.Stats().Documents != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("after Close: %+v", s.Stats())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCloseDocument_RacesConnections: forced closes while clients keep
// connecting, editing and leaving (run under the race detector): no hang,
// and each close returns.
func TestCloseDocument_RacesConnections(t *testing.T) {
	url, s := startTestServer(t, server.Options{Store: memstore.New()})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for k := 0; k < 3; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				c, _, err := websocket.Dial(ctx, url+"/doc", nil)
				if err == nil {
					_ = c.Write(ctx, websocket.MessageBinary, syncpkg.EncodeSyncStep1([]byte{0}))
					_, _, _ = c.Read(ctx)
					_ = c.Close(websocket.StatusNormalClosure, "")
				}
				cancel()
			}
		}(k)
	}
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := s.CloseDocument(ctx, "doc", true)
		cancel()
		if err != nil && !errors.Is(err, server.ErrDocumentNotFound) {
			t.Fatalf("close %d: %v", i, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(stop)
	wg.Wait()
}

// TestCloseDocument_HocuspocusReattach: after CloseDocument closed a
// Hocuspocus document, the provider attaching the key again gets a fresh
// admission on the reloaded document, which receives server writes.
func TestCloseDocument_HocuspocusReattach(t *testing.T) {
	store := memstore.New()
	url, s := startTestServer(t, server.Options{Store: store})
	hp := hpDial(t, url)
	hp.auth(t, "doc", "", syncpkg.ScopeReadWrite)
	hpSync(t, hp, "doc")
	if _, err := s.Apply(context.Background(), "doc", pushServer("before")); err != nil {
		t.Fatal(err)
	}
	hp.want(t, "doc", syncpkg.MessageSync)
	if err := s.CloseDocument(context.Background(), "doc", true); err != nil {
		t.Fatal(err)
	}
	hp.want(t, "doc", syncpkg.MessageClose)
	hp.auth(t, "doc", "", syncpkg.ScopeReadWrite)
	if got := hpSync(t, hp, "doc"); fmt.Sprint(got) != "[before]" {
		t.Fatalf("reattached sync %v", got)
	}
	if st := s.Stats(); st.Documents != 1 || st.Connections != 1 {
		t.Fatalf("stats %+v", st)
	}
	if _, err := s.Apply(context.Background(), "doc", pushServer("after")); err != nil {
		t.Fatal(err)
	}
	if f := hp.want(t, "doc", syncpkg.MessageSync); !containsAll(updateValues(t, f.Payload), "after") {
		t.Fatalf("reattached key got %+v", f)
	}
}
