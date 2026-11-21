package server

import (
	"context"
	"errors"
	"io"
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

// slowCloseSetup accepts one WebSocket over a pipe and returns the server's
// conn (as the writer holds it), the client, and the client's raw end.
func slowCloseSetup(t *testing.T, writeTimeout time.Duration) (*conn, *websocket.Conn, net.Conn) {
	t.Helper()
	s := New(Options{WriteTimeout: writeTimeout, OriginPatterns: []string{"*"}})
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
	var clientRaw net.Conn
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		serverEnd, clientEnd := net.Pipe()
		clientRaw = clientEnd
		l.ch <- serverEnd
		return clientEnd, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	client, _, err := websocket.Dial(ctx, "ws://pipe/room", &websocket.DialOptions{HTTPClient: &http.Client{Transport: &http.Transport{DialContext: dial}}})
	if err != nil {
		t.Fatal(err)
	}
	c := <-accepted
	t.Cleanup(func() {
		close(release)
		_ = client.CloseNow()
		cancel()
		_ = hs.Close()
		_ = l.Close()
		_ = s.Close(context.Background())
	})
	return c, client, clientRaw
}

// TestCloseForCause_FrameBoundedByWriteTimeout: the writer's close for a
// client that fell behind (a full or a stalled queue) gives the close frame
// WriteTimeout, as any write to that client, and the client's reply its
// usual time. coder/websocket gives the frame five seconds of its own, so a
// client that read nothing stayed connected five seconds past a short
// WriteTimeout (TestServer_NoWriterLeak timed out in CI when its queue
// filled before a write blocked).
func TestCloseForCause_FrameBoundedByWriteTimeout(t *testing.T) {
	t.Run("a client that reads nothing", func(t *testing.T) {
		for _, cause := range []closeCause{causeQueueStalled, causeQueueFull} {
			c, client, _ := slowCloseSetup(t, 200*time.Millisecond)
			start := time.Now()
			c.closeForCause(cause)
			if d := time.Since(start); d > time.Second {
				t.Errorf("cause %d: the close took %v, want about WriteTimeout (200ms)", cause, d)
			}
			// The server's transport is closed, and the client's read ends.
			if _, err := c.raw.Write([]byte{0}); err == nil {
				t.Errorf("cause %d: the server's transport is still open", cause)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if _, _, err := client.Read(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("cause %d: the client's read: %v, want the connection gone", cause, err)
			}
			cancel()
		}
	})
	t.Run("a client that reads late but within WriteTimeout gets the code", func(t *testing.T) {
		c, client, _ := slowCloseSetup(t, time.Second)
		got := make(chan error, 1)
		go func() {
			time.Sleep(300 * time.Millisecond)
			_, _, err := client.Read(context.Background())
			got <- err
		}()
		c.closeForCause(causeQueueStalled)
		err := <-got
		if websocket.CloseStatus(err) != websocket.StatusTryAgainLater {
			t.Fatalf("the client read %v, want close 1013", err)
		}
	})
	t.Run("a reply after WriteTimeout to a frame taken in time completes", func(t *testing.T) {
		// The client takes the close frame at once from its raw end and
		// answers 1.5 s later, past WriteTimeout (1 s): the frame went out
		// in time, so the close waits for the reply instead of cutting at
		// WriteTimeout.
		c, _, raw := slowCloseSetup(t, time.Second)
		go func() {
			buf := make([]byte, 64)
			if _, err := raw.Read(buf); err != nil {
				return
			}
			time.Sleep(1500 * time.Millisecond)
			// A masked close frame, code 1013, mask key 0.
			_, _ = raw.Write([]byte{0x88, 0x82, 0, 0, 0, 0, 0x03, 0xf5})
			_, _ = io.Copy(io.Discard, raw)
		}()
		start := time.Now()
		c.closeForCause(causeQueueStalled)
		if d := time.Since(start); d < 1400*time.Millisecond {
			t.Fatalf("the close ended after %v, before the client's reply at 1.5 s", d)
		}
	})
}
