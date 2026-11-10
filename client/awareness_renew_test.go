package client_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Deln0r/ygo/client"
	"github.com/Deln0r/ygo/internal/awareness"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/server"
)

// serveOn runs srv's handler on a tracking listener and returns its URL.
func serveOn(t *testing.T, srv *server.Server) (string, *trackingListener) {
	t.Helper()
	ln := mustListen(t)
	hs := &http.Server{Handler: srv.Handler()}
	go hs.Serve(ln)
	t.Cleanup(func() { _ = hs.Close() })
	return "ws://" + ln.Addr().String(), ln
}

func dialOpts(t *testing.T, opts client.Options, clientID uint64) *client.Client {
	t.Helper()
	opts.Doc = doc.NewDocWithOptions(doc.Options{ClientID: clientID})
	c, err := client.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// An idle client's presence outlives the server's awareness timeout: the
// client renews its state, as y-protocols does every 15 seconds. Before, it
// sent its state once, so the server swept it after the timeout and every
// peer saw it leave while it was still connected.
func TestClient_AwarenessRenewedPastServerTimeout(t *testing.T) {
	srv := server.New(server.Options{OriginPatterns: []string{"*"}, AwarenessTimeout: 2 * time.Second})
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	url, _ := serveOn(t, srv)

	a := dialOpts(t, client.Options{URL: url, DocName: "room", AwarenessTimeout: time.Second}, 61)
	b := dialOpts(t, client.Options{URL: url, DocName: "room"}, 62)
	waitFor(t, "a synced", a.Synced)
	waitFor(t, "b synced", b.Synced)
	a.SetAwarenessState([]byte(`{"name":"a"}`))
	waitFor(t, "b sees a", func() bool { return b.Awareness().States()[61] != nil })
	// Without renewal the sweep removes a within its timeout plus one sweep
	// tick, and a sends its state again on reading its own removal, so b
	// sees a leave and come back within milliseconds: b's events catch
	// that, a look at its states between them would not.
	var removed atomic.Bool
	b.Awareness().OnUpdate(func(s awareness.Summary, _ any) {
		for _, id := range s.Removed {
			if id == 61 {
				removed.Store(true)
			}
		}
	})

	// Three times the server's timeout.
	time.Sleep(6 * time.Second)
	if removed.Load() {
		t.Error("b saw a removed while a was connected")
	}
	if b.Awareness().States()[61] == nil {
		t.Error("b lost a's presence while a was connected")
	}
}

// A client that reconnects is visible again at once. The server keeps the
// clock of a disconnected client's removed state, so a state sent again at
// that clock was ignored, and the client stayed invisible until its next
// change.
func TestClient_AwarenessVisibleAfterReconnect(t *testing.T) {
	srv := server.New(server.Options{OriginPatterns: []string{"*"}})
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	urlA, lnA := serveOn(t, srv)
	urlB, _ := serveOn(t, srv)

	a := dialOpts(t, client.Options{URL: urlA, DocName: "room"}, 63)
	b := dialOpts(t, client.Options{URL: urlB, DocName: "room"}, 64)
	waitFor(t, "a synced", a.Synced)
	waitFor(t, "b synced", b.Synced)
	a.SetAwarenessState([]byte(`{"name":"a"}`))
	waitFor(t, "b sees a", func() bool { return b.Awareness().States()[63] != nil })

	lnA.dropAll()
	waitFor(t, "b sees a leave", func() bool { return b.Awareness().States()[63] == nil })
	waitFor(t, "a resynced", a.Synced)
	waitFor(t, "b sees a again", func() bool { return string(b.Awareness().States()[63]) == `{"name":"a"}` })
}

// reconnectedClient dials through its own listener and returns a channel
// that receives once per completed handshake after the first.
func reconnectedClient(t *testing.T, srv *server.Server, clientID uint64) (*client.Client, *trackingListener, chan struct{}) {
	t.Helper()
	url, ln := serveOn(t, srv)
	resynced := make(chan struct{}, 4)
	var handshakes atomic.Int32
	c := dialOpts(t, client.Options{URL: url, DocName: "room", OnSynced: func(v bool) {
		if v && handshakes.Add(1) > 1 {
			resynced <- struct{}{}
		}
	}}, clientID)
	waitFor(t, "synced", c.Synced)
	return c, ln, resynced
}

func waitChan(t *testing.T, what string, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
	}
}

// holdListener lets a test hold back the connections it accepts, so a
// reconnect waits at the server until the test releases it.
type holdListener struct {
	*trackingListener
	mu   sync.Mutex
	gate chan struct{}
}

func (l *holdListener) Accept() (net.Conn, error) {
	c, err := l.trackingListener.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	g := l.gate
	l.mu.Unlock()
	if g != nil {
		<-g
	}
	return c, nil
}

func (l *holdListener) hold() {
	l.mu.Lock()
	l.gate = make(chan struct{})
	l.mu.Unlock()
}

func (l *holdListener) release() {
	l.mu.Lock()
	if l.gate != nil {
		close(l.gate)
		l.gate = nil
	}
	l.mu.Unlock()
}

// A peer that left while this client was offline is gone once the client
// reconnects. The server answers the reconnect with the live states only,
// so a peer this client kept would stay forever; y-websocket drops every
// remote state when its connection closes, and so does the client now.
// b's reconnect is held until a third client has seen a leave, so the
// server has removed a before b asks for the states.
func TestClient_AwarenessForgetsPeersAcrossReconnect(t *testing.T) {
	srv := server.New(server.Options{OriginPatterns: []string{"*"}})
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	urlA, _ := serveOn(t, srv)
	lnB := &holdListener{trackingListener: mustListen(t)}
	hs := &http.Server{Handler: srv.Handler()}
	go hs.Serve(lnB)
	t.Cleanup(func() {
		lnB.release()
		_ = hs.Close()
	})

	a := dialOpts(t, client.Options{URL: urlA, DocName: "room"}, 65)
	o := dialOpts(t, client.Options{URL: urlA, DocName: "room"}, 71)
	resynced := make(chan struct{}, 4)
	var handshakes atomic.Int32
	b := dialOpts(t, client.Options{URL: "ws://" + lnB.Addr().String(), DocName: "room", OnSynced: func(v bool) {
		if v && handshakes.Add(1) > 1 {
			resynced <- struct{}{}
		}
	}}, 66)
	waitFor(t, "a synced", a.Synced)
	waitFor(t, "b synced", b.Synced)
	a.SetAwarenessState([]byte(`{"name":"a"}`))
	waitFor(t, "b sees a", func() bool { return b.Awareness().States()[65] != nil })
	waitFor(t, "o sees a", func() bool { return o.Awareness().States()[65] != nil })

	lnB.hold()
	lnB.dropAll()
	_ = a.Close()
	waitFor(t, "o sees a leave", func() bool { return o.Awareness().States()[65] == nil })
	lnB.release()
	waitChan(t, "b to reconnect", resynced)
	time.Sleep(200 * time.Millisecond)
	if b.Awareness().States()[65] != nil {
		t.Error("b still shows a, which left while b was offline")
	}
}

// A peer still online is visible again as soon as this client reconnects,
// though it sent nothing new: its state comes back at the clock this
// client saw before, which a kept tombstone would refuse until the peer's
// next renewal, up to 15 seconds later. This guards ForgetRemote forgetting
// the clocks; the code before it passes, since it removed nothing.
func TestClient_AwarenessKeepsOnlinePeersAcrossReconnect(t *testing.T) {
	srv := server.New(server.Options{OriginPatterns: []string{"*"}})
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	urlA, _ := serveOn(t, srv)
	a := dialOpts(t, client.Options{URL: urlA, DocName: "room"}, 67)
	b, lnB, resynced := reconnectedClient(t, srv, 68)
	waitFor(t, "a synced", a.Synced)
	a.SetAwarenessState([]byte(`{"name":"a"}`))
	waitFor(t, "b sees a", func() bool { return b.Awareness().States()[67] != nil })
	// As y-websocket orders it, b is no longer synced when it reports the
	// peers gone.
	var removedWhileSynced, removed atomic.Bool
	b.Awareness().OnChange(func(s awareness.Summary, _ any) {
		if len(s.Removed) > 0 {
			removed.Store(true)
			if b.Synced() {
				removedWhileSynced.Store(true)
			}
		}
	})

	lnB.dropAll()
	waitChan(t, "b to reconnect", resynced)
	waitFor(t, "b sees a after reconnecting", func() bool { return string(b.Awareness().States()[67]) == `{"name":"a"}` })
	if !removed.Load() || removedWhileSynced.Load() {
		t.Errorf("peers reported removed %v, while synced %v; want removed after synced turned false", removed.Load(), removedWhileSynced.Load())
	}
}

// halfOpenProxy forwards TCP connections to addr. cutClients closes the
// client side of every connection and leaves the server side open, as a
// network that drops without the server noticing; closeStale then closes
// those server sides, as the server notices at last.
type halfOpenProxy struct {
	ln                      net.Listener
	mu                      sync.Mutex
	clients, servers, stale []net.Conn
}

func newHalfOpenProxy(t *testing.T, addr string) *halfOpenProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &halfOpenProxy{ln: ln}
	go func() {
		for {
			cc, err := ln.Accept()
			if err != nil {
				return
			}
			sc, err := net.Dial("tcp", addr)
			if err != nil {
				_ = cc.Close()
				continue
			}
			p.mu.Lock()
			p.clients = append(p.clients, cc)
			p.servers = append(p.servers, sc)
			p.mu.Unlock()
			go func() { _, _ = io.Copy(sc, cc) }() // does not close sc when cc ends
			go func() { _, _ = io.Copy(cc, sc) }()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range append(append(p.clients, p.servers...), p.stale...) {
			_ = c.Close()
		}
	})
	return p
}

func (p *halfOpenProxy) url() string { return "ws://" + p.ln.Addr().String() }

func (p *halfOpenProxy) cutClients() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.clients {
		_ = c.Close()
	}
	p.stale = append(p.stale, p.servers...)
	p.clients, p.servers = nil, nil
}

func (p *halfOpenProxy) closeStale() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.stale {
		_ = c.Close()
	}
	p.stale = nil
}

// A client is visible again at once when the server learns of its old
// connection's end only after the client has reconnected. The server then
// removes the client's state at the clock the new connection set, and
// peers take the removal; the client bumps its clock against it and now
// sends its state again right away, as y-websocket does. Before, nothing
// went out until the next renewal, 15 seconds later.
func TestClient_AwarenessBackAfterStaleConnectionEnds(t *testing.T) {
	srv := server.New(server.Options{OriginPatterns: []string{"*"}})
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	url, ln := serveOn(t, srv)
	proxy := newHalfOpenProxy(t, ln.Addr().String())

	resynced := make(chan struct{}, 4)
	var handshakes atomic.Int32
	a := dialOpts(t, client.Options{URL: proxy.url(), DocName: "room", OnSynced: func(v bool) {
		if v && handshakes.Add(1) > 1 {
			resynced <- struct{}{}
		}
	}}, 69)
	b := dialOpts(t, client.Options{URL: url, DocName: "room"}, 70)
	waitFor(t, "a synced", a.Synced)
	waitFor(t, "b synced", b.Synced)
	var bLostA atomic.Bool
	b.Awareness().OnUpdate(func(s awareness.Summary, _ any) {
		for _, id := range s.Removed {
			if id == 69 {
				bLostA.Store(true)
			}
		}
	})
	a.SetAwarenessState([]byte(`{"name":"a"}`))
	waitFor(t, "b sees a", func() bool { return b.Awareness().States()[69] != nil })

	proxy.cutClients()
	waitChan(t, "a to reconnect", resynced)
	waitFor(t, "b sees a after the reconnect", func() bool { return b.Awareness().States()[69] != nil })
	if bLostA.Load() {
		t.Fatal("b lost a before the stale connection ended")
	}

	proxy.closeStale()
	waitFor(t, "the server to remove a with the stale connection", bLostA.Load)
	waitFor(t, "b sees a again", func() bool { return string(b.Awareness().States()[69]) == `{"name":"a"}` })
}

// An AwarenessTimeout under ten nanoseconds, a Duration given without its
// unit, made the renewal ticker's interval zero, and NewTicker panicked in
// the client's goroutine on the first connection.
func TestClient_TinyAwarenessTimeout(t *testing.T) {
	url := startServer(t)
	c := dialOpts(t, client.Options{URL: url, DocName: "room", AwarenessTimeout: 5}, 72)
	waitFor(t, "synced", c.Synced)
	c.SetAwarenessState([]byte(`{"n":1}`))
	time.Sleep(300 * time.Millisecond)
}

// A failed dial leaves the awareness alone, as y-websocket removes remote
// states only when an open connection closes: an Awareness the embedder
// also feeds from elsewhere keeps those states while the server is down.
func TestClient_FailedDialKeepsAwareness(t *testing.T) {
	ln := mustListen(t)
	url := "ws://" + ln.Addr().String()
	_ = ln.Close() // nothing listens: every dial fails

	aw := awareness.New(73)
	peer := awareness.New(74)
	peer.SetLocalState([]byte(`{"p":74}`))
	if _, err := aw.Apply(peer.Encode(nil), "elsewhere"); err != nil {
		t.Fatal(err)
	}
	failures := make(chan struct{}, 16)
	c, err := client.New(client.Options{
		URL: url, DocName: "room", Awareness: aw,
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
		OnError: func(error) {
			select {
			case failures <- struct{}{}:
			default:
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
	for i := 0; i < 3; i++ {
		waitChan(t, "a failed dial", failures)
	}
	if string(aw.States()[74]) != `{"p":74}` {
		t.Error("failed dials removed a state the client did not bring")
	}
}
