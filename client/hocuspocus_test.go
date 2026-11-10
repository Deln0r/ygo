package client_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Deln0r/ygo/client"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
)

// fakeHocus is a Hocuspocus server for one document, answering as
// @hocuspocus/server 4 does (HOCUSPOCUS_PROTOCOL.md): Authenticated or
// PermissionDenied for the token, SyncStep1 then SyncStep2 for a SyncStep1,
// SyncStatus for every SyncStep2 and Update, CLOSE "provider_initiated"
// for a CLOSE. It records every frame it receives, routing key included.
type fakeHocus struct {
	t      *testing.T
	doc    *doc.Doc
	mu     sync.Mutex
	frames [][]byte
	conns  []*websocket.Conn
	dials  atomic.Int32
	reject atomic.Bool // answer 503 instead of upgrading
	url    string
	// afterSync, when set, runs once a connection's handshake is done.
	afterSync func(conn *websocket.Conn)
	// holdStatus, when set, delays the SyncStatus for an Update until it
	// is closed.
	holdStatus chan struct{}
}

func newFakeHocus(t *testing.T) *fakeHocus {
	t.Helper()
	f := &fakeHocus{t: t, doc: doc.NewDocWithOptions(doc.Options{ClientID: 7})}
	ts := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(ts.Close)
	f.url = "ws://" + strings.TrimPrefix(ts.URL, "http://")
	return f
}

func (f *fakeHocus) send(ctx context.Context, conn *websocket.Conn, key string, env []byte) {
	_ = conn.Write(ctx, websocket.MessageBinary, append(syncpkg.EncodeRoute(key), env...))
}

func (f *fakeHocus) serve(w http.ResponseWriter, r *http.Request) {
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
	f.mu.Lock()
	f.conns = append(f.conns, conn)
	f.mu.Unlock()
	ctx := r.Context()
	readOnly := false
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.frames = append(f.frames, append([]byte(nil), raw...))
		f.mu.Unlock()
		key, rest, err := syncpkg.DecodeRoute(raw)
		if err != nil {
			_ = conn.Close(4401, "Unauthorized")
			return
		}
		fr, _, err := syncpkg.DecodeEnvelope(rest)
		if err != nil {
			continue
		}
		switch fr.Type {
		case syncpkg.MessageAuth:
			switch string(fr.Payload) {
			case "bad":
				f.send(ctx, conn, key, syncpkg.EncodeAuthPermissionDenied("bad-token"))
			case "ro":
				readOnly = true
				f.send(ctx, conn, key, syncpkg.EncodeAuthAuthenticated(syncpkg.ScopeReadOnly))
			default:
				f.send(ctx, conn, key, syncpkg.EncodeAuthAuthenticated(syncpkg.ScopeReadWrite))
			}
		case syncpkg.MessageSync:
			switch fr.SyncSub {
			case syncpkg.SyncStep1:
				rtxn := f.doc.ReadTxn()
				sv := encoding.EncodeStateVector(rtxn.Store().GetStateVector(), nil)
				rtxn.Close()
				remote, _, _ := encoding.DecodeStateVector(fr.Payload)
				rtxn = f.doc.ReadTxn()
				diff := encoding.EncodeDiff(f.doc, rtxn, remote)
				rtxn.Close()
				f.send(ctx, conn, key, syncpkg.EncodeSyncStep1(sv))
				f.send(ctx, conn, key, syncpkg.EncodeSyncStep2(diff))
			case syncpkg.SyncStep2, syncpkg.SyncUpdate:
				if readOnly {
					f.send(ctx, conn, key, syncpkg.EncodeSyncStatus(fr.SyncSub == syncpkg.SyncStep2 && bytes.Equal(fr.Payload, []byte{0, 0})))
					continue
				}
				_ = encoding.ApplyUpdate(f.doc, fr.Payload)
				if fr.SyncSub == syncpkg.SyncUpdate && f.holdStatus != nil {
					<-f.holdStatus
				}
				f.send(ctx, conn, key, syncpkg.EncodeSyncStatus(true))
				if fr.SyncSub == syncpkg.SyncStep2 && f.afterSync != nil {
					f.afterSync(conn)
				}
			}
		case syncpkg.MessageClose:
			f.send(ctx, conn, key, syncpkg.EncodeClose("provider_initiated"))
		}
	}
}

// received returns the frames received so far, as hex.
func (f *fakeHocus) received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.frames))
	for i, b := range f.frames {
		out[i] = hex.EncodeToString(b)
	}
	return out
}

// envelopes returns the received frames' envelopes for key, decoded.
func (f *fakeHocus) envelopes(key string) []*syncpkg.Frame {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*syncpkg.Frame
	for _, b := range f.frames {
		k, rest, err := syncpkg.DecodeRoute(b)
		if err != nil || k != key {
			continue
		}
		if fr, _, err := syncpkg.DecodeEnvelope(rest); err == nil {
			out = append(out, fr)
		}
	}
	return out
}

type hocusEvents struct {
	mu       sync.Mutex
	errs     []error
	synced   chan struct{}
	scopes   []string
	stateful []string
}

func hocusClient(t *testing.T, f *fakeHocus, mod func(*client.Options), d *doc.Doc) (*client.Client, *hocusEvents) {
	t.Helper()
	ev := &hocusEvents{synced: make(chan struct{}, 16)}
	if d == nil {
		d = doc.NewDocWithOptions(doc.Options{ClientID: 95})
	}
	opts := client.Options{
		URL: f.url + "/", DocName: "doc", Hocuspocus: true, Doc: d,
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 40 * time.Millisecond,
		OnSynced: func(v bool) {
			if v {
				ev.synced <- struct{}{}
			}
		},
		OnError: func(err error) {
			ev.mu.Lock()
			ev.errs = append(ev.errs, err)
			ev.mu.Unlock()
		},
		OnAuthenticated: func(s string) {
			ev.mu.Lock()
			ev.scopes = append(ev.scopes, s)
			ev.mu.Unlock()
		},
		OnStateless: func(p string) {
			ev.mu.Lock()
			ev.stateful = append(ev.stateful, p)
			ev.mu.Unlock()
		},
	}
	if mod != nil {
		mod(&opts)
	}
	c, err := client.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, ev
}

func (ev *hocusEvents) errors() []error {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	return append([]error(nil), ev.errs...)
}

const docKey = "03646f63" // varString("doc")

// The session opens with Auth (the token, no version string), then
// SyncStep1, every frame after the document name, and no QueryAwareness;
// the URL is dialed without the document name and the trailing slash.
func TestHocus_OpenFrames(t *testing.T) {
	f := newFakeHocus(t)
	c, ev := hocusClient(t, f, func(o *client.Options) { o.Token = "tok" }, nil)
	waitChan(t, "synced", ev.synced)
	got := f.received()
	if len(got) < 2 || got[0] != docKey+"0200"+"03746f6b" || got[1] != docKey+"00000100" {
		t.Fatalf("first frames %v, want Auth Token \"tok\" then SyncStep1, after the key", got)
	}
	for _, fr := range f.envelopes("doc") {
		if fr.Type == syncpkg.MessageQueryAwareness {
			t.Error("a QueryAwareness went out at connect")
		}
	}
	if c.Scope() != "read-write" {
		t.Errorf("scope %q", c.Scope())
	}
}

// Edits made while the client is offline go up in the reply to the
// server's SyncStep1 only, as the provider sends them, not also in an
// Update first; the empty update the queued flush then makes is not sent;
// the server confirms everything. The edit is made while the client is
// trying to connect, so a flush is queued, as between two sessions.
func TestHocus_OfflineEditsOnlyInTheReply(t *testing.T) {
	f := newFakeHocus(t)
	f.reject.Store(true)
	d := doc.NewDocWithOptions(doc.Options{ClientID: 96})
	m := types.NewMap(d.Branch("m"))
	c, ev := hocusClient(t, f, nil, d)
	waitFor(t, "a refused dial", func() bool { return f.dials.Load() > 0 })
	txn := d.WriteTxn()
	m.Set(txn, "k", "offline")
	txn.Commit()
	f.reject.Store(false)
	waitChan(t, "synced", ev.synced)
	waitFor(t, "Unsynced 0", func() bool { return c.Unsynced() == 0 })
	time.Sleep(200 * time.Millisecond)
	var step2, updates int
	for _, fr := range f.envelopes("doc") {
		if fr.Type == syncpkg.MessageSync && fr.SyncSub == syncpkg.SyncStep2 {
			step2++
		}
		if fr.Type == syncpkg.MessageSync && fr.SyncSub == syncpkg.SyncUpdate {
			updates++
		}
	}
	if step2 != 1 || updates != 0 {
		t.Errorf("%d SyncStep2 and %d Update frames, want 1 and 0", step2, updates)
	}
	sm := types.NewMap(f.doc.Branch("m")) // resolve the root outside the transaction
	rtxn := f.doc.ReadTxn()
	v := sm.Get("k")
	rtxn.Close()
	if v != "offline" {
		t.Errorf("server has %v", v)
	}
}

// Unsynced counts what the server has not confirmed; an edit raises it and
// its SyncStatus brings it back. Outside Hocuspocus mode it is 0.
func TestHocus_Unsynced(t *testing.T) {
	f := newFakeHocus(t)
	release := make(chan struct{})
	f.holdStatus = release
	c, ev := hocusClient(t, f, nil, nil)
	waitChan(t, "synced", ev.synced)
	waitFor(t, "Unsynced 0", func() bool { return c.Unsynced() == 0 })
	m := mapOf(c)
	txn := c.Doc().WriteTxn()
	m.Set(txn, "x", "1")
	txn.Commit()
	waitFor(t, "the update sent and unconfirmed", func() bool {
		for _, fr := range f.envelopes("doc") {
			if fr.Type == syncpkg.MessageSync && fr.SyncSub == syncpkg.SyncUpdate {
				return c.Unsynced() == 1
			}
		}
		return false
	})
	close(release)
	waitFor(t, "the update confirmed", func() bool { return c.Unsynced() == 0 })

	plain, err := client.New(client.Options{URL: "ws://127.0.0.1:1", DocName: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Unsynced() != 0 || plain.SendStateless("x") == nil {
		t.Error("outside Hocuspocus mode Unsynced must be 0 and SendStateless an error")
	}
}

// A refused token is one ErrPermissionDenied per attempt, with the
// server's reason and no read error after it, and the next dial waits
// deniedBackoff.
func TestHocus_DeniedOnceThenBackoff(t *testing.T) {
	restore := client.SetDeniedBackoff(400 * time.Millisecond)
	t.Cleanup(restore)
	f := newFakeHocus(t)
	_, ev := hocusClient(t, f, func(o *client.Options) { o.Token = "bad" }, nil)
	time.Sleep(1100 * time.Millisecond)
	errs := ev.errors()
	if len(errs) == 0 {
		t.Fatal("no error for a refused token")
	}
	for _, err := range errs {
		if !errors.Is(err, client.ErrPermissionDenied) || !strings.Contains(err.Error(), "bad-token") {
			t.Errorf("error %v, want ErrPermissionDenied: bad-token", err)
		}
	}
	if n := f.dials.Load(); n > 4 {
		t.Errorf("%d dials in 1.1 s with a 400 ms denial backoff", n)
	}
}

// A read-only connection's edit is ErrUpdateRefused, and the session goes
// on: no reconnect, Unsynced stays above 0.
func TestHocus_RefusedUpdate(t *testing.T) {
	f := newFakeHocus(t)
	c, ev := hocusClient(t, f, func(o *client.Options) { o.Token = "ro" }, nil)
	waitChan(t, "synced", ev.synced)
	if c.Scope() != "readonly" {
		t.Errorf("scope %q", c.Scope())
	}
	m := mapOf(c)
	txn := c.Doc().WriteTxn()
	m.Set(txn, "x", "1")
	txn.Commit()
	waitFor(t, "the refusal", func() bool { return len(ev.errors()) > 0 })
	if err := ev.errors()[0]; !errors.Is(err, client.ErrUpdateRefused) {
		t.Errorf("error %v, want ErrUpdateRefused", err)
	}
	time.Sleep(200 * time.Millisecond)
	if f.dials.Load() != 1 || c.Unsynced() == 0 {
		t.Errorf("dials %d, Unsynced %d; want 1 and above 0", f.dials.Load(), c.Unsynced())
	}
}

// A CLOSE from the server is one ErrDocumentClosed with its reason, and
// the client syncs again on a new connection.
func TestHocus_ServerCloseReconnects(t *testing.T) {
	f := newFakeHocus(t)
	var once atomic.Bool
	f.afterSync = func(conn *websocket.Conn) {
		if once.CompareAndSwap(false, true) {
			f.send(context.Background(), conn, "doc", syncpkg.EncodeClose("Reset Connection"))
		}
	}
	_, ev := hocusClient(t, f, nil, nil)
	waitChan(t, "synced", ev.synced)
	waitChan(t, "synced again", ev.synced)
	errs := ev.errors()
	if len(errs) != 1 || !errors.Is(errs[0], client.ErrDocumentClosed) || !strings.Contains(errs[0].Error(), "Reset Connection") {
		t.Errorf("errors %v, want one ErrDocumentClosed: Reset Connection", errs)
	}
}

// Close tells the server the client is done with the document (the bare
// CLOSE a provider sends), and the server's answer raises no error, also
// when it arrives before Close has cancelled the session (widened here).
func TestHocus_CloseSendsClose(t *testing.T) {
	t.Cleanup(client.SetAfterSendClose(func() { time.Sleep(300 * time.Millisecond) }))
	f := newFakeHocus(t)
	c, ev := hocusClient(t, f, nil, nil)
	waitChan(t, "synced", ev.synced)
	_ = c.Close()
	waitFor(t, "the CLOSE", func() bool {
		for _, h := range f.received() {
			if h == docKey+"07" {
				return true
			}
		}
		return false
	})
	if errs := ev.errors(); len(errs) != 0 {
		t.Errorf("errors after Close: %v", errs)
	}
}

// A server's token request gets the token again, from TokenFunc.
func TestHocus_TokenRequest(t *testing.T) {
	f := newFakeHocus(t)
	var calls atomic.Int32
	f.afterSync = func(conn *websocket.Conn) {
		f.send(context.Background(), conn, "doc", []byte{0x02, 0x00}) // Auth Token, no fields
	}
	_, ev := hocusClient(t, f, func(o *client.Options) {
		o.TokenFunc = func(context.Context) (string, error) {
			calls.Add(1)
			return "fresh", nil
		}
	}, nil)
	waitChan(t, "synced", ev.synced)
	waitFor(t, "the token sent again", func() bool { return calls.Load() >= 2 })
	waitFor(t, "two Auth frames", func() bool {
		n := 0
		for _, fr := range f.envelopes("doc") {
			if fr.Type == syncpkg.MessageAuth && string(fr.Payload) == "fresh" {
				n++
			}
		}
		return n >= 2
	})
}

// Frames for another document are dropped; a Ping is not answered (a
// Pong makes every server close the socket); stateless goes both ways.
func TestHocus_OtherKeyPingStateless(t *testing.T) {
	f := newFakeHocus(t)
	other := doc.NewDocWithOptions(doc.Options{ClientID: 9})
	om := types.NewMap(other.Branch("settings")) // the map getKey reads
	txn := other.WriteTxn()
	om.Set(txn, "leak", "yes")
	txn.Commit()
	f.afterSync = func(conn *websocket.Conn) {
		ctx := context.Background()
		f.send(ctx, conn, "other", syncpkg.EncodeSyncUpdate(encoding.EncodeStateAsUpdate(other)))
		f.send(ctx, conn, "doc", []byte{0x09})
		f.send(ctx, conn, "doc", syncpkg.EncodeStateless("hello"))
	}
	c, ev := hocusClient(t, f, nil, nil)
	waitChan(t, "synced", ev.synced)
	waitFor(t, "the stateless message", func() bool {
		ev.mu.Lock()
		defer ev.mu.Unlock()
		return len(ev.stateful) == 1 && ev.stateful[0] == "hello"
	})
	if err := c.SendStateless("up"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the client's stateless", func() bool {
		for _, fr := range f.envelopes("doc") {
			if fr.Type == syncpkg.MessageStateless && string(fr.Payload) == "up" {
				return true
			}
		}
		return false
	})
	if getKey(c, "leak") != nil {
		t.Error("an update for another document was applied")
	}
	for _, fr := range f.envelopes("doc") {
		if fr.Type == syncpkg.MessagePong {
			t.Error("the client answered a Ping")
		}
	}
}

// With nothing to send, the client sends a QueryAwareness every half
// awareness timeout, which keeps Hocuspocus 4's idle timeout off.
func TestHocus_Keepalive(t *testing.T) {
	f := newFakeHocus(t)
	_, ev := hocusClient(t, f, func(o *client.Options) { o.AwarenessTimeout = 400 * time.Millisecond }, nil)
	waitChan(t, "synced", ev.synced)
	time.Sleep(1200 * time.Millisecond)
	n := 0
	for _, fr := range f.envelopes("doc") {
		if fr.Type == syncpkg.MessageQueryAwareness {
			n++
		}
	}
	if n < 2 {
		t.Errorf("%d keepalives in 1.2 s at a 200 ms interval", n)
	}
}

// A token refresh the server asks for does not hold the read loop: here
// TokenFunc waits on its context the second time, and the server closes
// the socket meanwhile. The client must see the close and dial again;
// the refresh's context ends with the connection.
func TestHocus_TokenRefreshDoesNotBlockReads(t *testing.T) {
	f := newFakeHocus(t)
	var calls atomic.Int32
	var refreshEnded atomic.Bool
	refreshing := make(chan struct{})
	f.afterSync = func(conn *websocket.Conn) {
		if f.dials.Load() == 1 {
			f.send(context.Background(), conn, "doc", []byte{0x02, 0x00})
			// Close only once the refresh is running, so the second
			// TokenFunc call is the refresh, not the redial's.
			select {
			case <-refreshing:
			case <-time.After(5 * time.Second):
			}
			_ = conn.CloseNow()
		}
	}
	_, ev := hocusClient(t, f, func(o *client.Options) {
		o.TokenFunc = func(ctx context.Context) (string, error) {
			if calls.Add(1) == 2 {
				close(refreshing)
				<-ctx.Done()
				refreshEnded.Store(true)
				return "", ctx.Err()
			}
			return "ok", nil
		}
	}, nil)
	waitChan(t, "synced", ev.synced)
	waitFor(t, "a second dial after the server closed", func() bool { return f.dials.Load() >= 2 })
	waitFor(t, "the refresh's context ended", refreshEnded.Load)
}

// A refreshed token whose session ended before TokenFunc returned is
// dropped, not sent on the next session's connection, where the server
// would take it as a refresh (and an onTokenSync hook could close that
// session over it). Here TokenFunc answers the request only after the
// server has closed the first connection and the client is connected
// again.
func TestHocus_StaleRefreshDropped(t *testing.T) {
	f := newFakeHocus(t)
	var calls atomic.Int32
	refreshing := make(chan struct{})
	resynced := make(chan struct{})
	f.afterSync = func(conn *websocket.Conn) {
		if f.dials.Load() == 1 {
			f.send(context.Background(), conn, "doc", []byte{0x02, 0x00})
			<-refreshing
			_ = conn.CloseNow()
		}
	}
	_, ev := hocusClient(t, f, func(o *client.Options) {
		o.TokenFunc = func(ctx context.Context) (string, error) {
			if calls.Add(1) == 2 {
				close(refreshing)
				<-resynced // answer late, ignoring ctx
				return "stale", nil
			}
			return "ok", nil
		}
	}, nil)
	waitChan(t, "synced", ev.synced)
	waitChan(t, "synced again", ev.synced)
	close(resynced)
	time.Sleep(200 * time.Millisecond)
	for _, fr := range f.envelopes("doc") {
		if fr.Type == syncpkg.MessageAuth && string(fr.Payload) == "stale" {
			t.Fatal("a refresh for an ended session went out on the next one")
		}
	}
}

// A server that cannot be reached costs no TokenFunc call per dial: the
// token is asked for once a connection is up.
func TestHocus_NoTokenCallPerFailedDial(t *testing.T) {
	f := newFakeHocus(t)
	f.reject.Store(true)
	var calls atomic.Int32
	hocusClient(t, f, func(o *client.Options) {
		o.TokenFunc = func(context.Context) (string, error) {
			calls.Add(1)
			return "ok", nil
		}
	}, nil)
	waitFor(t, "five refused dials", func() bool { return f.dials.Load() >= 5 })
	if n := calls.Load(); n != 0 {
		t.Errorf("%d TokenFunc calls for refused dials, want 0", n)
	}
}

// A TokenFunc that fails (a revoked refresh token) costs the server no
// upgrade at the reconnect rate: the next dial waits deniedBackoff, as
// after a denial.
func TestHocus_TokenFuncErrorBackoff(t *testing.T) {
	t.Cleanup(client.SetDeniedBackoff(400 * time.Millisecond))
	f := newFakeHocus(t)
	_, ev := hocusClient(t, f, func(o *client.Options) {
		o.TokenFunc = func(context.Context) (string, error) { return "", errors.New("signed out") }
	}, nil)
	time.Sleep(1100 * time.Millisecond)
	if n := f.dials.Load(); n > 4 {
		t.Errorf("%d dials in 1.1 s with a 400 ms wait after a TokenFunc error", n)
	}
	errs := ev.errors()
	if len(errs) == 0 || !strings.Contains(errs[0].Error(), "signed out") {
		t.Errorf("errors %v, want the TokenFunc error", errs)
	}
}
