package server_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	syncpkg "github.com/Deln0r/ygo/internal/sync"
	"github.com/Deln0r/ygo/internal/types"
	"github.com/Deln0r/ygo/server"
)

// benchServer spins a server behind httptest and returns its ws:// base
// plus a cleanup. Self-contained so the benchmarks do not depend on the
// *testing.T-typed test helpers.
func benchServer(b *testing.B) (string, func()) {
	b.Helper()
	srv := server.New(server.Options{OriginPatterns: []string{"*"}})
	ts := httptest.NewServer(srv.Handler())
	wsURL := "ws://" + strings.TrimPrefix(ts.URL, "http://")
	return wsURL, func() {
		ts.Close()
		_ = srv.Close(context.Background())
	}
}

func benchDial(b *testing.B, wsURL, docName string) *websocket.Conn {
	b.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL+"/"+docName, nil)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	return c
}

// BenchmarkServer_BroadcastFanout measures the round-trip cost of one
// update through a room of N connections: the sender writes a fixed
// SyncUpdate and reads its own broadcast echo while N-1 idle peers drain
// their copies. ns/op grows with the fan-out width, isolating the
// server's apply + fan-out cost from client-side encoding (the payload is
// pre-built and constant).
func BenchmarkServer_BroadcastFanout(b *testing.B) {
	// Fixed small update: one array item.
	src := doc.NewDocWithOptions(doc.Options{ClientID: 1})
	arr := types.NewArray(src.Branch("items"))
	txn := src.WriteTxn()
	arr.Push(txn, "x")
	txn.Commit()
	env := syncpkg.EncodeSyncUpdate(encoding.EncodeStateAsUpdate(src))

	for _, peers := range []int{2, 10, 50} {
		b.Run(fmt.Sprintf("peers=%d", peers), func(b *testing.B) {
			wsURL, cleanup := benchServer(b)
			defer cleanup()
			ctx := context.Background()

			// N-1 receivers, each draining in the background until closed.
			var wg sync.WaitGroup
			receivers := make([]*websocket.Conn, 0, peers-1)
			for i := 0; i < peers-1; i++ {
				rc := benchDial(b, wsURL, "bench")
				receivers = append(receivers, rc)
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						if _, _, err := rc.Read(ctx); err != nil {
							return
						}
					}
				}()
			}

			// Sender: drain the initial SyncStep1, then loop.
			sc := benchDial(b, wsURL, "bench")
			readInitial(b, sc)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				writeOrFatal(b, sc, env)
				readUntilSyncUpdate(b, sc)
			}
			b.StopTimer()

			_ = sc.Close(websocket.StatusNormalClosure, "done")
			for _, rc := range receivers {
				_ = rc.Close(websocket.StatusNormalClosure, "done")
			}
			wg.Wait()
		})
	}
}

// BenchmarkServer_ConnectHandshake measures the per-connection cost of a
// dial + WebSocket upgrade + admission + initial-sync read + close, with
// the document kept resident by a long-lived anchor so the number
// reflects connection admission rather than document load/evict churn.
func BenchmarkServer_ConnectHandshake(b *testing.B) {
	wsURL, cleanup := benchServer(b)
	defer cleanup()
	ctx := context.Background()

	// Anchor keeps the room resident across the loop.
	anchor := benchDial(b, wsURL, "bench")
	readInitial(b, anchor)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if _, _, err := anchor.Read(ctx); err != nil {
				return
			}
		}
	}()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := benchDial(b, wsURL, "bench")
		readInitial(b, c)
		_ = c.Close(websocket.StatusNormalClosure, "done")
	}
	b.StopTimer()

	_ = anchor.Close(websocket.StatusNormalClosure, "done")
	wg.Wait()
}

func readInitial(b *testing.B, c *websocket.Conn) {
	b.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); err != nil {
		b.Fatalf("read initial sync: %v", err)
	}
}

func writeOrFatal(b *testing.B, c *websocket.Conn, env []byte) {
	b.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, env); err != nil {
		b.Fatalf("write: %v", err)
	}
}

func readUntilSyncUpdate(b *testing.B, c *websocket.Conn) {
	b.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, raw, err := c.Read(ctx)
		if err != nil {
			b.Fatalf("read echo: %v", err)
		}
		f, _, derr := syncpkg.DecodeEnvelope(raw)
		if derr != nil {
			continue
		}
		if f.Type == syncpkg.MessageSync && f.SyncSub == syncpkg.SyncUpdate {
			return
		}
	}
}
