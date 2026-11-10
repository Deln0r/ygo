package client_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Deln0r/ygo/client"
	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/server"
)

// freezeProxy forwards TCP connections to addr, optionally at a limited
// rate per direction. freeze makes every connection open at that moment
// a black hole: bytes either way are read and dropped and nothing is
// closed, as when a network path dies without a FIN or RST. With
// freezeNew set, every later connection is a black hole from the start.
type freezeProxy struct {
	ln        net.Listener
	upRate    int // client to server, bytes per second; 0: unlimited
	downRate  int // server to client
	upLimit   int // client to server, bytes per connection before it stops reading; 0: none
	stop      chan struct{}
	freezeNew atomic.Bool
	mu        sync.Mutex
	pairs     []*atomic.Bool
	conns     []net.Conn
}

func newFreezeProxy(t *testing.T, addr string, upRate, downRate, upLimit int) *freezeProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &freezeProxy{ln: ln, upRate: upRate, downRate: downRate, upLimit: upLimit, stop: make(chan struct{})}
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
			f := &atomic.Bool{}
			f.Store(p.freezeNew.Load())
			p.mu.Lock()
			p.pairs = append(p.pairs, f)
			p.conns = append(p.conns, cc, sc)
			p.mu.Unlock()
			go pump(sc, cc, f, p.upRate, p.upLimit, p.stop)
			go pump(cc, sc, f, p.downRate, 0, p.stop)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		close(p.stop)
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, c := range p.conns {
			_ = c.Close()
		}
	})
	return p
}

// pump copies src to dst until src ends, at most rate bytes per second
// when rate > 0, dropping what it reads once frozen is set. After limit
// bytes (when limit > 0) it stops reading until stop closes, so the
// sender's writes block. It never closes dst.
func pump(dst, src net.Conn, frozen *atomic.Bool, rate, limit int, stop chan struct{}) {
	size := 32 << 10
	var gap time.Duration
	if rate > 0 {
		size = 16 << 10
		gap = time.Duration(int64(size) * int64(time.Second) / int64(rate))
	}
	buf := make([]byte, size)
	forwarded := 0
	for {
		if limit > 0 && forwarded >= limit {
			<-stop
			return
		}
		n, err := src.Read(buf)
		forwarded += n
		if n > 0 && !frozen.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				_, _ = io.Copy(io.Discard, src)
				return
			}
			if gap > 0 {
				time.Sleep(gap * time.Duration(n) / time.Duration(size))
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *freezeProxy) url() string { return "ws://" + p.ln.Addr().String() }

func (p *freezeProxy) freeze() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, f := range p.pairs {
		f.Store(true)
	}
	p.pairs = nil
}

type syncEvents struct {
	up, down chan struct{}
	errs     chan error
}

func newSyncEvents() *syncEvents {
	return &syncEvents{up: make(chan struct{}, 16), down: make(chan struct{}, 16), errs: make(chan error, 16)}
}

func (ev *syncEvents) options(url string, timeout time.Duration, clientID uint64) client.Options {
	return client.Options{
		URL: url, DocName: "room", LivenessTimeout: timeout,
		Doc: doc.NewDocWithOptions(doc.Options{ClientID: clientID}),
		OnSynced: func(v bool) {
			ch := ev.down
			if v {
				ch = ev.up
			}
			select {
			case ch <- struct{}{}:
			default:
			}
		},
		OnError: func(err error) {
			select {
			case ev.errs <- err:
			default:
			}
		},
	}
}

func dialThrough(t *testing.T, url string, timeout time.Duration, clientID uint64) (*client.Client, *syncEvents) {
	t.Helper()
	ev := newSyncEvents()
	c, err := client.New(ev.options(url, timeout, clientID))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	waitChan(t, "the first handshake", ev.up)
	return c, ev
}

// liveServer starts a server, generous enough for the large transfers
// below, and a proxy in front of it; it returns the proxy and the
// server's own URL.
func liveServer(t *testing.T, upRate, downRate int) (*freezeProxy, string) {
	t.Helper()
	srv := server.New(server.Options{
		OriginPatterns: []string{"*"}, ReadLimit: 64 << 20,
		MaxSendQueueBytes: 64 << 20, WriteTimeout: time.Minute,
	})
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	url, ln := serveOn(t, srv)
	return newFreezeProxy(t, ln.Addr().String(), upRate, downRate, 0), url
}

func noDrop(t *testing.T, ev *syncEvents, what string) {
	t.Helper()
	select {
	case <-ev.down:
		t.Fatalf("the connection was dropped %s", what)
	default:
	}
}

// A connection that goes silent is dropped once nothing came from the
// server for LivenessTimeout and a ping got no pong, as y-websocket drops
// one after 30 seconds without a message, and the client reconnects.
// Before, it stayed synced on the dead connection.
func TestClient_SilentConnectionRedialed(t *testing.T) {
	proxy, _ := liveServer(t, 0, 0)
	c, ev := dialThrough(t, proxy.url(), 2*time.Second, 81)

	proxy.freeze()
	start := time.Now()
	waitChan(t, "synced to turn false", ev.down)
	if d := time.Since(start); d < 1800*time.Millisecond {
		t.Errorf("dropped after %v, before LivenessTimeout", d)
	}
	select {
	case err := <-ev.errs:
		if !errors.Is(err, client.ErrServerSilent) {
			t.Errorf("OnError got %v, want ErrServerSilent", err)
		}
	case <-time.After(time.Second):
		t.Error("no OnError for the silent connection")
	}
	waitChan(t, "a new handshake", ev.up)
	if !c.Synced() {
		t.Error("not synced after the redial")
	}
}

// An idle healthy connection, the only one in its room, is kept: once the
// server has been silent for half the timeout the client pings, and the
// pong counts as hearing from it.
func TestClient_IdleConnectionKeptByPings(t *testing.T) {
	proxy, _ := liveServer(t, 0, 0)
	c, ev := dialThrough(t, proxy.url(), 2*time.Second, 82)
	time.Sleep(5 * time.Second)
	noDrop(t, ev, "while idle and healthy")
	if n := client.Pongs(c); n < 2 {
		t.Errorf("%d pongs in 5 seconds with a 2-second timeout, want at least 2", n)
	}
}

const bigBlob = 8 << 20

func blobLen(c *client.Client) int {
	b, _ := getKey(c, "blob").([]byte)
	return len(b)
}

func setBlob(c *client.Client) {
	m := mapOf(c)
	txn := c.Doc().WriteTxn()
	m.Set(txn, "blob", []byte(strings.Repeat("x", bigBlob)))
	txn.Commit()
}

func waitLong(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// A message that takes longer than LivenessTimeout to arrive is not cut:
// the check counts the bytes as they come, not whole messages. 8 MiB at
// 2 MiB/s takes 4 seconds with a 2-second timeout.
func TestClient_SlowDownloadNotCut(t *testing.T) {
	proxy, direct := liveServer(t, 0, 2<<20)
	c, ev := dialThrough(t, proxy.url(), 2*time.Second, 84)
	peer := dial(t, direct, "room", 85)
	waitFor(t, "peer synced", peer.Synced)

	start := time.Now()
	setBlob(peer)
	waitLong(t, "the blob through the slow proxy", func() bool { return blobLen(c) == bigBlob })
	if d := time.Since(start); d < 2*time.Second {
		t.Logf("the download took %v, not longer than the timeout; the test proves less", d)
	}
	noDrop(t, ev, "during a slow download")
}

// A message that takes longer than LivenessTimeout to send is not cut:
// the kernel taking it piece by piece is progress. 8 MiB at 2 MiB/s with a
// 3-second timeout: the data the kernel buffers hold after the last piece
// must drain before the pong comes back.
func TestClient_SlowUploadNotCut(t *testing.T) {
	proxy, direct := liveServer(t, 2<<20, 0)
	c, ev := dialThrough(t, proxy.url(), 3*time.Second, 86)
	peer := dial(t, direct, "room", 87)
	waitFor(t, "peer synced", peer.Synced)

	start := time.Now()
	setBlob(c)
	waitLong(t, "the blob at the peer", func() bool { return blobLen(peer) == bigBlob })
	if d := time.Since(start); d < 3*time.Second {
		t.Logf("the upload took %v, not longer than the timeout; the test proves less", d)
	}
	time.Sleep(time.Second)
	noDrop(t, ev, "during a slow upload")
}

// A dial through a path that takes the TCP connection and forwards
// nothing fails after LivenessTimeout and is tried again. Before, the
// WebSocket handshake waited for good.
func TestClient_DialBounded(t *testing.T) {
	proxy, _ := liveServer(t, 0, 0)
	proxy.freezeNew.Store(true)
	ev := newSyncEvents()
	c, err := client.New(ev.options(proxy.url(), time.Second, 88))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	for i := 0; i < 2; i++ {
		select {
		case <-ev.errs:
		case <-time.After(5 * time.Second):
			t.Fatalf("%d dial failures reported, want 2", i)
		}
	}
}

// A negative LivenessTimeout turns the check off: no session runs it, so
// a frozen connection stays.
func TestClient_LivenessCheckOff(t *testing.T) {
	proxy, _ := liveServer(t, 0, 0)
	c, ev := dialThrough(t, proxy.url(), -1, 83)
	if n := client.LiveChecks(c); n != 0 {
		t.Errorf("%d sessions ran the check, want 0", n)
	}
	proxy.freeze()
	time.Sleep(2 * time.Second)
	noDrop(t, ev, "with the check off")
}

// A LivenessTimeout below a second is raised to one. At 50ms the check's
// 100ms tick saw every pong as already stale and cut a healthy
// connection every few ticks.
func TestClient_LivenessTimeoutFloor(t *testing.T) {
	proxy, _ := liveServer(t, 0, 0)
	c, ev := dialThrough(t, proxy.url(), 50*time.Millisecond, 90)
	time.Sleep(2500 * time.Millisecond)
	noDrop(t, ev, "with a 50ms timeout on a healthy connection")
	if client.LiveChecks(c) != 1 || client.LivenessTimeoutOf(c) != time.Second {
		t.Errorf("%d sessions at %v, want 1 at 1s", client.LiveChecks(c), client.LivenessTimeoutOf(c))
	}
}

// With a custom TLS dialer on http.DefaultTransport, the connection to a
// wss URL is still counted. Go's Transport opens it with DialTLSContext,
// bypassing DialContext, and the check saw nothing read and cut every
// healthy session after the timeout.
func TestClient_LivenessWithCustomTLSDialer(t *testing.T) {
	srv := server.New(server.Options{OriginPatterns: []string{"*"}})
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	ts := httptest.NewTLSServer(srv.Handler())
	t.Cleanup(ts.Close)

	dt := http.DefaultTransport.(*http.Transport)
	saved := dt.Clone()
	cfg := ts.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	dt.TLSClientConfig = cfg
	dt.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := &tls.Dialer{Config: cfg}
		return d.DialContext(ctx, network, addr)
	}
	t.Cleanup(func() {
		dt.TLSClientConfig = saved.TLSClientConfig
		dt.DialTLSContext = saved.DialTLSContext
	})

	c, ev := dialThrough(t, "wss://"+strings.TrimPrefix(ts.URL, "https://"), time.Second, 91)
	time.Sleep(3 * time.Second)
	noDrop(t, ev, "over a custom TLS dialer")
	if client.Pongs(c) < 1 || client.LiveChecks(c) != 1 {
		t.Errorf("pongs %d, sessions %d; want pongs and one session", client.Pongs(c), client.LiveChecks(c))
	}
}

// clientGoroutines counts the goroutines running client code.
func clientGoroutines() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	count := 0
	for _, g := range strings.Split(string(buf[:n]), "\n\n") {
		if strings.Contains(g, "github.com/Deln0r/ygo/client.(*Client)") {
			count++
		}
	}
	return count
}

// The check's goroutines, a ping waiting for a pong included, end with
// the client.
func TestClient_LivenessGoroutinesEndWithClose(t *testing.T) {
	proxy, _ := liveServer(t, 0, 0)
	before := clientGoroutines()
	ev := newSyncEvents()
	c, err := client.New(ev.options(proxy.url(), time.Second, 89))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitChan(t, "the first handshake", ev.up)
	proxy.freeze()
	time.Sleep(700 * time.Millisecond) // a ping is out, unanswered
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for clientGoroutines() > before {
		if time.Now().After(deadline) {
			t.Fatalf("%d client goroutines after Close, %d before", clientGoroutines(), before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A client busy with what it read (a large update applying on a slow
// device, a callback that blocks) is not cut: the pong waits unread in the
// kernel meanwhile. Before, the check counted that time as the server's
// silence, dropped the connection, and the redial brought the same work
// back. Here OnSynced blocks for three timeouts.
func TestClient_BusyReaderNotCut(t *testing.T) {
	proxy, _ := liveServer(t, 0, 0)
	ev := newSyncEvents()
	opts := ev.options(proxy.url(), time.Second, 92)
	var first atomic.Bool
	inner := opts.OnSynced
	opts.OnSynced = func(v bool) {
		if v && first.CompareAndSwap(false, true) {
			time.Sleep(3 * time.Second)
		}
		inner(v)
	}
	c, err := client.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	select { // the handshake's OnSynced sleeps 3 seconds first
	case <-ev.up:
	case <-time.After(15 * time.Second):
		t.Fatal("no first handshake")
	}
	time.Sleep(2 * time.Second)
	noDrop(t, ev, "while the client was busy")
	if n := client.LiveChecks(c); n != 1 {
		t.Errorf("%d sessions, want 1", n)
	}
}

// A write that the kernel stops taking is cut even while the client is
// busy with what it read: the reply to the server's SyncStep1 is written
// from the read loop, and on a path that died mid-reply it would hold the
// loop, and with it the pause, for good. The client holds 32 MiB the
// server lacks, more than the kernel buffers on either side absorb (Linux
// loopback took all of 8 MiB); the proxy forwards 256 KiB of it and stops
// reading. The cut must come from the stalled-write rule.
func TestClient_StalledWriteCutWhileBusy(t *testing.T) {
	srv := server.New(server.Options{OriginPatterns: []string{"*"}, ReadLimit: 64 << 20})
	t.Cleanup(func() { _ = srv.Close(context.Background()) })
	_, ln := serveOn(t, srv)
	proxy := newFreezeProxy(t, ln.Addr().String(), 0, 0, 256<<10)

	d := doc.NewDocWithOptions(doc.Options{ClientID: 93})
	m := types.NewMap(d.Branch("settings"))
	txn := d.WriteTxn()
	m.Set(txn, "blob", []byte(strings.Repeat("x", 32<<20)))
	txn.Commit()
	ev := newSyncEvents()
	opts := ev.options(proxy.url(), 2*time.Second, 93)
	opts.Doc = d
	c, err := client.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	deadline := time.After(10 * time.Second)
	for {
		select {
		case err := <-ev.errs:
			if errors.Is(err, client.ErrServerSilent) {
				if client.StallCuts(c) == 0 {
					t.Fatal("cut, but not by the stalled-write rule: the kernel took the whole reply")
				}
				return
			}
		case <-deadline:
			t.Fatal("a write stalled in the read loop was never cut")
		}
	}
}

// tlsHandshakes counts goroutines stuck in a TLS handshake.
func tlsHandshakes() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "crypto/tls.(*Conn).handshakeContext")
}

// A deprecated DialTLS that returns an unhandshaken *tls.Conn, against a
// path that takes the TCP connection and never answers: every failed dial
// left a goroutine and a socket in a handshake nobody could cancel, since
// the wrapper handshook it with a context of its own. Now net/http
// handshakes it inside the request, whose cancel closes it.
func TestClient_DeprecatedDialTLSNoLeak(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var held []net.Conn
	var mu sync.Mutex
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c) // never read, never answered
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})

	dt := http.DefaultTransport.(*http.Transport)
	saved := dt.DialTLS                                         //nolint:staticcheck // the deprecated dialer is what is tested
	dt.DialTLS = func(network, addr string) (net.Conn, error) { //nolint:staticcheck
		raw, err := net.Dial(network, addr)
		if err != nil {
			return nil, err
		}
		return tls.Client(raw, &tls.Config{InsecureSkipVerify: true}), nil //nolint:gosec // never connects
	}
	t.Cleanup(func() { dt.DialTLS = saved }) //nolint:staticcheck

	before := tlsHandshakes()
	ev := newSyncEvents()
	c, err := client.New(ev.options("wss://"+ln.Addr().String(), time.Second, 94))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		select {
		case <-ev.errs:
		case <-time.After(5 * time.Second):
			t.Fatalf("%d dial failures, want 3", i)
		}
	}
	_ = c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for tlsHandshakes() > before {
		if time.Now().After(deadline) {
			t.Fatalf("%d handshakes left after Close, %d before", tlsHandshakes(), before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
