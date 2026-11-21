package server

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// pipeConns is a net.Listener of net.Pipe ends: a write completes only when
// the other end reads, so a client that does not read blocks the server.
type pipeConns struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func (l *pipeConns) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *pipeConns) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *pipeConns) Addr() net.Addr { return &net.UnixAddr{Name: "pipe", Net: "pipe"} }

// TestCloseForCause_ClientNotReadingWithinWriteTimeout: a connection the
// writer closes because its client fell behind (a full or stalled queue)
// is gone within WriteTimeout even when the client reads nothing: the
// close frame is a write to that peer, which WriteTimeout bounds.
// coder/websocket gives the close frame five seconds of its own, so with
// a short WriteTimeout the connection outlived it by five seconds
// (TestServer_NoWriterLeak timed out in CI when its queue stalled before
// a write did).
func TestCloseForCause_ClientNotReadingWithinWriteTimeout(t *testing.T) {
	for _, cause := range []closeCause{causeQueueStalled, causeQueueFull} {
		s := New(Options{WriteTimeout: 200 * time.Millisecond, OriginPatterns: []string{"*"}})
		l := &pipeConns{ch: make(chan net.Conn), done: make(chan struct{})}
		accepted := make(chan *conn, 1)
		release := make(chan struct{})
		hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw, rec := recordHijack(w)
			ws, err := websocket.Accept(rw, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				t.Error(err)
				return
			}
			accepted <- &conn{server: s, ws: ws, raw: rec.conn}
			<-release
		})}
		go func() { _ = hs.Serve(l) }()
		dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
			serverEnd, clientEnd := net.Pipe()
			l.ch <- serverEnd
			return clientEnd, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		client, _, err := websocket.Dial(ctx, "ws://pipe/room", &websocket.DialOptions{HTTPClient: &http.Client{Transport: &http.Transport{DialContext: dial}}})
		if err != nil {
			t.Fatal(err)
		}
		c := <-accepted
		start := time.Now()
		c.closeForCause(cause)
		if d := time.Since(start); d > time.Second {
			t.Errorf("cause %d: closing a client that reads nothing took %v, want about WriteTimeout (200ms)", cause, d)
		}
		close(release)
		_ = client.CloseNow()
		cancel()
		_ = hs.Close()
		_ = l.Close()
		_ = s.Close(context.Background())
	}
}
