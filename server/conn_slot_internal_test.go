package server

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	syncpkg "github.com/Deln0r/ygo/internal/sync"
)

// acceptGate is an afterAccept hook that holds the first connection's
// handler right after its handshake until open, and lets every later one
// through.
type acceptGate struct {
	taken   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newAcceptGate() *acceptGate {
	return &acceptGate{entered: make(chan struct{}), release: make(chan struct{})}
}

func (g *acceptGate) hook() {
	if g.taken.CompareAndSwap(false, true) {
		close(g.entered)
		<-g.release
	}
}

func (g *acceptGate) open() { g.once.Do(func() { close(g.release) }) }

// startSlotServer serves s over httptest and returns its ws:// base URL.
func startSlotServer(t *testing.T, s *Server) string {
	t.Helper()
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		hs.Close()
		_ = s.Close(context.Background())
	})
	return "ws://" + strings.TrimPrefix(hs.URL, "http://")
}

func awaitTotalConns(t *testing.T, s *Server, want int64) {
	t.Helper()
	awaitTotalConnsWithin(t, s, want, 5*time.Second)
}

func awaitTotalConnsWithin(t *testing.T, s *Server, want int64, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for s.totalConns.Load() != want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := s.totalConns.Load(); got != want {
		t.Fatalf("totalConns = %d, want %d", got, want)
	}
}

func dialSlot(t *testing.T, ctx context.Context, url string, opts *websocket.DialOptions) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.Dial(ctx, url, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

// wantAdmitted reads the first frame the server sends an admitted
// connection: the y-websocket SyncStep1, or for Hocuspocus the reply to an
// authentication of key "a".
func wantAdmitted(t *testing.T, ctx context.Context, c *websocket.Conn, hocuspocus bool) {
	t.Helper()
	if hocuspocus {
		if err := c.Write(ctx, websocket.MessageBinary, append(syncpkg.EncodeRoute("a"), syncpkg.EncodeAuthToken("")...)); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatalf("the admitted connection: %v", err)
	}
}

// TestMaxConns_HandshakeOrderKeepsSlot: with MaxConns 1, a connection whose
// handshake completed holds the slot while its handler is still on its
// way to admission; a connection dialed after it is refused with 1013, and
// the first is admitted. The slot used to be taken after the handshake, so
// the later connection could take it first and the earlier one was closed
// (TestHocuspocusSocketAndDocumentLimits timed out in CI on that).
func TestMaxConns_HandshakeOrderKeepsSlot(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		hocuspocus bool
	}{{"y-websocket", "/room", false}, {"hocuspocus", "", true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Options{MaxConns: 1})
			g := newAcceptGate()
			s.afterAccept = g.hook
			url := startSlotServer(t, s)
			t.Cleanup(g.open)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			first := dialSlot(t, ctx, url+tc.path, nil)
			select {
			case <-g.entered:
			case <-ctx.Done():
				t.Fatal("the first handler never finished its handshake")
			}
			second := dialSlot(t, ctx, url+tc.path, nil)
			rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
			_, _, err := second.Read(rctx)
			rcancel()
			if websocket.CloseStatus(err) != websocket.StatusTryAgainLater {
				t.Fatalf("the later connection: %v, want close 1013", err)
			}
			g.open()
			wantAdmitted(t, ctx, first, tc.hocuspocus)
			if got := s.totalConns.Load(); got != 1 {
				t.Fatalf("totalConns = %d with one connection, want 1", got)
			}
			_ = first.Close(websocket.StatusNormalClosure, "done")
			awaitTotalConns(t, s, 0)
		})
	}
}

// TestMaxConns_SlotReturnedOnEveryExit: a slot taken before the handshake
// comes back on every way a connection can end without being admitted, a
// panic included, and when an admitted one ends; the next connection is
// then admitted and the count returns to zero.
func TestMaxConns_SlotReturnedOnEveryExit(t *testing.T) {
	load := func(docName string) ([]byte, error) {
		switch docName {
		case "fail":
			return nil, errors.New("load failure")
		case "panic":
			panic("load panic")
		}
		return nil, nil
	}
	connect := func(_, docName string, _ *http.Request) error {
		if docName == "panic-connect" {
			panic("connect panic")
		}
		if docName == "reject" {
			return errors.New("rejected")
		}
		return nil
	}
	for _, tc := range []struct {
		name     string
		maxConns int
		maxDocs  int
		hold     string // a room a connection holds during the attempt
		path     string
		origin   string
	}{
		{name: "handshake refused", maxConns: 1, path: "/room", origin: "http://elsewhere.example"},
		{name: "load error", maxConns: 1, path: "/fail"},
		{name: "load panic", maxConns: 1, path: "/panic"},
		{name: "OnConnect panic", maxConns: 1, path: "/panic-connect"},
		{name: "OnConnect rejection", maxConns: 1, path: "/reject"},
		{name: "document cap", maxConns: 2, maxDocs: 1, hold: "/held", path: "/other"},
		{name: "hocuspocus handshake refused", maxConns: 1, path: "", origin: "http://elsewhere.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Options{MaxConns: tc.maxConns, MaxDocs: tc.maxDocs, OnLoadDocument: load, OnConnect: connect})
			url := startSlotServer(t, s)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var held int64
			if tc.hold != "" {
				h := dialSlot(t, ctx, url+tc.hold, nil)
				wantAdmitted(t, ctx, h, false)
				held = 1
			}
			opts := &websocket.DialOptions{}
			if tc.origin != "" {
				opts.HTTPHeader = http.Header{"Origin": []string{tc.origin}}
			}
			if c, _, err := websocket.Dial(ctx, url+tc.path, opts); err == nil {
				_, _, _ = c.Read(ctx) // the server's close or a panic's reset
				_ = c.CloseNow()
			}
			awaitTotalConns(t, s, held)
			nextPath := "/ok"
			if tc.hold != "" {
				nextPath = tc.hold // the document cap admits a resident room only
			}
			next := dialSlot(t, ctx, url+nextPath, nil)
			wantAdmitted(t, ctx, next, false)
			_ = next.Close(websocket.StatusNormalClosure, "done")
			awaitTotalConns(t, s, held)
		})
	}
}

// TestMaxConns_UpgradeWithBodyRefused: an upgrade request that announces a
// body and withholds it is answered with 400 at once and holds no slot.
// Completing its handshake would make net/http read the body first, which
// the client can stall forever while the handler holds the connection slot.
func TestMaxConns_UpgradeWithBodyRefused(t *testing.T) {
	for _, path := range []string{"/room", "/"} {
		t.Run(path, func(t *testing.T) {
			s := New(Options{MaxConns: 1})
			url := startSlotServer(t, s)
			host := strings.TrimPrefix(url, "ws://")
			raw, err := net.Dial("tcp", host)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			req := "GET " + path + " HTTP/1.1\r\nHost: " + host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
				"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nContent-Length: 1\r\n\r\n"
			if _, err := raw.Write([]byte(req)); err != nil {
				t.Fatal(err)
			}
			_ = raw.SetReadDeadline(time.Now().Add(5 * time.Second))
			status, err := bufio.NewReader(raw).ReadString('\n')
			if err != nil || !strings.Contains(status, " 400 ") {
				t.Fatalf("response %q, %v; want 400", status, err)
			}
			awaitTotalConns(t, s, 0)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			next := dialSlot(t, ctx, url+"/ok", nil)
			wantAdmitted(t, ctx, next, false)
		})
	}
}

// TestMaxConns_SlotBackBeforeRefusalClose: a y-websocket connection refused
// at the document cap gives its slot back before its close, which waits for
// a client that does not answer (five seconds in coder/websocket, up to the
// close limit): within a second, while the refused handler still waits, the
// count has dropped and another connection is admitted.
func TestMaxConns_SlotBackBeforeRefusalClose(t *testing.T) {
	s := New(Options{MaxConns: 2, MaxDocs: 1})
	url := startSlotServer(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	held := dialSlot(t, ctx, url+"/held", nil)
	wantAdmitted(t, ctx, held, false)
	// Refused at the document cap; never read, so its close is not answered.
	_ = dialSlot(t, ctx, url+"/other", nil)
	awaitTotalConnsWithin(t, s, 1, time.Second)
	next := dialSlot(t, ctx, url+"/held", nil)
	wantAdmitted(t, ctx, next, false)
}

// TestMaxConns_StalledHandshakeWriteReturnsSlot: a client that sends an
// upgrade request and reads nothing cannot hold the handler, and its
// connection slot, in the handshake's writes: the 101 (over a pipe, a write
// completes only when the other end reads) and the error response to a key
// that is not valid, which quotes it, both end at WriteTimeout. The slot then
// comes back (for a 101 that failed, once the connection's first write or,
// for Hocuspocus, its AuthTimeout ends it) and a client that reads is
// admitted.
func TestMaxConns_StalledHandshakeWriteReturnsSlot(t *testing.T) {
	const key = "dGhlIHNhbXBsZSBub25jZQ=="
	for _, tc := range []struct {
		name, path, key string
		hocuspocus      bool
	}{
		{"y-websocket 101", "/room", key, false},
		{"hocuspocus 101", "/", key, true},
		{"y-websocket refusal", "/room", strings.Repeat("k", 32<<10), false},
		{"hocuspocus refusal", "/", strings.Repeat("k", 32<<10), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Options{MaxConns: 1, WriteTimeout: 500 * time.Millisecond, AuthTimeout: 700 * time.Millisecond, OriginPatterns: []string{"*"}})
			l := &pipeConns{ch: make(chan net.Conn), done: make(chan struct{})}
			hs := &http.Server{Handler: s.Handler()}
			go func() { _ = hs.Serve(l) }()
			t.Cleanup(func() {
				_ = hs.Close()
				_ = l.Close()
				_ = s.Close(context.Background())
			})
			serverEnd, stalled := net.Pipe()
			t.Cleanup(func() { _ = stalled.Close() })
			l.ch <- serverEnd
			req := "GET " + tc.path + " HTTP/1.1\r\nHost: pipe\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
				"Sec-WebSocket-Key: " + tc.key + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
			go func() { _, _ = stalled.Write([]byte(req)) }()
			// The handler takes the slot, then cannot write.
			awaitTotalConns(t, s, 1)
			awaitTotalConns(t, s, 0)

			dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
				a, b := net.Pipe()
				select {
				case l.ch <- a:
					return b, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			next := dialSlot(t, ctx, "ws://pipe"+tc.path, &websocket.DialOptions{HTTPClient: &http.Client{Transport: &http.Transport{DialContext: dial}}})
			wantAdmitted(t, ctx, next, tc.hocuspocus)
		})
	}
}
