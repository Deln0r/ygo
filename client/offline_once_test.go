package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Deln0r/ygo/client"
	"github.com/Deln0r/ygo/internal/awareness"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
)

// fakeYWS is a y-websocket server for one document, as yserve and the
// y-websocket server behave: SyncStep1 as the connection opens, SyncStep2
// for the client's SyncStep1, updates applied. It records the frames it
// receives and can refuse dials.
type fakeYWS struct {
	doc    *doc.Doc
	mu     sync.Mutex
	frames []*syncpkg.Frame
	reject atomic.Bool
	dials  atomic.Int32
	url    string
	// greeting, when set, is sent after the SyncStep1 at every connect.
	greeting []byte
}

func newFakeYWS(t *testing.T) *fakeYWS {
	t.Helper()
	f := &fakeYWS{doc: doc.NewDocWithOptions(doc.Options{ClientID: 8})}
	ts := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(ts.Close)
	f.url = "ws://" + strings.TrimPrefix(ts.URL, "http://")
	return f
}

func (f *fakeYWS) serve(w http.ResponseWriter, r *http.Request) {
	f.dials.Add(1)
	if f.reject.Load() {
		http.Error(w, "not yet", http.StatusServiceUnavailable)
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(-1)
	ctx := r.Context()
	rtxn := f.doc.ReadTxn()
	sv := encoding.EncodeStateVector(rtxn.Store().GetStateVector(), nil)
	rtxn.Close()
	_ = conn.Write(ctx, websocket.MessageBinary, syncpkg.EncodeSyncStep1(sv))
	if f.greeting != nil {
		_ = conn.Write(ctx, websocket.MessageBinary, f.greeting)
	}
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return
		}
		fr, _, err := syncpkg.DecodeEnvelope(raw)
		if err != nil {
			continue
		}
		f.mu.Lock()
		f.frames = append(f.frames, fr)
		f.mu.Unlock()
		if fr.Type != syncpkg.MessageSync {
			continue
		}
		switch fr.SyncSub {
		case syncpkg.SyncStep1:
			remote, _, _ := encoding.DecodeStateVector(fr.Payload)
			rtxn := f.doc.ReadTxn()
			diff := encoding.EncodeDiff(f.doc, rtxn, remote)
			rtxn.Close()
			_ = conn.Write(ctx, websocket.MessageBinary, syncpkg.EncodeSyncStep2(diff))
		case syncpkg.SyncStep2, syncpkg.SyncUpdate:
			_ = encoding.ApplyUpdate(f.doc, fr.Payload)
		}
	}
}

// counts returns how many SyncStep2 frames from the client carried
// something (not the empty 00 00), and how many Update frames it sent at
// all: an empty update is one the client must not send either.
func (f *fakeYWS) counts() (step2, updates int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, fr := range f.frames {
		if fr.Type != syncpkg.MessageSync {
			continue
		}
		switch fr.SyncSub {
		case syncpkg.SyncStep2:
			if !(len(fr.Payload) == 2 && fr.Payload[0] == 0 && fr.Payload[1] == 0) {
				step2++
			}
		case syncpkg.SyncUpdate:
			updates++
		}
	}
	return step2, updates
}

// Edits made while the y-websocket client is offline go up once, in the
// reply to the server's SyncStep1. Before, the flush the edit had queued
// sent them as an update right after the client's SyncStep1 as well, and
// the reply, computed against the server's state from before that update,
// carried them again. The edit is made while the client is trying to
// connect, so a flush is queued, as between two sessions.
func TestClient_OfflineEditsGoUpOnce(t *testing.T) {
	f := newFakeYWS(t)
	f.reject.Store(true)
	d := doc.NewDocWithOptions(doc.Options{ClientID: 97})
	m := types.NewMap(d.Branch("settings"))
	synced := make(chan struct{}, 4)
	c, err := client.New(client.Options{
		URL: f.url, DocName: "room", Doc: d,
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 40 * time.Millisecond,
		OnSynced: func(v bool) {
			if v {
				synced <- struct{}{}
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	waitFor(t, "a refused dial", func() bool { return f.dials.Load() > 0 })
	txn := d.WriteTxn()
	m.Set(txn, "k", "offline")
	txn.Commit()
	f.reject.Store(false)
	waitChan(t, "synced", synced)
	time.Sleep(200 * time.Millisecond)
	step2, updates := f.counts()
	if step2 != 1 || updates != 0 {
		t.Errorf("the offline edit went up in %d SyncStep2 and %d Update frames, want 1 and 0", step2, updates)
	}
	sm := types.NewMap(f.doc.Branch("settings"))
	rtxn := f.doc.ReadTxn()
	v := sm.Get("k")
	rtxn.Close()
	if v != "offline" {
		t.Errorf("server has %v", v)
	}

	// A live edit still goes out as an update.
	txn = d.WriteTxn()
	m.Set(txn, "k2", "live")
	txn.Commit()
	waitFor(t, "the live edit", func() bool { _, u := f.counts(); return u == 1 })
}

// A peer the client has not heard from for PeerTimeout is dropped, as
// y-protocols drops it, even when the server never sends its removal (a
// server that does not sweep, or a removal lost on the way). Before, it
// stayed until the connection closed.
func TestClient_SilentPeerDropped(t *testing.T) {
	f := newFakeYWS(t)
	peer := awareness.New(55)
	peer.SetLocalState([]byte(`{"user":"gone"}`))
	f.greeting = syncpkg.EncodeAwareness(peer.Encode(nil))
	c, err := client.New(client.Options{
		URL: f.url, DocName: "room", PeerTimeout: time.Second,
		Doc: doc.NewDocWithOptions(doc.Options{ClientID: 98}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	waitFor(t, "the peer's presence", func() bool { return c.Awareness().States()[55] != nil })
	start := time.Now()
	waitFor(t, "the silent peer dropped", func() bool { return c.Awareness().States()[55] == nil })
	if d := time.Since(start); d < 800*time.Millisecond {
		t.Errorf("dropped after %v, before the timeout", d)
	}
}
