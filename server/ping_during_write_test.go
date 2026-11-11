package server_test

import (
	"context"
	"io"
	"net"
	"strings"
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
// second, the other way at full speed.
func slowDownProxy(t *testing.T, addr string, rate int) string {
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
						time.Sleep(gap * time.Duration(n) / time.Duration(len(buf)))
					}
					if err != nil {
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
	slowURL := slowDownProxy(t, host, 1<<20)

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

	time.Sleep(1500 * time.Millisecond) // the server is writing the 8 MiB now
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
