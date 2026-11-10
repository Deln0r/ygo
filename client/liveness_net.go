//go:build !js

package client

import (
	"context"
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
	total := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > writeChunk {
			chunk = chunk[:writeChunk]
		}
		n, err := c.Conn.Write(chunk)
		total += n
		if n > 0 {
			c.l.wrote(n)
		}
		if err != nil {
			return total, err
		}
		p = p[n:]
	}
	return total, nil
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
	dialTCP := t.DialContext
	if dialTCP == nil {
		dialTCP = (&net.Dialer{}).DialContext
	}
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		nc, err := dialTCP(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &trackedConn{Conn: nc, l: l}, nil
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, url, &websocket.DialOptions{HTTPClient: &http.Client{Transport: t}})
	if err != nil {
		t.CloseIdleConnections()
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
			sinceRead := time.Duration(now - l.lastRead.Load())
			if sinceRead < timeout/2 {
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
