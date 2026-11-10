//go:build !js

package client

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// trackedConn is the TCP connection under a session's WebSocket, seen by
// the liveness check: every byte read, and every chunk the kernel takes.
// TLS and an HTTP proxy run on top of it unchanged.
type trackedConn struct {
	net.Conn
	l *liveness
}

// writeChunk splits a large write so the kernel's taking it shows up
// piece by piece.
const writeChunk = 16 << 10

func (c *trackedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.l.read()
	}
	return n, err
}

func (c *trackedConn) Write(p []byte) (int, error) {
	// Stamp before counting, so the check never sees a write in progress
	// with the previous write's stamp.
	c.l.lastWriteActivity.Store(c.l.now())
	c.l.writing.Add(1)
	defer c.l.writing.Add(-1)
	total := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > writeChunk {
			chunk = chunk[:writeChunk]
		}
		c.l.lastWriteActivity.Store(c.l.now())
		n, err := c.Conn.Write(chunk)
		total += n
		if n > 0 {
			c.l.wrote(n)
			c.l.lastWriteActivity.Store(c.l.now())
		}
		if err != nil {
			return total, err
		}
		p = p[n:]
	}
	return total, nil
}

// track makes every connection t opens a trackedConn, through whichever
// dialers t has: a custom TLS dialer, when set, opens the connection for
// https and for an https proxy without DialContext, and the deprecated
// Dial and DialTLS are kept rather than replaced.
func track(t *http.Transport, l *liveness) {
	wrap := func(nc net.Conn, err error) (net.Conn, error) {
		if err != nil {
			return nil, err
		}
		return &trackedConn{Conn: nc, l: l}, nil
	}
	switch {
	case t.DialContext != nil:
		dial := t.DialContext
		t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return wrap(dial(ctx, network, addr))
		}
	case t.Dial != nil: //nolint:staticcheck // the caller's deprecated dialer, kept
		dial := t.Dial                                          //nolint:staticcheck
		t.Dial = func(network, addr string) (net.Conn, error) { //nolint:staticcheck
			return wrap(dial(network, addr))
		}
	default:
		dial := (&net.Dialer{}).DialContext
		t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return wrap(dial(ctx, network, addr))
		}
	}
	// net/http handshakes a *tls.Conn from a custom TLS dialer itself and
	// refuses an h2 one for a WebSocket upgrade; behind a trackedConn it
	// sees neither, so the wrapper does both first, bounded by the dial's
	// context. The deprecated DialTLS has no context to bound a handshake
	// with, so its connection is wrapped as it comes and handshakes on its
	// first write, which the request's cancel can still end.
	wrapTLS := func(ctx context.Context, nc net.Conn, err error) (net.Conn, error) {
		if err != nil {
			return nil, err
		}
		if tc, ok := nc.(*tls.Conn); ok {
			if err := tc.HandshakeContext(ctx); err != nil {
				_ = tc.Close()
				return nil, err
			}
			if tc.ConnectionState().NegotiatedProtocol == "h2" {
				_ = tc.Close()
				return nil, errors.New("client: the TLS dialer negotiated h2; a WebSocket upgrade needs HTTP/1.1")
			}
		}
		return wrap(nc, nil)
	}
	if dial := t.DialTLSContext; dial != nil {
		t.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			nc, err := dial(ctx, network, addr)
			return wrapTLS(ctx, nc, err)
		}
	}
	if dial := t.DialTLS; dial != nil { //nolint:staticcheck
		t.DialTLS = func(network, addr string) (net.Conn, error) { //nolint:staticcheck
			return wrap(dial(network, addr))
		}
	}
}

// dial opens a session's connection. With the liveness check on, it is
// bounded by LivenessTimeout and runs over a trackedConn, and the returned
// liveness is non-nil.
func (c *Client) dial(ctx context.Context, url string) (*websocket.Conn, *liveness, error) {
	timeout := c.opts.LivenessTimeout
	base, ok := http.DefaultTransport.(*http.Transport)
	if timeout < 0 || !ok {
		// Off, or an http.DefaultTransport replaced by something whose
		// connections cannot be tracked: no check for this client.
		conn, _, err := websocket.Dial(ctx, url, nil)
		return conn, nil, err
	}
	l := newLiveness(&c.pongs)
	t := base.Clone()
	track(t, l)
	c.liveChecks.Add(1)
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, url, &websocket.DialOptions{HTTPClient: &http.Client{Transport: t}})
	// The upgraded connection is the caller's now; anything left in the
	// pool (a connection a redirect left behind) goes.
	t.CloseIdleConnections()
	if err != nil {
		return nil, nil, err
	}
	l.read()
	l.lastMark.Store(l.now())
	return conn, l, nil
}

// watch runs the liveness check for one session until ctx ends, and
// reports whether it dropped the connection. Every tick (a tenth of the
// timeout, at least 100ms) it looks at what the connection did. Once
// nothing was read for half the timeout it pings, which an RFC 6455 server
// answers with a pong. It drops the connection when nothing was read for
// the whole timeout, no write made progress in that time, and a ping went
// unanswered for half the timeout counted in ticks: after the process was
// suspended the check runs that long again before it drops anything, so a
// pong that arrived meanwhile is read first.
func (c *Client) watch(ctx context.Context, conn *websocket.Conn, l *liveness) bool {
	timeout := c.opts.LivenessTimeout
	tick := max(timeout/10, 100*time.Millisecond)
	needTicks := int((timeout/2 + tick - 1) / tick)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	var wg sync.WaitGroup
	defer wg.Wait()
	pingDone := make(chan struct{}, 1)
	pinging := false
	ticksSincePing := -1 // -1: no ping since the last read
	for {
		select {
		case <-ctx.Done():
			return false
		case <-pingDone:
			pinging = false
		case <-ticker.C:
			now := l.now()
			sinceRead := time.Duration(now - max(l.lastRead.Load(), l.lastIdle.Load()))
			if sinceRead < timeout/2 {
				ticksSincePing = -1
				continue
			}
			// A write of which the kernel took no 16 KiB chunk for the
			// whole timeout, with nothing read for half of it: the path is
			// dead, busy read loop or not.
			if l.writing.Load() > 0 && time.Duration(now-l.lastWriteActivity.Load()) >= timeout {
				c.stallCuts.Add(1)
				_ = conn.CloseNow()
				return true
			}
			// The read loop is busy with a frame (an update applying, a
			// callback, a reply being written): what the server sent since
			// waits unread, so the silence is the client's own.
			if l.busy.Load() {
				ticksSincePing = -1
				continue
			}
			if !pinging {
				pinging = true
				wg.Add(1)
				go func() {
					defer wg.Done()
					if conn.Ping(ctx) == nil {
						l.pongCount.Add(1)
					}
					pingDone <- struct{}{}
				}()
			}
			if ticksSincePing < 0 {
				ticksSincePing = 0
				continue
			}
			ticksSincePing++
			sinceMark := time.Duration(now - l.lastMark.Load())
			if ticksSincePing >= needTicks && sinceRead >= timeout && sinceMark >= timeout {
				_ = conn.CloseNow()
				return true
			}
		}
	}
}
