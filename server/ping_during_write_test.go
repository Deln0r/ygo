package server_test

import (
	"context"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/server"
)

// slowDownProxy forwards TCP to addr, server to client at rate bytes per
// second, the other way at full speed. delivered, when not nil, counts the
// bytes sent to the client.
func slowDownProxy(t *testing.T, addr string, rate int, delivered *atomic.Int64) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var conns []net.Conn
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
			conns = append(conns, cc, sc)
			go func() { _, _ = io.Copy(sc, cc) }()
			go func() {
				buf := make([]byte, 16<<10)
				gap := time.Duration(int64(len(buf)) * int64(time.Second) / int64(rate))
				for {
					n, err := sc.Read(buf)
					if n > 0 {
						if _, werr := cc.Write(buf[:n]); werr != nil {
							return
						}
						if delivered != nil {
							delivered.Add(int64(n))
						}
						time.Sleep(gap * time.Duration(n) / time.Duration(len(buf)))
					}
					if err != nil {
						_ = cc.Close() // the server ended it: so does the client's side
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return "ws://" + ln.Addr().String()
}

// A ping that arrives while the server writes one long message to that
// client gets its pong, and the client is not dropped. The server answers
// pings from its read loop, and the pong needs the connection's frame
// lock; the message used to go out as one frame under that lock, so a
// write of more than 5 seconds made the pong fail and the read loop end
// the connection. Here 8 MiB reaches the client at 1 MiB/s.
func TestServer_PingDuringLongWrite(t *testing.T) {
	url, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"}, ReadLimit: 64 << 20,
		MaxSendQueueBytes: 64 << 20, WriteTimeout: time.Minute,
	})
	host := strings.TrimPrefix(strings.TrimPrefix(url, "ws://"), "http://")
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	var delivered atomic.Int64
	slowURL := slowDownProxy(t, host, 1<<20, &delivered)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	slow, _, err := websocket.Dial(ctx, slowURL+"/room", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.CloseNow()
	slow.SetReadLimit(-1)
	got := make(chan int, 1)
	readErr := make(chan error, 1)
	go func() {
		for {
			_, msg, err := slow.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			if len(msg) > 4<<20 {
				got <- len(msg)
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond) // the handshake frames are through

	peer, _, err := websocket.Dial(ctx, url+"/room", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.CloseNow()
	d := doc.NewDocWithOptions(doc.Options{ClientID: 77})
	m := types.NewMap(d.Branch("m"))
	txn := d.WriteTxn()
	m.Set(txn, "blob", []byte(strings.Repeat("x", 8<<20)))
	txn.Commit()
	if err := peer.Write(ctx, websocket.MessageBinary, syncpkg.EncodeSyncUpdate(encoding.EncodeStateAsUpdate(d))); err != nil {
		t.Fatal(err)
	}

	// Ping once the server is a megabyte into the 8 MiB write.
	for delivered.Load() < 1<<20 {
		time.Sleep(20 * time.Millisecond)
	}
	pctx, pcancel := context.WithTimeout(ctx, 15*time.Second)
	defer pcancel()
	if err := slow.Ping(pctx); err != nil {
		t.Fatalf("no pong during the long write: %v", err)
	}
	select {
	case n := <-got:
		if n < 8<<20 {
			t.Errorf("got %d bytes, want the whole update", n)
		}
	case err := <-readErr:
		t.Fatalf("the connection ended during the long write: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the update never arrived")
	}
}

// A slow client joins a room whose document takes longer than WriteTimeout
// to send, while a peer keeps editing, and converges. The write timeout
// bounded the whole message, so the initial sync was cut every time; and
// the peer's edits, queued while it was written, counted as the client
// falling behind its room. Now the timeout is how long a write may make no
// progress, and fan-out queued during the connection's own reply waits
// from the reply's end. Here 6 MiB goes at 1 MiB/s with a 2-second
// WriteTimeout and an edit every 300 ms.
func TestServer_SlowClientGetsLargeDocument(t *testing.T) {
	url, _ := startTestServer(t, server.Options{
		OriginPatterns: []string{"*"}, ReadLimit: 64 << 20,
		MaxSendQueueBytes: 64 << 20, WriteTimeout: 2 * time.Second,
	})
	host := strings.TrimPrefix(url, "ws://")
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	slowURL := slowDownProxy(t, host, 1<<20, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	peer, _, err := websocket.Dial(ctx, url+"/room", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.CloseNow()
	peer.SetReadLimit(-1)
	go func() { // drain what the server sends the peer
		for {
			if _, _, err := peer.Read(ctx); err != nil {
				return
			}
		}
	}()
	d := doc.NewDocWithOptions(doc.Options{ClientID: 78})
	m := types.NewMap(d.Branch("m"))
	txn := d.WriteTxn()
	m.Set(txn, "blob", []byte(strings.Repeat("y", 6<<20)))
	txn.Commit()
	if err := peer.Write(ctx, websocket.MessageBinary, syncpkg.EncodeSyncUpdate(encoding.EncodeStateAsUpdate(d))); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // the server has the document

	stop := make(chan struct{})
	defer close(stop)
	go func() { // a small edit every 300 ms
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(300 * time.Millisecond):
			}
			rtxn := d.ReadTxn()
			sv := rtxn.Store().GetStateVector()
			rtxn.Close()
			txn := d.WriteTxn()
			m.Set(txn, "n", int64(i))
			txn.Commit()
			rtxn = d.ReadTxn()
			diff := encoding.EncodeDiff(d, rtxn, sv)
			rtxn.Close()
			if err := peer.Write(ctx, websocket.MessageBinary, syncpkg.EncodeSyncUpdate(diff)); err != nil {
				return
			}
		}
	}()

	slow, _, err := websocket.Dial(ctx, slowURL+"/room", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.CloseNow()
	slow.SetReadLimit(-1)
	// Ask for the document, as a y-websocket client opens.
	if err := slow.Write(ctx, websocket.MessageBinary, syncpkg.EncodeSyncStep1([]byte{0})); err != nil {
		t.Fatal(err)
	}
	big, after := false, 0
	for after < 3 {
		_, msg, err := slow.Read(ctx)
		if err != nil {
			t.Fatalf("the slow client was cut (%v); document received: %v, updates after it: %d", err, big, after)
		}
		if len(msg) > 4<<20 {
			big = true
		} else if big {
			after++
		}
	}
}
